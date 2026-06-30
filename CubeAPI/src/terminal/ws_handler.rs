// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! WebSocket terminal handler.
//!
//! Accepts WebSocket upgrade requests at
//! `GET /sandboxes/{sandboxID}/terminal?token=<session_token>&container=<name>`,
//! authenticates the user, validates the sandbox state, then bridges
//! the WebSocket to an envd `process.Process/Connect` stream inside the
//! target container.

use axum::{
    extract::ws::{Message, WebSocket, WebSocketUpgrade},
    extract::{Path, Query, State},
    http::StatusCode,
    response::{IntoResponse, Response},
    Json,
};
use futures::StreamExt;
use serde::Deserialize;
use std::time::Duration;

use crate::{error::AppError, models::ApiError, models::SandboxState, state::AppState};

use super::{
    proxy::{self, EnvdEvent, FrameBuffer},
    session::{SessionTracker, TerminalCloseReason, TerminalSession},
};

/// Default idle timeout for terminal sessions (30 minutes).
pub const DEFAULT_IDLE_TIMEOUT_SECS: u64 = 30 * 60;

/// Query parameters for the WebSocket terminal endpoint.
#[derive(Debug, Deserialize)]
pub struct TerminalQuery {
    /// Session authentication token (from WebUI login).
    #[serde(default)]
    pub token: Option<String>,
    /// Target container name. Defaults to the sandbox's default container.
    #[serde(default)]
    pub container: Option<String>,
}

/// Shared state for the terminal WS handler.
#[derive(Clone)]
pub struct TerminalState {
    pub tracker: SessionTracker,
    pub idle_timeout_secs: u64,
}

impl TerminalState {
    pub fn new(idle_timeout_secs: u64) -> Self {
        Self {
            tracker: SessionTracker::new(),
            idle_timeout_secs,
        }
    }
}

/// The WebSocket upgrade handler.
///
/// Auth is handled here via query parameter (`?token=...`) rather than through
/// the usual middleware, because browsers cannot set custom headers on
/// WebSocket upgrade requests.
pub async fn ws_terminal_handler(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    Query(params): Query<TerminalQuery>,
    ws: WebSocketUpgrade,
) -> Result<Response, AppError> {
    let sandbox_id = sandbox_id.trim().to_string();
    if sandbox_id.is_empty() {
        return Ok((
            StatusCode::BAD_REQUEST,
            Json(ApiError::new(400, "sandboxID is required")),
        )
            .into_response());
    }

    let container_name = params
        .container
        .as_deref()
        .unwrap_or("default")
        .to_string();

    // ── Auth ────────────────────────────────────────────────────────────
    let user = authenticate_terminal(&state, params.token.as_deref()).await?;

    // ── Sandbox validation ──────────────────────────────────────────────
    let detail = state.services.sandboxes.get_sandbox(&sandbox_id).await?;

    if detail.state != SandboxState::Running {
        let state_str = match detail.state {
            SandboxState::Running => "running",
            SandboxState::Paused => "paused",
            SandboxState::Pausing => "pausing",
        };
        return Ok((
            StatusCode::CONFLICT,
            Json(serde_json::json!({
                "error": "sandbox not running",
                "state": state_str
            })),
        )
            .into_response());
    }

    let envd_access_token = detail.envd_access_token.unwrap_or_default();
    if envd_access_token.is_empty() {
        return Ok((
            StatusCode::INTERNAL_SERVER_ERROR,
            Json(ApiError::new(500, "sandbox has no envd access token")),
        )
            .into_response());
    }

    // ── Build envd connection URL through CubeProxy ─────────────────────
    let envd_url = build_envd_url(&state.config.cubemaster_url, &sandbox_id);

    // ── Create terminal session record ──────────────────────────────────
    let session = TerminalSession::new(
        sandbox_id.clone(),
        container_name.clone(),
        user.clone(),
        "ws-client".to_string(),
        state.terminal_state.idle_timeout_secs,
    );
    let session_id = session.session_id;

    tracing::info!(
        session_id = %session_id,
        sandbox_id = %sandbox_id,
        container = %container_name,
        user = %user,
        "terminal session starting"
    );
    tracing::info!(
        event_type = "terminal_session_start",
        session_id = %session_id,
        sandbox_id = %sandbox_id,
        container = %container_name,
        user = %user,
        "terminal audit"
    );

    state.terminal_state.tracker.create(session);

    // ── Upgrade to WebSocket ────────────────────────────────────────────
    let tracker = state.terminal_state.tracker.clone();
    let http_client = state.http_client.clone();
    let idle_timeout_secs = state.terminal_state.idle_timeout_secs;

    Ok(ws.on_upgrade(move |socket| {
        handle_terminal_socket(
            socket,
            session_id,
            sandbox_id,
            container_name,
            user,
            envd_access_token,
            envd_url,
            tracker,
            http_client,
            idle_timeout_secs,
        )
    }))
}

/// Authenticate via callback or passthrough.
async fn authenticate_terminal(
    state: &AppState,
    token: Option<&str>,
) -> Result<String, AppError> {
    let callback_url = state
        .config
        .auth_callback_url
        .as_deref()
        .filter(|u| !u.is_empty());

    match callback_url {
        Some(url) => {
            let token = token.ok_or_else(|| {
                AppError::Unauthorized(
                    "Missing authentication: provide 'token' query parameter".to_string(),
                )
            })?;

            if token.trim().is_empty() {
                return Err(AppError::Unauthorized("Empty authentication token".to_string()));
            }

            let resp = state
                .http_client
                .post(url)
                .header("Authorization", format!("Bearer {}", token))
                .header("X-Request-Path", "/sandboxes/{id}/terminal")
                .header("X-Request-Method", "GET")
                .send()
                .await
                .map_err(|e| {
                    tracing::error!(error = %e, "auth callback request failed");
                    AppError::Internal(anyhow::anyhow!("Auth callback unreachable: {}", e))
                })?;

            if resp.status().as_u16() != 200 {
                return Err(AppError::Unauthorized(
                    "Authentication rejected by callback".to_string(),
                ));
            }
            Ok("authenticated".to_string())
        }
        None => Ok("anonymous".to_string()),
    }
}

/// Build the envd endpoint URL through CubeProxy.
///
/// CubeMaster is typically at `http://<host>:8089`; the CubeProxy HTTP
/// listener is on port 8081 of the same host.  The proxy routes
/// `/sandbox/<id>/<port>/...` to the correct sandbox container.
fn build_envd_url(cubemaster_url: &str, sandbox_id: &str) -> String {
    let base = cubemaster_url
        .trim_end_matches('/')
        .replace(":8089", ":8081");

    format!(
        "{}/sandbox/{}/49983/process.Process/Connect",
        base, sandbox_id
    )
}

/// Main bidirectional proxy loop.
///
/// Architecture:
/// - A Tokio mpsc channel feeds WS stdin frames into a streaming reqwest body.
/// - The envd response is read as a byte stream and decoded into events,
///   which are forwarded to the WS as binary frames.
/// - An idle timer ticks every 30s; if no activity for the configured timeout,
///   both sides are closed.
async fn handle_terminal_socket(
    mut ws_socket: WebSocket,
    session_id: crate::terminal::session::SessionId,
    sandbox_id: String,
    container_name: String,
    user: String,
    envd_access_token: String,
    envd_url: String,
    tracker: SessionTracker,
    http_client: reqwest::Client,
    _idle_timeout_secs: u64,
) {
    // Channel for stdin: WS frames → envd request body stream.
    let (stdin_tx, stdin_rx) = tokio::sync::mpsc::channel::<Vec<u8>>(256);
    let (_close_tx, _close_rx) = tokio::sync::watch::channel(false);

    // Build the streaming request body from the stdin channel.
    let body_stream = tokio_stream::wrappers::ReceiverStream::new(stdin_rx)
        .map(|chunk| Ok::<_, std::io::Error>(proxy::encode_stdin_frame(&chunk)))
        .chain(futures::stream::once(async {
            Ok::<_, std::io::Error>(proxy::encode_end_stream_frame())
        }));

    let envd_resp = match http_client
        .post(&envd_url)
        .header("Content-Type", "application/connect+json")
        .header("Connect-Protocol-Version", "1")
        .header("X-Access-Token", &envd_access_token)
        .header("Authorization", format!("Basic {}", base64_encode("root:")))
        .body(reqwest::Body::wrap_stream(body_stream))
        .send()
        .await
    {
        Ok(r) if r.status().is_success() => r,
        Ok(r) => {
            tracing::error!(session_id = %session_id, status = %r.status(), "envd connect rejected");
            let _ = ws_socket
                .send(Message::Close(Some(axum::extract::ws::CloseFrame {
                    code: 1011,
                    reason: "envd connection failed".into(),
                })))
                .await;
            cleanup_session(
                &tracker, session_id, TerminalCloseReason::Error,
                &sandbox_id, &container_name, &user,
            );
            return;
        }
        Err(e) => {
            tracing::error!(session_id = %session_id, error = %e, "envd connect error");
            let _ = ws_socket
                .send(Message::Close(Some(axum::extract::ws::CloseFrame {
                    code: 1011,
                    reason: "envd unreachable".into(),
                })))
                .await;
            cleanup_session(
                &tracker, session_id, TerminalCloseReason::Error,
                &sandbox_id, &container_name, &user,
            );
            return;
        }
    };

    tracing::info!(session_id = %session_id, "bidirectional proxy started");

    let mut envd_body = envd_resp.bytes_stream();
    let mut envd_buf = FrameBuffer::new();
    let mut idle_timer = tokio::time::interval(Duration::from_secs(30));

    loop {
        tokio::select! {
            // ── WS → envd (stdin) ──────────────────────────────────────
            ws_msg = ws_socket.recv() => {
                let touched = match ws_msg {
                    Some(Ok(Message::Binary(data))) => {
                        let _ = stdin_tx.send(data.to_vec()).await;
                        true
                    }
                    Some(Ok(Message::Text(text))) => {
                        handle_ws_text(&text, &stdin_tx).await;
                        true
                    }
                    Some(Ok(Message::Ping(data))) => {
                        let _ = ws_socket.send(Message::Pong(data)).await;
                        false
                    }
                    Some(Ok(Message::Pong(_))) => false,
                    Some(Ok(Message::Close(_))) | None => {
                        tracing::info!(session_id = %session_id, "WS closed by client");
                        let _ = _close_tx.send(true);
                        break;
                    }
                    Some(Err(e)) => {
                        tracing::warn!(session_id = %session_id, error = %e, "WS recv error");
                        let _ = _close_tx.send(true);
                        break;
                    }
                };
                if touched {
                    tracker.touch(&session_id);
                }
            }

            // ── envd → WS (stdout/stderr) ─────────────────────────────
            chunk = envd_body.next() => {
                match chunk {
                    Some(Ok(bytes)) => {
                        envd_buf.extend(&bytes);
                        if let Err(e) = process_envd_frames(
                            &mut envd_buf, &mut ws_socket, session_id,
                            &tracker, &sandbox_id, &container_name, &user,
                        ).await {
                            tracing::error!(session_id = %session_id, error = %e);
                            return;
                        }
                    }
                    Some(Err(e)) => {
                        tracing::error!(session_id = %session_id, error = %e, "envd stream error");
                        let _ = ws_socket.send(Message::Close(Some(
                            axum::extract::ws::CloseFrame {
                                code: 1011, reason: "envd stream error".into(),
                            }
                        ))).await;
                        cleanup_session(
                            &tracker, session_id, TerminalCloseReason::Error,
                            &sandbox_id, &container_name, &user,
                        );
                        return;
                    }
                    None => {
                        tracing::info!(session_id = %session_id, "envd stream ended");
                        let _ = ws_socket.send(Message::Close(Some(
                            axum::extract::ws::CloseFrame {
                                code: 1000, reason: "connection closed".into(),
                            }
                        ))).await;
                        cleanup_session(
                            &tracker, session_id, TerminalCloseReason::ClientDisconnect,
                            &sandbox_id, &container_name, &user,
                        );
                        return;
                    }
                }
            }

            // ── Idle timeout ──────────────────────────────────────────
            _ = idle_timer.tick() => {
                if let Some(entry) = tracker.get(&session_id) {
                    if entry.is_idle() {
                        tracing::info!(session_id = %session_id, "idle timeout");
                        let _ = ws_socket.send(Message::Close(Some(
                            axum::extract::ws::CloseFrame {
                                code: 1001, reason: "idle timeout".into(),
                            }
                        ))).await;
                        let _ = _close_tx.send(true);
                        cleanup_session(
                            &tracker, session_id, TerminalCloseReason::IdleTimeout,
                            &sandbox_id, &container_name, &user,
                        );
                        return;
                    }
                } else {
                    return; // session already removed
                }
            }
        }
    }

    cleanup_session(
        &tracker, session_id, TerminalCloseReason::ClientDisconnect,
        &sandbox_id, &container_name, &user,
    );
}

/// Handle incoming WS text frames (resize, other control messages).
async fn handle_ws_text(text: &str, stdin_tx: &tokio::sync::mpsc::Sender<Vec<u8>>) {
    if let Ok(ctrl) = serde_json::from_str::<serde_json::Value>(text) {
        if ctrl.get("type").and_then(|v| v.as_str()) == Some("resize") {
            if let (Some(cols), Some(rows)) = (
                ctrl.get("cols").and_then(|v| v.as_u64()),
                ctrl.get("rows").and_then(|v| v.as_u64()),
            ) {
                if cols > 0 && rows > 0 {
                    let resize_msg = serde_json::json!({
                        "event": {
                            "data": {
                                "pty": base64_encode(
                                    &format!("{{\"cols\":{},\"rows\":{}}}", cols, rows)
                                )
                            }
                        }
                    });
                    if let Ok(payload) = serde_json::to_vec(&resize_msg) {
                        let frame = proxy::encode_frame(0x00, &payload);
                        let _ = stdin_tx.send(frame.to_vec()).await;
                    }
                }
            }
        }
    }
}

/// Process all complete frames in the envd buffer, forwarding events to WS.
/// Returns `Ok(())` to continue, or `Err(msg)` if the connection should end.
async fn process_envd_frames(
    buf: &mut FrameBuffer,
    ws: &mut WebSocket,
    session_id: crate::terminal::session::SessionId,
    tracker: &SessionTracker,
    sandbox_id: &str,
    container_name: &str,
    user: &str,
) -> Result<(), String> {
    loop {
        match buf.try_take_frame() {
            Ok(Some((flags, payload))) => {
                if flags == proxy::FLAG_END_STREAM {
                    tracing::info!(session_id = %session_id, "envd end-of-stream");
                    let _ = ws
                        .send(Message::Close(Some(axum::extract::ws::CloseFrame {
                            code: 1000,
                            reason: "process exited".into(),
                        })))
                        .await;
                    cleanup_session(
                        tracker, session_id, TerminalCloseReason::ClientDisconnect,
                        sandbox_id, container_name, user,
                    );
                    return Err("envd end-of-stream".to_string());
                }
                if flags == proxy::FLAG_COMPRESSED {
                    tracing::warn!(session_id = %session_id, "unsupported compressed frame");
                    continue;
                }
                // Parse JSON events
                match proxy::parse_event(&payload) {
                    Ok(events) => {
                        for event in events {
                            let send_result = match event {
                                EnvdEvent::Stdout(data) => {
                                    ws.send(Message::Binary(data.into())).await
                                }
                                EnvdEvent::Stderr(data) => {
                                    // Prefix with 0x02 to distinguish stderr
                                    let mut frame = vec![0x02u8];
                                    frame.extend_from_slice(&data);
                                    ws.send(Message::Binary(frame.into())).await
                                }
                                EnvdEvent::Pty(data) => {
                                    ws.send(Message::Binary(data.into())).await
                                }
                                EnvdEvent::Start { pid } => {
                                    tracing::debug!(session_id = %session_id, pid = pid, "process started");
                                    Ok(())
                                }
                                EnvdEvent::End { exit_code } => {
                                    tracing::info!(session_id = %session_id, exit_code = exit_code, "process ended");
                                    let _ = ws
                                        .send(Message::Close(Some(
                                            axum::extract::ws::CloseFrame {
                                                code: 1000,
                                                reason: format!("process exited with code {}", exit_code).into(),
                                            },
                                        )))
                                        .await;
                                    cleanup_session(
                                        tracker, session_id, TerminalCloseReason::ClientDisconnect,
                                        sandbox_id, container_name, user,
                                    );
                                    return Err("envd process ended".to_string());
                                }
                                EnvdEvent::Keepalive => Ok(()),
                            };
                            if send_result.is_err() {
                                return Err("WS send error".to_string());
                            }
                        }
                    }
                    Err(e) => {
                        tracing::warn!(session_id = %session_id, error = %e, "envd event parse error");
                    }
                }
            }
            Ok(None) => return Ok(()),
            Err(e) => {
                tracing::warn!(session_id = %session_id, error = %e, "envd frame decode error");
                return Ok(());
            }
        }
    }
}

/// Emit audit log and remove session from tracker.
fn cleanup_session(
    tracker: &SessionTracker,
    session_id: crate::terminal::session::SessionId,
    reason: TerminalCloseReason,
    sandbox_id: &str,
    container_name: &str,
    user: &str,
) {
    if let Some(session) = tracker.remove(&session_id) {
        tracing::info!(
            event_type = "terminal_session_end",
            session_id = %session_id,
            sandbox_id = %sandbox_id,
            container = %container_name,
            user = %user,
            close_reason = reason.as_str(),
            duration_ms = session.duration_ms(),
            "terminal audit"
        );
    } else {
        tracing::info!(
            event_type = "terminal_session_end",
            session_id = %session_id,
            sandbox_id = %sandbox_id,
            container = %container_name,
            user = %user,
            close_reason = reason.as_str(),
            duration_ms = 0u64,
            "terminal audit"
        );
    }
}

fn base64_encode(s: &str) -> String {
    use base64::{engine::general_purpose::STANDARD as BASE64, Engine};
    BASE64.encode(s.as_bytes())
}

/// Close all terminal sessions for a given sandbox (called on pause/destroy).
pub async fn close_sessions_for_sandbox(
    tracker: &SessionTracker,
    sandbox_id: &str,
    reason: TerminalCloseReason,
) {
    let sessions = tracker.remove_by_sandbox(sandbox_id);
    let count = sessions.len();
    for session in &sessions {
        tracing::info!(
            event_type = "terminal_session_end",
            session_id = %session.session_id,
            sandbox_id = %sandbox_id,
            container = %session.container_name,
            user = %session.user,
            close_reason = reason.as_str(),
            duration_ms = session.duration_ms(),
            "terminal audit"
        );
    }
    if count > 0 {
        tracing::info!(
            sandbox_id = %sandbox_id,
            count = sessions.len(),
            reason = reason.as_str(),
            "closed terminal sessions for sandbox"
        );
    }
}