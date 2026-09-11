// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Process input endpoints, output pumps and terminal-event publication helpers.

use std::sync::{Arc, Mutex};

use tokio::io::unix::AsyncFd;
use tokio::io::AsyncReadExt;
use tokio::sync::broadcast;

use crate::process::wire::{DataEvent, EndEvent};

const READ_CHUNK: usize = 32 * 1024;
/// Once the direct child has been reaped, inherited stdout/stderr or PTY
/// slave descriptors must not keep the process entry alive forever. Normal
/// exits reach EOF immediately; this grace period only catches background or
/// daemonized descendants that deliberately retain those descriptors.
pub(super) const OUTPUT_DRAIN_GRACE: std::time::Duration = std::time::Duration::from_millis(500);

/// One output event published on a process's broadcast bus. `Clone` because
/// `broadcast::Sender::send` fans a copy out to every subscriber.
#[derive(Clone, Debug)]
pub enum PumpEvent {
    Data(DataEvent),
    End(EndEvent),
    SpawnError(String),
    DeadlineExceeded,
}

/// Process-owned input endpoint. The mutex serializes writes from unary and
/// streaming RPCs without holding the global process-table lock across I/O.
#[derive(Debug)]
pub enum InputWriter {
    Pty(AsyncFd<std::fs::File>),
    /// `None` means stdin was disabled at Start or has already been closed.
    Pipe(Option<tokio::process::ChildStdin>),
}

pub type InputHandle = Arc<tokio::sync::Mutex<InputWriter>>;

pub(super) fn decorate_terminal(
    event: PumpEvent,
    termination: &Arc<Mutex<Option<String>>>,
    cgroup: &Arc<Mutex<Option<Arc<crate::process::cgroup::ProcessCgroup>>>>,
) -> PumpEvent {
    let PumpEvent::End(mut end) = event else {
        return event;
    };
    let cause = termination
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
        .clone();
    let oom_killed = end.signal == Some(libc::SIGKILL) && cause.is_none() && {
        let group = cgroup
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .clone();
        match group {
            Some(group) => match group.oom_killed() {
                Ok(killed) => killed,
                Err(error) => {
                    // An unreadable memory.events file means the OOM state is
                    // unknown, not a confirmed non-OOM exit. Keep the optional
                    // wire field absent and make the loss observable.
                    tracing::warn!(
                        "process cgroup {}: unable to determine OOM termination: {error}",
                        group.path().display()
                    );
                    false
                }
            },
            None => false,
        }
    };
    if oom_killed {
        end.oom_killed = Some(true);
        end.killed_by = Some("oom".to_string());
    } else if let Some(cause) = cause {
        end.killed_by = Some(cause);
    }
    PumpEvent::End(end)
}

pub(super) fn terminal_after_output(
    output_name: &str,
    output_result: std::io::Result<()>,
    wait_result: std::io::Result<std::process::ExitStatus>,
) -> PumpEvent {
    match (output_result, wait_result) {
        (Ok(()), Ok(status)) => PumpEvent::End(EndEvent::from_exit_status(status)),
        (Ok(()), Err(wait_error)) => PumpEvent::SpawnError(format!("wait failed: {wait_error}")),
        (Err(_), Ok(status)) if output_name == "pty" => {
            PumpEvent::End(EndEvent::from_exit_status(status))
        }
        (Err(read_error), Ok(_)) => {
            PumpEvent::SpawnError(format!("{output_name} read failed: {read_error}"))
        }
        (Err(read_error), Err(wait_error)) => PumpEvent::SpawnError(format!(
            "{output_name} read failed: {read_error}; wait failed: {wait_error}"
        )),
    }
}

pub(super) fn terminal_after_wait(
    output_name: &str,
    pid: u32,
    wait_result: std::io::Result<std::process::ExitStatus>,
    output_result: Result<std::io::Result<()>, tokio::time::error::Elapsed>,
) -> PumpEvent {
    let status = match wait_result {
        Ok(status) => status,
        Err(wait_error) => return PumpEvent::SpawnError(format!("wait failed: {wait_error}")),
    };
    match output_result {
        Ok(Ok(())) => PumpEvent::End(EndEvent::from_exit_status(status)),
        Ok(Err(read_error)) if output_name == "pty" => {
            tracing::warn!(pid, "error reading from pty: {read_error}");
            PumpEvent::End(EndEvent::from_exit_status(status))
        }
        Ok(Err(read_error)) => {
            PumpEvent::SpawnError(format!("{output_name} read failed: {read_error}"))
        }
        Err(_) => {
            tracing::warn!(
                "pid {pid}: {output_name} remained open after the direct child exited; closing it after {:?}",
                OUTPUT_DRAIN_GRACE
            );
            PumpEvent::End(EndEvent::from_exit_status(status))
        }
    }
}

/// Pump a pty master fd into `DataEvent { pty }` frames. Mirrors `pump_pipe`:
/// keep draining once the last subscriber is gone (so the child never blocks
/// on a full pty buffer) but stop encoding.
pub(super) async fn pump_pty(
    master: AsyncFd<std::fs::File>,
    tx: broadcast::Sender<PumpEvent>,
) -> std::io::Result<()> {
    use base64::Engine;
    use std::io::Read;
    let mut buf = vec![0u8; READ_CHUNK];
    loop {
        let mut readiness = master.readable().await?;
        match readiness.try_io(|inner| inner.get_ref().read(&mut buf)) {
            Ok(Ok(0)) => return Ok(()),
            Ok(Err(e)) if is_pty_eof(&e) => return Ok(()),
            Ok(Err(e)) if e.kind() == std::io::ErrorKind::Interrupted => continue,
            Ok(Err(e)) => return Err(e),
            Err(_would_block) => continue,
            Ok(Ok(n)) => {
                // A disconnected Start must not permanently disable output
                // for a later Connect. Skip work while nobody is attached,
                // but re-check on every read so reattachment resumes delivery.
                if tx.receiver_count() == 0 {
                    continue;
                }
                let b64 = base64::engine::general_purpose::STANDARD.encode(&buf[..n]);
                let event = DataEvent {
                    pty: Some(b64),
                    ..Default::default()
                };
                let _ = tx.send(PumpEvent::Data(event));
            }
        }
    }
}

/// Write bytes to a non-blocking PTY master without using tokio's blocking
/// filesystem pool. Readiness is re-registered after EAGAIN, so cancellation
/// of the RPC releases the fd promptly even when the child is not reading.
pub async fn write_pty(master: &AsyncFd<std::fs::File>, data: &[u8]) -> std::io::Result<()> {
    use std::io::Write;
    let mut offset = 0;
    while offset < data.len() {
        let mut readiness = master.writable().await?;
        match readiness.try_io(|inner| inner.get_ref().write(&data[offset..])) {
            Ok(Ok(0)) => {
                return Err(std::io::Error::new(
                    std::io::ErrorKind::WriteZero,
                    "pty write returned zero",
                ))
            }
            Ok(Ok(n)) => offset += n,
            Ok(Err(e)) if e.kind() == std::io::ErrorKind::Interrupted => continue,
            Ok(Err(e)) => return Err(e),
            Err(_would_block) => continue,
        }
    }
    Ok(())
}

/// Linux returns EIO when the last PTY slave closes. It is the PTY equivalent
/// of EOF; other errors must remain visible to the process stream.
fn is_pty_eof(error: &std::io::Error) -> bool {
    error.raw_os_error() == Some(libc::EIO)
}

pub(super) async fn pump_pipe<R>(
    pipe: Option<R>,
    tx: broadcast::Sender<PumpEvent>,
    is_stderr: bool,
) -> std::io::Result<()>
where
    R: tokio::io::AsyncRead + Unpin,
{
    use base64::Engine;
    let Some(mut pipe) = pipe else { return Ok(()) };
    let mut buf = vec![0u8; READ_CHUNK];
    loop {
        match pipe.read(&mut buf).await {
            Ok(0) => return Ok(()),
            Err(e) if e.kind() == std::io::ErrorKind::Interrupted => continue,
            Err(e) => return Err(e),
            Ok(n) => {
                if tx.receiver_count() == 0 {
                    continue;
                }
                let b64 = base64::engine::general_purpose::STANDARD.encode(&buf[..n]);
                let event = if is_stderr {
                    DataEvent {
                        stderr: Some(b64),
                        ..Default::default()
                    }
                } else {
                    DataEvent {
                        stdout: Some(b64),
                        ..Default::default()
                    }
                };
                let _ = tx.send(PumpEvent::Data(event));
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::process::engine::spawn;
    use crate::process::engine::tests::current_user;
    use std::collections::HashMap;

    #[test]
    fn oom_metadata_requires_sigkill_and_preserves_recorded_causes() {
        use std::os::unix::process::ExitStatusExt;

        let directory = tempfile::tempdir().unwrap();
        let events = directory.path().join("memory.events");
        std::fs::write(&events, "oom_kill 0\n").unwrap();
        let group = Arc::new(crate::process::cgroup::ProcessCgroup::new(
            directory.path().to_path_buf(),
            std::fs::File::open(directory.path()).unwrap(),
        ));
        let cgroup = Arc::new(Mutex::new(Some(group)));
        for (status, cause, delta, expected_oom, expected_cause) in [
            (0, None, 1, false, None),
            (libc::SIGTERM, None, 1, false, None),
            (libc::SIGKILL, None, 0, false, None),
            (libc::SIGKILL, None, 1, true, Some("oom")),
            (libc::SIGKILL, Some("timeout"), 1, false, Some("timeout")),
            (libc::SIGKILL, Some("user"), 1, false, Some("user")),
        ] {
            std::fs::write(&events, format!("oom_kill {delta}\n")).unwrap();
            let termination = Arc::new(Mutex::new(cause.map(str::to_string)));
            let event = PumpEvent::End(EndEvent::from_exit_status(
                std::process::ExitStatus::from_raw(status),
            ));
            let PumpEvent::End(end) = decorate_terminal(event, &termination, &cgroup) else {
                panic!("lost terminal event");
            };
            assert_eq!(end.oom_killed.unwrap_or(false), expected_oom);
            assert_eq!(end.killed_by.as_deref(), expected_cause);
        }
    }

    #[test]
    fn pty_read_errors_preserve_child_exit_status() {
        use std::os::unix::process::ExitStatusExt;

        let terminal = terminal_after_output(
            "pty",
            Err(std::io::Error::from_raw_os_error(libc::EBADF)),
            Ok(std::process::ExitStatus::from_raw(libc::SIGTERM)),
        );
        let PumpEvent::End(end) = terminal else {
            panic!("PTY read failure replaced child exit status: {terminal:?}");
        };
        assert_eq!(end.signal, Some(libc::SIGTERM));

        let terminal = terminal_after_wait(
            "pty",
            42,
            Ok(std::process::ExitStatus::from_raw(libc::SIGTERM)),
            Ok(Err(std::io::Error::from_raw_os_error(libc::EBADF))),
        );
        let PumpEvent::End(end) = terminal else {
            panic!("PTY drain failure replaced child exit status: {terminal:?}");
        };
        assert_eq!(end.signal, Some(libc::SIGTERM));
    }

    #[tokio::test]
    async fn output_delivery_resumes_for_a_later_subscriber() {
        use base64::Engine;

        let user = current_user();
        let proc = spawn(
            "/bin/sh",
            &[
                "-c".into(),
                "printf before; sleep 0.25; printf after; sleep 0.05".into(),
            ],
            HashMap::new(),
            "/".into(),
            &user,
            false,
            None,
        )
        .unwrap();
        let sender = proc.sender.clone();
        drop(proc.initial);
        tokio::time::sleep(std::time::Duration::from_millis(100)).await;
        let mut attached = sender.subscribe();
        let mut output = Vec::new();
        loop {
            match tokio::time::timeout(std::time::Duration::from_secs(3), attached.recv())
                .await
                .expect("reattached subscriber timed out")
                .expect("output bus closed before End")
            {
                PumpEvent::Data(data) => {
                    if let Some(data) = data.stdout {
                        output.extend(
                            base64::engine::general_purpose::STANDARD
                                .decode(data)
                                .unwrap(),
                        );
                    }
                }
                PumpEvent::End(_) => break,
                PumpEvent::SpawnError(e) => panic!("spawn error: {e}"),
                PumpEvent::DeadlineExceeded => panic!("unexpected deadline"),
            }
        }
        assert!(
            String::from_utf8_lossy(&output).contains("after"),
            "pump stopped publishing after the first subscriber disconnected"
        );
    }

    #[test]
    fn only_eio_is_treated_as_pty_eof() {
        assert!(is_pty_eof(&std::io::Error::from_raw_os_error(libc::EIO)));
        assert!(!is_pty_eof(&std::io::Error::from_raw_os_error(libc::EBADF)));
        assert!(!is_pty_eof(&std::io::Error::from(
            std::io::ErrorKind::Interrupted
        )));
    }
}
