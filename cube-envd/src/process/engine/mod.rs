// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Child-process engine: how a command is spawned, wired to pipes or a pty,
//! fed, reaped and cleaned up. Knows nothing about RPC or wire types beyond
//! the event types it publishes.
//!
//! Contract: upstream `internal/services/process/` spawn path and the
//! `exec.Cmd` behaviours it mirrors (session/controlling terminal, credential
//! drop, process-group and cgroup placement, output draining).
//!
//! Existing `engine::*` entry points remain available while spawn configuration,
//! PTY setup, IO and process-group cleanup live in private submodules.

use std::sync::{Arc, Mutex};

use tokio::sync::{broadcast, oneshot, Notify};

mod child;
mod cleanup;
mod io;
mod pty;
mod spawn;

pub use cleanup::kill_process_group;
pub use io::{write_pty, InputHandle, InputWriter, PumpEvent};
pub use pty::{resize_pty, spawn_pty_with_cgroup};
pub use spawn::{merged_env, resolve_cwd, spawn_with_cgroup};

#[cfg(test)]
pub use pty::spawn_pty;

#[derive(Debug)]
pub struct SpawnedProcess {
    pub pid: u32,
    /// First subscriber on the process's output bus. Created before the pump
    /// task is spawned so it never misses an early event (a broadcast receiver
    /// sees only events published after it subscribes — there is no replay of
    /// pre-subscription history). `Connect` attaches a later subscriber via
    /// `sender.subscribe()`; the pump task keeps the bus alive for the child's
    /// whole lifetime.
    pub initial: broadcast::Receiver<PumpEvent>,
    /// A clone of the bus's Sender, kept so `Connect` can hand a fresh
    /// receiver to an Nth subscriber attaching to a running process.
    pub sender: broadcast::Sender<PumpEvent>,
    /// A duplicate of the pty master fd (None for a pipe-spawned process),
    /// kept so `Update` can resize the window while the pump owns the original.
    pub pty_master: Option<std::fs::File>,
    /// Writable stdin/pty endpoint retained for the input RPC family.
    pub input: InputHandle,
    /// Resolves after the direct child has been reaped and the terminal event
    /// has been cached/published. The process service owns this receiver so
    /// deadline cancellation and table cleanup follow the child's real
    /// lifetime, not HTTP response backpressure.
    pub completion: oneshot::Receiver<()>,
    /// Terminal event cache shared with the process table. A Connect racing
    /// with terminal publication can use this cache to receive the complete
    /// End/SpawnError event instead of subscribing after the broadcast head
    /// and observing a bare channel close.
    pub terminal: Arc<std::sync::Mutex<Option<PumpEvent>>>,
    /// Fired immediately after `child.wait()` returns, before output-drain
    /// grace. Deadline supervision uses this signal so a child that exited
    /// before its deadline is never misclassified merely because a detached
    /// descendant kept stdout/stderr open.
    pub reaped: Arc<Notify>,
    /// Shared termination metadata set by the supervisor or SendSignal
    /// before killing the process. The output pump folds it into EndEvent
    /// without changing the legacy status/error fields.
    pub termination: Arc<Mutex<Option<String>>>,
    /// Filled by the process service immediately after spawn. Keeping this
    /// indirection avoids making the low-level spawn API depend on cgroup
    /// allocation ordering while still allowing the pump to inspect
    /// memory.events before publishing the terminal event.
    pub cgroup: Arc<Mutex<Option<Arc<crate::process::cgroup::ProcessCgroup>>>>,
}

#[cfg(test)]
mod tests {
    use crate::platform::identity::User;

    pub(super) fn current_user() -> User {
        // Run exec tests as the invoking user so they work unprivileged.
        let uid = unsafe { libc::getuid() };
        let gid = unsafe { libc::getgid() };
        User {
            name: "test".into(),
            uid,
            gid,
            home: std::env::var("HOME").unwrap_or_else(|_| "/tmp".into()),
            groups: vec![gid],
        }
    }
}
