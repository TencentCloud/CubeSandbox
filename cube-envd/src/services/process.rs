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
use tokio::sync::mpsc;

use crate::auth::User;
use crate::connect;
use crate::error::{ConnectCode, ConnectError};
use crate::exec;
use crate::msg::process::{
    parse_signal, Event, EventEnvelope, ListResponse, ProcessInfo, SendSignalRequest, StartEvent,
    StartRequest,
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

/// Handle `process.Process/Start`.
pub fn start(
    state: Arc<AppState>,
    req: StartRequest,
    user: User,
    deadline: Option<std::time::Duration>,
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

    let spawned = match exec::spawn(&req.process.cmd, &req.process.args, env, cwd, &user) {
        Ok(s) => s,
        Err(e) => {
            // Baseline behavior for a missing binary is a full event stream
            // (start → stderr → end 127), not a spawn-level RPC error.
            return frame_stream_response(missing_cmd_stream(&req.process.cmd, &e));
        }
    };

    let pid = spawned.pid;
    let handle = state.insert_process(ProcEntry {
        pid,
        tag: req.tag.clone(),
        config: req.process.clone(),
    });

    // Frames channel: the HTTP body reads from `rx`; the driver task keeps
    // consuming pump events even if the client goes away so the child is
    // always reaped and the process table stays accurate.
    let (tx, rx) = mpsc::channel::<Bytes>(64);
    let driver_state = state.clone();
    tokio::spawn(async move {
        drive_stream(driver_state, pid, handle, spawned.events, tx, deadline).await;
    });

    frame_stream_response(tokio_stream::wrappers::ReceiverStream::new(rx))
}

async fn drive_stream(
    state: Arc<AppState>,
    pid: u32,
    handle: crate::state::ProcHandle,
    mut events: mpsc::Receiver<exec::PumpEvent>,
    tx: mpsc::Sender<Bytes>,
    deadline: Option<std::time::Duration>,
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

    // Baseline behavior: upstream envd emits a `keepalive` event on a quiet
    // Start stream so intermediaries (CubeProxy, LBs) don't cut an idle
    // connection while a long-running silent command is still alive.
    let mut keepalive = tokio::time::interval(std::time::Duration::from_secs(30));
    keepalive.reset(); // first tick fires after one period, not immediately

    let mut timed_out = false;
    loop {
        tokio::select! {
            ev = events.recv() => match ev {
                Some(exec::PumpEvent::Data(d)) => {
                    // After a timeout the EndStream error frame has already
                    // been sent and must stay the LAST frame on the wire;
                    // keep draining (so the child is reaped) but drop the
                    // buffered output instead of framing it.
                    if !timed_out {
                        keepalive.reset();
                        send(event_frame(Event::Data(d))).await;
                    }
                }
                Some(exec::PumpEvent::End(end)) => {
                    if !timed_out {
                        send(event_frame(Event::End(end))).await;
                        send(connect::end_stream_ok()).await;
                    }
                    break;
                }
                Some(exec::PumpEvent::SpawnError(msg)) => {
                    if !timed_out {
                        send(connect::end_stream_error(&ConnectError::new(
                            ConnectCode::Internal,
                            msg,
                        )))
                        .await;
                    }
                    break;
                }
                None => break,
            },
            _ = keepalive.tick(), if !timed_out => {
                send(event_frame(Event::KeepAlive(serde_json::Map::new()))).await;
            }
            _ = &mut timeout, if !timed_out => {
                timed_out = true;
                // Baseline: deadline expiry kills the process and the stream
                // ends with deadline_exceeded; no End event is emitted.
                //
                // Between the child exiting and this signal there is an
                // unavoidable pgid-reuse window (the kernel could hand the
                // freed pgid to an unrelated process). Upstream Go envd has
                // the same window; closing it would need pidfd-based
                // signalling, out of scope for the MVP.
                let _ = exec::kill_process_group(pid, libc::SIGKILL);
                send(connect::end_stream_error(&ConnectError::new(
                    ConnectCode::DeadlineExceeded,
                    "context deadline exceeded",
                )))
                .await;
                // Keep looping (without the timeout arm) to drain and reap.
            }
        }
    }
    state.remove_process(handle);
}

/// Synthetic event stream matching the baseline shape for a missing binary:
/// start → stderr line → end(127). The baseline stderr text comes from the
/// nice(1) wrapper upstream uses; cube-envd emits its own prefix but keeps
/// the recognizable `'<cmd>': No such file or directory` suffix and code.
///
/// The start event carries `pid: 0`: nothing was spawned, so there is no
/// real pid to report. Upstream reports the nice(1) wrapper's pid there — a
/// documented difference (README "Known behavioral differences"); a pid of a
/// process that failed exec is unusable for Connect/SendSignal either way.
///
/// A pre_exec chdir failure (cwd exists but the target user cannot enter
/// it) also surfaces here as EACCES and is worded against the command;
/// the exit code (126) matches what a shell would report for either cause.
fn missing_cmd_stream(
    cmd: &str,
    err: &std::io::Error,
) -> impl futures::Stream<Item = Bytes> + Send + 'static {
    use base64::Engine;
    let (text, code) = if err.kind() == std::io::ErrorKind::NotFound {
        (
            format!("cube-envd: '{cmd}': No such file or directory\n"),
            127,
        )
    } else if err.kind() == std::io::ErrorKind::PermissionDenied {
        (format!("cube-envd: '{cmd}': Permission denied\n"), 126)
    } else {
        (format!("cube-envd: '{cmd}': {err}\n"), 126)
    };
    let stderr_b64 = base64::engine::general_purpose::STANDARD.encode(text.as_bytes());
    let end = crate::msg::process::EndEvent {
        exit_code: code,
        exited: true,
        status: format!("exit status {code}"),
        error: Some(format!("exit status {code}")),
    };
    futures::stream::iter([
        event_frame(Event::Start(StartEvent { pid: 0 })),
        event_frame(Event::Data(crate::msg::process::DataEvent {
            stderr: Some(stderr_b64),
            ..Default::default()
        })),
        event_frame(Event::End(end)),
        connect::end_stream_ok(),
    ])
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
    use futures::StreamExt;

    #[test]
    fn list_shape() {
        let state = AppState::new();
        assert_eq!(list(&state), serde_json::json!({}));
        state.insert_process(ProcEntry {
            pid: 7,
            tag: Some("t".into()),
            config: crate::msg::process::ProcessConfig {
                cmd: "/bin/bash".into(),
                args: vec!["-c".into(), "x".into()],
                ..Default::default()
            },
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

    #[tokio::test]
    async fn missing_cmd_stream_shape() {
        let err = std::io::Error::new(std::io::ErrorKind::NotFound, "no such file");
        let frames: Vec<Bytes> = missing_cmd_stream("/no/such/bin", &err).collect().await;
        assert_eq!(frames.len(), 4);
        let start: serde_json::Value = serde_json::from_slice(&frames[0][5..]).unwrap();
        assert_eq!(start, serde_json::json!({"event":{"start":{"pid":0}}}));
        let end: serde_json::Value = serde_json::from_slice(&frames[2][5..]).unwrap();
        assert_eq!(end["event"]["end"]["exitCode"], 127);
        assert_eq!(frames[3][0], connect::END_STREAM_FLAG);
    }
}
