// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Shared daemon state: /init-injected env vars, optional access token and
//! the table of processes started through `process.Process/Start`.

use std::collections::HashMap;
#[cfg(test)]
use std::os::unix::io::RawFd;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex, MutexGuard, PoisonError, RwLock, RwLockReadGuard, RwLockWriteGuard};

use tokio::sync::broadcast;

use crate::cgroup::{self, Manager, ProcType};
use crate::exec;
use crate::msg::process::ProcessConfig;

type ProcessControl = (
    u32,
    Option<Arc<cgroup::ProcessCgroup>>,
    Arc<Mutex<Option<String>>>,
);

/// Recover a poisoned lock instead of propagating the panic. A single handler
/// panicking while holding one of these locks must not brick the daemon for
/// every later request (#1227: no silent failure, but also no cascading death);
/// the guarded data is a plain map/option with no cross-field invariant that a
/// half-finished write could corrupt, so taking the inner guard is safe.
fn lock<T>(m: &Mutex<T>) -> MutexGuard<'_, T> {
    m.lock().unwrap_or_else(PoisonError::into_inner)
}
fn read<T>(l: &RwLock<T>) -> RwLockReadGuard<'_, T> {
    l.read().unwrap_or_else(PoisonError::into_inner)
}
fn write<T>(l: &RwLock<T>) -> RwLockWriteGuard<'_, T> {
    l.write().unwrap_or_else(PoisonError::into_inner)
}

pub struct ProcEntry {
    pub pid: u32,
    pub tag: Option<String>,
    pub config: ProcessConfig,
    /// Output bus the pump publishes on. Held so `Connect` can attach a new
    /// subscriber to an already-running process via `sender.subscribe()`.
    pub sender: broadcast::Sender<exec::PumpEvent>,
    /// Duplicate of the pty master fd (None for a pipe-spawned process), kept
    /// so `Update` can resize the window while the pump owns the original.
    pub pty_master: Option<std::fs::File>,
    /// Process-owned writable endpoint used by SendInput/StreamInput and
    /// CloseStdin. Cloned out of the table before any async write is awaited.
    pub input: exec::InputHandle,
    /// Optional per-command cgroup leaf. The supervisor retains its own clone
    /// after removing the entry so escaped descendants can still be killed and
    /// the leaf removed without keeping the process table visible.
    pub cgroup: Option<Arc<cgroup::ProcessCgroup>>,
    /// Shared cause marker consumed by the output pump when it publishes End.
    pub termination: Arc<std::sync::Mutex<Option<String>>>,
    /// Terminal event published by the output pump. This closes the small
    /// Connect-vs-exit race where a subscriber could otherwise attach after
    /// the broadcast terminal event and wait forever for a channel close.
    pub terminal: Arc<Mutex<Option<exec::PumpEvent>>>,
}

/// Opaque, process-lifetime-unique key for a live process in the table.
/// Keying by this instead of by pid is what prevents a finished process's
/// cleanup from evicting a *different* process that the OS happened to give
/// the same recycled pid.
pub type ProcHandle = u64;

/// Outcome of `AppState::resize_pty`, split so the caller can map each case to
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

pub struct AppState {
    env_vars: RwLock<HashMap<String, String>>,
    access_token: RwLock<Option<String>>,
    /// User assumed when a request names none. `/init`'s `defaultUser`
    /// overrides it; until then it mirrors upstream's compile-time constant
    /// "root", which `/init` only replaces when the field is present and
    /// non-empty.
    default_user: RwLock<String>,
    /// Working directory supplied by `/init` (`defaultWorkdir`). Upstream
    /// substitutes it only for an *empty* path
    /// (`execcontext.ResolveDefaultWorkdir`), so the file surface rarely sees
    /// it while `process.Start` without a cwd does.
    default_workdir: RwLock<Option<String>>,
    /// Nanosecond high-water mark of the `/init` timestamps that were applied
    /// (upstream `utils.AtomicMax`). Setting the system clock from it is
    /// deliberately NOT implemented — see the declared differences in
    /// `rest/mod.rs`.
    last_set_time: Mutex<i64>,
    /// Set once the first /init lands so `envd --version` probes and health
    /// checks are unaffected either way.
    pub initialized: AtomicBool,
    processes: Mutex<HashMap<ProcHandle, ProcEntry>>,
    next_handle: AtomicU64,
    /// cgroup v2 subtree manager (item 1.8). Non-`Option`: startup failure is
    /// a `NoopManager` instance, not an absent value (mirrors upstream
    /// `createCgroupManager`'s named return + defer swap, plan §0.1). `new()`
    /// always starts with the no-op fallback so unit tests never touch
    /// /sys/fs/cgroup; `main.rs` swaps in the real manager exactly once via
    /// `with_cgroup(cgroup::init())`. Runtime allocation failures reject the
    /// request; existing processes retain their leaf handles for cleanup.
    cgroup: Arc<dyn Manager>,
}

impl AppState {
    pub fn new() -> Self {
        let mut env_vars = HashMap::new();
        // Upstream envd exposes E2B_SANDBOX through /envs and command
        // environments; with -isnotfc (the only mode CubeSandbox runs) the
        // value is "false".
        env_vars.insert("E2B_SANDBOX".to_string(), "false".to_string());
        Self {
            env_vars: RwLock::new(env_vars),
            access_token: RwLock::new(None),
            default_user: RwLock::new(crate::auth::DEFAULT_USER.to_string()),
            default_workdir: RwLock::new(None),
            last_set_time: Mutex::new(0),
            initialized: AtomicBool::new(false),
            processes: Mutex::new(HashMap::new()),
            next_handle: AtomicU64::new(1),
            cgroup: Arc::new(cgroup::NoopManager),
        }
    }

    /// Startup wiring (main.rs calls this once): swap in the real cgroup
    /// manager and keep it for the daemon lifetime. `new()` stays no-op so
    /// every unit test constructs `AppState` without probing the host cgroup
    /// tree; the manager choice is then fixed by this single call.
    pub fn with_cgroup(mut self, cgroup: Arc<dyn Manager>) -> Self {
        self.cgroup = cgroup;
        self
    }

    /// cgroup dir fd for `t`, or `None` under the Noop fallback. Handed to
    /// `exec::spawn` at the process service layer (mirrors upstream
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

    /// Merge (not replace) env vars — matches the Go envd baseline: repeated
    /// /init calls accumulate variables.
    pub fn merge_env_vars(&self, vars: HashMap<String, String>) {
        let mut guard = write(&self.env_vars);
        guard.extend(vars);
        self.initialized.store(true, Ordering::Relaxed);
    }

    pub fn env_vars(&self) -> HashMap<String, String> {
        read(&self.env_vars).clone()
    }

    pub fn set_access_token(&self, token: String) {
        *write(&self.access_token) = Some(token);
    }

    /// Borrow the configured token so `/init` can validate the token carried
    /// in its body without cloning the secret.
    pub fn access_token(&self) -> RwLockReadGuard<'_, Option<String>> {
        read(&self.access_token)
    }

    pub fn default_user(&self) -> String {
        read(&self.default_user).clone()
    }

    pub fn default_workdir(&self) -> Option<String> {
        read(&self.default_workdir).clone()
    }

    /// Apply `/init`'s `defaultUser` / `defaultWorkdir`. Upstream ignores both
    /// when the field is absent *or* an empty string, so an empty value must
    /// not wipe the previous default.
    pub fn apply_init_defaults(&self, user: Option<&str>, workdir: Option<&str>) {
        if let Some(user) = user.filter(|u| !u.is_empty()) {
            *write(&self.default_user) = user.to_string();
        }
        if let Some(workdir) = workdir.filter(|w| !w.is_empty()) {
            *write(&self.default_workdir) = Some(workdir.to_string());
        }
    }

    /// `/init` timestamp gate (upstream `utils.AtomicMax.SetToGreater`):
    /// returns true when the request may update the state and raises the
    /// high-water mark. A request without a timestamp always proceeds.
    pub fn claim_timestamp(&self, incoming: Option<i64>) -> bool {
        let mut guard = lock(&self.last_set_time);
        if !timestamp_gate(*guard, incoming) {
            return false;
        }
        if let Some(nanos) = incoming {
            *guard = nanos;
        }
        true
    }

    /// Returns Err(()) when a token has been configured via /init and the
    /// provided header value does not match. When no token was configured
    /// the check always passes (baseline: uninitialized envd ignores
    /// X-Access-Token entirely). The comparison is constant-time so a caller
    /// cannot recover the token byte-by-byte from response timing.
    pub fn check_access_token(&self, header: Option<&str>) -> Result<(), ()> {
        match read(&self.access_token).as_deref() {
            None => Ok(()),
            Some(expected) => match header {
                Some(got) if constant_time_eq(expected.as_bytes(), got.as_bytes()) => Ok(()),
                _ => Err(()),
            },
        }
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
    pub fn mark_terminal(&self, handle: ProcHandle, event: exec::PumpEvent) {
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
    ) -> Option<(u32, broadcast::Receiver<exec::PumpEvent>)> {
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
    pub fn input_handle(&self, pid: Option<u32>, tag: Option<&str>) -> Option<exec::InputHandle> {
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
        exec::resize_pty(master, cols, rows).map_err(PtyResizeError::Io)
    }
}

/// `/init` timestamp comparison, mirroring `utils.AtomicMax.SetToGreater`
/// (an older request is dropped; an equal one passes and refreshes the mark).
/// This is protocol surface — a contract for retrying orchestrators — not a
/// local hot path: Cubelet never sends a timestamp at all (envVars-only).
pub fn timestamp_gate(prev_nanos: i64, incoming: Option<i64>) -> bool {
    match incoming {
        None => true,
        Some(nanos) => prev_nanos <= nanos,
    }
}

/// Length-aware constant-time byte comparison. Runs in time independent of
/// where the first mismatch is (the length check leaks only the token length,
/// not its contents), so token verification can't be turned into a timing
/// oracle. Small enough not to warrant a dependency.
pub(crate) fn constant_time_eq(a: &[u8], b: &[u8]) -> bool {
    if a.len() != b.len() {
        return false;
    }
    let mut diff = 0u8;
    for (x, y) in a.iter().zip(b.iter()) {
        diff |= x ^ y;
    }
    diff == 0
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Build a ProcEntry with a throwaway broadcast bus — these tests exercise
    /// pid/tag resolution and reaping, never the output bus itself.
    fn proc_entry(pid: u32, tag: Option<&str>) -> ProcEntry {
        let (sender, _rx) = broadcast::channel::<exec::PumpEvent>(1);
        ProcEntry {
            pid,
            tag: tag.map(String::from),
            config: ProcessConfig::default(),
            sender,
            pty_master: None,
            input: Arc::new(tokio::sync::Mutex::new(exec::InputWriter::Pipe(None))),
            cgroup: None,
            termination: Arc::new(Mutex::new(None)),
            terminal: Arc::new(Mutex::new(None)),
        }
    }

    #[test]
    fn init_env_vars_merge_not_replace() {
        let s = AppState::new();
        s.merge_env_vars(HashMap::from([("A".into(), "1".into())]));
        s.merge_env_vars(HashMap::from([("B".into(), "2".into())]));
        let vars = s.env_vars();
        assert_eq!(vars.get("A").map(String::as_str), Some("1"));
        assert_eq!(vars.get("B").map(String::as_str), Some("2"));
        assert_eq!(vars.get("E2B_SANDBOX").map(String::as_str), Some("false"));
    }

    /// The two lines the process service depends on (plan §3.4): a fresh
    /// `AppState` has no cgroup manager (`fd() == None` — unit tests never
    /// probe the host tree), and `with_cgroup` swaps in the real one so the
    /// same call starts returning `Some`. A stub manager stands in for
    /// `Cgroup2Manager` (whose constructor needs a real cgroup v2 mount).
    #[test]
    fn cgroup_fd_defaults_noop_then_with_cgroup_swaps_in() {
        struct Stub;
        impl Manager for Stub {
            fn fd(&self, _t: ProcType) -> Option<RawFd> {
                Some(42)
            }
        }

        let s = AppState::new();
        assert_eq!(s.cgroup_fd(ProcType::User), None);
        assert_eq!(s.cgroup_fd(ProcType::Pty), None);

        let s = s.with_cgroup(Arc::new(Stub));
        assert_eq!(s.cgroup_fd(ProcType::User), Some(42));
        assert_eq!(s.cgroup_fd(ProcType::Pty), Some(42));
    }

    #[test]
    fn access_token_semantics() {
        let s = AppState::new();
        // Uninitialized: any header (or none) passes.
        assert!(s.check_access_token(None).is_ok());
        assert!(s.check_access_token(Some("whatever")).is_ok());
        s.set_access_token("secret".into());
        assert!(s.check_access_token(Some("secret")).is_ok());
        assert!(s.check_access_token(Some("wrong")).is_err());
        // Length mismatch and prefix match both rejected.
        assert!(s.check_access_token(Some("secretx")).is_err());
        assert!(s.check_access_token(Some("sec")).is_err());
        assert!(s.check_access_token(None).is_err());
    }

    #[test]
    fn timestamp_gate_matches_atomic_max() {
        // Upstream utils.AtomicMax.SetToGreater: older is rejected, an equal
        // timestamp passes (and is stored again).
        assert!(timestamp_gate(0, Some(1)));
        assert!(timestamp_gate(10, Some(10)));
        assert!(!timestamp_gate(10, Some(9)));
        // No timestamp at all: /init always applies its data.
        assert!(timestamp_gate(10, None));
    }

    #[test]
    fn init_defaults_ignore_absent_and_empty() {
        let s = AppState::new();
        assert_eq!(s.default_user(), "root");
        assert_eq!(s.default_workdir(), None);
        // Empty strings must not wipe the previous default.
        s.apply_init_defaults(Some(""), Some(""));
        assert_eq!(s.default_user(), "root");
        assert_eq!(s.default_workdir(), None);
        // Absent fields are no-ops as well.
        s.apply_init_defaults(None, None);
        assert_eq!(s.default_user(), "root");
        // Non-empty values take effect and survive later empty/absent ones.
        s.apply_init_defaults(Some("user"), Some("/home/user"));
        assert_eq!(s.default_user(), "user");
        assert_eq!(s.default_workdir().as_deref(), Some("/home/user"));
        s.apply_init_defaults(Some(""), None);
        assert_eq!(s.default_user(), "user");
        assert_eq!(s.default_workdir().as_deref(), Some("/home/user"));
    }

    #[test]
    fn claim_timestamp_tracks_high_water_mark() {
        let s = AppState::new();
        // First /init with any timestamp wins (the mark starts at 0).
        assert!(s.claim_timestamp(Some(1000)));
        // Same timestamp: allowed again (upstream SetToGreater semantics).
        assert!(s.claim_timestamp(Some(1000)));
        // Older: dropped — and the state must stay untouched.
        assert!(!s.claim_timestamp(Some(999)));
        // Newer: applied, raising the mark.
        assert!(s.claim_timestamp(Some(1001)));
        assert!(!s.claim_timestamp(Some(1000)));
        // No timestamp: always applied, without moving the mark.
        assert!(s.claim_timestamp(None));
        assert!(s.claim_timestamp(Some(1001)));
    }

    #[test]
    fn constant_time_eq_basics() {
        assert!(constant_time_eq(b"", b""));
        assert!(constant_time_eq(b"abc", b"abc"));
        assert!(!constant_time_eq(b"abc", b"abd"));
        assert!(!constant_time_eq(b"abc", b"ab"));
        assert!(!constant_time_eq(b"ab", b"abc"));
    }

    #[test]
    fn process_table_selectors() {
        let s = AppState::new();
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
        let s = AppState::new();
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
        let s = AppState::new();
        let (tx, _rx) = broadcast::channel::<exec::PumpEvent>(4);
        let data = |v: &str| {
            exec::PumpEvent::Data(crate::msg::process::DataEvent {
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
            input: Arc::new(tokio::sync::Mutex::new(exec::InputWriter::Pipe(None))),
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
            exec::PumpEvent::Data(d) => assert_eq!(d.stdout.as_deref(), Some("after")),
            _ => panic!("expected a Data event"),
        }

        // Unknown selectors resolve to none.
        assert!(s.subscribe(Some(999), None).is_none());
        assert!(s.subscribe(None, Some("nope")).is_none());
    }

    #[tokio::test]
    async fn subscribe_after_terminal_publication_gets_cached_event() {
        let s = AppState::new();
        let (sender, _rx) = broadcast::channel::<exec::PumpEvent>(4);
        let handle = s.insert_process(ProcEntry {
            pid: 9,
            tag: Some("finished".into()),
            config: ProcessConfig::default(),
            sender,
            pty_master: None,
            input: Arc::new(tokio::sync::Mutex::new(exec::InputWriter::Pipe(None))),
            cgroup: None,
            termination: Arc::new(Mutex::new(None)),
            terminal: Arc::new(Mutex::new(None)),
        });
        let terminal = exec::PumpEvent::End(crate::msg::process::EndEvent {
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
        assert!(matches!(events.recv().await, Ok(exec::PumpEvent::End(_))));
        s.remove_process(handle);
        assert!(s.subscribe(Some(9), None).is_none());
    }

    #[test]
    fn resize_pty_resolves_and_reports() {
        let s = AppState::new();

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
        let (sender, _rx) = broadcast::channel::<exec::PumpEvent>(1);
        s.insert_process(ProcEntry {
            pid: 8,
            tag: Some("bad-pty".into()),
            config: ProcessConfig::default(),
            sender,
            pty_master: Some(not_a_tty),
            input: Arc::new(tokio::sync::Mutex::new(exec::InputWriter::Pipe(None))),
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
