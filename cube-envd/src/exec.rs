// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Process execution: privilege drop, environment merging and the
//! stdout/stderr pump feeding the Start event stream.

use std::collections::HashMap;
use std::os::fd::{AsRawFd, FromRawFd, RawFd};
use std::process::Stdio;
use std::sync::Arc;
use std::sync::Mutex;

use tokio::io::unix::AsyncFd;
use tokio::io::AsyncReadExt;
use tokio::sync::{broadcast, oneshot, Notify};

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
/// Once the direct child has been reaped, inherited stdout/stderr or PTY
/// slave descriptors must not keep the process entry alive forever. Normal
/// exits reach EOF immediately; this grace period only catches background or
/// daemonized descendants that deliberately retain those descriptors.
const OUTPUT_DRAIN_GRACE: std::time::Duration = std::time::Duration::from_millis(500);

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
    pub cgroup: Arc<Mutex<Option<Arc<crate::cgroup::ProcessCgroup>>>>,
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

#[cfg(test)]
pub fn spawn(
    cmd: &str,
    args: &[String],
    env: HashMap<String, String>,
    cwd: String,
    user: &User,
    stdin_enabled: bool,
    cgroup_fd: Option<RawFd>,
) -> std::io::Result<SpawnedProcess> {
    spawn_with_cgroup(cmd, args, env, cwd, user, stdin_enabled, cgroup_fd, None)
}

/// Spawn a pipe-backed process and seed the cgroup metadata before the pump
/// task starts. This closes the fast-exit race where an OOM/termination event
/// could otherwise be decorated before the process service stores its leaf.
#[allow(clippy::too_many_arguments)]
pub fn spawn_with_cgroup(
    cmd: &str,
    args: &[String],
    env: HashMap<String, String>,
    cwd: String,
    user: &User,
    stdin_enabled: bool,
    cgroup_fd: Option<RawFd>,
    process_cgroup: Option<Arc<crate::cgroup::ProcessCgroup>>,
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
        let output = async {
            tokio::try_join!(
                pump_pipe(stdout, tx.clone(), false),
                pump_pipe(stderr, tx.clone(), true)
            )
            .map(|_| ())
        };
        tokio::pin!(output);
        let wait = child.wait();
        tokio::pin!(wait);

        // Poll wait and the output pumps together. Waiting for EOF first can
        // leave the direct child as a zombie forever when a daemonized
        // descendant inherits a pipe. Once wait wins, give already-buffered
        // output a short chance to drain, then close our read ends.
        let terminal = tokio::select! {
            output_result = &mut output => {
                if output_result.is_err() {
                    let _ = kill_process_group(pid, libc::SIGKILL);
                }
                let wait_result = wait.await;
                reaped_for_pump.notify_one();
                terminal_after_output("process output", output_result, wait_result)
            }
            wait_result = &mut wait => {
                reaped_for_pump.notify_one();
                let output_result = tokio::time::timeout(OUTPUT_DRAIN_GRACE, &mut output).await;
                terminal_after_wait("process output", pid, wait_result, output_result)
            }
        };
        let terminal = decorate_terminal(terminal, &termination_for_pump, &cgroup_for_pump);
        let mut slot = terminal_for_pump
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        *slot = Some(terminal.clone());
        drop(slot);
        let _ = tx.send(terminal);
        // Signal completion only after the terminal event is cached and
        // published. This gives the supervisor a race-free handoff point for
        // process-table removal.
        let _ = completion_tx.send(());
    });

    Ok(SpawnedProcess {
        pid,
        initial,
        sender,
        pty_master: None,
        input,
        completion,
        terminal,
        reaped,
        termination,
        cgroup,
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

/// PTY variant of [`spawn_with_cgroup`].
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

fn decorate_terminal(
    event: PumpEvent,
    termination: &Arc<Mutex<Option<String>>>,
    cgroup: &Arc<Mutex<Option<Arc<crate::cgroup::ProcessCgroup>>>>,
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

fn terminal_after_output(
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

fn terminal_after_wait(
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
async fn pump_pty(
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

async fn pump_pipe<R>(
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

    #[test]
    fn oom_metadata_requires_sigkill_and_preserves_recorded_causes() {
        use std::os::unix::process::ExitStatusExt;

        let directory = tempfile::tempdir().unwrap();
        let events = directory.path().join("memory.events");
        std::fs::write(&events, "oom_kill 0\n").unwrap();
        let group = Arc::new(crate::cgroup::ProcessCgroup::new(
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
                Ok(PumpEvent::DeadlineExceeded) => panic!("unexpected deadline"),
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
