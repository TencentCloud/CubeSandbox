// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Command configuration, privilege setup and pipe-backed process creation.

use std::collections::HashMap;
use std::os::fd::RawFd;
use std::process::Stdio;
use std::sync::{Arc, Mutex};

use tokio::sync::{broadcast, oneshot, Notify};

use crate::platform::config::Config;
use crate::platform::identity::User;

use super::cleanup::kill_process_group;
use super::io::{
    decorate_terminal, pump_pipe, terminal_after_output, terminal_after_wait, OUTPUT_DRAIN_GRACE,
};
use super::{InputWriter, PumpEvent, SpawnedProcess};

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
    process_cgroup: Option<Arc<crate::process::cgroup::ProcessCgroup>>,
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

#[cfg(test)]
mod tests {
    use super::*;
    use crate::process::engine::spawn_pty;
    use crate::process::engine::tests::current_user;
    use crate::process::wire::EndEvent;

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
}
