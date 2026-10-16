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
//! Layout: `spawn` holds the single entry point, command configuration and
//! privilege setup; `pty` allocates the terminal pair and resizes it; `io` owns
//! the output pumps and the terminal policy; `cleanup` tears process groups
//! down; `child` is the clone/exec mechanism underneath all of them.

use std::sync::{Arc, Mutex};

use tokio::sync::{oneshot, Notify};

use crate::process::{OutputBus, Subscription};

mod child;
mod cleanup;
mod io;
mod pty;
mod spawn;

pub use cleanup::kill_process_group;
pub use io::{write_pty, InputHandle, InputWriter, PumpEvent};
pub use pty::resize_pty;
pub use spawn::{merged_env, resolve_cwd, spawn, Spawn};

#[derive(Debug)]
pub struct SpawnedProcess {
    pub pid: u32,
    /// First subscription on the process's output bus. Created before the pump
    /// task is spawned so it never misses an early event (a subscription sees
    /// only events published after it attaches — there is no replay of earlier
    /// history). `Connect` attaches a later subscription via
    /// `sender.subscribe()`; the pump task keeps the bus alive for the child's
    /// whole lifetime.
    pub initial: Subscription,
    /// The bus handle, kept so `Connect` can attach an Nth subscription to a
    /// running process.
    pub sender: Arc<OutputBus>,
    /// A duplicate of the pty master fd (None for a pipe-spawned process),
    /// kept so `Update` can resize the window while the pump owns the original.
    pub pty_master: Option<std::fs::File>,
    /// Writable stdin/pty endpoint retained for the input RPC family.
    pub input: InputHandle,
    /// Resolves as soon as the direct child has been reaped. The process
    /// service owns this receiver so deadline cancellation and table cleanup
    /// follow the child's real lifetime: draining the remaining output and
    /// publishing the terminal event stay independent of it, so a subscriber
    /// that is not reading can never delay reaping.
    pub completion: oneshot::Receiver<()>,
    /// Terminal event cache shared with the process table. A Connect racing
    /// with terminal publication can use this cache to receive the complete
    /// End/SpawnError event instead of attaching after the terminal event was
    /// published and observing a bare channel close.
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
