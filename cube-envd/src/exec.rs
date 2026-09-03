// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Process execution: privilege drop, environment merging and the
//! stdout/stderr pump feeding the Start event stream.

use std::collections::HashMap;
use std::os::fd::{AsRawFd, FromRawFd, RawFd};
use std::process::Stdio;
use std::sync::Arc;

use tokio::io::AsyncReadExt;
use tokio::sync::broadcast;

use crate::auth::User;
use crate::msg::process::{DataEvent, EndEvent};
use crate::state::AppState;

/// Write this child's pid into `dirfd`'s `cgroup.procs`. Runs inside the
/// forked child before exec, so it must be allocation-free and call only
/// async-signal-safe libc. `dirfd` is a manager-owned cgroup directory fd
/// (borrowed for the daemon lifetime — never closed here; the open on
/// `cgroup.procs` carries O_CLOEXEC so the fd cannot leak past exec).
fn place_in_cgroup(dirfd: RawFd) -> std::io::Result<()> {
    let procs = b"cgroup.procs\0";
    // SAFETY: pre_exec runs in the forked child, single-threaded; dirfd is a
    // valid fd inherited from the parent.
    let fd = unsafe {
        libc::openat(
            dirfd,
            procs.as_ptr() as *const libc::c_char,
            libc::O_WRONLY | libc::O_CLOEXEC,
        )
    };
    if fd < 0 {
        return Err(std::io::Error::last_os_error());
    }

    // Format the pid without allocation: `<pid>\n`.
    let pid = unsafe { libc::getpid() };
    debug_assert!(pid > 0, "forked child always has a pid");
    let mut num = [0u8; 16];
    let mut n = num.len();
    let mut v = pid.max(1) as u32;
    while v > 0 {
        n -= 1;
        num[n] = b'0' + (v % 10) as u8;
        v /= 10;
    }
    let mut buf = [0u8; 17];
    let len = num.len() - n;
    buf[..len].copy_from_slice(&num[n..]);
    buf[len] = b'\n';

    // Loop until the whole pid line is written (a short write on a regular
    // file should not happen, but the cgroupfs write path may return EINTR).
    let mut off = 0usize;
    let total = len + 1;
    let result = loop {
        // SAFETY: buf is fully initialized for [off, total); fd is ours.
        let w = unsafe {
            libc::write(
                fd,
                buf[off..total].as_ptr() as *const libc::c_void,
                total - off,
            )
        };
        if w < 0 {
            let e = std::io::Error::last_os_error();
            if e.kind() == std::io::ErrorKind::Interrupted {
                continue;
            }
            break Err(e);
        }
        if w == 0 {
            // A zero-length write on a regular file is unreachable, but keep
            // the branch allocation-free like the rest of this function:
            // io::Error::new(WriteZero, ...) boxes a message.
            break Err(std::io::Error::from_raw_os_error(libc::EIO));
        }
        off += w as usize;
        if off >= total {
            break Ok(());
        }
    };
    // SAFETY: fd was opened by us above and not closed elsewhere.
    unsafe { libc::close(fd) };
    result
}

/// Matches upstream envd's default PATH for spawned commands.
const DEFAULT_PATH: &str = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin";
const READ_CHUNK: usize = 32 * 1024;

/// One output event published on a process's broadcast bus. `Clone` because
/// `broadcast::Sender::send` fans a copy out to every subscriber.
#[derive(Clone)]
pub enum PumpEvent {
    Data(DataEvent),
    End(EndEvent),
    SpawnError(String),
}

/// Process-owned input endpoint. The mutex serializes writes from unary and
/// streaming RPCs without holding the global process-table lock across I/O.
#[derive(Debug)]
pub enum InputWriter {
    Pty(tokio::fs::File),
    /// `None` means stdin was disabled at Start or has already been closed.
    Pipe(Option<tokio::process::ChildStdin>),
}

pub type InputHandle = Arc<tokio::sync::Mutex<InputWriter>>;

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
}

/// Merge order (later wins): built-in defaults < /init env vars < request envs.
pub fn merged_env(
    state: &AppState,
    user: &User,
    request_envs: &HashMap<String, String>,
) -> HashMap<String, String> {
    let mut env = HashMap::new();
    env.insert("PATH".to_string(), DEFAULT_PATH.to_string());
    env.insert("HOME".to_string(), user.home.clone());
    env.insert("USER".to_string(), user.name.clone());
    env.insert("LOGNAME".to_string(), user.name.clone());
    env.insert("TERM".to_string(), "xterm".to_string());
    env.extend(state.env_vars());
    env.extend(request_envs.clone());
    env
}

/// Resolve the requested working directory to an absolute path.
///
/// - An explicit `cwd` (relative anchored at the user's home) must name an
///   existing directory; otherwise this returns `Err(message)` which the
///   caller surfaces as `invalid_argument`. Upstream Go envd rejects a missing
///   or non-directory cwd the same way — cube-envd used to silently fall back
///   to `/` and run the command anyway, which #1227 forbids (no silent success
///   on invalid input).
/// - With no `cwd`, default to the user's home, tolerating a missing home like
///   upstream by falling back to `/`.
///
/// Existence is checked here as root, but the actual `chdir` happens *after*
/// the privilege drop in `spawn`, so a directory the target user cannot enter
/// (e.g. another user's `/root`) still fails with permission denied rather than
/// running as if it were accessible.
pub fn resolve_cwd(cwd: Option<&str>, user: &User) -> Result<String, String> {
    match cwd {
        Some(c) => {
            let dir = crate::auth::resolve_path(c, user);
            let p = std::path::Path::new(&dir);
            if p.is_dir() {
                Ok(dir)
            } else if p.exists() {
                Err(format!("cwd '{dir}' is not a directory"))
            } else {
                Err(format!("cwd '{dir}' does not exist"))
            }
        }
        None => {
            if std::path::Path::new(&user.home).is_dir() {
                Ok(user.home.clone())
            } else {
                tracing::warn!("home {} does not exist, using / as cwd", user.home);
                Ok("/".to_string())
            }
        }
    }
}

/// Build the shared `pre_exec` closure: cgroup placement, process/session
/// setup, privilege drop, then chdir.
fn child_pre_exec(
    user: &User,
    cwd: &str,
    cgroup_fd: Option<RawFd>,
    controlling_tty: bool,
) -> std::io::Result<impl FnMut() -> std::io::Result<()> + Send + Sync> {
    let uid = user.uid;
    let gid = user.gid;
    let groups: Vec<libc::gid_t> = user.groups.iter().map(|g| *g as libc::gid_t).collect();
    let cwd_c = std::ffi::CString::new(cwd.as_bytes())
        .map_err(|_| std::io::Error::new(std::io::ErrorKind::InvalidInput, "cwd contains NUL"))?;
    Ok(move || unsafe {
        // Place the child while it still has the daemon's cgroup privileges.
        if let Some(dirfd) = cgroup_fd {
            place_in_cgroup(dirfd)?;
        }

        if controlling_tty {
            // A controlling terminal can only be acquired by a session leader.
            // std::process has already installed the PTY slave on fd 0 before
            // pre_exec runs, so attach that fd after creating the session.
            // setsid also makes pid == sid == pgid, preserving whole-group
            // signalling through kill(-pid, ...).
            if libc::setsid() < 0 {
                return Err(std::io::Error::last_os_error());
            }
            if libc::ioctl(libc::STDIN_FILENO, libc::TIOCSCTTY, 0) != 0 {
                return Err(std::io::Error::last_os_error());
            }
        } else if libc::setpgid(0, 0) != 0 {
            // Pipe-spawned commands need their own process group so timeout
            // and SendSignal reach descendants as well as the direct child.
            return Err(std::io::Error::last_os_error());
        }

        let drop_privs = !(libc::geteuid() == uid && libc::getegid() == gid);
        if drop_privs {
            if libc::setgroups(groups.len(), groups.as_ptr()) != 0 {
                return Err(std::io::Error::last_os_error());
            }
            if libc::setgid(gid) != 0 {
                return Err(std::io::Error::last_os_error());
            }
            if libc::setuid(uid) != 0 {
                return Err(std::io::Error::last_os_error());
            }
        }
        if libc::chdir(cwd_c.as_ptr()) != 0 {
            return Err(std::io::Error::last_os_error());
        }
        Ok(())
    })
}

pub fn spawn(
    cmd: &str,
    args: &[String],
    env: HashMap<String, String>,
    cwd: String,
    user: &User,
    stdin_enabled: bool,
    cgroup_fd: Option<RawFd>,
) -> std::io::Result<SpawnedProcess> {
    let mut command = tokio::process::Command::new(cmd);
    command
        .args(args)
        .env_clear()
        .envs(&env)
        .stdin(if stdin_enabled {
            Stdio::piped()
        } else {
            Stdio::null()
        })
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .kill_on_drop(false);
    unsafe {
        command.pre_exec(child_pre_exec(user, &cwd, cgroup_fd, false)?);
    }

    let mut child = command.spawn()?;
    // A successfully spawned child always has an id until it is awaited; the
    // fallback to 0 never fires in practice, but kill_process_group guards
    // against 0/1 regardless so a bogus pid can never signal envd's own group.
    let pid = child.id().unwrap_or_default();
    let input = Arc::new(tokio::sync::Mutex::new(InputWriter::Pipe(
        child.stdin.take(),
    )));
    let stdout = child.stdout.take();
    let stderr = child.stderr.take();

    // A bounded broadcast (capacity 64) is the per-process output bus: the
    // pump publishes here and each connection subscribes. A subscriber that
    // falls behind the ring is dropped on its own `Lagged` error instead of
    // backpressuring the pump — the cancel-on-overflow shape upstream #3292
    // recommends, so one stale subscriber can't wedge the whole fan-out.
    // `initial` is created *before* the pump task so the first subscriber
    // never misses an early event.
    let (tx, initial) = broadcast::channel::<PumpEvent>(64);
    // A clone kept for `Connect` to subscribe later subscribers; the pump task
    // moves `tx` itself below.
    let sender = tx.clone();

    tokio::spawn(async move {
        let out_task = pump_pipe(stdout, tx.clone(), false);
        let err_task = pump_pipe(stderr, tx.clone(), true);
        // Drain both pipes fully before reporting the exit status so no
        // DataEvent can arrive after the EndEvent.
        let (_, _) = tokio::join!(out_task, err_task);
        match child.wait().await {
            Ok(status) => {
                let _ = tx.send(PumpEvent::End(EndEvent::from_exit_status(status)));
            }
            Err(e) => {
                let _ = tx.send(PumpEvent::SpawnError(format!("wait failed: {e}")));
            }
        }
    });

    Ok(SpawnedProcess {
        pid,
        initial,
        sender,
        pty_master: None,
        input,
    })
}

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
    let master = unsafe { libc::posix_openpt(libc::O_RDWR | libc::O_NOCTTY | libc::O_CLOEXEC) };
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
pub fn spawn_pty(
    cmd: &str,
    args: &[String],
    env: HashMap<String, String>,
    cwd: String,
    user: &User,
    size: (u16, u16),
    cgroup_fd: Option<RawFd>,
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
    let master = tokio::fs::File::from_std(master_file);
    let input = Arc::new(tokio::sync::Mutex::new(InputWriter::Pty(
        tokio::fs::File::from_std(input_master),
    )));

    tokio::spawn(async move {
        // Read the master until EOF (the child exited and closed its slave),
        // then reap — the same "drain fully before End" contract as `spawn`.
        let pump_error = pump_pty(master, tx.clone()).await.err();
        if pump_error.is_some() {
            let _ = kill_process_group(pid, libc::SIGKILL);
        }
        let wait_result = child.wait().await;
        match (pump_error, wait_result) {
            (Some(read_error), Ok(_)) => {
                let _ = tx.send(PumpEvent::SpawnError(format!(
                    "pty read failed: {read_error}"
                )));
            }
            (Some(read_error), Err(wait_error)) => {
                let _ = tx.send(PumpEvent::SpawnError(format!(
                    "pty read failed: {read_error}; wait failed: {wait_error}"
                )));
            }
            (None, Ok(status)) => {
                let _ = tx.send(PumpEvent::End(EndEvent::from_exit_status(status)));
            }
            (None, Err(e)) => {
                let _ = tx.send(PumpEvent::SpawnError(format!("wait failed: {e}")));
            }
        }
    });

    Ok(SpawnedProcess {
        pid,
        initial,
        sender,
        pty_master: Some(resize_master),
        input,
    })
}

/// Pump a pty master fd into `DataEvent { pty }` frames. Mirrors `pump_pipe`:
/// keep draining once the last subscriber is gone (so the child never blocks
/// on a full pty buffer) but stop encoding.
async fn pump_pty(
    mut master: tokio::fs::File,
    tx: broadcast::Sender<PumpEvent>,
) -> std::io::Result<()> {
    use base64::Engine;
    let mut buf = vec![0u8; READ_CHUNK];
    let mut receiver_gone = false;
    loop {
        match master.read(&mut buf).await {
            Ok(0) => return Ok(()),
            Err(e) if is_pty_eof(&e) => return Ok(()),
            Err(e) if e.kind() == std::io::ErrorKind::Interrupted => continue,
            Err(e) => return Err(e),
            Ok(n) => {
                if receiver_gone {
                    continue;
                }
                let b64 = base64::engine::general_purpose::STANDARD.encode(&buf[..n]);
                let event = DataEvent {
                    pty: Some(b64),
                    ..Default::default()
                };
                if tx.send(PumpEvent::Data(event)).is_err() {
                    receiver_gone = true;
                }
            }
        }
    }
}

/// Linux returns EIO when the last PTY slave closes. It is the PTY equivalent
/// of EOF; other errors must remain visible to the process stream.
fn is_pty_eof(error: &std::io::Error) -> bool {
    error.raw_os_error() == Some(libc::EIO)
}

async fn pump_pipe<R>(pipe: Option<R>, tx: broadcast::Sender<PumpEvent>, is_stderr: bool)
where
    R: tokio::io::AsyncRead + Unpin,
{
    use base64::Engine;
    let Some(mut pipe) = pipe else { return };
    let mut buf = vec![0u8; READ_CHUNK];
    // Set once the last receiver goes away: keep draining so the child never
    // blocks on a full pipe, but skip encoding/sending. Currently unreachable
    // — `initial` is the sole receiver and drive_stream holds it until it has
    // seen End, which the pump sends only after both pipes drain — but it
    // becomes live once Connect adds subscribers that can all drop while a
    // lingering descendant still holds the pipe open.
    let mut receiver_gone = false;
    loop {
        match pipe.read(&mut buf).await {
            Ok(0) | Err(_) => break,
            Ok(n) => {
                if receiver_gone {
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
                // `send` fails only when every subscriber has gone away; keep
                // draining the pipe so the child never blocks, but stop
                // encoding once nobody is listening.
                if tx.send(PumpEvent::Data(event)).is_err() {
                    receiver_gone = true;
                }
            }
        }
    }
}

/// Signal an entire process group started by `spawn` (which puts each child in
/// its own group with pgid == pid). Sending to `-pid` reaches the child and any
/// descendants it forked, so a timeout or SendSignal cleans up the whole tree
/// instead of orphaning grandchildren.
///
/// Refuses pid <= 1: `kill(0, …)`/`kill(-0, …)` would target envd's OWN process
/// group and `kill(-1, …)` every process the daemon may signal, either of which
/// would take envd itself down. A spawned child never legitimately has such a
/// pid; a bogus one is dropped rather than acted on.
pub fn kill_process_group(pid: u32, signo: i32) -> std::io::Result<()> {
    if pid <= 1 {
        return Err(std::io::Error::new(
            std::io::ErrorKind::InvalidInput,
            format!("refusing to signal pid {pid}"),
        ));
    }
    let rc = unsafe { libc::kill(-(pid as libc::pid_t), signo) };
    if rc != 0 {
        return Err(std::io::Error::last_os_error());
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn current_user() -> User {
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

    #[tokio::test]
    async fn spawn_captures_stdout_stderr_and_exit() {
        let user = current_user();
        let env = HashMap::from([("PATH".to_string(), DEFAULT_PATH.to_string())]);
        let mut proc = spawn(
            "/bin/sh",
            &["-c".into(), "echo out1; echo err1 >&2; exit 3".into()],
            env,
            "/".into(),
            &user,
            false,
            None,
        )
        .unwrap();
        assert!(proc.pid > 0);

        let mut stdout = Vec::new();
        let mut stderr = Vec::new();
        let mut end: Option<EndEvent> = None;
        loop {
            match proc.initial.recv().await {
                Ok(PumpEvent::Data(d)) => {
                    use base64::Engine;
                    if let Some(s) = d.stdout {
                        stdout.extend(base64::engine::general_purpose::STANDARD.decode(s).unwrap());
                    }
                    if let Some(s) = d.stderr {
                        stderr.extend(base64::engine::general_purpose::STANDARD.decode(s).unwrap());
                    }
                }
                Ok(PumpEvent::End(e)) => {
                    end = Some(e);
                    break;
                }
                Ok(PumpEvent::SpawnError(e)) => panic!("spawn error: {e}"),
                Err(_) => break,
            }
        }
        assert_eq!(String::from_utf8_lossy(&stdout), "out1\n");
        assert_eq!(String::from_utf8_lossy(&stderr), "err1\n");
        let end = end.expect("end event");
        assert_eq!(end.exit_code, 3);
        assert!(end.exited);
        assert_eq!(end.status, "exit status 3");
    }

    #[tokio::test]
    #[ignore = "A1 probe (plan §5): needs root + a writable cgroup v2 mount"]
    async fn spawn_lands_child_in_its_cgroup() {
        // Locks the "dir fd is still live at pre_exec time" ordering
        // assumption against toolchain upgrades: with a real cgroup dir fd,
        // pre_exec's openat on it must succeed so the child lands inside that
        // subtree. Needs root and a writable cgroup v2 fs:
        //   sudo cargo test -- --ignored spawn_lands_child_in_its_cgroup
        use std::os::unix::io::AsRawFd;
        use std::path::{Path, PathBuf};
        use std::time::Duration;

        assert_eq!(
            unsafe { libc::geteuid() },
            0,
            "A1 probe needs root: sudo cargo test -- --ignored spawn_lands_child_in_its_cgroup"
        );

        let root = Path::new("/sys/fs/cgroup");
        let name = format!("cube-a1-{}", std::process::id());
        let dir = root.join(&name);
        std::fs::create_dir(&dir)
            .unwrap_or_else(|e| panic!("mkdir {dir:?} (cgroup v2 writable?): {e}"));

        // rmdir needs the cgroup empty; the child is SIGKILLed before the
        // guard runs. Leftover dirs (prefix cube-a1-) can be removed manually.
        struct Cleanup(PathBuf);
        impl Drop for Cleanup {
            fn drop(&mut self) {
                let _ = std::fs::remove_dir(&self.0);
            }
        }
        let _cleanup = Cleanup(dir.clone());

        let user = current_user();
        let dirfd = std::fs::File::open(&dir).unwrap();
        let proc = spawn(
            "/bin/sh",
            &["-c".into(), "sleep 5".into()],
            HashMap::new(),
            "/".into(),
            &user,
            false,
            Some(dirfd.as_raw_fd()),
        )
        .unwrap();

        // pre_exec writes the pid before exec; poll cgroup.procs until the
        // child shows up (it must land in the subtree, never silently
        // outside it).
        let procs_path = dir.join("cgroup.procs");
        let deadline = std::time::Instant::now() + Duration::from_secs(3);
        loop {
            let content = std::fs::read_to_string(&procs_path).unwrap_or_default();
            if content
                .split_whitespace()
                .any(|p| p == proc.pid.to_string())
            {
                break;
            }
            assert!(
                std::time::Instant::now() < deadline,
                "child pid {} never appeared in {procs_path:?}",
                proc.pid
            );
            std::thread::sleep(Duration::from_millis(50));
        }
        // The kernel agrees via /proc: the child's cgroup path ends with the
        // probe directory name.
        let cg = std::fs::read_to_string(format!("/proc/{}/cgroup", proc.pid)).unwrap();
        assert!(
            cg.trim().ends_with(&name),
            "child cgroup {cg:?} not under probe dir {name}"
        );

        kill_process_group(proc.pid, libc::SIGKILL).unwrap();
        // Let the child die so the cleanup rmdir has a chance (best effort;
        // the guard ignores failure).
        std::thread::sleep(Duration::from_millis(100));
    }

    #[tokio::test]
    async fn spawn_fails_fast_when_cgroup_placement_fails() {
        // Cgroup placement runs first in pre_exec and any error aborts the
        // spawn (upstream clone3 semantics): a directory fd without a
        // writable cgroup.procs must fail the spawn, never "succeed" with
        // the process outside its subtree.
        use std::os::unix::io::AsRawFd;
        let user = current_user();
        let dir = tempfile::tempdir().unwrap();
        let dirfd = std::fs::File::open(dir.path()).unwrap();
        let err = spawn(
            "/bin/sh",
            &["-c".into(), "echo should-not-run".into()],
            HashMap::new(),
            "/".into(),
            &user,
            false,
            Some(dirfd.as_raw_fd()),
        )
        .unwrap_err();
        assert_eq!(err.kind(), std::io::ErrorKind::NotFound);

        // An invalid fd fails the same way; the child never execs either way.
        let err = spawn(
            "/bin/sh",
            &["-c".into(), "echo should-not-run".into()],
            HashMap::new(),
            "/".into(),
            &user,
            false,
            Some(-1),
        )
        .unwrap_err();
        assert_eq!(err.raw_os_error(), Some(libc::EBADF));

        // PTY spawns must honor the same fail-fast placement contract.
        let err = spawn_pty(
            "/bin/sh",
            &["-c".into(), "echo should-not-run".into()],
            HashMap::new(),
            "/".into(),
            &user,
            (80, 24),
            Some(-1),
        )
        .unwrap_err();
        assert_eq!(err.raw_os_error(), Some(libc::EBADF));
    }

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

    #[test]
    fn only_eio_is_treated_as_pty_eof() {
        assert!(is_pty_eof(&std::io::Error::from_raw_os_error(libc::EIO)));
        assert!(!is_pty_eof(&std::io::Error::from_raw_os_error(libc::EBADF)));
        assert!(!is_pty_eof(&std::io::Error::from(
            std::io::ErrorKind::Interrupted
        )));
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
            }
        }

        let text = String::from_utf8_lossy(&output);
        assert!(text.contains("WINCH"), "PTY output: {text:?}");
        assert!(text.contains("43 132"), "PTY output: {text:?}");
    }

    #[tokio::test]
    async fn signal_end_event_shape() {
        let user = current_user();
        let mut proc = spawn(
            "/bin/sh",
            &["-c".into(), "sleep 30".into()],
            HashMap::new(),
            "/".into(),
            &user,
            false,
            None,
        )
        .unwrap();
        tokio::time::sleep(std::time::Duration::from_millis(100)).await;
        kill_process_group(proc.pid, libc::SIGKILL).unwrap();
        let mut end = None;
        loop {
            match proc.initial.recv().await {
                Ok(PumpEvent::End(e)) => {
                    end = Some(e);
                    break;
                }
                Ok(_) => {}
                Err(_) => break,
            }
        }
        let end = end.unwrap();
        assert_eq!(end.exit_code, -1);
        assert!(!end.exited);
        assert_eq!(end.status, "signal: killed");
    }

    #[test]
    fn cwd_resolution() {
        let user = current_user();
        assert_eq!(resolve_cwd(Some("/tmp"), &user).unwrap(), "/tmp");
        // A missing or non-directory cwd is now rejected, not silently /.
        assert!(resolve_cwd(Some("/no/such/dir/xyz"), &user).is_err());
        assert!(resolve_cwd(Some("/etc/hostname"), &user).is_err());
        // No cwd → the user's home (exists in the test environment).
        assert!(resolve_cwd(None, &user).is_ok());
    }

    #[tokio::test]
    async fn kill_process_group_refuses_low_pids() {
        assert!(kill_process_group(0, libc::SIGKILL).is_err());
        assert!(kill_process_group(1, libc::SIGKILL).is_err());
    }

    #[test]
    fn env_merge_order() {
        let state = AppState::new();
        state.merge_env_vars(HashMap::from([
            ("FROM_INIT".to_string(), "1".to_string()),
            ("PATH".to_string(), "/init-path".to_string()),
        ]));
        let user = current_user();
        let req = HashMap::from([("PATH".to_string(), "/req-path".to_string())]);
        let env = merged_env(&state, &user, &req);
        assert_eq!(env["PATH"], "/req-path"); // request wins over init
        assert_eq!(env["FROM_INIT"], "1");
        assert_eq!(env["E2B_SANDBOX"], "false");
        assert_eq!(env["USER"], "test");
    }
}
