// Copyright (c) 2019-2020 Ant Financial
// Copyright (c) 2020 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

use std::sync::Arc;

use anyhow::{anyhow, Result};
use capctl::prctl::set_subreaper;
use nix::sys::wait::WaitPidFlag;
use nix::sys::wait::{self, WaitStatus};
use nix::unistd;
use slog::{error, info, o, Logger};
use tokio::select;
use tokio::signal::unix::{signal, SignalKind};
use tokio::sync::watch::Receiver;
use tokio::sync::Mutex;
use unistd::Pid;

use crate::sandbox::Sandbox;

async fn handle_sigchild(logger: Logger, sandbox: Arc<Mutex<Sandbox>>) -> Result<()> {
    loop {
        // Avoid reaping the undesirable child's signal, e.g., execute_hook's
        // The lock should be released immediately.

        let lock = rustjail::container::WAIT_PID_LOCKER.lock().await;

        let result = wait::waitpid(
            Some(Pid::from_raw(-1)),
            Some(WaitPidFlag::WNOHANG | WaitPidFlag::__WALL),
        );

        let wait_status = match result {
            Ok(s) => {
                if s == WaitStatus::StillAlive {
                    return Ok(());
                }
                s
            }
            Err(_) => return Ok(()),
        };
        drop(lock);

        debug!(logger, "wait_status"; "wait_status result" => format!("{:?}", wait_status));

        if let Some(pid) = wait_status.pid() {
            let raw_pid = pid.as_raw();
            let child_pid = format!("{}", raw_pid);

            let logger = logger.new(o!("child-pid" => child_pid));

            let sandbox_ref = sandbox.clone();
            let mut sandbox = sandbox_ref.lock().await;

            let process = sandbox.find_process(raw_pid);
            if process.is_none() {
                continue;
            }

            let p = process.unwrap();

            let ret: i32 = match wait_status {
                WaitStatus::Exited(_, c) => c,
                WaitStatus::Signaled(_, sig, _) => sig as i32,
                _ => {
                    info!(logger, "got wrong status for process";
                                  "child-status" => format!("{:?}", wait_status));
                    continue;
                }
            };

            // To avoid deadlocking the entire agent by holding the sandbox lock while
            // waiting for passfd output to drain, extract the tasks and the io struct,
            // drop the lock, then reacquire the lock to finish up.
            p.exit_code = ret;
            p.exited = true;
            for watcher in p.exit_watchers.iter_mut() {
                let _ = watcher.try_send(ret);
            }
            let passfd_stdin_task = p.passfd_stdin_task.take();
            let passfd_tasks: Vec<_> = p.passfd_tasks.drain(..).collect();
            let mut proc_io = p.proc_io.take();
            let container_id = p.container_id.clone();
            let exec_id = p.exec_id.clone();

            drop(sandbox); // RELEASE LOCK BEFORE AWAIT!

            if let Some(io) = &mut proc_io {
                if tokio::time::timeout(std::time::Duration::from_secs(2), io.wg_output.wait())
                    .await
                    .is_err()
                {
                    warn!(
                        logger,
                        "passfd output drain timed out; output may be truncated";
                        "container_id" => container_id,
                        "exec_id" => exec_id,
                        "output_truncated" => true,
                    );
                }
            }
            if let Some(task) = passfd_stdin_task {
                task.abort();
                let _ = task.await;
            }
            for task in passfd_tasks {
                task.abort();
                let _ = task.await;
            }

            // proc_io was taken only to wait for its output workers without holding
            // the sandbox lock. Drop any streams left after the drain instead of
            // restoring an empty or partially consumed IO state.
            drop(proc_io);

            // REACQUIRE LOCK
            let mut sandbox = sandbox_ref.lock().await;
            let process = sandbox.find_process(raw_pid);
            if process.is_none() {
                continue;
            }
            let p = process.unwrap();

            p.exit_code = ret;
            p.exited = true;
            let _ = p.exit_tx.take();

            debug!(logger, "notify term to close");
            // close the socket file to notify readStdio to close terminal specifically
            // in case this process's terminal has been inherited by its children.
            p.notify_term_close();
        }
    }
}

pub async fn setup_signal_handler(
    logger: Logger,
    sandbox: Arc<Mutex<Sandbox>>,
    mut shutdown: Receiver<bool>,
) -> Result<()> {
    let logger = logger.new(o!("subsystem" => "signals"));

    set_subreaper(true)
        .map_err(|err| anyhow!(err).context("failed to setup agent as a child subreaper"))?;

    let mut sigchild_stream = signal(SignalKind::child())?;

    loop {
        select! {
            _ = shutdown.changed() => {
                info!(logger, "got shutdown request");
                break;
            }

            _ = sigchild_stream.recv() => {
                let result = handle_sigchild(logger.clone(), sandbox.clone()).await;

                match result {
                    Ok(()) => (),
                    Err(e) => {
                        // Log errors, but don't abort - just wait for more signals!
                        error!(logger, "failed to handle signal"; "error" => format!("{:?}", e));
                    }
                }
            }
        }
    }

    Ok(())
}

#[cfg(test)]
mod tests {
    use tokio::pin;
    use tokio::sync::watch::channel;
    use tokio::time::Duration;

    use super::*;

    #[tokio::test]
    async fn test_setup_signal_handler() {
        let logger = slog::Logger::root(slog::Discard, o!());
        let s = Sandbox::new(&logger).unwrap();

        let sandbox = Arc::new(Mutex::new(s));

        let (tx, rx) = channel(true);

        let handle = tokio::spawn(setup_signal_handler(logger, sandbox, rx));

        let timeout = tokio::time::sleep(Duration::from_secs(1));
        pin!(timeout);

        tx.send(true).expect("failed to request shutdown");

        loop {
            select! {
                _ = handle => {
                    println!("INFO: task completed");
                    break;
                },
                _ = &mut timeout => {
                    panic!("signal thread failed to stop");
                }
            }
        }
    }
}
