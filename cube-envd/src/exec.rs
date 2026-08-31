// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Process execution: privilege drop, environment merging and the
//! stdout/stderr pump feeding the Start event stream.

use std::collections::HashMap;
use std::process::Stdio;

use tokio::io::AsyncReadExt;
use tokio::sync::broadcast;

use crate::auth::User;
use crate::msg::process::{DataEvent, EndEvent};
use crate::state::AppState;

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

pub fn spawn(
    cmd: &str,
    args: &[String],
    env: HashMap<String, String>,
    cwd: String,
    user: &User,
) -> std::io::Result<SpawnedProcess> {
    let mut command = tokio::process::Command::new(cmd);
    command
        .args(args)
        .env_clear()
        .envs(&env)
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .kill_on_drop(false);

    let uid = user.uid;
    let gid = user.gid;
    let groups: Vec<libc::gid_t> = user.groups.iter().map(|g| *g as libc::gid_t).collect();
    let cwd_c = std::ffi::CString::new(cwd.as_bytes())
        .map_err(|_| std::io::Error::new(std::io::ErrorKind::InvalidInput, "cwd contains NUL"))?;
    unsafe {
        command.pre_exec(move || {
            // Put the child in its own process group (leader pgid == pid) so a
            // timeout or SendSignal can reach the whole tree — a shell plus the
            // descendants it forks — via kill(-pgid), not just the direct
            // child. Mirrors upstream envd's SysProcAttr{Setpgid: true}.
            if libc::setpgid(0, 0) != 0 {
                return Err(std::io::Error::last_os_error());
            }
            // Drop privileges: supplementary groups first, then gid, then uid.
            // Skip when already running as the target identity (unprivileged
            // unit tests; setgroups needs CAP_SETGID even for a no-op).
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
            // chdir AFTER dropping privileges so the target user's directory
            // permissions are enforced: a dir root can enter but `user` cannot
            // must fail here, not run as if it were accessible.
            if libc::chdir(cwd_c.as_ptr()) != 0 {
                return Err(std::io::Error::last_os_error());
            }
            Ok(())
        });
    }

    let mut child = command.spawn()?;
    // A successfully spawned child always has an id until it is awaited; the
    // fallback to 0 never fires in practice, but kill_process_group guards
    // against 0/1 regardless so a bogus pid can never signal envd's own group.
    let pid = child.id().unwrap_or_default();
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
    })
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
    async fn signal_end_event_shape() {
        let user = current_user();
        let mut proc = spawn(
            "/bin/sh",
            &["-c".into(), "sleep 30".into()],
            HashMap::new(),
            "/".into(),
            &user,
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
