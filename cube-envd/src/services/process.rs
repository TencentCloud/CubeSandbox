// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! `process.Process` RPC implementations.
//!
//! Baseline-verified streaming semantics:
//! - the whole Start response is HTTP 200; all errors (bad user, deadline,
//!   unimplemented capability) travel as EndStream error frames;
//! - `Connect-Timeout-Ms` expiry KILLS the child and ends the stream with
//!   `deadline_exceeded`;
//! - a client disconnect does NOT kill the child — it keeps running and
//!   stays visible to `List` until it exits on its own.

use std::sync::Arc;

use bytes::Bytes;
use tokio::sync::broadcast::error::RecvError;
use tokio::sync::{broadcast, mpsc};

use crate::auth::User;
use crate::cgroup::ProcType;
use crate::connect;
use crate::error::{ConnectCode, ConnectError};
use crate::exec;
use crate::msg::process::{
    parse_signal, ConnectRequest, Event, EventEnvelope, ListResponse, ProcessInfo,
    SendSignalRequest, StartEvent, StartRequest,
};
use crate::state::{AppState, ProcEntry};

pub fn frame_stream_response(
    frames: impl futures::Stream<Item = Bytes> + Send + 'static,
) -> axum::response::Response {
    use futures::StreamExt;
    let body = axum::body::Body::from_stream(frames.map(Ok::<_, std::convert::Infallible>));
    axum::response::Response::builder()
        .status(axum::http::StatusCode::OK)
        .header(
            axum::http::header::CONTENT_TYPE,
            connect::STREAM_CONTENT_TYPE,
        )
        .body(body)
        .expect("build stream response")
}

/// A streaming response that carries a single EndStream error frame.
pub fn stream_error_response(err: ConnectError) -> axum::response::Response {
    frame_stream_response(futures::stream::iter([connect::end_stream_error(&err)]))
}

fn event_frame(event: Event) -> Bytes {
    let value =
        serde_json::to_value(EventEnvelope { event }).unwrap_or_else(|_| serde_json::json!({}));
    connect::message_frame(&value)
}

/// Target OOM score and nice level for user commands (upstream
/// handler.go:29-30). The wrapper applies them in the child before the
/// command exists, so nothing can inherit envd's protected values.
const DEFAULT_OOM_SCORE: i32 = 100;
const DEFAULT_NICE: i32 = 0;

/// Nice value of the current process, as the wrapper delta needs it.
/// Reads the raw syscall the way upstream does (handler.go:82-88): the
/// kernel encodes the result as `20 - nice`, so `20 - prio` is the nice
/// value. Reading the raw encoding rather than the C library's
/// `getpriority` is deliberate — libc converts to the nice value itself,
/// where `-1` is both a legitimate nice level and the error return, so a
/// daemon started at nice -1 could not be told apart from a failure. The
/// raw encoding maps every valid nice to `1..=40`, leaving -1 unambiguously
/// an error. (Do not apply `20 - prio` to a libc `getpriority` result: that
/// combination yields delta -20 at the daemon's nice 0 and the wrapper then
/// tries to raise priority as the unprivileged user, failing with EPERM.)
fn current_nice() -> i32 {
    // SAFETY: getpriority(PRIO_PROCESS, 0) addresses this process.
    let prio = unsafe { libc::syscall(libc::SYS_getpriority, libc::PRIO_PROCESS, 0) };
    if prio < 0 {
        return 0;
    }
    20 - prio as i32
}

/// Assemble the OOM/nice wrapper upstream prepends to every user command
/// (handler.go:98-105): `echo <oom> > /proc/$$/oom_score_adj && exec
/// /usr/bin/nice -n <delta> "$@"`, run as `/bin/sh -c <script> -- <cmd>
/// <args...>`. The command replaces sh via exec (same pid) after the score
/// and nice are set in the child. nice(1) is a relative adjustment, hence
/// the delta from the current (inherited) nice to the target.
fn oom_nice_wrapper(cmd: &str, args: &[String]) -> (String, Vec<String>) {
    let delta = DEFAULT_NICE - current_nice();
    let script = format!(
        "echo {DEFAULT_OOM_SCORE} > /proc/$$/oom_score_adj && exec /usr/bin/nice -n {delta} \"${{@}}\""
    );
    let mut sh_args = vec!["-c".to_string(), script, "--".to_string(), cmd.to_string()];
    sh_args.extend(args.iter().cloned());
    ("/bin/sh".to_string(), sh_args)
}

/// The user's original command as one shell-style string, wrapper internals
/// stripped — mirrors upstream `Handler.userCommand` (handler.go:71-74,
/// `strings.Join(append([cmd], args...), " ")`). No shell escaping: upstream
/// does none either; this text only feeds error messages.
fn user_command(cmd: &str, args: &[String]) -> String {
    std::iter::once(cmd)
        .chain(args.iter().map(String::as_str))
        .collect::<Vec<_>>()
        .join(" ")
}

/// Which cgroup subtree a Start request belongs to (upstream
/// handler.go:340 getProcType). PTY starts are rejected before this runs
/// today, so this is always `User` in practice, but the interface must not
/// assume it — when PTY lands it switches to `Pty` on its own.
pub(crate) fn get_proc_type(req: &StartRequest) -> ProcType {
    if req.pty.is_some() {
        ProcType::Pty
    } else {
        ProcType::User
    }
}

/// Handle `process.Process/Start`.
pub fn start(
    state: Arc<AppState>,
    req: StartRequest,
    user: User,
    deadline: Option<std::time::Duration>,
    keepalive_interval: std::time::Duration,
) -> axum::response::Response {
    if req.pty.is_some() {
        return stream_error_response(ConnectError::unimplemented("PTY (Start.pty)"));
    }
    if req.stdin == Some(true) {
        return stream_error_response(ConnectError::unimplemented(
            "interactive stdin (Start.stdin=true)",
        ));
    }
    if req.process.cmd.is_empty() {
        return stream_error_response(ConnectError::new(
            ConnectCode::InvalidArgument,
            "process config has no cmd",
        ));
    }

    let env = exec::merged_env(&state, &user, &req.process.envs);
    let cwd = match exec::resolve_cwd(req.process.cwd.as_deref(), &user) {
        Ok(c) => c,
        Err(msg) => {
            // Invalid working directory: reject like upstream instead of
            // silently running in `/` (#1227: no silent success).
            return stream_error_response(ConnectError::new(ConnectCode::InvalidArgument, msg));
        }
    };

    // Upstream wraps every start in an OOM/nice shell (handler.go:98-105):
    // the wrapper also makes a "missing binary" a wrapper-level failure with
    // /usr/bin/nice's own wording (exit 127 event flow), not a spawn error.
    let (wrapper_cmd, wrapper_args) = oom_nice_wrapper(&req.process.cmd, &req.process.args);
    let cgroup_fd = state.cgroup_fd(get_proc_type(&req));
    let spawned = match exec::spawn(&wrapper_cmd, &wrapper_args, env, cwd, &user, cgroup_fd) {
        Ok(s) => s,
        Err(e) => {
            // A genuine spawn failure is an InvalidArgument RPC error
            // (upstream start.go:207-209), not an event stream. Only real
            // system failures reach here now — the wrapper (/bin/sh) always
            // exists, so a missing command never does.
            return stream_error_response(ConnectError::new(
                ConnectCode::InvalidArgument,
                format!(
                    "error starting process '{}': {e}",
                    user_command(&req.process.cmd, &req.process.args)
                ),
            ));
        }
    };

    let exec::SpawnedProcess {
        pid,
        initial,
        sender,
    } = spawned;
    let handle = state.insert_process(ProcEntry {
        pid,
        tag: req.tag.clone(),
        config: req.process.clone(),
        sender,
    });

    // Frames channel: the HTTP body reads from `rx`; the driver task keeps
    // consuming pump events even if the client goes away so the child is
    // always reaped and the process table stays accurate.
    let (tx, rx) = mpsc::channel::<Bytes>(64);
    let driver_state = state.clone();
    tokio::spawn(async move {
        drive_stream(
            driver_state,
            pid,
            Some(handle),
            initial,
            tx,
            deadline,
            keepalive_interval,
        )
        .await;
    });

    frame_stream_response(tokio_stream::wrappers::ReceiverStream::new(rx))
}

/// Handle `process.Process/Connect` (attach, server-streaming).
///
/// Attach differs from `Start` only in lifecycle: it does not spawn, so there
/// is no deadline kill, and it does not reap the process-table entry — the
/// `Start` stream owns both. A `Connect` arriving after the process was
/// reaped resolves to `not_found`; one arriving in the tiny window between the
/// child ending and the `Start` stream reaping it is cut off with `Closed`
/// (no history to replay), a race upstream has as well.
pub fn connect(
    state: Arc<AppState>,
    req: ConnectRequest,
    keepalive_interval: std::time::Duration,
) -> axum::response::Response {
    let (pid, tag) = req.process.flatten();
    let Some((pid, events)) = state.subscribe(pid, tag.as_deref()) else {
        // Match Go envd's wording for a selector resolving to no live process
        // (the same helper SendSignal/List use).
        let detail = match (pid, tag.as_deref()) {
            (Some(p), _) => format!("process with pid {p} not found"),
            (None, Some(t)) => format!("process with tag {t} not found"),
            (None, None) => "process not found".to_string(),
        };
        return stream_error_response(ConnectError::new(ConnectCode::NotFound, detail));
    };

    // Attach, don't spawn: no deadline kill and no reap on completion. The
    // fresh receiver starts at the current ring head, so history is not
    // replayed.
    let (tx, rx) = mpsc::channel::<Bytes>(64);
    let driver_state = state.clone();
    tokio::spawn(async move {
        drive_stream(
            driver_state,
            pid,
            None,
            events,
            tx,
            None,
            keepalive_interval,
        )
        .await;
    });

    frame_stream_response(tokio_stream::wrappers::ReceiverStream::new(rx))
}

async fn drive_stream(
    state: Arc<AppState>,
    pid: u32,
    handle: Option<crate::state::ProcHandle>,
    mut events: broadcast::Receiver<exec::PumpEvent>,
    tx: mpsc::Sender<Bytes>,
    deadline: Option<std::time::Duration>,
    keepalive_interval: std::time::Duration,
) {
    // send() ignores errors: a dropped receiver means the client is gone,
    // but the child must still be drained and reaped.
    let send = |b: Bytes| {
        let tx = tx.clone();
        async move {
            let _ = tx.send(b).await;
        }
    };

    send(event_frame(Event::Start(StartEvent { pid }))).await;

    let sleep_forever = std::time::Duration::from_secs(60 * 60 * 24 * 365);
    let timeout = tokio::time::sleep(deadline.unwrap_or(sleep_forever));
    tokio::pin!(timeout);

    // Keepalive: upstream envd emits a `keepalive` event on a quiet Start
    // stream so intermediaries (CubeProxy, LBs) don't cut an idle connection
    // while a long-running silent command is still alive. The cadence is the
    // `Keepalive-Ping-Interval` header in whole seconds, defaulting to 30s.
    let mut keepalive = tokio::time::interval(keepalive_interval);
    keepalive.reset(); // first tick fires after one period, not immediately

    // `stream_closed` flips once a terminal EndStream error frame (deadline
    // expiry or a too-slow disconnect) has been sent: from then on the wire
    // must end with that frame, so remaining output is drained (to reap the
    // child) but never framed again. `timed_out` only tracks whether the
    // deadline kill has already fired, so the kill happens exactly once even
    // if the stream was already closed for a different reason.
    let mut timed_out = false;
    let mut stream_closed = false;
    loop {
        tokio::select! {
            ev = events.recv() => match ev {
                Ok(exec::PumpEvent::Data(d)) => {
                    if !stream_closed {
                        keepalive.reset();
                        send(event_frame(Event::Data(d))).await;
                    }
                }
                Ok(exec::PumpEvent::End(end)) => {
                    if !stream_closed {
                        send(event_frame(Event::End(end))).await;
                        send(connect::end_stream_ok()).await;
                    }
                    break;
                }
                Ok(exec::PumpEvent::SpawnError(msg)) => {
                    if !stream_closed {
                        send(connect::end_stream_error(&ConnectError::new(
                            ConnectCode::Internal,
                            msg,
                        )))
                        .await;
                    }
                    break;
                }
                // A subscriber that falls too far behind the ring gets its own
                // `Lagged` and only its own stream ends here — avoiding
                // upstream #3292, where one stale subscriber wedges the whole
                // fan-out. The child and every other subscriber are untouched;
                // we keep draining so the child is still reaped.
                Err(RecvError::Lagged(n)) => {
                    if !stream_closed {
                        send(connect::end_stream_error(&ConnectError::new(
                            ConnectCode::ResourceExhausted,
                            format!("output consumer too slow: {n} events dropped"),
                        )))
                        .await;
                    }
                    stream_closed = true;
                }
                // Every sender is gone without an End event (the pump task
                // died); nothing more can arrive.
                Err(RecvError::Closed) => break,
            },
            _ = keepalive.tick(), if !stream_closed => {
                send(event_frame(Event::KeepAlive(serde_json::Map::new()))).await;
            }
            _ = &mut timeout, if !timed_out => {
                timed_out = true;
                // Baseline: deadline expiry kills the process and the stream
                // ends with deadline_exceeded; no End event is emitted. The
                // deadline is a property of the command, so it fires even if
                // the stream was already closed (e.g. a too-slow client).
                //
                // Between the child exiting and this signal there is an
                // unavoidable pgid-reuse window (the kernel could hand the
                // freed pgid to an unrelated process). Upstream Go envd has
                // the same window; closing it would need pidfd-based
                // signalling, out of scope for the MVP.
                let _ = exec::kill_process_group(pid, libc::SIGKILL);
                if !stream_closed {
                    send(connect::end_stream_error(&ConnectError::new(
                        ConnectCode::DeadlineExceeded,
                        "context deadline exceeded",
                    )))
                    .await;
                }
                stream_closed = true;
                // Keep looping (without the timeout arm) to drain and reap.
            }
        }
    }
    // Only the Start stream owns the process-table entry. A Connect attach
    // passes `handle: None` — its completion (a disconnect) must not reap the
    // process, which stays owned by the Start stream until the child exits.
    if let Some(handle) = handle {
        state.remove_process(handle);
    }
}

/// Handle `process.Process/List` (unary).
pub fn list(state: &AppState) -> serde_json::Value {
    let processes = state
        .list_processes()
        .into_iter()
        .map(|(pid, tag, config)| ProcessInfo {
            config,
            pid: Some(pid),
            tag,
        })
        .collect();
    serde_json::to_value(ListResponse { processes }).unwrap_or_else(|_| serde_json::json!({}))
}

/// Handle `process.Process/SendSignal` (unary).
pub fn send_signal(
    state: &AppState,
    req: &SendSignalRequest,
) -> Result<serde_json::Value, ConnectError> {
    let (pid, tag) = req.process.flatten();
    let target = state.find_pid(pid, tag.as_deref()).ok_or_else(|| {
        // Match Go envd's specific wording so a client logging the message
        // sees the same text: "process with pid N not found" / "... tag X ...".
        let detail = match (pid, tag.as_deref()) {
            (Some(p), _) => format!("process with pid {p} not found"),
            (None, Some(t)) => format!("process with tag {t} not found"),
            (None, None) => "process not found".to_string(),
        };
        ConnectError::new(ConnectCode::NotFound, detail)
    })?;
    let signo = parse_signal(req.signal.as_ref()).ok_or_else(|| {
        ConnectError::new(
            ConnectCode::InvalidArgument,
            format!("unsupported signal: {:?}", req.signal),
        )
    })?;
    // The table can still hold a pid whose process exited but was not yet
    // reaped; kill(-pid) then fails with ESRCH. Report that as not_found
    // (the process is gone from the caller's perspective, matching Go),
    // not a misleading Internal.
    exec::kill_process_group(target, signo).map_err(|e| {
        if e.raw_os_error() == Some(libc::ESRCH) {
            let detail = match (pid, tag.as_deref()) {
                (Some(p), _) => format!("process with pid {p} not found"),
                (None, Some(t)) => format!("process with tag {t} not found"),
                (None, None) => "process not found".to_string(),
            };
            ConnectError::new(ConnectCode::NotFound, detail)
        } else {
            ConnectError::new(ConnectCode::Internal, format!("kill failed: {e}"))
        }
    })?;
    Ok(serde_json::json!({}))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn list_shape() {
        let state = AppState::new();
        assert_eq!(list(&state), serde_json::json!({}));
        let (sender, _rx) = broadcast::channel::<exec::PumpEvent>(1);
        state.insert_process(ProcEntry {
            pid: 7,
            tag: Some("t".into()),
            config: crate::msg::process::ProcessConfig {
                cmd: "/bin/bash".into(),
                args: vec!["-c".into(), "x".into()],
                ..Default::default()
            },
            sender,
        });
        let v = list(&state);
        assert_eq!(v["processes"][0]["pid"], 7);
        assert_eq!(v["processes"][0]["tag"], "t");
        assert_eq!(v["processes"][0]["config"]["cmd"], "/bin/bash");
    }

    #[test]
    fn send_signal_unknown_process() {
        let state = AppState::new();
        let req: SendSignalRequest =
            serde_json::from_str(r#"{"process":{"tag":"nope"},"signal":"SIGNAL_SIGKILL"}"#)
                .unwrap();
        let err = send_signal(&state, &req).unwrap_err();
        assert_eq!(err.code, ConnectCode::NotFound);
    }

    #[test]
    fn oom_nice_wrapper_shape() {
        // Script structure must match upstream handler.go:98-105: OOM score
        // written first, then exec /usr/bin/nice with a relative delta.
        let (cmd, args) = oom_nice_wrapper("/no/such/bin", &[]);
        assert_eq!(cmd, "/bin/sh");
        assert_eq!(args.len(), 4);
        assert_eq!(args[0], "-c");
        assert!(args[1].starts_with("echo 100 > /proc/$$/oom_score_adj && exec /usr/bin/nice -n "));
        assert!(args[1].ends_with(" \"${@}\""));
        assert_eq!(args[2], "--");
        assert_eq!(args[3], "/no/such/bin");

        let (_, args) = oom_nice_wrapper("/bin/echo", &["hi".to_string()]);
        assert_eq!(args[3], "/bin/echo");
        assert_eq!(args[4], "hi");
    }

    #[test]
    fn current_nice_returns_the_nice_value() {
        // Cross-check against libc's getpriority, which converts the kernel's
        // raw `20 - nice` encoding back to the nice value itself — an
        // independent source for the same number. This locks the convention:
        // returning the raw encoding (or applying `20 - prio` to the libc
        // result) both yield 20 at the daemon's nice 0, i.e. delta -20 and a
        // wrapper that tries to raise priority as the unprivileged user.
        // SAFETY: PRIO_PROCESS with pid 0 addresses this process.
        let libc_nice = unsafe { libc::getpriority(libc::PRIO_PROCESS, 0) };
        if libc_nice == -1 {
            // Ambiguous (real nice -1 vs error) and unreachable here: the
            // test process inherits nice 0.
            return;
        }
        assert_eq!(current_nice(), libc_nice);
    }

    #[test]
    fn user_command_joins_cmd_and_args_like_upstream() {
        // Mirrors handler.go:71-74: plain space join of cmd + args, no shell
        // escaping — the text is only for error messages.
        assert_eq!(user_command("sleep", &[]), "sleep");
        assert_eq!(user_command("sleep", &["30".to_string()]), "sleep 30");
        assert_eq!(
            user_command("/bin/echo", &["a b".to_string(), "c".to_string()]),
            "/bin/echo a b c"
        );
    }

    #[test]
    fn get_proc_type_maps_pty_and_default() {
        let base = StartRequest {
            process: crate::msg::process::ProcessConfig {
                cmd: "x".into(),
                ..Default::default()
            },
            pty: None,
            tag: None,
            stdin: None,
        };
        assert_eq!(get_proc_type(&base), ProcType::User);
        let pty_req = StartRequest {
            pty: Some(crate::msg::process::Pty { size: None }),
            ..base
        };
        assert_eq!(get_proc_type(&pty_req), ProcType::Pty);
    }

    #[tokio::test]
    async fn drive_stream_lagged_cuts_off_slow_subscriber() {
        let state = Arc::new(AppState::new());
        // Capacity-1 ring: publishing two events before the driver reads any
        // overflows the ring, so its first recv() reports Lagged instead of
        // delivering the overwritten event.
        let (pub_tx, events) = broadcast::channel::<exec::PumpEvent>(1);
        let handle = state.insert_process(ProcEntry {
            pid: 42,
            tag: None,
            config: crate::msg::process::ProcessConfig {
                cmd: "/bin/echo".into(),
                ..Default::default()
            },
            sender: pub_tx.clone(),
        });
        let data = |s: &str| {
            exec::PumpEvent::Data(crate::msg::process::DataEvent {
                stdout: Some(s.into()),
                ..Default::default()
            })
        };
        assert!(pub_tx.send(data("a")).is_ok());
        assert!(pub_tx.send(data("b")).is_ok());

        let (tx, mut rx) = mpsc::channel::<Bytes>(16);
        let driver_state = state.clone();
        let driver = tokio::spawn(async move {
            drive_stream(
                driver_state,
                42,
                Some(handle),
                events,
                tx,
                None,
                std::time::Duration::from_secs(30),
            )
            .await;
        });

        // The cutoff has already closed the stream; publish End so the driver
        // drains, breaks, and reaps the process entry.
        assert!(pub_tx
            .send(exec::PumpEvent::End(crate::msg::process::EndEvent {
                exit_code: 0,
                exited: true,
                status: "exit status 0".into(),
                error: None,
            }))
            .is_ok());

        let mut frames = Vec::new();
        while let Some(f) = rx.recv().await {
            frames.push(f);
        }
        driver.await.unwrap();

        // Start, then exactly one terminal EndStream error frame — no Data,
        // no End event, no end_stream_ok. The lagging subscriber is cut off
        // and the child (here, already ended) is still reaped.
        assert_eq!(frames.len(), 2);
        let start: serde_json::Value = serde_json::from_slice(&frames[0][5..]).unwrap();
        assert_eq!(start["event"]["start"]["pid"], 42);
        assert_eq!(frames[1][0], connect::END_STREAM_FLAG);
        let err: serde_json::Value = serde_json::from_slice(&frames[1][5..]).unwrap();
        assert_eq!(err["error"]["code"], "resource_exhausted");
    }
}
