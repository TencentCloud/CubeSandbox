// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! The process table: which processes are running, how to find them by pid or
//! tag, and how their output buses and per-command cgroup leaves are reached.
//!
//! This is process-domain runtime state, not shared daemon state: `ProcEntry`
//! owns wire config, the output bus, the input endpoint and the cgroup leaf, so
//! the table lives with the domain that produces and consumes those values.
//! `app/state.rs` embeds it in the composition root.

use std::collections::HashMap;
#[cfg(test)]
use std::os::unix::io::RawFd;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};

use tokio::sync::broadcast;

use crate::platform::lock::lock;
use crate::process::cgroup::{self, Manager, ProcType};
use crate::process::engine;
use crate::process::wire::ProcessConfig;

type ProcessControl = (
    u32,
    Option<Arc<cgroup::ProcessCgroup>>,
    Arc<Mutex<Option<String>>>,
);

pub struct ProcEntry {
    pub pid: u32,
    pub tag: Option<String>,
    pub config: ProcessConfig,
    /// Output bus the pump publishes on. Held so `Connect` can attach a new
    /// subscriber to an already-running process via `sender.subscribe()`.
    pub sender: broadcast::Sender<engine::PumpEvent>,
    /// Duplicate of the pty master fd (None for a pipe-spawned process), kept
    /// so `Update` can resize the window while the pump owns the original.
    pub pty_master: Option<std::fs::File>,
    /// Process-owned writable endpoint used by SendInput/StreamInput and
    /// CloseStdin. Cloned out of the table before any async write is awaited.
    pub input: engine::InputHandle,
    /// Optional per-command cgroup leaf. The supervisor retains its own clone
    /// after removing the entry so escaped descendants can still be killed and
    /// the leaf removed without keeping the process table visible.
    pub cgroup: Option<Arc<cgroup::ProcessCgroup>>,
    /// Shared cause marker consumed by the output pump when it publishes End.
    pub termination: Arc<std::sync::Mutex<Option<String>>>,
    /// Terminal event published by the output pump. This closes the small
    /// Connect-vs-exit race where a subscriber could otherwise attach after
    /// the broadcast terminal event and wait forever for a channel close.
    pub terminal: Arc<Mutex<Option<engine::PumpEvent>>>,
}

/// Opaque, process-lifetime-unique key for a live process in the table.
/// Keying by this instead of by pid is what prevents a finished process's
/// cleanup from evicting a *different* process that the OS happened to give
/// the same recycled pid.
pub type ProcHandle = u64;

/// Outcome of `ProcessTable::resize_pty`, split so the caller can map each case to
/// the right Connect error: `NotFound` for a selector resolving to no live
/// process, `NotAPty` for a live process started without a pty, and `Io` for
/// an ioctl failure (e.g. the pty was already torn down).
#[derive(Debug)]
pub enum PtyResizeError {
    NotFound,
    NotAPty,
    Io(std::io::Error),
}

/// Resolve a flat selector to a live entry, mirroring the pid-wins / most
/// recent-tag-wins rule used by `find_pid` and `subscribe`.
fn find_entry<'a>(
    processes: &'a HashMap<ProcHandle, ProcEntry>,
    pid: Option<u32>,
    tag: Option<&str>,
) -> Option<&'a ProcEntry> {
    if let Some(p) = pid {
        processes.values().find(|e| e.pid == p)
    } else if let Some(t) = tag {
        processes
            .iter()
            .filter(|(_, e)| e.tag.as_deref() == Some(t))
            .max_by_key(|(handle, _)| **handle)
            .map(|(_, e)| e)
    } else {
        None
    }
}

pub struct ProcessTable {
    processes: Mutex<HashMap<ProcHandle, ProcEntry>>,
    next_handle: AtomicU64,
    /// cgroup v2 subtree manager. Non-`Option`: startup failure is
    /// a `NoopManager` instance, not an absent value (mirrors upstream
    /// `createCgroupManager`'s named return + defer swap). `new()` always
    /// starts with the no-op fallback so unit tests never touch
    /// /sys/fs/cgroup; the daemon swaps in the real manager exactly once at
    /// startup. Runtime allocation failures reject the
    /// request; existing processes retain their leaf handles for cleanup.
    cgroup: Arc<dyn Manager>,
}

impl ProcessTable {
    pub fn new(cgroup: Arc<dyn Manager>) -> Self {
        Self {
            processes: Mutex::new(HashMap::new()),
            next_handle: AtomicU64::new(1),
            cgroup,
        }
    }

    /// cgroup dir fd for `t`, or `None` under the Noop fallback. Handed to
    /// `engine::spawn` at the process service layer (mirrors upstream
    /// `getProcType` + `GetFileDescriptor` in handler.go).
    #[cfg(test)]
    pub fn cgroup_fd(&self, t: ProcType) -> Option<RawFd> {
        self.cgroup.fd(t)
    }

    /// Allocate a per-command cgroup; runtime failures reject the request
    /// rather than starting an unconfined command.
    pub fn create_process_cgroup(
        &self,
        t: ProcType,
    ) -> std::io::Result<Option<Arc<cgroup::ProcessCgroup>>> {
        self.cgroup.create_process(t)
    }

    pub fn insert_process(&self, entry: ProcEntry) -> ProcHandle {
        let handle = self.next_handle.fetch_add(1, Ordering::Relaxed);
        lock(&self.processes).insert(handle, entry);
        handle
    }

    /// Remove a process by the handle returned from `insert_process`. Keying
    /// on the handle (not the pid) means a finished process only ever evicts
    /// its own entry, even if the OS has already recycled its pid into a newer
    /// process recorded in the table.
    pub fn remove_process(&self, handle: ProcHandle) {
        lock(&self.processes).remove(&handle);
    }

    /// Cache a terminal event before removing a process entry. Existing
    /// subscribers still receive it through the broadcast bus; a new
    /// subscriber racing in the removal window receives the cached event via
    /// a one-shot broadcast channel.
    pub fn mark_terminal(&self, handle: ProcHandle, event: engine::PumpEvent) {
        let guard = lock(&self.processes);
        if let Some(entry) = guard.get(&handle) {
            *lock(&entry.terminal) = Some(event);
        }
    }

    pub fn list_processes(&self) -> Vec<(u32, Option<String>, ProcessConfig)> {
        let guard = lock(&self.processes);
        let mut out: Vec<_> = guard
            .values()
            .map(|e| (e.pid, e.tag.clone(), e.config.clone()))
            .collect();
        out.sort_by_key(|(pid, _, _)| *pid);
        out
    }

    /// Resolve a flat ProcessSelector (pid or tag) to a live pid. When several
    /// live processes share a tag the most recently started one wins, matching
    /// how a caller reusing a tag would expect the latest to be addressed.
    pub fn find_pid(&self, pid: Option<u32>, tag: Option<&str>) -> Option<u32> {
        let guard = lock(&self.processes);
        if let Some(p) = pid {
            return guard.values().any(|e| e.pid == p).then_some(p);
        }
        if let Some(t) = tag {
            return guard
                .iter()
                .filter(|(_, e)| e.tag.as_deref() == Some(t))
                .max_by_key(|(handle, _)| **handle)
                .map(|(_, e)| e.pid);
        }
        None
    }

    /// Resolve the signalling target together with its per-command cgroup.
    /// Cloning the Arc releases the process-table lock before cgroup.kill or
    /// kill(2), while the supervisor's handle key still prevents PID-reuse
    /// cleanup from removing a newer entry.
    pub fn process_control(&self, pid: Option<u32>, tag: Option<&str>) -> Option<ProcessControl> {
        let guard = lock(&self.processes);
        find_entry(&guard, pid, tag)
            .map(|entry| (entry.pid, entry.cgroup.clone(), entry.termination.clone()))
    }

    /// Resolve a selector to a live process and subscribe to its output bus.
    /// `Connect` attaches this way: the fresh `broadcast::Receiver` starts at
    /// the current head of the ring, so it sees only events published after
    /// the attach (no replay of history). Resolution mirrors `find_pid` — an
    /// explicit pid wins, otherwise the most recent tag match.
    pub fn subscribe(
        &self,
        pid: Option<u32>,
        tag: Option<&str>,
    ) -> Option<(u32, broadcast::Receiver<engine::PumpEvent>)> {
        let guard = lock(&self.processes);
        find_entry(&guard, pid, tag).map(|e| {
            // Subscribe before inspecting the terminal cache. The pump writes
            // the cache and publishes the terminal event without holding the
            // process-table lock; checking first would leave a race window in
            // which Connect misses the event and later observes only a closed
            // bus. If the cache was already populated, replace the receiver
            // with a one-shot channel carrying the cached event.
            let receiver = e.sender.subscribe();
            if let Some(terminal) = lock(&e.terminal).clone() {
                let (sender, receiver) = broadcast::channel(1);
                let _ = sender.send(terminal);
                (e.pid, receiver)
            } else {
                (e.pid, receiver)
            }
        })
    }

    /// Resolve a selector and clone its process-owned input endpoint. The
    /// process-table lock is released before callers await the input mutex or
    /// perform I/O, so one blocked stdin cannot stall unrelated RPCs.
    pub fn input_handle(&self, pid: Option<u32>, tag: Option<&str>) -> Option<engine::InputHandle> {
        let guard = lock(&self.processes);
        find_entry(&guard, pid, tag).map(|e| e.input.clone())
    }

    /// Resize the pty window of a live process selected by pid or tag. The
    /// ioctl happens under the process-table lock — it is a fast, non-blocking
    /// syscall and holding the lock keeps the entry alive for the duration.
    pub fn resize_pty(
        &self,
        pid: Option<u32>,
        tag: Option<&str>,
        cols: u16,
        rows: u16,
    ) -> Result<(), PtyResizeError> {
        let guard = lock(&self.processes);
        let entry = find_entry(&guard, pid, tag).ok_or(PtyResizeError::NotFound)?;
        let master = entry.pty_master.as_ref().ok_or(PtyResizeError::NotAPty)?;
        engine::resize_pty(master, cols, rows).map_err(PtyResizeError::Io)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Build a ProcEntry with a throwaway broadcast bus — these tests exercise
    /// pid/tag resolution and reaping, never the output bus itself.
    fn proc_entry(pid: u32, tag: Option<&str>) -> ProcEntry {
        let (sender, _rx) = broadcast::channel::<engine::PumpEvent>(1);
        ProcEntry {
            pid,
            tag: tag.map(String::from),
            config: ProcessConfig::default(),
            sender,
            pty_master: None,
            input: Arc::new(tokio::sync::Mutex::new(engine::InputWriter::Pipe(None))),
            cgroup: None,
            termination: Arc::new(Mutex::new(None)),
            terminal: Arc::new(Mutex::new(None)),
        }
    }

    /// The two lines the process service depends on: a fresh
    /// `ProcessTable::new(NoopManager)` has no cgroup manager (`fd() == None` —
    /// unit tests never probe the host tree), and constructing it with a real
    /// manager makes the same call return `Some`. A stub manager stands in for
    /// `Cgroup2Manager` (whose constructor needs a real cgroup v2 mount).
    #[test]
    fn cgroup_fd_defaults_noop_then_with_cgroup_swaps_in() {
        struct Stub;
        impl Manager for Stub {
            fn fd(&self, _t: ProcType) -> Option<RawFd> {
                Some(42)
            }
        }

        let s = ProcessTable::new(Arc::new(crate::process::cgroup::NoopManager));
        assert_eq!(s.cgroup_fd(ProcType::User), None);
        assert_eq!(s.cgroup_fd(ProcType::Pty), None);

        let s = ProcessTable::new(Arc::new(Stub));
        assert_eq!(s.cgroup_fd(ProcType::User), Some(42));
        assert_eq!(s.cgroup_fd(ProcType::Pty), Some(42));
    }

    #[test]
    fn process_table_selectors() {
        let s = ProcessTable::new(Arc::new(crate::process::cgroup::NoopManager));
        let h = s.insert_process(proc_entry(42, Some("t1")));
        assert_eq!(s.find_pid(Some(42), None), Some(42));
        assert_eq!(s.find_pid(None, Some("t1")), Some(42));
        assert_eq!(s.find_pid(Some(41), None), None);
        assert_eq!(s.find_pid(None, Some("nope")), None);
        s.remove_process(h);
        assert_eq!(s.find_pid(None, Some("t1")), None);
    }

    #[test]
    fn remove_by_handle_does_not_evict_recycled_pid() {
        // A finished process and a newer one share the same recycled pid; the
        // old one's cleanup must not evict the live entry.
        let s = ProcessTable::new(Arc::new(crate::process::cgroup::NoopManager));
        let old = s.insert_process(proc_entry(100, Some("old")));
        let _new = s.insert_process(proc_entry(100, Some("new")));
        s.remove_process(old);
        // The pid is still live (owned by the newer process) and its tag wins.
        assert_eq!(s.find_pid(Some(100), None), Some(100));
        assert_eq!(s.find_pid(None, Some("new")), Some(100));
        assert_eq!(s.find_pid(None, Some("old")), None);
    }

    #[tokio::test]
    async fn subscribe_resolves_and_skips_pre_attach_history() {
        let s = ProcessTable::new(Arc::new(crate::process::cgroup::NoopManager));
        let (tx, _rx) = broadcast::channel::<engine::PumpEvent>(4);
        let data = |v: &str| {
            engine::PumpEvent::Data(crate::process::wire::DataEvent {
                stdout: Some(v.into()),
                ..Default::default()
            })
        };
        // An event published before the attach is history: a Connect subscriber
        // starts at the current ring head and must not see it.
        assert!(tx.send(data("before")).is_ok());

        s.insert_process(ProcEntry {
            pid: 7,
            tag: Some("t".into()),
            config: ProcessConfig::default(),
            sender: tx.clone(),
            pty_master: None,
            input: Arc::new(tokio::sync::Mutex::new(engine::InputWriter::Pipe(None))),
            cgroup: None,
            termination: Arc::new(Mutex::new(None)),
            terminal: Arc::new(Mutex::new(None)),
        });

        // pid and tag both resolve to the same live process.
        let (pid, mut rx) = s.subscribe(Some(7), None).expect("resolve by pid");
        assert_eq!(pid, 7);
        assert_eq!(
            s.subscribe(None, Some("t")).map(|(p, _)| p),
            Some(7),
            "resolve by tag"
        );

        // Only the post-attach event is delivered — "before" is not replayed.
        assert!(tx.send(data("after")).is_ok());
        match rx.recv().await.expect("post-attach event") {
            engine::PumpEvent::Data(d) => assert_eq!(d.stdout.as_deref(), Some("after")),
            _ => panic!("expected a Data event"),
        }

        // Unknown selectors resolve to none.
        assert!(s.subscribe(Some(999), None).is_none());
        assert!(s.subscribe(None, Some("nope")).is_none());
    }

    #[tokio::test]
    async fn subscribe_after_terminal_publication_gets_cached_event() {
        let s = ProcessTable::new(Arc::new(crate::process::cgroup::NoopManager));
        let (sender, _rx) = broadcast::channel::<engine::PumpEvent>(4);
        let handle = s.insert_process(ProcEntry {
            pid: 9,
            tag: Some("finished".into()),
            config: ProcessConfig::default(),
            sender,
            pty_master: None,
            input: Arc::new(tokio::sync::Mutex::new(engine::InputWriter::Pipe(None))),
            cgroup: None,
            termination: Arc::new(Mutex::new(None)),
            terminal: Arc::new(Mutex::new(None)),
        });
        let terminal = engine::PumpEvent::End(crate::process::wire::EndEvent {
            exit_code: 0,
            exited: true,
            status: "exit status 0".into(),
            error: None,
            signal: None,
            oom_killed: None,
            killed_by: None,
        });
        s.mark_terminal(handle, terminal.clone());

        let (_, mut events) = s.subscribe(Some(9), None).expect("finished entry remains");
        assert!(matches!(events.recv().await, Ok(engine::PumpEvent::End(_))));
        s.remove_process(handle);
        assert!(s.subscribe(Some(9), None).is_none());
    }

    #[test]
    fn resize_pty_resolves_and_reports() {
        let s = ProcessTable::new(Arc::new(crate::process::cgroup::NoopManager));

        // Unknown selector → NotFound.
        assert!(matches!(
            s.resize_pty(Some(42), None, 80, 24),
            Err(PtyResizeError::NotFound)
        ));

        // A live process with no pty → NotAPty.
        let _h = s.insert_process(proc_entry(7, Some("no-pty")));
        assert!(matches!(
            s.resize_pty(Some(7), None, 80, 24),
            Err(PtyResizeError::NotAPty)
        ));

        // A live process whose "pty" is not a terminal → ioctl fails → Io.
        let not_a_tty = std::fs::File::open("/dev/null").unwrap();
        let (sender, _rx) = broadcast::channel::<engine::PumpEvent>(1);
        s.insert_process(ProcEntry {
            pid: 8,
            tag: Some("bad-pty".into()),
            config: ProcessConfig::default(),
            sender,
            pty_master: Some(not_a_tty),
            input: Arc::new(tokio::sync::Mutex::new(engine::InputWriter::Pipe(None))),
            cgroup: None,
            termination: Arc::new(Mutex::new(None)),
            terminal: Arc::new(Mutex::new(None)),
        });
        assert!(matches!(
            s.resize_pty(Some(8), None, 80, 24),
            Err(PtyResizeError::Io(_))
        ));
    }
}
