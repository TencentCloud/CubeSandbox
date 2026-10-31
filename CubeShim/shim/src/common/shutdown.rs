// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

use std::sync::mpsc::{self, Receiver, RecvTimeoutError, Sender};
use std::time::{Duration, Instant};

/// Last resort for final VM teardown. VMM API calls and thread joins are
/// synchronous: an async timeout alone cannot stop them or release their TAPs.
/// This watchdog runs outside the async runtime and terminates only this shim.
pub struct ShutdownDeadline {
    cancel: Sender<()>,
}

impl ShutdownDeadline {
    pub fn arm(timeout: Duration) -> Self {
        Self::with_action(timeout, || {
            // Do not run destructors: they may contain the blocked VMM join.
            unsafe { libc::_exit(1) }
        })
        // Final teardown must remain bounded even under thread exhaustion.
        .unwrap_or_else(|error| {
            eprintln!("cannot arm VM shutdown watchdog: {}; exiting shim", error);
            unsafe { libc::_exit(1) }
        })
    }

    fn with_action<F>(timeout: Duration, expired: F) -> Result<Self, String>
    where
        F: FnOnce() + Send + 'static,
    {
        let expires_at = Instant::now() + timeout;
        let (cancel, receiver) = mpsc::channel();
        std::thread::Builder::new()
            .name("vm-shutdown-deadline".into())
            .stack_size(64 * 1024)
            .spawn(move || {
                match receiver.recv_timeout(expires_at.saturating_duration_since(Instant::now())) {
                    Ok(()) => return,
                    Err(RecvTimeoutError::Disconnected) => {
                        // Cancellation must not turn a 30-second deadline into
                        // an immediate exit while another cleanup is in flight.
                        std::thread::sleep(expires_at.saturating_duration_since(Instant::now()));
                    }
                    Err(RecvTimeoutError::Timeout) => {}
                }
                expired();
            })
            .map_err(|e| format!("start VM shutdown watchdog: {}", e))?;
        Ok(Self { cancel })
    }

    /// A completed physical teardown can cancel the watchdog, including one
    /// reporting that the VMM thread already failed. Dropping a
    /// cancelled async future leaves the original deadline armed.
    pub fn complete(self) {
        let _ = self.cancel.send(());
    }
}

/// Wait for a terminal notification without treating an intermediate event as
/// completion or extending the caller's deadline.
pub fn wait_for_event_until<T>(
    receiver: &Receiver<T>,
    deadline: Instant,
    matches: impl Fn(&T) -> bool,
) -> Result<bool, RecvTimeoutError> {
    loop {
        let remaining = deadline.saturating_duration_since(Instant::now());
        if remaining.is_zero() {
            return Ok(false);
        }
        match receiver.recv_timeout(remaining) {
            Ok(event) if matches(&event) => return Ok(true),
            Ok(_) => continue,
            Err(RecvTimeoutError::Timeout) => return Ok(false),
            Err(error) => return Err(error),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::mpsc::RecvTimeoutError;

    #[test]
    fn shutdown_notification_can_arrive_after_one_second() {
        let (sender, receiver) = mpsc::channel();
        let worker = std::thread::spawn(move || {
            sender.send(false).unwrap();
            std::thread::sleep(Duration::from_millis(1100));
            sender.send(true).unwrap();
        });
        let started = Instant::now();
        assert_eq!(
            wait_for_event_until(&receiver, started + Duration::from_secs(3), |event| *event),
            Ok(true)
        );
        assert!(started.elapsed() >= Duration::from_secs(1));
        worker.join().unwrap();
    }

    #[test]
    fn unrelated_events_do_not_extend_shutdown_deadline() {
        let (sender, receiver) = mpsc::channel();
        sender.send(false).unwrap();
        let started = Instant::now();
        assert_eq!(
            wait_for_event_until(&receiver, started + Duration::from_millis(30), |event| {
                *event
            }),
            Ok(false)
        );
        assert!(started.elapsed() >= Duration::from_millis(30));
    }

    #[test]
    fn disconnected_shutdown_channel_is_reported() {
        let (sender, receiver) = mpsc::channel::<bool>();
        drop(sender);
        assert_eq!(
            wait_for_event_until(
                &receiver,
                Instant::now() + Duration::from_secs(1),
                |event| *event
            ),
            Err(RecvTimeoutError::Disconnected)
        );
    }

    #[test]
    fn shutdown_deadline_fires_without_async_runtime_progress() {
        let (sent, received) = mpsc::channel();
        let _deadline = ShutdownDeadline::with_action(Duration::from_millis(20), move || {
            sent.send(()).unwrap();
        })
        .unwrap();
        received.recv_timeout(Duration::from_secs(2)).unwrap();
    }

    #[test]
    fn completed_shutdown_disarms_deadline() {
        let (sent, received) = mpsc::channel();
        let deadline = ShutdownDeadline::with_action(Duration::from_secs(1), move || {
            sent.send(()).unwrap();
        })
        .unwrap();
        deadline.complete();
        assert!(matches!(
            received.recv_timeout(Duration::from_secs(2)),
            Err(RecvTimeoutError::Disconnected)
        ));
    }

    #[test]
    fn abandoned_shutdown_does_not_disarm_deadline() {
        let (sent, received) = mpsc::channel();
        let timeout = Duration::from_millis(100);
        let started = Instant::now();
        let deadline = ShutdownDeadline::with_action(timeout, move || {
            sent.send(()).unwrap();
        })
        .unwrap();
        drop(deadline);
        received.recv_timeout(Duration::from_secs(2)).unwrap();
        assert!(started.elapsed() >= timeout);
    }

    #[test]
    fn deadline_terminates_a_stuck_shim() {
        const CHILD: &str = "SHIM_SHUTDOWN_TEST_CHILD";
        if std::env::var_os(CHILD).is_some() {
            let _deadline = ShutdownDeadline::with_action(Duration::from_millis(40), || unsafe {
                libc::_exit(73)
            })
            .unwrap();
            // Reproduce a synchronous VMM join that cannot make progress.
            std::thread::spawn(|| loop {
                std::thread::park();
            })
            .join()
            .unwrap();
            unreachable!();
        }
        let module = module_path!().split_once("::").unwrap().1;
        let test_name = format!("{}::deadline_terminates_a_stuck_shim", module);
        let started = std::time::Instant::now();
        let mut child = std::process::Command::new(std::env::current_exe().unwrap())
            .args(["--exact", test_name.as_str()])
            .env(CHILD, "1")
            .stdout(std::process::Stdio::null())
            .spawn()
            .unwrap();
        loop {
            if let Some(status) = child.try_wait().unwrap() {
                assert_eq!(status.code(), Some(73));
                assert!(started.elapsed() >= Duration::from_millis(40));
                break;
            }
            if started.elapsed() > Duration::from_secs(3) {
                let _ = child.kill();
                let _ = child.wait();
                panic!("stuck shutdown did not terminate its shim process");
            }
            std::thread::sleep(Duration::from_millis(10));
        }
    }
}
