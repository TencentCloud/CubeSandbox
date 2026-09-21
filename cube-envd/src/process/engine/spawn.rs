// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Command configuration, privilege setup and process creation for both kinds of
//! stdio: pipes and a pty.

use std::collections::HashMap;
use std::os::fd::{AsRawFd, FromRawFd, RawFd};
use std::process::Stdio;
use std::sync::{Arc, Mutex};

use tokio::io::unix::AsyncFd;
use tokio::sync::{oneshot, watch, Notify};

use crate::process::{AttachmentClaim, OutputBus};

use crate::platform::config::Config;
use crate::platform::identity::User;

use super::child::{spawn_without_fork, ChildHandle, ChildSpec, ChildStdio, SpawnFailure};
use super::cleanup::kill_process_group;
use super::io::{
    decorate_terminal, pump_pipe, pump_pty, terminal_after_output, terminal_after_wait,
    unread_bytes, OutputKind, OUTPUT_DRAIN_GRACE,
};
use super::{InputHandle, InputWriter, SpawnedProcess};

/// Write this child's pid into `dirfd`'s `cgroup.procs`. Runs in the child
/// before `execve`, so it must be allocation-free and call only async-signal-safe
/// libc. `dirfd` is a manager-owned cgroup directory fd
/// (borrowed for the daemon lifetime — never closed here; the open on
/// `cgroup.procs` carries O_CLOEXEC so the fd cannot leak past exec).
fn place_in_cgroup(dirfd: RawFd) -> std::io::Result<()> {
    let procs = b"cgroup.procs\0";
    // SAFETY: runs in the child before exec, single-threaded; dirfd is a
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
    debug_assert!(pid > 0, "a child always has a pid");
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
pub(super) const DEFAULT_PATH: &str =
    "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin";

/// Merge order (later wins): built-in defaults < /init env vars < request envs.
pub fn merged_env(
    config: &Config,
    user: &User,
    request_envs: &HashMap<String, String>,
) -> HashMap<String, String> {
    let mut env = HashMap::new();
    env.insert("PATH".to_string(), DEFAULT_PATH.to_string());
    env.insert("HOME".to_string(), user.home.clone());
    env.insert("USER".to_string(), user.name.clone());
    env.insert("LOGNAME".to_string(), user.name.clone());
    env.insert("TERM".to_string(), "xterm".to_string());
    env.extend(config.env_vars());
    env.extend(request_envs.clone());
    env
}

/// Resolve the requested working directory to an absolute path.
///
/// - An explicit `cwd` (relative anchored at the user's home) must name an
///   existing directory; otherwise this returns `Err(message)` which the
///   caller surfaces as `invalid_argument`. Upstream Go envd rejects a missing
///   or non-directory cwd the same way; cube-envd must not silently fall back
///   to `/` and run the command anyway (#1227: no silent success on invalid
///   input).
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
            let dir = crate::platform::identity::resolve_path(c, user);
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
pub(super) fn child_pre_exec(
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

/// One command to start, taking the request shape upstream's `handler.New` takes:
/// the command with an optional pty, which upstream splits on at `handler.go:192`
/// and branches on again in `Start` (`handler.go:425`). The child is created with
/// `clone(CLONE_VM|CLONE_VFORK)` (see [`super::child`]), falling back to `fork`
/// only when the host rejects it; a failure that came back through the child's
/// report pipe is a real command error and is not retried, or the command would
/// run twice.
///
/// `posix_spawn` itself is not usable here because it cannot express the
/// credential drop (the daemon runs as root and commands run as the requested
/// user), so the child-side setup runs our own code — the same
/// [`child_pre_exec`] closure both mechanisms install.
pub struct Spawn<'a> {
    pub cmd: &'a str,
    pub args: &'a [String],
    pub env: HashMap<String, String>,
    pub cwd: String,
    pub user: &'a User,
    /// Pipe stdin. Ignored when `pty` is set: input goes to the terminal, as it
    /// does upstream.
    pub stdin: bool,
    /// `(cols, rows)` for a pty, `None` for pipes.
    pub pty: Option<(u16, u16)>,
    pub cgroup_fd: Option<RawFd>,
    pub process_cgroup: Option<Arc<crate::process::cgroup::ProcessCgroup>>,
}

/// The parent's end of a pty: what the pump reads, what input writes to, and the
/// duplicate `Update` resizes through.
struct PtyParts {
    master: AsyncFd<std::fs::File>,
    input: InputHandle,
    resize: std::fs::File,
}

/// What the pump reads, which is the one thing the two kinds do not share.
enum Streams {
    Pipes {
        stdout: Option<tokio::process::ChildStdout>,
        stderr: Option<tokio::process::ChildStderr>,
    },
    Pty(AsyncFd<std::fs::File>),
}

/// Spawn a command and seed the cgroup metadata before the pump task starts.
/// This closes the fast-exit race where an OOM/termination event could otherwise
/// be decorated before the process service stores its leaf.
pub fn spawn(req: Spawn<'_>) -> std::io::Result<SpawnedProcess> {
    // The process-wide attachment budget is claimed before anything at all is
    // created: a command whose first stream could never be attached has to be
    // refused, not spawned and then killed. The claim is released if this
    // function returns early, and the bus below takes it over on success.
    let claim = AttachmentClaim::claim().map_err(|_| {
        std::io::Error::new(
            std::io::ErrorKind::WouldBlock,
            "too many concurrent output attachments",
        )
    })?;
    // Stdio first: a pty pair is opened and its master registered before any
    // child exists, so a registration failure cannot strand one.
    let (child_stdio, slave, pty) = match req.pty {
        Some((cols, rows)) => {
            let (master, slave) = super::pty::open_pty(cols, rows)?;
            let resize = master.try_clone()?;
            let input_master = master.try_clone()?;
            let master = AsyncFd::new(master)?;
            let input = Arc::new(tokio::sync::Mutex::new(InputWriter::Pty(AsyncFd::new(
                input_master,
            )?)));
            (
                ChildStdio::Inherit(slave.as_raw_fd()),
                Some(slave),
                Some(PtyParts {
                    master,
                    input,
                    resize,
                }),
            )
        }
        None => (ChildStdio::Pipes { stdin: req.stdin }, None, None),
    };

    let fork_free = {
        let mut before_exec = child_pre_exec(req.user, &req.cwd, req.cgroup_fd, req.pty.is_some())?;
        spawn_without_fork(ChildSpec {
            cmd: req.cmd,
            args: req.args,
            env: &req.env,
            path: req
                .env
                .get("PATH")
                .map(String::as_str)
                .unwrap_or(DEFAULT_PATH),
            stdio: child_stdio,
            before_exec: &mut before_exec,
        })
    };
    let (mut child, stdin, stdout, stderr) = match fork_free {
        Ok(raw) => (
            ChildHandle::Raw {
                pid: raw.pid,
                pidfd: raw.pidfd,
            },
            raw.stdin,
            raw.stdout,
            raw.stderr,
        ),
        Err(SpawnFailure::Child(err)) => return Err(err),
        Err(SpawnFailure::Unsupported(err)) => {
            static WARNED: std::sync::Once = std::sync::Once::new();
            WARNED.call_once(|| {
                tracing::warn!(
                    "fork-free spawn unavailable ({err}); commands will use fork, \
                     which costs a copy-on-write fault per page the daemon rewrites"
                );
            });
            spawn_forked(&req, child_stdio)?
        }
    };

    // The child holds its own reference through fds 0/1/2 now; ours has to go,
    // or the master never reports HUP when the child exits and every command
    // waits out the output-drain grace.
    drop(slave);

    let (input, streams, pty_master, kind) = match pty {
        Some(parts) => (
            parts.input,
            Streams::Pty(parts.master),
            Some(parts.resize),
            OutputKind::Pty,
        ),
        None => (
            Arc::new(tokio::sync::Mutex::new(InputWriter::Pipe(stdin))),
            Streams::Pipes { stdout, stderr },
            None,
            OutputKind::Process,
        ),
    };

    // A successfully spawned child always has an id until it is awaited; the
    // fallback to 0 never fires in practice, but kill_process_group guards
    // against 0/1 regardless so a bogus pid can never signal envd's own group.
    let pid = child.id().unwrap_or_default();

    // The per-process output bus (see `process::bus`): the pump publishes here
    // and each connection holds a subscription. A subscriber that falls behind
    // backpressures the pump rather than losing data, and one that makes no
    // progress for the eviction window is disconnected on its own.
    // `initial` is created *before* the pump task so the first subscription
    // never misses an early event.
    let (bus, mut initial) = OutputBus::new_claimed(claim);
    // A clone kept for `Connect` to attach later subscribers; the pump task
    // moves `bus` itself below.
    let sender = Arc::clone(&bus);
    // Duplicates of the output read ends outlive the pump task, so the grace
    // branch can ask `FIONREAD` whether it abandoned buffered output or simply
    // stopped a reader that had nothing left. Either duplicate keeps the pipe
    // (or pty master) open after the pumps drop theirs, which is what makes the
    // question answerable at all. `dup_owned` owns them so that *every* way out
    // of the pump task -- either `select!` arm, or the task being dropped before
    // it runs -- closes them, and its `F_DUPFD_CLOEXEC` keeps them out of the fd
    // table of every child spawned from then on.
    let drain_probes: Vec<std::fs::File> = {
        let mut read_ends: Vec<RawFd> = Vec::new();
        match &streams {
            Streams::Pipes { stdout, stderr } => {
                if let Some(stream) = stdout {
                    read_ends.push(stream.as_raw_fd());
                }
                if let Some(stream) = stderr {
                    read_ends.push(stream.as_raw_fd());
                }
            }
            Streams::Pty(master) => read_ends.push(master.as_raw_fd()),
        }
        read_ends
            .into_iter()
            .filter_map(|fd| match dup_owned(fd) {
                Ok(file) => Some(file),
                Err(error) => {
                    tracing::debug!("could not duplicate a drain probe: {error}");
                    None
                }
            })
            .collect()
    };
    let (completion_tx, completion) = oneshot::channel();
    let terminal = Arc::new(std::sync::Mutex::new(None));
    // The Start subscription gets the same terminal cache as later `Connect`s,
    // so a bus that disappears before the terminal event still reports the real
    // exit instead of an internal error.
    initial.watch_terminal_cache(Arc::clone(&terminal));
    let terminal_for_pump = terminal.clone();
    let reaped = Arc::new(Notify::new());
    let reaped_for_pump = reaped.clone();
    let termination = Arc::new(Mutex::new(None));
    let cgroup = Arc::new(Mutex::new(req.process_cgroup));
    let termination_for_pump = termination.clone();
    let cgroup_for_pump = cgroup.clone();

    let (stop_tx, stop_rx) = watch::channel(false);
    tokio::spawn(async move {
        // Keeps the drain stop channel open for as long as the pumps run, so a
        // closed sender can never be mistaken for "stop reading".
        let stop_tx = stop_tx;
        // The pumps get their own handle so the terminal event can still be
        // published through `bus` once they finish.
        let output_bus = Arc::clone(&bus);
        // The pumps own a receiver clone; `stop_rx` stays here so the reap
        // branch can read whether the grace already stopped them.
        let output_stop = stop_rx.clone();
        let output = async move {
            match streams {
                // `try_join!` awaits internally, so the pipe arm yields a
                // `Result`; the pty arm has to be awaited to match it.
                Streams::Pipes { stdout, stderr } => tokio::try_join!(
                    pump_pipe(stdout, Arc::clone(&output_bus), output_stop.clone(), false),
                    pump_pipe(stderr, Arc::clone(&output_bus), output_stop.clone(), true)
                )
                .map(|_| ()),
                Streams::Pty(master) => {
                    pump_pty(master, Arc::clone(&output_bus), output_stop.clone()).await
                }
            }
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
                if let Err(error) = &output_result {
                    if kind.read_error_is_fatal() {
                        let _ = kill_process_group(pid, libc::SIGKILL);
                    } else {
                        tracing::warn!(pid, "error reading from pty: {error}");
                    }
                }
                let wait_result = wait.await;
                reaped_for_pump.notify_one();
                // The child is reaped: process-table cleanup may proceed now.
                // Draining the rest of the output stays independent of it.
                let _ = completion_tx.send(());
                terminal_after_output(kind, output_result, wait_result)
            }
            wait_result = &mut wait => {
                reaped_for_pump.notify_one();
                // The child is reaped: process-table cleanup may proceed now.
                // Draining the rest of the output stays independent of it.
                let _ = completion_tx.send(());
                // Bound only the wait for *new* data. Once the direct child is
                // reaped its buffered output is read to EOF, so the grace only
                // decides how long to wait for a descendant that inherited the
                // pipe; it never cancels a publish that is already in flight.
                let grace_tx = stop_tx.clone();
                tokio::spawn(async move {
                    tokio::time::sleep(OUTPUT_DRAIN_GRACE).await;
                    let _ = grace_tx.send(true);
                });
                let output_result = (&mut output).await;
                let stopped = *stop_rx.borrow();
                // Only a stopped reader can have abandoned anything, and only
                // when bytes were actually left behind.
                let abandoned =
                    stopped && drain_probes.iter().any(|fd| unread_bytes(fd.as_raw_fd()) > 0);
                terminal_after_wait(kind, pid, wait_result, output_result, stopped, abandoned)
            }
        };
        // The probes answered the only question they exist for. Dropping them in
        // one place, after the `select!`, is what keeps the output arm -- where
        // the pumps finish first -- from leaking them.
        drop(drain_probes);
        let terminal = decorate_terminal(terminal, &termination_for_pump, &cgroup_for_pump);
        let mut slot = terminal_for_pump
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        *slot = Some(terminal.clone());
        drop(slot);
        let _ = bus.publish_terminal(terminal);
    });

    Ok(SpawnedProcess {
        pid,
        initial,
        sender,
        pty_master,
        input,
        completion,
        terminal,
        reaped,
        termination,
        cgroup,
    })
}

/// What the `fork` mechanism hands back: the child plus its standard streams,
/// which the caller wraps according to the stdio it asked for.
type ForkedChild = (
    ChildHandle,
    Option<tokio::process::ChildStdin>,
    Option<tokio::process::ChildStdout>,
    Option<tokio::process::ChildStderr>,
);

/// A close-on-exec duplicate of `fd`.
///
/// `F_DUPFD_CLOEXEC` rather than `dup` plus `fcntl`: envd spawns on many threads,
/// so the duplicate must be unmistakably close-on-exec the moment it exists, or a
/// concurrent `execve` inherits it. Both callers -- the `std::process` fallback,
/// which has to own one descriptor per standard stream, and the drain probes in
/// [`spawn`], which outlive their command -- rely on that.
fn dup_owned(fd: RawFd) -> std::io::Result<std::fs::File> {
    // SAFETY: `fcntl` with a valid descriptor and flag; it returns a fresh fd.
    let dup = unsafe { libc::fcntl(fd, libc::F_DUPFD_CLOEXEC, 3) };
    if dup < 0 {
        return Err(std::io::Error::last_os_error());
    }
    // SAFETY: the descriptor is fresh and owned here.
    Ok(unsafe { std::fs::File::from_raw_fd(dup) })
}

/// The `fork` mechanism: `std::process` runs the same [`child_pre_exec`] closure
/// between `fork` and `exec`, and hands back the same handles the fork-free
/// mechanism produces.
fn spawn_forked(req: &Spawn<'_>, stdio: ChildStdio) -> std::io::Result<ForkedChild> {
    let mut command = tokio::process::Command::new(req.cmd);
    command
        .args(req.args)
        .env_clear()
        .envs(&req.env)
        .kill_on_drop(false);
    match stdio {
        ChildStdio::Pipes { stdin } => {
            command
                .stdin(if stdin { Stdio::piped() } else { Stdio::null() })
                .stdout(Stdio::piped())
                .stderr(Stdio::piped());
        }
        // `std` dups these onto 0/1/2 and closes the originals, exactly as the
        // child does on the fork-free path.
        ChildStdio::Inherit(fd) => {
            command
                .stdin(Stdio::from(dup_owned(fd)?))
                .stdout(Stdio::from(dup_owned(fd)?))
                .stderr(Stdio::from(dup_owned(fd)?));
        }
    }
    unsafe {
        command.pre_exec(child_pre_exec(
            req.user,
            &req.cwd,
            req.cgroup_fd,
            matches!(stdio, ChildStdio::Inherit(_)),
        )?);
    }

    let mut child = command.spawn()?;
    let stdin = child.stdin.take();
    let stdout = child.stdout.take();
    let stderr = child.stderr.take();
    Ok((ChildHandle::Command(child), stdin, stdout, stderr))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::process::engine::tests::current_user;
    use crate::process::engine::PumpEvent;
    use crate::process::wire::EndEvent;

    /// The descriptor-counting tests below read the process-wide table, so they
    /// must not run (and leak, if the code regresses) while the other one is
    /// measuring. Other tests spawning concurrently can add a handful of
    /// transient descriptors, which is what the tolerance in each assertion is
    /// for.
    static DESCRIPTOR_TESTS: tokio::sync::Mutex<()> = tokio::sync::Mutex::const_new(());

    /// Descriptors another test may hold open for the duration of one spawn.
    /// The leak this guards against is two per command, so this cannot mask it.
    const LEAK_TOLERANCE: usize = 4;

    #[tokio::test]
    async fn spawn_captures_stdout_stderr_and_exit() {
        let user = current_user();
        let env = HashMap::from([("PATH".to_string(), DEFAULT_PATH.to_string())]);
        let mut proc = spawn(Spawn {
            cmd: "/bin/sh",
            args: &["-c".into(), "echo out1; echo err1 >&2; exit 3".into()],
            env,
            cwd: "/".into(),
            user: &user,
            stdin: false,
            pty: None,
            cgroup_fd: None,
            process_cgroup: None,
        })
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

    /// Regression for the drain probes below: they are created before the pumps
    /// start and consulted only by the grace branch, so the branch where the
    /// pumps finish first (about half of all short commands) used to drop their
    /// raw descriptors without closing them -- a long-running daemon loses about
    /// one per command, and every later child inherits the rest because `dup`
    /// leaves `FD_CLOEXEC` clear.
    ///
    /// Which arm `select!` takes is not forceable from a test, so a run that
    /// happens to take the wait branch every time passes; over 64 spawns that is
    /// vanishingly unlikely, and the assertion cannot fail spuriously.
    #[tokio::test]
    async fn spawning_many_processes_does_not_leak_descriptors() {
        let _serialized = DESCRIPTOR_TESTS.lock().await;
        let user = current_user();
        let env = HashMap::from([("PATH".to_string(), DEFAULT_PATH.to_string())]);

        fn open_descriptors() -> usize {
            std::fs::read_dir("/proc/self/fd")
                .expect("/proc/self/fd")
                .count()
        }

        async fn run_once(user: &User, env: &HashMap<String, String>) {
            let mut proc = spawn(Spawn {
                cmd: "/bin/sh",
                args: &["-c".into(), "true".into()],
                env: env.clone(),
                cwd: "/".into(),
                user,
                stdin: false,
                pty: None,
                cgroup_fd: None,
                process_cgroup: None,
            })
            .unwrap();
            loop {
                match proc.initial.recv().await {
                    Ok(PumpEvent::End(_)) | Ok(PumpEvent::SpawnError(_)) | Err(_) => break,
                    Ok(_) => continue,
                }
            }
        }

        // Warm-up: the first command creates the descriptors the daemon keeps
        // (epoll, eventfd, the log file), which are not part of the steady state.
        run_once(&user, &env).await;
        tokio::time::sleep(std::time::Duration::from_millis(50)).await;
        let before = open_descriptors();
        for _ in 0..64 {
            run_once(&user, &env).await;
        }
        // The pump task closes the probes on its way out, just after it
        // publishes the end event.
        tokio::time::sleep(std::time::Duration::from_millis(100)).await;
        let after = open_descriptors();
        assert!(
            after <= before + LEAK_TOLERANCE,
            "64 spawns grew the descriptor table from {before} to {after}"
        );
    }

    /// The budget is claimed before the child exists, so an exhausted budget
    /// must refuse the command *without* leaving a process behind -- and must
    /// release the slot it could not use.
    ///
    /// `#[ignore]` because it fills the process-wide budget, which would fail
    /// every other test that spawns in parallel:
    ///   cargo test -- --ignored --test-threads=1 a_refused_command_is_not_spawned
    #[tokio::test]
    #[ignore = "fills the process-wide attachment budget; run with --test-threads=1"]
    async fn a_refused_command_is_not_spawned() {
        fn marker_processes(marker: &str) -> usize {
            let mut found = 0;
            for entry in std::fs::read_dir("/proc").expect("/proc").flatten() {
                let cmdline = entry.path().join("cmdline");
                if let Ok(bytes) = std::fs::read(&cmdline) {
                    if String::from_utf8_lossy(&bytes).contains(marker) {
                        found += 1;
                    }
                }
            }
            found
        }

        // Hold every slot in the process-wide budget.
        let mut held = Vec::new();
        while let Ok(claim) = AttachmentClaim::claim() {
            held.push(claim);
        }
        assert_eq!(
            held.len(),
            crate::process::bus_global_limit_for_tests(),
            "the claim loop should stop exactly at the ceiling"
        );

        let marker = "cube-envd-refused-marker";
        let user = current_user();
        let env = HashMap::from([("PATH".to_string(), DEFAULT_PATH.to_string())]);
        let refused = spawn(Spawn {
            cmd: "/bin/sh",
            args: &["-c".into(), format!("sleep 30 # {marker}")],
            env: env.clone(),
            cwd: "/".into(),
            user: &user,
            stdin: false,
            pty: None,
            cgroup_fd: None,
            process_cgroup: None,
        })
        .expect_err("an exhausted budget must refuse the command");
        assert_eq!(
            refused.kind(),
            std::io::ErrorKind::WouldBlock,
            "the refusal has to be recognizable to the Start handler: {refused}"
        );
        // Nothing was created, so nothing has to be cleaned up.
        assert_eq!(marker_processes(marker), 0, "the child must not exist");

        // Releasing the budget makes the same command work again.
        drop(held);
        let mut proc = spawn(Spawn {
            cmd: "/bin/sh",
            args: &["-c".into(), format!("true # {marker}")],
            env,
            cwd: "/".into(),
            user: &user,
            stdin: false,
            pty: None,
            cgroup_fd: None,
            process_cgroup: None,
        })
        .expect("the released budget admits the command");
        loop {
            match proc.initial.recv().await {
                Ok(PumpEvent::End(_)) | Err(_) => break,
                Ok(_) => continue,
            }
        }
    }

    /// The deterministic form of the same regression: a command that closes its
    /// own `stdout`/`stderr` and then keeps running makes both pumps reach EOF
    /// while `wait` is still pending, so `select!` takes the output arm *every*
    /// time -- the arm that used to drop the probes without closing them. The
    /// second command then shows whether envd's leftovers reached a child.
    #[tokio::test]
    async fn the_output_arm_closes_its_drain_probes() {
        let _serialized = DESCRIPTOR_TESTS.lock().await;
        let user = current_user();
        let env = HashMap::from([("PATH".to_string(), DEFAULT_PATH.to_string())]);

        async fn command(user: &User, env: &HashMap<String, String>, cmd: &str) -> String {
            let mut proc = spawn(Spawn {
                cmd: "/bin/sh",
                args: &["-c".into(), cmd.into()],
                env: env.clone(),
                cwd: "/".into(),
                user,
                stdin: false,
                pty: None,
                cgroup_fd: None,
                process_cgroup: None,
            })
            .unwrap();
            let mut out = String::new();
            loop {
                match proc.initial.recv().await {
                    Ok(PumpEvent::Data(d)) => {
                        use base64::Engine;
                        if let Some(s) = d.stdout {
                            out.push_str(&String::from_utf8_lossy(
                                &base64::engine::general_purpose::STANDARD.decode(s).unwrap(),
                            ));
                        }
                    }
                    Ok(PumpEvent::End(_)) | Ok(PumpEvent::SpawnError(_)) | Err(_) => return out,
                    Ok(_) => continue,
                }
            }
        }

        // `/proc/self/fd` of the command itself: `ls` inherits its parent's
        // table, so an inherited probe would be counted here.
        let descriptors_of_a_command = "ls /proc/self/fd | wc -l";
        let before: usize = command(&user, &env, descriptors_of_a_command)
            .await
            .trim()
            .parse()
            .expect("a descriptor count");

        for _ in 0..8 {
            command(&user, &env, "exec 1>&- 2>&-; sleep 0.05").await;
        }
        // The pump task closes the probes just after it publishes the end event.
        tokio::time::sleep(std::time::Duration::from_millis(100)).await;

        let after: usize = command(&user, &env, descriptors_of_a_command)
            .await
            .trim()
            .parse()
            .expect("a descriptor count");
        assert!(
            after <= before + LEAK_TOLERANCE,
            "after 8 output-arm commands a new command saw {after} descriptors, not {before}"
        );
    }

    /// The deterministic half of the same bug: whichever arm the race picks, a
    /// probe duplicate must never be inheritable by the next child.
    #[test]
    fn a_duplicated_probe_is_close_on_exec() {
        let mut fds = [0 as libc::c_int; 2];
        // SAFETY: `fds` is a valid two-element array for `pipe(2)`.
        assert_eq!(unsafe { libc::pipe(fds.as_mut_ptr()) }, 0);
        // SAFETY: both descriptors were just created and are owned from here.
        let read_end = unsafe { std::fs::File::from_raw_fd(fds[0]) };
        // SAFETY: same descriptors, each taken exactly once.
        let write_end = unsafe { std::fs::File::from_raw_fd(fds[1]) };

        let duplicate = dup_owned(read_end.as_raw_fd()).expect("duplicate a read end");
        // SAFETY: `duplicate` is open and `F_GETFD` only reads flags.
        let flags = unsafe { libc::fcntl(duplicate.as_raw_fd(), libc::F_GETFD) };
        assert!(flags >= 0, "F_GETFD: {}", std::io::Error::last_os_error());
        assert_ne!(
            flags & libc::FD_CLOEXEC,
            0,
            "a probe duplicate must not survive execve into a child"
        );
        drop(write_end);
    }

    #[tokio::test]
    #[ignore = "A1 probe: needs root + a writable cgroup v2 mount"]
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
        let proc = spawn(Spawn {
            cmd: "/bin/sh",
            args: &["-c".into(), "sleep 5".into()],
            env: HashMap::new(),
            cwd: "/".into(),
            user: &user,
            stdin: false,
            pty: None,
            cgroup_fd: Some(dirfd.as_raw_fd()),
            process_cgroup: None,
        })
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

    /// The reason a per-command cgroup exists: a descendant that calls `setsid`
    /// leaves the command's process group, so a bare `kill(-pid)` cannot reach
    /// it and only `cgroup.kill` can. The fork-free spawn must land the whole
    /// tree in the leaf, exactly like the fork path, or that escape reopens.
    ///
    /// Needs root and a writable cgroup v2 fs:
    ///   sudo cargo test -- --ignored cgroup_kill_reaches_a_setsid_escapee
    #[tokio::test]
    #[ignore = "needs root + a writable cgroup v2 mount"]
    async fn cgroup_kill_reaches_a_setsid_escapee() {
        use std::path::{Path, PathBuf};
        use std::time::Duration;

        assert_eq!(
            unsafe { libc::geteuid() },
            0,
            "needs root: sudo cargo test -- --ignored cgroup_kill_reaches_a_setsid_escapee"
        );

        let root = Path::new("/sys/fs/cgroup");
        let name = format!("cube-escape-{}", std::process::id());
        let dir = root.join(&name);
        std::fs::create_dir(&dir)
            .unwrap_or_else(|e| panic!("mkdir {dir:?} (cgroup v2 writable?): {e}"));
        struct Cleanup(PathBuf);
        impl Drop for Cleanup {
            fn drop(&mut self) {
                let _ = std::fs::remove_dir(&self.0);
            }
        }
        let _cleanup = Cleanup(dir.clone());

        let dirfd = std::fs::File::open(&dir).unwrap();
        let cgroup = crate::process::cgroup::ProcessCgroup::new(dir.clone(), dirfd);
        let user = current_user();
        let proc = spawn(Spawn {
            cmd: "/bin/sh",
            args: &[
                "-c".into(),
                // `setsid` + a plain background child: one stays in the
                // command's process group, one leaves it.
                "setsid sleep 30 & sleep 30 & wait".into(),
            ],
            env: HashMap::new(),
            cwd: "/".into(),
            user: &user,
            stdin: false,
            pty: None,
            cgroup_fd: Some(cgroup.fd()),
            process_cgroup: None,
        })
        .unwrap();

        // Wait until the whole tree has landed: sh, its group child, and the
        // escapee, which is in a session of its own.
        let procs_path = dir.join("cgroup.procs");
        let deadline = std::time::Instant::now() + Duration::from_secs(3);
        // `group_of` is `None` for a pid that is already gone; that must not
        // read as "a different group", or a failed /proc read would fake the
        // escapee this test depends on.
        let leaves_the_group = |pids: &[u32]| {
            pids.iter()
                .any(|p| group_of(*p).is_some_and(|g| g != proc.pid))
        };
        let pids: Vec<u32> = loop {
            let pids: Vec<u32> = std::fs::read_to_string(&procs_path)
                .unwrap_or_default()
                .split_whitespace()
                .filter_map(|p| p.parse().ok())
                .collect();
            if pids.len() >= 3 && leaves_the_group(&pids) {
                break pids;
            }
            assert!(
                std::time::Instant::now() < deadline,
                "never saw the whole tree in {procs_path:?} (saw {pids:?})"
            );
            std::thread::sleep(Duration::from_millis(50));
        };

        // The escapee is a real one: at least one member sits outside the
        // command's process group, so a bare `kill(-pid)` would miss it.
        assert!(
            leaves_the_group(&pids),
            "no descendant left the process group; the test proves nothing: {pids:?}"
        );

        let escapee = pids
            .iter()
            .copied()
            .find(|p| group_of(*p).is_some_and(|g| g != proc.pid))
            .expect("checked above");

        cgroup.kill_all().unwrap();
        let deadline = std::time::Instant::now() + Duration::from_secs(3);
        for pid in pids {
            while running(pid) {
                assert!(
                    std::time::Instant::now() < deadline,
                    "pid {pid} survived cgroup.kill"
                );
                std::thread::sleep(Duration::from_millis(50));
            }
        }
        // The escapee is a grandchild, so nothing in this test reaps it: it has
        // to disappear from /proc, not merely become a zombie (which `kill(pid,
        // 0)` would still report as present).
        while std::path::Path::new(&format!("/proc/{escapee}")).exists() {
            assert!(
                std::time::Instant::now() < deadline,
                "escapee {escapee} left a process behind after cgroup.kill"
            );
            std::thread::sleep(Duration::from_millis(50));
        }
    }

    /// Whether a pid is still running, as opposed to reaped or a zombie.
    fn running(pid: u32) -> bool {
        let Some(state) = std::fs::read_to_string(format!("/proc/{pid}/stat"))
            .ok()
            .and_then(|stat| {
                stat.rsplit(')')
                    .next()
                    .and_then(|rest| rest.split_whitespace().next())
                    .and_then(|state| state.chars().next())
            })
        else {
            return false;
        };
        !matches!(state, 'Z' | 'X' | 'x')
    }

    /// A pid's process-group id from `/proc`, as the escape check needs it.
    ///
    /// `None` when the pid is gone; the caller must not fold that into a
    /// sentinel, because any sentinel differs from the command's pid and would
    /// look exactly like the escape this test is trying to observe.
    fn group_of(pid: u32) -> Option<u32> {
        // Fields after the parenthesised comm: state ppid pgrp ...
        std::fs::read_to_string(format!("/proc/{pid}/stat"))
            .ok()?
            .rsplit(')')
            .next()
            .and_then(|rest| rest.split_whitespace().nth(2))
            .and_then(|pgrp| pgrp.parse().ok())
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
        let err = spawn(Spawn {
            cmd: "/bin/sh",
            args: &["-c".into(), "echo should-not-run".into()],
            env: HashMap::new(),
            cwd: "/".into(),
            user: &user,
            stdin: false,
            pty: None,
            cgroup_fd: Some(dirfd.as_raw_fd()),
            process_cgroup: None,
        })
        .unwrap_err();
        assert_eq!(err.kind(), std::io::ErrorKind::NotFound);

        // An invalid fd fails the same way; the child never execs either way.
        let err = spawn(Spawn {
            cmd: "/bin/sh",
            args: &["-c".into(), "echo should-not-run".into()],
            env: HashMap::new(),
            cwd: "/".into(),
            user: &user,
            stdin: false,
            pty: None,
            cgroup_fd: Some(-1),
            process_cgroup: None,
        })
        .unwrap_err();
        assert_eq!(err.raw_os_error(), Some(libc::EBADF));

        // PTY spawns must honor the same fail-fast placement contract.
        let err = spawn(Spawn {
            stdin: false,
            cmd: "/bin/sh",
            args: &["-c".into(), "echo should-not-run".into()],
            env: HashMap::new(),
            cwd: "/".into(),
            user: &user,
            pty: Some((80, 24)),
            cgroup_fd: Some(-1),
            process_cgroup: None,
        })
        .unwrap_err();
        assert_eq!(err.raw_os_error(), Some(libc::EBADF));
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

    #[test]
    fn env_merge_order() {
        let config = Config::new();
        config.merge_env_vars(HashMap::from([
            ("FROM_INIT".to_string(), "1".to_string()),
            ("PATH".to_string(), "/init-path".to_string()),
        ]));
        let user = current_user();
        let req = HashMap::from([("PATH".to_string(), "/req-path".to_string())]);
        let env = merged_env(&config, &user, &req);
        assert_eq!(env["PATH"], "/req-path"); // request wins over init
        assert_eq!(env["FROM_INIT"], "1");
        assert_eq!(env["E2B_SANDBOX"], "false");
        assert_eq!(env["USER"], "test");
    }

    /// The fork mechanism is reached when a host rejects the fork-free
    /// primitive, so it keeps its own guard: the same pipes and the same exit
    /// status the caller would see.
    #[tokio::test]
    async fn the_fork_fallback_spawns_identically() {
        use tokio::io::AsyncReadExt;

        let user = current_user();
        let args = ["-c".into(), "echo out; echo err >&2; exit 4".into()];
        let req = Spawn {
            cmd: "/bin/sh",
            args: &args,
            env: HashMap::from([("PATH".to_string(), DEFAULT_PATH.to_string())]),
            cwd: "/".into(),
            user: &user,
            stdin: false,
            pty: None,
            cgroup_fd: None,
            process_cgroup: None,
        };
        let (mut child, _stdin, stdout, stderr) =
            spawn_forked(&req, ChildStdio::Pipes { stdin: false }).unwrap();
        assert!(child.id().is_some_and(|pid| pid > 0));

        let mut out = String::new();
        let mut err = String::new();
        stdout.unwrap().read_to_string(&mut out).await.unwrap();
        stderr.unwrap().read_to_string(&mut err).await.unwrap();
        assert_eq!(out, "out\n");
        assert_eq!(err, "err\n");
        assert_eq!(child.wait().await.unwrap().code(), Some(4));
    }
}
