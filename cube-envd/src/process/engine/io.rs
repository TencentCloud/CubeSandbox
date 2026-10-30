// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Process input endpoints, output pumps and terminal-event publication helpers.

use std::sync::{Arc, Mutex};

use tokio::io::unix::AsyncFd;
use tokio::io::AsyncReadExt;
use tokio::sync::watch;

use crate::process::wire::{DataEvent, EndEvent};
use crate::process::OutputBus;

/// One read from a child's pipe or pty. Every frame costs a fixed amount of
/// cross-task work (a queue slot, a wakeup, a base64 string, a JSON envelope),
/// and this sandbox's cost per handoff dominates the per-byte cost, so the
/// chunk is sized to what a pipe can carry in one go: the pipe capacity below
/// is raised to match, and a `cat`-style writer fills it.
const READ_CHUNK: usize = 128 * 1024;

/// Ask the kernel for a child pipe large enough to fill `READ_CHUNK` in one
/// read. Linux caps this at `/proc/sys/fs/pipe-max-size` (1 MiB by default), so
/// the request is best effort and a refusal only means smaller reads.
pub(super) fn widen_pipe<F: std::os::fd::AsRawFd>(pipe: &F, name: &str) {
    let fd = pipe.as_raw_fd();
    // SAFETY: `fd` is an open pipe read end owned by the caller.
    let result = unsafe { libc::fcntl(fd, libc::F_SETPIPE_SZ, READ_CHUNK as libc::c_int) };
    if result < 0 {
        tracing::debug!(
            "{name}: could not widen the pipe: {}",
            std::io::Error::last_os_error()
        );
    }
}
/// Once the direct child has been reaped, inherited stdout/stderr or PTY
/// slave descriptors must not keep the process entry alive forever. Normal
/// exits reach EOF immediately; this grace period only catches background or
/// daemonized descendants that deliberately retain those descriptors.
pub(super) const OUTPUT_DRAIN_GRACE: std::time::Duration = std::time::Duration::from_millis(500);

/// One output event published on a process's output bus. `Clone` because
/// `OutputBus::publish` fans a copy out to every subscriber.
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

/// Which stream a pump reads. The two things that differ because of it live
/// here rather than being keyed off a string: the label a terminal message
/// carries (client-visible), and whether a read error is a failure at all.
#[derive(Clone, Copy, PartialEq, Eq)]
pub(super) enum OutputKind {
    Process,
    Pty,
}

impl OutputKind {
    pub(super) fn label(self) -> &'static str {
        match self {
            Self::Process => "process output",
            Self::Pty => "pty",
        }
    }

    /// Reading a pty master fails with `EIO` once the last slave closes, which
    /// is how an interactive session normally ends; a pipe that fails is a real
    /// read failure.
    pub(super) fn read_error_is_fatal(self) -> bool {
        matches!(self, Self::Process)
    }
}

/// Bytes still buffered on a pipe or pty read end, via `FIONREAD`.
///
/// The drain grace stops reading when a descendant keeps the pipe open; this
/// tells "there was nothing left" from "output was abandoned", and only the
/// latter may end the stream with an error.
pub(super) fn unread_bytes(fd: std::os::fd::RawFd) -> usize {
    let mut pending: libc::c_int = 0;
    // SAFETY: `fd` is an open descriptor and FIONREAD writes one `c_int`.
    if unsafe { libc::ioctl(fd, libc::FIONREAD, &mut pending) } < 0 {
        return 0;
    }
    pending.max(0) as usize
}

/// The `End` for a reaped child, marked when the grace abandoned output.
fn end_event(status: std::process::ExitStatus, abandoned: bool) -> EndEvent {
    let mut event = EndEvent::from_exit_status(status);
    event.output_truncated = abandoned;
    event
}

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
    kind: OutputKind,
    output_result: std::io::Result<()>,
    wait_result: std::io::Result<std::process::ExitStatus>,
) -> PumpEvent {
    let output_name = kind.label();
    match (output_result, wait_result) {
        (Ok(()), Ok(status)) => PumpEvent::End(EndEvent::from_exit_status(status)),
        (Ok(()), Err(wait_error)) => PumpEvent::SpawnError(format!("wait failed: {wait_error}")),
        (Err(_), Ok(status)) if !kind.read_error_is_fatal() => {
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

/// Build the terminal event once the direct child has been reaped.
///
/// `abandoned` is true when the grace stopped reading with bytes still buffered
/// (the caller probes the pipe with `FIONREAD`), and it is what keeps this from
/// being a silent truncation: the event keeps the child's real exit status, and
/// the trailer turns into an error (see `process::pump::end_trailer`).
///
/// The distinction matters because the usual cause of a stop is a descendant
/// that inherited the pipe (`sh -c 'daemon & echo done'`), whose output is
/// already complete: that case has nothing buffered, ends normally, and must
/// not be turned into an RPC error. Only output that was really left behind is
/// reported as truncated.
pub(super) fn terminal_after_wait(
    kind: OutputKind,
    pid: u32,
    wait_result: std::io::Result<std::process::ExitStatus>,
    output_result: std::io::Result<()>,
    stopped_by_grace: bool,
    abandoned: bool,
) -> PumpEvent {
    let output_name = kind.label();
    let status = match wait_result {
        Ok(status) => status,
        Err(wait_error) => return PumpEvent::SpawnError(format!("wait failed: {wait_error}")),
    };
    if stopped_by_grace {
        tracing::warn!(
            "pid {pid}: {output_name} remained open after the direct child exited; stopped reading after {:?}",
            OUTPUT_DRAIN_GRACE
        );
    }
    match output_result {
        Ok(()) => PumpEvent::End(end_event(status, abandoned)),
        Err(read_error) if !kind.read_error_is_fatal() => {
            tracing::warn!(pid, "error reading from pty: {read_error}");
            PumpEvent::End(end_event(status, abandoned))
        }
        Err(read_error) => {
            PumpEvent::SpawnError(format!("{output_name} read failed: {read_error}"))
        }
    }
}

/// Pump a pty master fd into `DataEvent { pty }` frames. Mirrors `pump_pipe`:
/// keep draining once the last subscriber is gone (so the child never blocks
/// on a full pty buffer) but stop encoding.
pub(super) async fn pump_pty(
    master: AsyncFd<std::fs::File>,
    bus: Arc<OutputBus>,
    mut stop: watch::Receiver<bool>,
) -> std::io::Result<()> {
    use base64::Engine;
    use std::io::Read;
    widen_pipe(&master, "pty");
    let mut buf = vec![0u8; READ_CHUNK];
    loop {
        // The drain grace only *stops reading*: a publish that is already in
        // flight still runs to completion, so bytes read from the child are
        // never dropped because the grace expired.
        let mut readiness = tokio::select! {
            biased;
            _ = stop.changed() => return Ok(()),
            readiness = master.readable() => readiness?,
        };
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
                if bus.subscriber_count() == 0 {
                    continue;
                }
                let b64 = base64::engine::general_purpose::STANDARD.encode(&buf[..n]);
                let event = DataEvent {
                    pty: Some(b64),
                    ..Default::default()
                };
                bus.publish_data(PumpEvent::Data(event)).await;
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
    bus: Arc<OutputBus>,
    mut stop: watch::Receiver<bool>,
    is_stderr: bool,
) -> std::io::Result<()>
where
    R: tokio::io::AsyncRead + Unpin + std::os::fd::AsRawFd,
{
    use base64::Engine;
    let Some(mut pipe) = pipe else { return Ok(()) };
    widen_pipe(&pipe, if is_stderr { "stderr" } else { "stdout" });
    let mut buf = vec![0u8; READ_CHUNK];
    loop {
        let read = tokio::select! {
            biased;
            _ = stop.changed() => return Ok(()),
            read = pipe.read(&mut buf) => read,
        };
        match read {
            Ok(0) => return Ok(()),
            Err(e) if e.kind() == std::io::ErrorKind::Interrupted => continue,
            Err(e) => return Err(e),
            Ok(n) => {
                if bus.subscriber_count() == 0 {
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
                bus.publish_data(PumpEvent::Data(event)).await;
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::process::engine::tests::current_user;
    use crate::process::engine::{spawn, Spawn};
    use std::collections::HashMap;

    /// `widen_pipe` is best effort by design (the kernel caps the request at
    /// `pipe-max-size`), so what matters is that the fd it touched still works
    /// as a pipe afterwards: a wrong fd or a bad argument would break output.
    #[test]
    fn widening_a_pipe_keeps_it_usable() {
        use std::io::{Read, Write};
        use std::os::fd::FromRawFd;

        let mut fds = [0 as libc::c_int; 2];
        // SAFETY: `fds` is a valid two-element array for `pipe(2)`.
        assert_eq!(unsafe { libc::pipe(fds.as_mut_ptr()) }, 0);
        // SAFETY: both descriptors were just created and are owned from here.
        let read_end = unsafe { std::fs::File::from_raw_fd(fds[0]) };
        // SAFETY: same descriptors, each taken exactly once.
        let write_end = unsafe { std::fs::File::from_raw_fd(fds[1]) };

        widen_pipe(&read_end, "test");
        // SAFETY: `read_end` is an open pipe whose size is queried read-only.
        let size = unsafe { libc::fcntl(fds[0], libc::F_GETPIPE_SZ) };
        assert!(size > 0, "a pipe keeps a positive size, got {size}");

        (&write_end).write_all(b"ping").unwrap();
        let mut buf = [0u8; 4];
        (&read_end).read_exact(&mut buf).unwrap();
        assert_eq!(&buf, b"ping");
    }

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

    /// What a read error means, and the text the client sees, depend on the
    /// output kind: reading a pty master fails with `EIO` once the last slave
    /// closes, which is how an interactive session normally ends, while a pipe
    /// read error is a real failure the terminal event has to report under the
    /// `process output` label.
    #[test]
    fn read_errors_follow_the_output_kind() {
        use std::os::unix::process::ExitStatusExt;

        let terminal = terminal_after_output(
            OutputKind::Pty,
            Err(std::io::Error::from_raw_os_error(libc::EBADF)),
            Ok(std::process::ExitStatus::from_raw(libc::SIGTERM)),
        );
        let PumpEvent::End(end) = terminal else {
            panic!("PTY read failure replaced child exit status: {terminal:?}");
        };
        assert_eq!(end.signal, Some(libc::SIGTERM));

        let terminal = terminal_after_wait(
            OutputKind::Pty,
            42,
            Ok(std::process::ExitStatus::from_raw(libc::SIGTERM)),
            Err(std::io::Error::from_raw_os_error(libc::EBADF)),
            false,
            false,
        );
        let PumpEvent::End(end) = terminal else {
            panic!("PTY drain failure replaced child exit status: {terminal:?}");
        };
        assert_eq!(end.signal, Some(libc::SIGTERM));

        let terminal = terminal_after_output(
            OutputKind::Process,
            Err(std::io::Error::from_raw_os_error(libc::EBADF)),
            Ok(std::process::ExitStatus::from_raw(0)),
        );
        let PumpEvent::SpawnError(message) = terminal else {
            panic!("a pipe read failure must be a spawn error: {terminal:?}");
        };
        assert!(
            message.starts_with("process output read failed:"),
            "label lost from the client-visible message: {message}"
        );
    }

    #[tokio::test]
    async fn output_delivery_resumes_for_a_later_subscriber() {
        use base64::Engine;

        let user = current_user();
        let proc = spawn(Spawn {
            cmd: "/bin/sh",
            args: &[
                "-c".into(),
                "printf before; sleep 0.25; printf after; sleep 0.05".into(),
            ],
            env: HashMap::new(),
            cwd: "/".into(),
            user: &user,
            stdin: false,
            pty: None,
            cgroup_fd: None,
            process_cgroup: None,
        })
        .unwrap();
        let sender = proc.sender.clone();
        drop(proc.initial);
        tokio::time::sleep(std::time::Duration::from_millis(100)).await;
        let mut attached = sender.subscribe().expect("attach within the limit");
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
