// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Process waiting, deadline supervision and cgroup cleanup.

use std::sync::Arc;

use tokio::sync::broadcast;

use super::metadata;
use crate::process::cgroup;
use crate::process::engine;
use crate::process::table::ProcessTable;

pub(crate) const PROCESS_REAP_GRACE: std::time::Duration = std::time::Duration::from_secs(2);

#[allow(clippy::too_many_arguments)]
pub(crate) async fn supervise_process(
    table: Arc<ProcessTable>,
    handle: crate::process::table::ProcHandle,
    pid: u32,
    sender: broadcast::Sender<engine::PumpEvent>,
    mut completion: tokio::sync::oneshot::Receiver<()>,
    deadline: Option<std::time::Duration>,
    cgroup: Option<Arc<cgroup::ProcessCgroup>>,
    reaped: Arc<tokio::sync::Notify>,
    termination: Arc<std::sync::Mutex<Option<String>>>,
) {
    if let Some(deadline) = deadline {
        let reaped_signal = reaped.notified();
        tokio::pin!(reaped_signal);
        tokio::select! {
            // Prefer a direct-child reap that became ready at the same instant
            // as the deadline; a process that already exited is not timed out
            // merely because output-drain grace is still running.
            biased;
            _ = &mut reaped_signal => {
                let result = tokio::time::timeout(PROCESS_REAP_GRACE, &mut completion).await;
                let monitor_ok = matches!(result, Ok(Ok(())));
                if !monitor_ok {
                    let _ = kill_process_tree(pid, cgroup.as_ref());
                    let error = engine::PumpEvent::SpawnError(
                        "process monitor stopped before reporting exit".into(),
                    );
                    table.mark_terminal(handle, error.clone());
                    table.remove_process(handle);
                    let _ = sender.send(error);
                } else {
                    table.remove_process(handle);
                }
                if monitor_ok {
                    kill_descendants_and_cleanup(pid, cgroup).await;
                } else {
                    cleanup_process_cgroup(cgroup).await;
                }
            }
            _ = tokio::time::sleep(deadline) => {
                // Remove first so a concurrent Connect/Input/Update cannot
                // attach to a command whose deadline has already expired.
                table.remove_process(handle);
                // Publish the deadline marker before signalling the child.
                // cgroup.kill can make the pump race to publish End on another
                // runtime worker; ordering this event first guarantees every
                // still-attached stream observes deadline_exceeded rather than
                // a misleading normal End.
                let _ = sender.send(engine::PumpEvent::DeadlineExceeded);
                // Keep the timeout marker and kill syscall atomic with
                // respect to EndEvent decoration and SendSignal.
                let kill_result =
                    metadata::with_cause(&termination, "timeout", || {
                        kill_process_tree(pid, cgroup.as_ref())
                    });
                if let Err(e) = kill_result {
                    if e.raw_os_error() != Some(libc::ESRCH) {
                        tracing::warn!("pid {pid}: deadline kill failed: {e}");
                    }
                }
                // Retain supervision while streams await the child's EndEvent.
                // The direct child must actually be reaped. Bound this wait
                // so a failed/denied kill cannot leak the supervisor forever.
                if tokio::time::timeout(PROCESS_REAP_GRACE, &mut completion)
                    .await
                    .is_err()
                {
                    tracing::warn!(
                        "pid {pid}: timed out waiting for direct child reap after deadline kill"
                    );
                }
                cleanup_process_cgroup(cgroup).await;
            }
        }
    } else {
        let monitor_ok = completion.await.is_ok();
        if !monitor_ok {
            let _ = kill_process_tree(pid, cgroup.as_ref());
            let error = engine::PumpEvent::SpawnError(
                "process monitor stopped before reporting exit".into(),
            );
            table.mark_terminal(handle, error.clone());
            table.remove_process(handle);
            let _ = sender.send(error);
        } else {
            table.remove_process(handle);
        }
        if monitor_ok {
            kill_descendants_and_cleanup(pid, cgroup).await;
        } else {
            cleanup_process_cgroup(cgroup).await;
        }
    }
}

/// Kill the direct process group, using the per-command cgroup as the stronger
/// mechanism when available. cgroup.kill is the only operation that reaches a
/// descendant which called setsid(); a missing/failed cgroup falls back to the
/// established process-group signal.
pub(crate) fn kill_process_tree(
    pid: u32,
    cgroup: Option<&Arc<cgroup::ProcessCgroup>>,
) -> std::io::Result<()> {
    if let Some(cgroup) = cgroup {
        match cgroup.kill_all() {
            Ok(()) => return Ok(()),
            Err(e) if e.raw_os_error() == Some(libc::ESRCH) => {
                // An empty per-command leaf is authoritative: the direct
                // child has already gone away. Do not fall back to
                // kill(-pid), whose process-group id may have been recycled
                // for an unrelated process.
                return Err(e);
            }
            Err(e) => {
                tracing::warn!("pid {pid}: cgroup.kill failed, falling back to process group: {e}")
            }
        }
    }
    engine::kill_process_group(pid, libc::SIGKILL)
}

pub(crate) async fn kill_descendants_and_cleanup(
    pid: u32,
    cgroup: Option<Arc<cgroup::ProcessCgroup>>,
) {
    let cgroup_result = if let Some(cgroup_ref) = cgroup.as_ref() {
        match cgroup_ref.kill_all() {
            Ok(()) => Some(true),
            Err(e) if e.raw_os_error() == Some(libc::ESRCH) => {
                // Empty cgroup means there are no descendants to clean. In
                // particular, do not signal a potentially recycled pgid.
                Some(false)
            }
            Err(e) => {
                tracing::warn!("pid {pid}: cgroup.kill after exit failed: {e}");
                None
            }
        }
    } else {
        None
    };

    // A Noop/degraded cgroup manager still needs to reap descendants that
    // stayed in the original process group. Only cgroup.kill reaches a
    // setsid() escapee; when no cgroup is available, retain the established
    // process-group fallback for ordinary descendants.
    if cgroup_result.is_none() {
        if let Err(group_error) = engine::kill_process_group(pid, libc::SIGKILL) {
            if group_error.raw_os_error() != Some(libc::ESRCH) {
                tracing::warn!("pid {pid}: process-group descendant cleanup failed: {group_error}");
            }
        }
    }
    cleanup_process_cgroup(cgroup).await;
}

pub(crate) async fn cleanup_process_cgroup(cgroup: Option<Arc<cgroup::ProcessCgroup>>) {
    let Some(cgroup) = cgroup else {
        return;
    };
    let deadline = tokio::time::Instant::now() + std::time::Duration::from_secs(2);
    loop {
        match cgroup.remove_if_empty() {
            Ok(()) => return,
            Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                if tokio::time::Instant::now() >= deadline {
                    tracing::warn!(
                        "cgroup: timed out removing process leaf {}",
                        cgroup.path().display()
                    );
                    return;
                }
                tokio::time::sleep(std::time::Duration::from_millis(10)).await;
            }
            Err(e) => {
                tracing::warn!(
                    "cgroup: failed to remove process leaf {}: {e}",
                    cgroup.path().display()
                );
                return;
            }
        }
    }
}
