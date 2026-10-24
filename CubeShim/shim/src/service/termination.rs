// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::Duration;

pub(super) const CLEANUP_TIMEOUT: Duration = Duration::from_secs(10);

/// Once terminal cleanup starts, this process must exit even if an embedded
/// VMM API request, a lock, or a synchronous thread join never returns.
/// A Tokio timer cannot enforce that deadline when its workers are blocked.
/// If a native thread cannot be started, return the error so the caller can
/// log the lost deadline guarantee and still attempt normal cleanup.
#[derive(Default)]
pub(super) struct Deadline {
    armed: AtomicBool,
    cleaned: AtomicBool,
}

impl Deadline {
    pub(super) fn is_armed(&self) -> bool {
        self.armed.load(Ordering::SeqCst)
    }

    pub(super) fn cleanup_complete(&self) {
        self.cleaned.store(true, Ordering::SeqCst);
    }

    pub(super) fn arm(self: &Arc<Self>, timeout: Duration) -> std::io::Result<()> {
        self.arm_with(timeout, |work| {
            std::thread::Builder::new()
                .name("shim-exit-deadline".into())
                .spawn(work)
                .map(|_| ())
        })
    }

    fn arm_with(
        self: &Arc<Self>,
        timeout: Duration,
        spawn: impl FnOnce(Box<dyn FnOnce() + Send>) -> std::io::Result<()>,
    ) -> std::io::Result<()> {
        if self.armed.swap(true, Ordering::SeqCst) {
            return Ok(());
        }
        let deadline = self.clone();
        spawn(Box::new(move || {
            std::thread::sleep(timeout);
            // Do not flush the VMM logger either: Logger::log_flush takes its
            // buffer mutex and writes to its output, which can block forever.
            // Do not run destructors/atexit handlers: VmmInstance::drop can
            // block on the same VMM API or join that prevented cleanup.
            // Exiting the whole process closes the embedded VM's descriptors
            // before Cubelet can observe process death and reclaim resources.
            let code = if deadline.cleaned.load(Ordering::SeqCst) {
                0
            } else {
                1
            };
            unsafe { libc::_exit(code) }
        }))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{Read, Write};
    use std::process::{Command, Stdio};
    use std::time::Instant;

    #[test]
    fn deadline_child() {
        let Ok(mode) = std::env::var("CUBE_SHIM_DEADLINE_TEST") else {
            return;
        };
        // Re-exec requires the libtest --exact protocol. The parent checks this
        // marker so an incompatible harness running zero tests cannot pass.
        std::io::stdout()
            .write_all(b"deadline-child-started\n")
            .unwrap();
        let terminating = Arc::new(Deadline::default());
        terminating.arm(Duration::from_millis(100)).unwrap();
        // A duplicate shutdown must not extend the original deadline.
        terminating.arm(Duration::from_secs(60)).unwrap();
        assert!(terminating.is_armed());
        if mode == "cleaned-but-blocked" {
            terminating.cleanup_complete();
        }
        if mode == "clean" {
            std::process::exit(0);
        }
        // Model the synchronous VMM join in a process with no child VMM.
        let worker = std::thread::spawn(|| loop {
            std::thread::park();
        });
        worker.join().unwrap();
    }

    fn run_child(mode: &str) -> std::process::ExitStatus {
        let mut child = Command::new(std::env::current_exe().unwrap())
            .args(["--exact", "service::termination::tests::deadline_child"])
            .env("CUBE_SHIM_DEADLINE_TEST", mode)
            .stdout(Stdio::piped())
            .spawn()
            .unwrap();
        let start = Instant::now();
        loop {
            if let Some(status) = child.try_wait().unwrap() {
                let mut output = String::new();
                child
                    .stdout
                    .take()
                    .unwrap()
                    .read_to_string(&mut output)
                    .unwrap();
                assert!(
                    output.contains("deadline-child-started"),
                    "child harness did not execute the selected test"
                );
                return status;
            }
            if start.elapsed() > Duration::from_secs(5) {
                child.kill().unwrap();
                child.wait().unwrap();
                panic!("terminal shim did not exit within the deadline");
            }
            std::thread::sleep(Duration::from_millis(10));
        }
    }

    #[test]
    fn repeated_blocked_cleanup_exits_without_child_processes() {
        for _ in 0..10 {
            assert_eq!(run_child("blocked").code(), Some(1));
        }
    }

    #[test]
    fn graceful_exit_precedes_deadline() {
        assert!(run_child("clean").success());
    }

    #[test]
    fn completed_cleanup_with_stalled_process_exit_returns_success() {
        assert_eq!(run_child("cleaned-but-blocked").code(), Some(0));
    }

    #[test]
    fn watchdog_spawn_failure_allows_terminal_cleanup_to_continue() {
        let deadline = Arc::new(Deadline::default());
        let result = deadline.arm_with(Duration::from_secs(10), |_| {
            Err(std::io::Error::from_raw_os_error(libc::EAGAIN))
        });
        assert!(result.is_err());
        assert!(deadline.is_armed());
        deadline.cleanup_complete();
        assert!(deadline.cleaned.load(Ordering::SeqCst));
    }
}
