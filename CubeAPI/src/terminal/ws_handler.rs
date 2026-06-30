// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! WebSocket terminal handler.
//!
//! Accepts WebSocket upgrade requests at
//! `GET /sandboxes/{sandboxID}/terminal?token=<session_token>&container=<name>`,
//! authenticates the user, validates the sandbox state, then bridges
//! the WebSocket to an envd `process.Process/Connect` stream inside the
//! target container.
//!
//! ## Security note: token in query parameter
//!
//! The auth token is carried in the WebSocket URL query parameter because
//! browsers cannot set custom headers (`Authorization`, etc.) on the
//! WebSocket upgrade handshake. This means the token will appear in:
//!
//! - Server access logs (this service, reverse proxies, load balancers)
//! - The `Referer` header of any resource requests initiated during the
//!   terminal session
//!
//! Mitigations:
//! - Pages serving the terminal SHOULD set `Referrer-Policy: no-referrer`
//! - Consider issuing short-lived sub-tokens scoped to this endpoint
//! - The `SameSite=Strict` cookie approach is not viable here due to
//!   the `token` parameter being in the query string, not a cookie

use axum::{
    extract::ws::{Message, WebSocket, WebSocketUpgrade},
    extract::{ConnectInfo, Path, Query, State},
    http::{header, HeaderMap, StatusCode},
    response::{IntoResponse, Response},
    Json,
};
use futures::StreamExt;
use regex::Regex;
use serde::Deserialize;
use std::net::SocketAddr;
use std::sync::LazyLock;
use std::time::Duration;

use crate::{error::AppError, models::ApiError, models::SandboxState, state::AppState};

use super::{
    proxy::{self, EnvdEvent, FrameBuffer},
    session::{SessionTracker, TerminalCloseReason, TerminalSession},
};

/// Allowed characters in sandbox IDs (prevent path traversal).
static SANDBOX_ID_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"^[a-zA-Z0-9_-]+$").expect("sandbox ID regex must compile")
});

/// Allowed characters in container names (prevent log injection and DoS).
static CONTAINER_NAME_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$").expect("container name regex must compile")
});

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

/// Default max terminal sessions per sandbox.
pub const DEFAULT_MAX_SESSIONS_PER_SANDBOX: usize = 5;
/// Default max terminal sessions per user.
pub const DEFAULT_MAX_SESSIONS_PER_USER: usize = 3;

/// Shared state for the terminal WS handler.
#[derive(Clone)]
pub struct TerminalState {
    pub tracker: SessionTracker,
    pub idle_timeout_secs: u64,
    pub max_sessions_per_sandbox: usize,
    pub max_sessions_per_user: usize,
}

impl TerminalState {
    pub fn new(idle_timeout_secs: u64) -> Self {
        Self {
            tracker: SessionTracker::new(),
            idle_timeout_secs,
            max_sessions_per_sandbox: DEFAULT_MAX_SESSIONS_PER_SANDBOX,
            max_sessions_per_user: DEFAULT_MAX_SESSIONS_PER_USER,
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
    headers: HeaderMap,
    connect_info: Option<ConnectInfo<SocketAddr>>,
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

    // Validate sandbox ID format to prevent path traversal / injection.
    if !SANDBOX_ID_RE.is_match(&sandbox_id) {
        return Ok((
            StatusCode::BAD_REQUEST,
            Json(ApiError::new(400, "sandboxID contains invalid characters")),
        )
            .into_response());
    }

    let container_name = params
        .container
        .as_deref()
        .unwrap_or("default")
        .to_string();

    // Validate container name to prevent log injection and DoS.
    if !CONTAINER_NAME_RE.is_match(&container_name) {
        return Ok((
            StatusCode::BAD_REQUEST,
            Json(ApiError::new(
                400,
                "container name contains invalid characters or is too long",
            )),
        )
            .into_response());
    }

    // ── Origin validation (prevent cross-origin WS hijacking) ────────────
    let auth_configured = state
        .config
        .auth_callback_url
        .as_deref()
        .is_some_and(|u| !u.is_empty());
    if let Some(origin) = headers.get(header::ORIGIN) {
        let origin_str = origin.to_str().unwrap_or("");
        // Skip empty origins (treat as absent — some proxies strip it).
        if !origin_str.is_empty() {
            if let Some(host) = headers.get(header::HOST) {
                let host_str = host.to_str().unwrap_or("");
                if let Ok(parsed) = url::Url::parse(origin_str) {
                    let origin_host = parsed.host_str().unwrap_or("");
                    let host_only = host_str.split(':').next().unwrap_or(host_str);
                    if origin_host != host_only {
                        tracing::warn!(
                            origin = %origin_str,
                            host = %host_str,
                            "rejected cross-origin terminal WebSocket"
                        );
                        return Ok((
                            StatusCode::FORBIDDEN,
                            Json(ApiError::new(403, "cross-origin terminal access denied")),
                        )
                            .into_response());
                    }
                }
            }
        }
    } else if auth_configured {
        // When auth is configured, require Origin header from browsers.
        // Non-browser clients that don't send Origin will be caught at the
        // auth layer; this is a defense-in-depth measure.
        tracing::info!("terminal WS upgrade without Origin header (auth enabled)");
    }

    // ── Auth ────────────────────────────────────────────────────────────
    let user = authenticate_terminal(&state, params.token.as_deref(), &sandbox_id).await?;

    // ── Sandbox validation ──────────────────────────────────────────────
    let detail = state.services.sandboxes.get_sandbox(&sandbox_id).await?;

    if detail.state != SandboxState::Running {
        let state_str = match detail.state {
            SandboxState::Paused => "paused",
            SandboxState::Pausing => "pausing",
            s => {
                tracing::warn!(state = ?s, "unexpected sandbox state in terminal handler");
                return Ok((
                    StatusCode::CONFLICT,
                    Json(serde_json::json!({
                        "error": "sandbox not running",
                        "state": "unknown"
                    })),
                )
                    .into_response());
            }
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
            Json(ApiError::new(500, "sandbox configuration error")),
        )
            .into_response());
    }

    // ── Connection limits (re-check after async auth + validation) ──────
    // The initial check at function entry reduces contention; this
    // second check narrows the TOCTOU window to just the insert below.
    let sandbox_count = state
        .terminal_state
        .tracker
        .count_by_sandbox(&sandbox_id);
    if sandbox_count >= state.terminal_state.max_sessions_per_sandbox {
        return Ok((
            StatusCode::TOO_MANY_REQUESTS,
            Json(ApiError::new(
                429,
                format!(
                    "too many terminal sessions for this sandbox (max {})",
                    state.terminal_state.max_sessions_per_sandbox
                ),
            )),
        )
            .into_response());
    }

    let user_count = state
        .terminal_state
        .tracker
        .count_by_user(&user);
    if user_count >= state.terminal_state.max_sessions_per_user {
        return Ok((
            StatusCode::TOO_MANY_REQUESTS,
            Json(ApiError::new(
                429,
                format!(
                    "too many terminal sessions for this user (max {})",
                    state.terminal_state.max_sessions_per_user
                ),
            )),
        )
            .into_response());
    }

    // ── Build envd connection URL through CubeProxy ─────────────────────
    let envd_url = build_envd_url(&state.config.cubemaster_url, &sandbox_id)?;

    // ── Determine remote address ────────────────────────────────────────
    let remote_addr = if let Some(ConnectInfo(addr)) = connect_info {
        addr.to_string()
    } else {
        headers
            .get(axum::http::header::HeaderName::from_static("x-forwarded-for"))
            .and_then(|v| v.to_str().ok())
            .unwrap_or("unknown")
            .to_string()
    };

    // ── Create terminal session record ──────────────────────────────────
    let session = TerminalSession::new(
        sandbox_id.clone(),
        container_name.clone(),
        user.clone(),
        remote_addr,
        state.terminal_state.idle_timeout_secs,
    );
    let session_id = session.session_id;

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
///
/// When an auth callback URL is configured, the token is forwarded and the
/// callback's response is inspected for a user identity:
///   - Prefers `X-Authenticated-User` header
///   - Falls back to JSON `{"user": "..."}` body
///   - Falls back to `"authenticated"` if neither is present
///
/// When auth is not configured, returns `"anonymous"`.
async fn authenticate_terminal(
    state: &AppState,
    token: Option<&str>,
    sandbox_id: &str,
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
                .header(
                    "X-Request-Path",
                    format!("/sandboxes/{}/terminal", sandbox_id),
                )
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

            // Extract user identity from callback response.
            // Prefer X-Authenticated-User header; fall back to JSON body field "user".
            let user_from_header = resp
                .headers()
                .get("x-authenticated-user")
                .and_then(|v| v.to_str().ok())
                .map(|s| s.to_string());

            let user = if let Some(u) = user_from_header {
                u
            } else {
                resp.text()
                    .await
                    .ok()
                    .and_then(|body| {
                        serde_json::from_str::<serde_json::Value>(&body)
                            .ok()
                            .and_then(|v| v.get("user").and_then(|u| u.as_str()).map(String::from))
                    })
                    .unwrap_or_else(|| "authenticated".to_string())
            };

            Ok(user)
        }
        None => Ok("anonymous".to_string()),
    }
}

/// Build the envd endpoint URL through CubeProxy.
///
/// CubeMaster is typically at `http://<host>:8089`; the CubeProxy HTTP
/// listener is on port 8081 of the same host.  The proxy routes
/// `/sandbox/<id>/<port>/...` to the correct sandbox container.
fn build_envd_url(cubemaster_url: &str, sandbox_id: &str) -> Result<String, AppError> {
    let mut url = url::Url::parse(cubemaster_url)
        .map_err(|e| AppError::Internal(anyhow::anyhow!("invalid cubemaster_url: {}", e)))?;

    url.set_port(Some(8081))
        .map_err(|_| AppError::Internal(anyhow::anyhow!("failed to set proxy port")))?;

    url.set_path(&format!(
        "/sandbox/{}/49983/process.Process/Connect",
        sandbox_id
    ));

    Ok(url.to_string())
}

/// Main bidirectional proxy loop.
///
/// Architecture:
/// - A Tokio mpsc channel feeds WS stdin frames into a streaming reqwest body.
/// - The envd response is read as a byte stream and decoded into events,
///   which are forwarded to the WS as binary frames.
/// - An idle timer ticks at half the configured idle timeout (capped at 15s);
///   if no activity for the configured timeout, both sides are closed.
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
    idle_timeout_secs: u64,
) {
    // Channel for stdin: WS frames → envd request body stream.
    let (stdin_tx, stdin_rx) = tokio::sync::mpsc::channel::<Vec<u8>>(256);

    // Build the streaming request body: ProcessStartRequest, then stdin frames,
    // then end-of-stream marker. The initial ProcessStartRequest tells envd what
    // command to run (interactive bash login shell).
    let connect_req = proxy::build_connect_request("/bin/bash", &["-l".to_string()]);
    let connect_req_bytes = match serde_json::to_vec(&connect_req) {
        Ok(b) => b,
        Err(e) => {
            tracing::error!(session_id = %session_id, error = %e, "connect request serialization failed");
            let _ = ws_socket
                .send(Message::Close(Some(axum::extract::ws::CloseFrame {
                    code: 1011,
                    reason: "internal error".into(),
                })))
                .await;
            cleanup_session(
                &tracker, session_id, TerminalCloseReason::Error,
                &sandbox_id, &container_name, &user,
            );
            return;
        }
    };
    let body_stream = futures::stream::once(async move {
        Ok::<_, std::io::Error>(proxy::encode_frame(proxy::FLAG_DATA, &connect_req_bytes))
    })
    .chain(
        tokio_stream::wrappers::ReceiverStream::new(stdin_rx)
            .map(|chunk| Ok::<_, std::io::Error>(proxy::encode_stdin_frame(&chunk)))
    )
    .chain(futures::stream::once(async {
        Ok::<_, std::io::Error>(proxy::encode_end_stream_frame())
    }));

    let envd_resp = match http_client
        .post(&envd_url)
        .header("Content-Type", "application/connect+json")
        .header("Connect-Protocol-Version", "1")
        .header("X-Access-Token", &envd_access_token)
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
                    reason: "connection failed".into(),
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
                    reason: "connection failed".into(),
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

    // Create a shutdown signal channel so close_sessions_for_sandbox can
    // notify this task to send a WS close frame on sandbox pause/destroy.
    let (shutdown_tx, mut shutdown_rx) = tokio::sync::watch::channel(None);
    tracker.register_shutdown_sender(session_id, shutdown_tx);

    let mut envd_body = envd_resp.bytes_stream();
    let mut envd_buf = FrameBuffer::new();
    // Tick at half the idle timeout (capped at 30s) to catch idle sessions promptly.
    let idle_tick = Duration::from_secs((idle_timeout_secs.min(30) / 2).max(1));
    let mut idle_timer = tokio::time::interval(idle_tick);

    loop {
        tokio::select! {
            // ── WS → envd (stdin) ──────────────────────────────────────
            ws_msg = ws_socket.recv() => {
                let touched = match ws_msg {
                    Some(Ok(Message::Binary(data))) => {
                        if stdin_tx.try_send(data).is_err() {
                            tracing::warn!(session_id = %session_id, "stdin channel full, dropping input");
                        }
                        true
                    }
                    Some(Ok(Message::Text(text))) => {
                        handle_ws_text(&text, &stdin_tx);
                        true
                    }
                    Some(Ok(Message::Ping(data))) => {
                        let _ = ws_socket.send(Message::Pong(data)).await;
                        false
                    }
                    Some(Ok(Message::Pong(_))) => false,
                    Some(Ok(Message::Close(_))) | None => {
                        tracing::info!(session_id = %session_id, "WS closed by client");
                        break;
                    }
                    Some(Err(e)) => {
                        tracing::warn!(session_id = %session_id, error = %e, "WS recv error");
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
                        if let Err(e) = envd_buf.extend(&bytes) {
                            tracing::error!(session_id = %session_id, error = %e, "frame buffer overflow");
                            let _ = ws_socket.send(Message::Close(Some(
                                axum::extract::ws::CloseFrame {
                                    code: 1009, reason: "message too large".into(),
                                }
                            ))).await;
                            cleanup_session(
                                &tracker, session_id, TerminalCloseReason::Error,
                                &sandbox_id, &container_name, &user,
                            );
                            return;
                        }
                        match process_envd_frames(
                            &mut envd_buf, &mut ws_socket, session_id,
                            &tracker, &sandbox_id, &container_name, &user,
                        ).await {
                            FrameResult::Continue => {}
                            FrameResult::NormalExit(reason) => {
                                tracing::info!(session_id = %session_id, reason = %reason, "envd session ended");
                                return;
                            }
                            FrameResult::Error(reason) => {
                                tracing::error!(session_id = %session_id, error = %reason, "envd session error");
                                return;
                            }
                        }
                    }
                    Some(Err(e)) => {
                        tracing::error!(session_id = %session_id, error = %e, "envd stream error");
                        let _ = ws_socket.send(Message::Close(Some(
                            axum::extract::ws::CloseFrame {
                                code: 1011, reason: "internal error".into(),
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

            // ── Shutdown signal (sandbox pause/destroy) ──────────────
            _ = shutdown_rx.changed() => {
                let reason = shutdown_rx.borrow_and_update().unwrap_or(TerminalCloseReason::Error);
                tracing::info!(session_id = %session_id, reason = reason.as_str(), "shutdown signal received");
                let ws_code = match reason {
                    TerminalCloseReason::SandboxPaused => 1001, // going away
                    TerminalCloseReason::SandboxDestroyed => 1001,
                    _ => 1000,
                };
                let _ = ws_socket.send(Message::Close(Some(
                    axum::extract::ws::CloseFrame {
                        code: ws_code,
                        reason: reason.ws_close_reason().into(),
                    }
                ))).await;
                cleanup_session(
                    &tracker, session_id, reason,
                    &sandbox_id, &container_name, &user,
                );
                return;
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
fn handle_ws_text(text: &str, stdin_tx: &tokio::sync::mpsc::Sender<Vec<u8>>) {
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
                        if stdin_tx.try_send(frame.to_vec()).is_err() {
                            tracing::warn!("stdin channel full, dropping resize event");
                        }
                    }
                }
            }
        }
    }
}

/// Return type for `process_envd_frames` that distinguishes normal termination
/// from actual errors, so the caller can log at the appropriate level.
enum FrameResult {
    /// More frames may be available; caller should continue.
    Continue,
    /// The envd connection ended normally (end-of-stream, process exit).
    /// Cleanup has already been performed.
    NormalExit(String),
    /// An actual error occurred (e.g., WS send failure).
    /// Cleanup has already been performed.
    Error(String),
}

/// Process all complete frames in the envd buffer, forwarding events to WS.
/// Returns `FrameResult::Continue` to keep processing, or a terminal variant if
/// the connection should end.
async fn process_envd_frames(
    buf: &mut FrameBuffer,
    ws: &mut WebSocket,
    session_id: crate::terminal::session::SessionId,
    tracker: &SessionTracker,
    sandbox_id: &str,
    container_name: &str,
    user: &str,
) -> FrameResult {
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
                    return FrameResult::NormalExit("envd end-of-stream".to_string());
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
                                    return FrameResult::NormalExit("envd process ended".to_string());
                                }
                                EnvdEvent::Keepalive => Ok(()),
                            };
                            if send_result.is_err() {
                                cleanup_session(
                                    tracker, session_id, TerminalCloseReason::Error,
                                    sandbox_id, container_name, user,
                                );
                                return FrameResult::Error("WS send error".to_string());
                            }
                        }
                    }
                    Err(e) => {
                        tracing::warn!(session_id = %session_id, error = %e, "envd event parse error");
                    }
                }
            }
            Ok(None) => return FrameResult::Continue,
            Err(e) => {
                tracing::warn!(session_id = %session_id, error = %e, "envd frame decode error");
                return FrameResult::Continue;
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
    // Always remove the shutdown sender — prevents stale entries when a
    // session exits before receiving an external shutdown signal.
    tracker.remove_shutdown_sender(&session_id);

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
///
/// Sends a shutdown signal to each active session task via its watch channel,
/// then removes any sessions that haven't already self-cleaned up.
pub async fn close_sessions_for_sandbox(
    tracker: &SessionTracker,
    sandbox_id: &str,
    reason: TerminalCloseReason,
) {
    // 1. Signal each session task to send a WS close frame and exit.
    let signaled = tracker.signal_shutdown_for_sandbox(sandbox_id, reason);

    // 2. Remove any remaining sessions (tasks that didn't self-clean).
    let sessions = tracker.remove_by_sandbox(sandbox_id);
    let count = sessions.len();
    for session in &sessions {
        // Also clean up stale shutdown senders.
        tracker.remove_shutdown_sender(&session.session_id);
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
    if signaled.len() + count > 0 {
        tracing::info!(
            sandbox_id = %sandbox_id,
            signaled = signaled.len(),
            removed = count,
            reason = reason.as_str(),
            "closed terminal sessions for sandbox"
        );
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use base64::Engine;

    #[test]
    fn test_sandbox_id_regex_valid() {
        assert!(SANDBOX_ID_RE.is_match("abc123"));
        assert!(SANDBOX_ID_RE.is_match("sandbox-42"));
        assert!(SANDBOX_ID_RE.is_match("test_sb_001"));
        assert!(SANDBOX_ID_RE.is_match("a"));
    }

    #[test]
    fn test_sandbox_id_regex_rejects_slashes() {
        assert!(!SANDBOX_ID_RE.is_match("../etc/passwd"));
        assert!(!SANDBOX_ID_RE.is_match("sb/../../"));
        assert!(!SANDBOX_ID_RE.is_match("a b"));
        assert!(!SANDBOX_ID_RE.is_match("sb\x00null"));
    }

    #[test]
    fn test_build_envd_url_http() {
        let url = build_envd_url("http://10.0.0.1:8089", "sb-abc").unwrap();
        assert_eq!(
            url,
            "http://10.0.0.1:8081/sandbox/sb-abc/49983/process.Process/Connect"
        );
    }

    #[test]
    fn test_build_envd_url_https() {
        let url = build_envd_url("https://cube.example.com:8089", "sb-xyz").unwrap();
        assert_eq!(
            url,
            "https://cube.example.com:8081/sandbox/sb-xyz/49983/process.Process/Connect"
        );
    }

    #[test]
    fn test_build_envd_url_invalid_input() {
        assert!(build_envd_url("not a url", "sb-abc").is_err());
    }

    #[test]
    fn test_handle_ws_text_resize() {
        let (tx, mut rx) = tokio::sync::mpsc::channel::<Vec<u8>>(16);
        handle_ws_text(r#"{"type":"resize","cols":120,"rows":40}"#, &tx);

        let rt = tokio::runtime::Runtime::new().unwrap();
        let frame = rt.block_on(async { rx.recv().await });
        assert!(frame.is_some());
        let frame = frame.unwrap();
        assert_eq!(frame[0], 0x00);
        let payload_len = u32::from_be_bytes([frame[1], frame[2], frame[3], frame[4]]) as usize;
        assert_eq!(frame.len(), 5 + payload_len);

        let payload: serde_json::Value =
            serde_json::from_slice(&frame[5..]).expect("payload should be valid JSON");
        let b64_pty = payload["event"]["data"]["pty"]
            .as_str()
            .expect("pty field should be a base64 string");
        let pty_bytes = base64::engine::general_purpose::STANDARD
            .decode(b64_pty)
            .expect("pty should be valid base64");
        let pty_str = std::str::from_utf8(&pty_bytes).expect("pty should be valid UTF-8");
        assert!(pty_str.contains("120"), "pty payload should contain cols=120: {}", pty_str);
        assert!(pty_str.contains("40"), "pty payload should contain rows=40: {}", pty_str);
    }

    #[test]
    fn test_handle_ws_text_non_resize_ignored() {
        let (tx, _rx) = tokio::sync::mpsc::channel::<Vec<u8>>(16);
        handle_ws_text(r#"{"type":"ping"}"#, &tx);
        // No panic, no side effect.
    }

    #[test]
    fn test_handle_ws_text_invalid_json_ignored() {
        let (tx, _rx) = tokio::sync::mpsc::channel::<Vec<u8>>(16);
        handle_ws_text("not json", &tx);
        // No panic, no side effect.
    }

    #[test]
    fn test_handle_ws_text_resize_zero_cols_ignored() {
        let (tx, _rx) = tokio::sync::mpsc::channel::<Vec<u8>>(16);
        handle_ws_text(r#"{"type":"resize","cols":0,"rows":40}"#, &tx);
        // Zero cols is rejected by the > 0 guard.
    }
}