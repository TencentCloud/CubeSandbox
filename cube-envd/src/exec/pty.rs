// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! PTY allocation, controlling-terminal process creation and window resizing.

use std::collections::HashMap;
use std::os::fd::{AsRawFd, FromRawFd, RawFd};
use std::process::Stdio;
use std::sync::{Arc, Mutex};

use tokio::io::unix::AsyncFd;
use tokio::sync::{broadcast, oneshot, Notify};

use crate::auth::User;

use super::io::{
    decorate_terminal, pump_pty, terminal_after_output, terminal_after_wait, OUTPUT_DRAIN_GRACE,
};
use super::spawn::child_pre_exec;
use super::{InputWriter, PumpEvent, SpawnedProcess};

/// Allocate a pty pair the portable, non-libutil way and return `(master,
/// slave)`.
///
/// `openpty`/`forkpty` live in `libutil.so.1` on glibc < 2.34 (the ubuntu20.04
/// builder runs glibc 2.31), and the libc crate declares them as plain
/// `extern "C"` symbols with no `-lutil` link, so calling `libc::openpty`
/// would fail to link the unit tests. Every primitive used here — `posix_openpt`,
/// `grantpt`, `unlockpt`, `TIOCGPTN`, `open` — is in libc proper on both glibc
/// and musl (this is also the sequence upstream `creack/pty` uses). Both fds
/// carry `O_CLOEXEC` so the master never leaks into the child's fd table and
/// keeps the pty open past the child's exit.
fn open_pty(cols: u16, rows: u16) -> std::io::Result<(std::fs::File, std::fs::File)> {
    let master = unsafe {
        libc::posix_openpt(libc::O_RDWR | libc::O_NOCTTY | libc::O_CLOEXEC | libc::O_NONBLOCK)
    };
    if master < 0 {
        return Err(std::io::Error::last_os_error());
    }
    // grantpt sets the slave's ownership, unlockpt clears its lock; both must
    // succeed before the slave device can be opened.
    if unsafe { libc::grantpt(master) } != 0 || unsafe { libc::unlockpt(master) } != 0 {
        let err = std::io::Error::last_os_error();
        unsafe { libc::close(master) };
        return Err(err);
    }
    // TIOCGPTN reads the pty's minor number; the slave is then /dev/pts/N.
    let mut minor: libc::c_int = 0;
    if unsafe { libc::ioctl(master, libc::TIOCGPTN, &mut minor) } != 0 {
        let err = std::io::Error::last_os_error();
        unsafe { libc::close(master) };
        return Err(err);
    }
    let path = std::ffi::CString::new(format!("/dev/pts/{minor}"))
        .map_err(|_| std::io::Error::new(std::io::ErrorKind::InvalidInput, "pty path"))?;
    let slave = unsafe {
        libc::open(
            path.as_ptr(),
            libc::O_RDWR | libc::O_NOCTTY | libc::O_CLOEXEC,
        )
    };
    if slave < 0 {
        let err = std::io::Error::last_os_error();
        unsafe { libc::close(master) };
        return Err(err);
    }
    // Seed the window size before the child starts, matching pty.StartWithSize.
    let winsize = libc::winsize {
        ws_row: rows,
        ws_col: cols,
        ws_xpixel: 0,
        ws_ypixel: 0,
    };
    if unsafe { libc::ioctl(slave, libc::TIOCSWINSZ, &winsize) } != 0 {
        let err = std::io::Error::last_os_error();
        unsafe {
            libc::close(slave);
            libc::close(master);
        }
        return Err(err);
    }

    let master_file = unsafe { std::fs::File::from_raw_fd(master) };
    let slave_file = unsafe { std::fs::File::from_raw_fd(slave) };
    Ok((master_file, slave_file))
}

/// Resize the window of an already-allocated pty (`TIOCSWINSZ` on the master).
///
/// The kernel stores a single `winsize` per pty pair, so setting it on the
/// master is visible to the child on the slave — this is how `Update` resizes
/// a running pty without touching the child's fd table. Zero values are passed
/// through to the kernel, matching upstream's direct `uint16` conversion.
pub fn resize_pty(master: &std::fs::File, cols: u16, rows: u16) -> std::io::Result<()> {
    let winsize = libc::winsize {
        ws_row: rows,
        ws_col: cols,
        ws_xpixel: 0,
        ws_ypixel: 0,
    };
    let rc = unsafe { libc::ioctl(master.as_raw_fd(), libc::TIOCSWINSZ, &winsize) };
    if rc != 0 {
        Err(std::io::Error::last_os_error())
    } else {
        Ok(())
    }
}

/// Spawn a command with its stdio attached to a freshly allocated pty.
///
/// The slave becomes the child's stdin/stdout/stderr (so stdout and stderr
/// merge into the single master, and later `SendInput` writes the master); the
/// master is what the pump reads the child's output from. A pty keeps its
/// default line discipline (ONLCR on), so the child's `\n` reaches the master
/// as `\r\n` — the baseline `data.pty` payload is CRLF-translated.
///
/// `cols`/`rows` seed the window size at allocation (`TIOCSWINSZ`); zero values
/// are passed through, matching upstream's empty `pty.Winsize{}`.
#[cfg(test)]
pub fn spawn_pty(
    cmd: &str,
    args: &[String],
    env: HashMap<String, String>,
    cwd: String,
    user: &User,
    size: (u16, u16),
    cgroup_fd: Option<RawFd>,
) -> std::io::Result<SpawnedProcess> {
    spawn_pty_with_cgroup(cmd, args, env, cwd, user, size, cgroup_fd, None)
}

/// PTY variant of [`super::spawn_with_cgroup`].
#[allow(clippy::too_many_arguments)]
pub fn spawn_pty_with_cgroup(
    cmd: &str,
    args: &[String],
    env: HashMap<String, String>,
    cwd: String,
    user: &User,
    size: (u16, u16),
    cgroup_fd: Option<RawFd>,
    process_cgroup: Option<Arc<crate::cgroup::ProcessCgroup>>,
) -> std::io::Result<SpawnedProcess> {
    // `open_pty` returns (master, slave); `Stdio::from(File)` dups the slave
    // fd onto 0/1/2 in the child, needing three handles because `Stdio::from`
    // consumes its `File` (the original is the one consumed last).
    let (cols, rows) = size;
    let (master_file, slave_file) = open_pty(cols, rows)?;
    // Separate dups are retained for Update and input; the pump task owns the
    // original below. Input writes are serialized by the process-owned mutex.
    let resize_master = master_file.try_clone()?;
    let input_master = master_file.try_clone()?;
    // Register both async master handles before spawning. If registration
    // fails, no child exists yet and all PTY descriptors are dropped cleanly.
    let master = AsyncFd::new(master_file)?;
    let input = Arc::new(tokio::sync::Mutex::new(InputWriter::Pty(AsyncFd::new(
        input_master,
    )?)));

    let mut command = tokio::process::Command::new(cmd);
    command
        .args(args)
        .env_clear()
        .envs(&env)
        .stdin(Stdio::from(slave_file.try_clone()?))
        .stdout(Stdio::from(slave_file.try_clone()?))
        .stderr(Stdio::from(slave_file))
        .kill_on_drop(false);
    unsafe {
        command.pre_exec(child_pre_exec(user, &cwd, cgroup_fd, true)?);
    }

    let mut child = command.spawn()?;
    let pid = child.id().unwrap_or_default();

    let (tx, initial) = broadcast::channel::<PumpEvent>(64);
    // A clone kept for `Connect` to subscribe later subscribers; the pump task
    // moves `tx` itself below.
    let sender = tx.clone();
    let (completion_tx, completion) = oneshot::channel();
    let terminal = Arc::new(std::sync::Mutex::new(None));
    let terminal_for_pump = terminal.clone();
    let reaped = Arc::new(Notify::new());
    let reaped_for_pump = reaped.clone();
    let termination = Arc::new(Mutex::new(None));
    let cgroup = Arc::new(Mutex::new(process_cgroup));
    let termination_for_pump = termination.clone();
    let cgroup_for_pump = cgroup.clone();

    tokio::spawn(async move {
        let output = pump_pty(master, tx.clone());
        tokio::pin!(output);
        let wait = child.wait();
        tokio::pin!(wait);

        let terminal = tokio::select! {
            output_result = &mut output => {
                if let Err(error) = &output_result {
                    tracing::warn!(pid, "error reading from pty: {error}");
                }
                let wait_result = wait.await;
                reaped_for_pump.notify_one();
                terminal_after_output("pty", output_result, wait_result)
            }
            wait_result = &mut wait => {
                reaped_for_pump.notify_one();
                let output_result = tokio::time::timeout(OUTPUT_DRAIN_GRACE, &mut output).await;
                terminal_after_wait("pty", pid, wait_result, output_result)
            }
        };
        let terminal = decorate_terminal(terminal, &termination_for_pump, &cgroup_for_pump);
        let mut slot = terminal_for_pump
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        *slot = Some(terminal.clone());
        drop(slot);
        let _ = tx.send(terminal);
        let _ = completion_tx.send(());
    });

    Ok(SpawnedProcess {
        pid,
        initial,
        sender,
        pty_master: Some(resize_master),
        input,
        completion,
        terminal,
        reaped,
        termination,
        cgroup,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::exec::spawn::DEFAULT_PATH;
    use crate::exec::tests::current_user;
    use crate::msg::process::EndEvent;

    #[tokio::test]
    async fn spawn_pty_captures_output_and_exit() {
        let user = current_user();
        let env = HashMap::from([("PATH".to_string(), DEFAULT_PATH.to_string())]);
        let mut proc = spawn_pty(
            "/bin/sh",
            &["-c".into(), "echo pty-test".into()],
            env,
            "/".into(),
            &user,
            (80, 24),
            None,
        )
        .unwrap();
        assert!(proc.pid > 0);

        let mut pty = Vec::new();
        let mut end: Option<EndEvent> = None;
        loop {
            match proc.initial.recv().await {
                Ok(PumpEvent::Data(d)) => {
                    use base64::Engine;
                    if let Some(s) = d.pty {
                        pty.extend(base64::engine::general_purpose::STANDARD.decode(s).unwrap());
                    }
                }
                Ok(PumpEvent::End(e)) => {
                    end = Some(e);
                    break;
                }
                Ok(PumpEvent::SpawnError(e)) => panic!("spawn error: {e}"),
                Ok(PumpEvent::DeadlineExceeded) => panic!("unexpected deadline"),
                Err(_) => break,
            }
        }
        // The pty line discipline translates the child's '\n' to '\r\n'.
        assert_eq!(String::from_utf8_lossy(&pty), "pty-test\r\n");
        let end = end.expect("end event");
        assert_eq!(end.exit_code, 0);
        assert!(end.exited);
        assert_eq!(end.status, "exit status 0");
    }

    #[tokio::test]
    async fn direct_child_is_reaped_when_detached_descendant_keeps_pty_open() {
        use base64::Engine;

        struct KillOnDrop(Option<u32>);
        impl Drop for KillOnDrop {
            fn drop(&mut self) {
                if let Some(pid) = self.0 {
                    unsafe {
                        libc::kill(-(pid as libc::pid_t), libc::SIGKILL);
                    }
                }
            }
        }

        let user = current_user();
        let mut proc = spawn_pty(
            "/bin/sh",
            &[
                "-c".into(),
                "setsid /bin/sh -c 'trap \"\" HUP; sleep 10' & echo DESC:$!; exit 0".into(),
            ],
            HashMap::from([("PATH".to_string(), DEFAULT_PATH.to_string())]),
            "/".into(),
            &user,
            (80, 24),
            None,
        )
        .unwrap();
        let direct_pid = proc.pid;
        let mut output = Vec::new();
        let descendant_pid = loop {
            let event =
                tokio::time::timeout(std::time::Duration::from_secs(3), proc.initial.recv())
                    .await
                    .expect("PTY never reported detached descendant")
                    .expect("PTY stream closed before descendant pid");
            match event {
                PumpEvent::Data(data) => {
                    if let Some(data) = data.pty {
                        output.extend(
                            base64::engine::general_purpose::STANDARD
                                .decode(data)
                                .unwrap(),
                        );
                    }
                    if let Some(pid) = String::from_utf8_lossy(&output)
                        .lines()
                        .find_map(|line| line.trim().strip_prefix("DESC:"))
                        .and_then(|pid| pid.parse::<u32>().ok())
                    {
                        break pid;
                    }
                }
                PumpEvent::End(end) => panic!("PTY ended before descendant pid: {end:?}"),
                PumpEvent::SpawnError(e) => panic!("spawn error: {e}"),
                PumpEvent::DeadlineExceeded => panic!("unexpected deadline"),
            }
        };
        let mut cleanup = KillOnDrop(Some(descendant_pid));

        let reap_deadline = std::time::Instant::now() + std::time::Duration::from_secs(1);
        let direct_proc = std::path::PathBuf::from(format!("/proc/{direct_pid}"));
        while direct_proc.exists() && std::time::Instant::now() < reap_deadline {
            tokio::time::sleep(std::time::Duration::from_millis(10)).await;
        }
        assert!(
            !direct_proc.exists(),
            "direct child {direct_pid} remained as a zombie while descendant {descendant_pid} held the PTY"
        );

        loop {
            match tokio::time::timeout(std::time::Duration::from_secs(2), proc.initial.recv())
                .await
                .expect("PTY output drain stayed open indefinitely")
                .expect("PTY stream closed before End")
            {
                PumpEvent::End(end) => {
                    assert_eq!(end.exit_code, 0);
                    break;
                }
                PumpEvent::Data(_) => {}
                PumpEvent::SpawnError(e) => panic!("spawn error: {e}"),
                PumpEvent::DeadlineExceeded => panic!("unexpected deadline"),
            }
        }

        unsafe {
            libc::kill(-(descendant_pid as libc::pid_t), libc::SIGKILL);
        }
        cleanup.0 = None;
    }

    #[test]
    fn resize_pty_updates_window_size() {
        use std::os::unix::io::AsRawFd;
        let (master, _slave) = open_pty(80, 24).unwrap();
        resize_pty(&master, 120, 40).unwrap();
        let mut ws = libc::winsize {
            ws_row: 0,
            ws_col: 0,
            ws_xpixel: 0,
            ws_ypixel: 0,
        };
        let rc = unsafe { libc::ioctl(master.as_raw_fd(), libc::TIOCGWINSZ, &mut ws) };
        assert_eq!(rc, 0, "TIOCGWINSZ read back failed");
        assert_eq!(ws.ws_row, 40);
        assert_eq!(ws.ws_col, 120);
    }

    #[tokio::test]
    async fn spawn_pty_has_a_controlling_terminal_and_foreground_group() {
        use base64::Engine;

        let user = current_user();
        let mut proc = spawn_pty(
            "/bin/sh",
            &[
                "-c".into(),
                "if { : </dev/tty; } 2>/dev/null; then echo DEVTTY=yes; else echo DEVTTY=no; fi; ps -o pid= -o sid= -o pgid= -o tpgid= -p $$".into(),
            ],
            HashMap::new(),
            "/".into(),
            &user,
            (80, 24),
            None,
        )
        .unwrap();
        let pid = proc.pid;

        let mut output = Vec::new();
        loop {
            let event =
                tokio::time::timeout(std::time::Duration::from_secs(5), proc.initial.recv())
                    .await
                    .expect("PTY process timed out")
                    .expect("PTY event stream closed before End");
            match event {
                PumpEvent::Data(d) => {
                    if let Some(data) = d.pty {
                        output.extend(
                            base64::engine::general_purpose::STANDARD
                                .decode(data)
                                .unwrap(),
                        );
                    }
                }
                PumpEvent::End(end) => {
                    assert_eq!(end.exit_code, 0);
                    break;
                }
                PumpEvent::SpawnError(e) => panic!("spawn error: {e}"),
                PumpEvent::DeadlineExceeded => panic!("unexpected deadline"),
            }
        }

        let text = String::from_utf8_lossy(&output);
        assert!(text.contains("DEVTTY=yes"), "PTY output: {text:?}");
        let ids = text
            .lines()
            .find_map(|line| {
                let values = line
                    .split_whitespace()
                    .map(str::parse::<u32>)
                    .collect::<Result<Vec<_>, _>>()
                    .ok()?;
                (values.len() == 4).then_some(values)
            })
            .expect("pid/sid/pgid/tpgid line");
        assert_eq!(ids, vec![pid, pid, pid, pid]);
    }

    #[tokio::test]
    async fn resize_pty_delivers_sigwinch_to_the_foreground_group() {
        use base64::Engine;

        let user = current_user();
        let mut proc = spawn_pty(
            "/bin/sh",
            &[
                "-c".into(),
                "trap 'echo WINCH; stty size; exit 0' WINCH; echo READY; while :; do sleep 1; done"
                    .into(),
            ],
            HashMap::new(),
            "/".into(),
            &user,
            (80, 24),
            None,
        )
        .unwrap();
        let resize_master = proc.pty_master.take().expect("PTY resize fd");
        let mut output = Vec::new();

        while !String::from_utf8_lossy(&output).contains("READY") {
            let event =
                tokio::time::timeout(std::time::Duration::from_secs(5), proc.initial.recv())
                    .await
                    .expect("PTY did not become ready")
                    .expect("PTY event stream closed before READY");
            match event {
                PumpEvent::Data(d) => {
                    if let Some(data) = d.pty {
                        output.extend(
                            base64::engine::general_purpose::STANDARD
                                .decode(data)
                                .unwrap(),
                        );
                    }
                }
                PumpEvent::End(end) => panic!("PTY exited before resize: {end:?}"),
                PumpEvent::SpawnError(e) => panic!("spawn error: {e}"),
                PumpEvent::DeadlineExceeded => panic!("unexpected deadline"),
            }
        }

        resize_pty(&resize_master, 132, 43).unwrap();
        loop {
            let event =
                tokio::time::timeout(std::time::Duration::from_secs(5), proc.initial.recv())
                    .await
                    .expect("PTY did not handle SIGWINCH")
                    .expect("PTY event stream closed before End");
            match event {
                PumpEvent::Data(d) => {
                    if let Some(data) = d.pty {
                        output.extend(
                            base64::engine::general_purpose::STANDARD
                                .decode(data)
                                .unwrap(),
                        );
                    }
                }
                PumpEvent::End(end) => {
                    assert_eq!(end.exit_code, 0);
                    break;
                }
                PumpEvent::SpawnError(e) => panic!("spawn error: {e}"),
                PumpEvent::DeadlineExceeded => panic!("unexpected deadline"),
            }
        }

        let text = String::from_utf8_lossy(&output);
        assert!(text.contains("WINCH"), "PTY output: {text:?}");
        assert!(text.contains("43 132"), "PTY output: {text:?}");
    }
}
