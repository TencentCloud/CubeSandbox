// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use axum::{
    extract::{
        ws::{Message, WebSocket, WebSocketUpgrade},
        ConnectInfo, Extension, Path, Query, State,
    },
    http::HeaderMap,
    response::IntoResponse,
};
use base64::{engine::general_purpose::STANDARD as BASE64, Engine as _};
use bytes::BytesMut;
use futures::{SinkExt, StreamExt};
use serde::{Deserialize, Serialize};
use serde_json::json;
use tokio::sync::{mpsc, oneshot, RwLock};
use tokio_util::sync::CancellationToken;

use crate::{
    envd,
    error::{AppError, AppResult},
    middleware::auth::{AuthContext, SharedAuthContext},
    middleware::terminal_audit::{extract_remote_ip, log_terminal_event},
    models::{SandboxContainer, SandboxContainerState, SandboxState},
    state::AppState,
};
use std::time::{Duration, Instant, SystemTime};
use std::{
    net::SocketAddr,
    sync::{
        atomic::{AtomicBool, AtomicU16, AtomicU64, Ordering},
        Arc,
    },
};

pub(crate) const DEFAULT_COLS: u16 = 80;
pub(crate) const DEFAULT_ROWS: u16 = 24;
const SIGNAL_SIGKILL: &str = "SIGNAL_SIGKILL";

#[derive(Debug, Deserialize)]
pub(crate) struct TerminalQuery {
    #[serde(default = "default_cols")]
    pub(crate) cols: u16,
    #[serde(default = "default_rows")]
    pub(crate) rows: u16,
    #[serde(default)]
    pub(crate) container: Option<String>,
}

fn default_cols() -> u16 {
    DEFAULT_COLS
}

fn default_rows() -> u16 {
    DEFAULT_ROWS
}

#[derive(Debug, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
enum ClientMessage {
    Input { data: String },
    Resize { cols: u16, rows: u16 },
}

#[derive(Debug, Serialize)]
#[serde(tag = "type", rename_all = "snake_case")]
enum ServerMessage {
    Output {
        data: String,
    },
    Error {
        message: String,
    },
    Close {
        #[serde(skip_serializing_if = "Option::is_none")]
        reason: Option<String>,
    },
}

#[derive(Debug)]
enum TerminalAction {
    Input(Vec<u8>),
    Resize { cols: u16, rows: u16 },
}

/// Locate a running container by container ID.  The WebUI sends container IDs
/// from the sandbox detail response, so avoid name fallback that can shadow an
/// actual ID when containers have colliding names.
fn select_running_container<'a>(
    containers: &'a [SandboxContainer],
    container_id: &str,
) -> AppResult<&'a SandboxContainer> {
    let container = containers
        .iter()
        .find(|c| c.container_id == container_id)
        .ok_or_else(|| {
            AppError::BadRequest(format!("container '{}' not found in sandbox", container_id))
        })?;
    if container.state != SandboxContainerState::Running {
        return Err(AppError::Conflict(format!(
            "container {} is not running",
            container_id
        )));
    }
    Ok(container)
}

/// WebSocket entry point for the sandbox terminal.
pub async fn terminal_ws(
    ws: WebSocketUpgrade,
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    Query(query): Query<TerminalQuery>,
    Extension(auth_ctx_ext): Extension<SharedAuthContext>,
    headers: HeaderMap,
    connect_info: Option<ConnectInfo<SocketAddr>>,
) -> AppResult<impl IntoResponse> {
    let detail = state.services.sandboxes.get_sandbox(&sandbox_id).await?;

    if detail.state != SandboxState::Running {
        return Err(AppError::Conflict(format!(
            "sandbox {} is not running",
            sandbox_id
        )));
    }

    let selected_container_id = query
        .container
        .as_deref()
        .map(|container_id| {
            select_running_container(&detail.containers, container_id)
                .map(|c| c.container_id.clone())
        })
        .transpose()?;

    let domain = detail
        .domain
        .filter(|d| !d.trim().is_empty())
        .unwrap_or_else(|| state.config.sandbox_domain.clone());
    if domain.trim().is_empty() {
        return Err(AppError::Internal(anyhow::anyhow!(
            "sandbox domain is not configured"
        )));
    }

    let remote_ip = extract_remote_ip(&headers, connect_info.map(|ci| ci.0));
    let auth_ctx = auth_ctx_ext.read().await.clone();

    Ok(ws.on_upgrade(move |socket| {
        handle_terminal_socket(
            socket,
            state,
            sandbox_id,
            domain,
            PtySize {
                cols: query.cols,
                rows: query.rows,
            },
            selected_container_id,
            remote_ip,
            auth_ctx,
        )
    }))
}

#[derive(Debug, Clone, Copy)]
struct PtySize {
    cols: u16,
    rows: u16,
}

async fn handle_terminal_socket(
    socket: WebSocket,
    state: AppState,
    sandbox_id: String,
    domain: String,
    initial_size: PtySize,
    selected_container_id: Option<String>,
    remote_ip: String,
    auth_ctx: AuthContext,
) {
    let start = Instant::now();
    let connected = Arc::new(AtomicBool::new(false));
    let current_size = Arc::new((
        AtomicU16::new(initial_size.cols),
        AtomicU16::new(initial_size.rows),
    ));
    let last_activity = Arc::new(AtomicU64::new(unix_now_secs()));
    let shutdown_reason: Arc<RwLock<Option<String>>> = Arc::new(RwLock::new(None));

    let (ws_tx, ws_rx) = socket.split();
    let cancel = CancellationToken::new();
    let (pid_tx, pid_rx) = oneshot::channel::<i64>();
    let (action_tx, action_rx) = mpsc::channel::<TerminalAction>(64);
    let (ping_tx, ping_rx) = mpsc::channel::<()>(2);

    let mut set = tokio::task::JoinSet::<String>::new();
    set.spawn(envd_reader(
        state.clone(),
        sandbox_id.clone(),
        domain.clone(),
        initial_size,
        selected_container_id.clone(),
        remote_ip.clone(),
        auth_ctx.clone(),
        connected.clone(),
        Some(pid_tx),
        ws_tx,
        shutdown_reason.clone(),
        ping_rx,
        cancel.child_token(),
    ));
    set.spawn(envd_writer(
        state.clone(),
        sandbox_id.clone(),
        domain,
        pid_rx,
        action_rx,
        current_size.clone(),
        cancel.child_token(),
    ));
    set.spawn(ws_reader_task(
        action_tx,
        ws_rx,
        last_activity.clone(),
        shutdown_reason.clone(),
        cancel.child_token(),
    ));

    let idle_timeout = Duration::from_secs(state.config.terminal_idle_timeout_seconds);
    if idle_timeout > Duration::ZERO {
        set.spawn(idle_watcher(
            last_activity.clone(),
            shutdown_reason.clone(),
            idle_timeout,
            cancel.clone(),
        ));
    }

    let keepalive_interval = Duration::from_secs(state.config.terminal_keepalive_interval_seconds);
    if keepalive_interval > Duration::ZERO {
        set.spawn(keepalive_task(
            ping_tx,
            keepalive_interval,
            cancel.child_token(),
        ));
    }

    // Wait for any task to finish, then signal the others to shut down.
    let disconnect_reason = match set.join_next().await {
        Some(Ok(reason)) => reason,
        Some(Err(e)) => {
            tracing::error!(error = %e, "terminal task panicked");
            "task_panicked".to_string()
        }
        None => "unknown".to_string(),
    };
    cancel.cancel();

    // Give the remaining tasks a short grace period to clean up (the writer
    // needs to SIGKILL the PTY).  Abort anything still stuck afterwards.
    while !set.is_empty() {
        match tokio::time::timeout(Duration::from_secs(5), set.join_next()).await {
            Ok(Some(_)) => {}
            _ => {
                set.abort_all();
                break;
            }
        }
    }

    if connected.load(Ordering::Relaxed) {
        let cols = current_size.0.load(Ordering::Relaxed);
        let rows = current_size.1.load(Ordering::Relaxed);
        let mut extra = vec![
            ("duration_ms", (start.elapsed().as_millis() as u64).into()),
            ("disconnect_reason", disconnect_reason.clone().into()),
        ];
        if let Some(ref container_id) = selected_container_id {
            extra.push(("container", container_id.clone().into()));
        }
        log_terminal_event(
            &state.logger,
            "terminal.disconnect",
            &sandbox_id,
            &remote_ip,
            &auth_ctx,
            cols,
            rows,
            extra,
        )
        .await;
    }
}

/// Reads the envd `process.Process/Start` stream and forwards PTY output to
/// the browser.  Signals the writer task once the `start` event is received.
/// Returns a short human-readable reason string used for audit logs.
fn build_start_payload(size: PtySize, selected_container_id: Option<&str>) -> serde_json::Value {
    let mut payload = json!({
        "process": {
            "cmd": "/bin/bash",
            "args": ["-i", "-l"],
            "envs": {
                "TERM": "xterm-256color",
                "LANG": "C.UTF-8",
                "LC_ALL": "C.UTF-8"
            }
        },
        "pty": {
            "size": {
                "rows": size.rows,
                "cols": size.cols
            }
        }
    });

    if let Some(container_id) = selected_container_id {
        payload["process"]["container_id"] = json!(container_id);
    }

    payload
}

async fn envd_reader(
    state: AppState,
    sandbox_id: String,
    domain: String,
    size: PtySize,
    selected_container_id: Option<String>,
    remote_ip: String,
    auth_ctx: AuthContext,
    connected: Arc<AtomicBool>,
    mut pid_tx: Option<oneshot::Sender<i64>>,
    mut ws_tx: futures::stream::SplitSink<WebSocket, Message>,
    shutdown_reason: Arc<RwLock<Option<String>>>,
    mut ping_rx: mpsc::Receiver<()>,
    cancel: CancellationToken,
) -> String {
    let start_process = build_start_payload(size, selected_container_id.as_deref());

    let body = match serde_json::to_vec(&start_process) {
        Ok(bytes) => envd::connect_envelope(&bytes),
        Err(e) => {
            let _ = send_error(
                &mut ws_tx,
                &format!("failed to serialize start request: {}", e),
            )
            .await;
            tracing::error!(error = %e, "failed to serialize terminal start request");
            return "start_serialize_error".to_string();
        }
    };

    let host = envd::envd_host(envd::ENVD_PORT, &sandbox_id, &domain);
    let auth_header = envd::basic_auth_header(
        &state.config.envd_auth_username,
        state.config.envd_auth_password.as_deref(),
    );
    let req = state
        .http_client
        .post(envd::envd_process_url("Start"))
        .header("Host", host)
        .header("Content-Type", envd::CONNECT_JSON)
        .header("Connect-Protocol-Version", envd::CONNECT_PROTOCOL_VERSION)
        .header("Connect-Content-Encoding", "identity")
        .header("Authorization", auth_header)
        .body(body);

    let resp = match req.send().await {
        Ok(r) => r,
        Err(e) => {
            let _ = send_error(
                &mut ws_tx,
                &format!("failed to connect to sandbox terminal: {}", e),
            )
            .await;
            tracing::error!(error = %e, "failed to connect to sandbox terminal");
            return "envd_start_request_error".to_string();
        }
    };

    if !resp.status().is_success() {
        let status = resp.status();
        let body_text = resp.text().await.unwrap_or_default();
        let _ = send_error(
            &mut ws_tx,
            &format!(
                "envd terminal start failed: HTTP {} - {}",
                status, body_text
            ),
        )
        .await;
        tracing::error!(status = %status, "envd terminal start returned HTTP error");
        return "envd_start_http_error".to_string();
    }

    let mut stream = resp.bytes_stream();
    let mut buffer = BytesMut::new();
    let mut finish_reason = "envd_stream_closed".to_string();

    loop {
        let chunk = tokio::select! {
            _ = cancel.cancelled() => {
                let reason = shutdown_reason.read().await.clone();
                let close_reason = reason.clone();
                let _ = send_msg(
                    &mut ws_tx,
                    &ServerMessage::Close { reason: close_reason },
                )
                .await;
                return reason.unwrap_or_else(|| "cancelled".to_string());
            },
            ping = ping_rx.recv() => {
                match ping {
                    Some(()) => {
                        if let Err(e) = ws_tx.send(Message::Ping(vec![])).await {
                            tracing::debug!(error = %e, "failed to send WebSocket ping");
                        }
                    }
                    None => return "keepalive_stopped".to_string(),
                }
                continue;
            },
            item = stream.next() => item,
        };

        match chunk {
            Some(Ok(bytes)) => buffer.extend_from_slice(&bytes),
            Some(Err(e)) => {
                let _ =
                    send_error(&mut ws_tx, &format!("error reading terminal stream: {}", e)).await;
                tracing::debug!(error = %e, "error reading terminal stream");
                finish_reason = "envd_stream_error".to_string();
                break;
            }
            None => break,
        }

        while buffer.len() >= 5 {
            let flags = buffer[0];
            let len = u32::from_be_bytes([buffer[1], buffer[2], buffer[3], buffer[4]]) as usize;
            if len > 8 * 1024 * 1024 {
                let _ = send_error(&mut ws_tx, "terminal stream frame exceeds maximum size").await;
                return "envd_frame_too_large".to_string();
            }
            if buffer.len() < 5 + len {
                break;
            }

            let payload = buffer.split_to(5 + len).split_off(5).freeze();

            if flags & envd::CONNECT_COMPRESSED_FLAG != 0 {
                let _ = send_error(
                    &mut ws_tx,
                    "compressed terminal stream frames are not supported",
                )
                .await;
                return "envd_compressed_frame".to_string();
            }

            if flags & envd::CONNECT_END_STREAM_FLAG != 0 {
                // Trailer frame.  If it carries an error, surface it.
                if let Ok(v) = serde_json::from_slice::<serde_json::Value>(&payload) {
                    if let Some(err) = v.get("error").and_then(|e| e.as_str()) {
                        let _ =
                            send_error(&mut ws_tx, &format!("envd terminal error: {}", err)).await;
                    }
                }
                break;
            }

            let event: serde_json::Value = match serde_json::from_slice(&payload) {
                Ok(v) => v,
                Err(e) => {
                    tracing::warn!(error = %e, "invalid envd terminal event JSON");
                    continue;
                }
            };

            if let Some(start) = event.get("event").and_then(|e| e.get("start")) {
                if let Some(pid) = start.get("pid").and_then(|p| p.as_i64()) {
                    if let Some(tx) = pid_tx.take() {
                        let _ = tx.send(pid);
                        if !connected.swap(true, Ordering::Relaxed) {
                            let mut extra = vec![];
                            if let Some(ref container_id) = selected_container_id {
                                extra.push(("container", container_id.clone().into()));
                            }
                            log_terminal_event(
                                &state.logger,
                                "terminal.connect",
                                &sandbox_id,
                                &remote_ip,
                                &auth_ctx,
                                size.cols,
                                size.rows,
                                extra,
                            )
                            .await;
                        }
                    }
                }
            }

            if let Some(data) = event.get("event").and_then(|e| e.get("data")) {
                if let Some(pty) = data.get("pty").and_then(|p| p.as_str()) {
                    let _ = send_msg(
                        &mut ws_tx,
                        &ServerMessage::Output {
                            data: pty.to_string(),
                        },
                    )
                    .await;
                }
            }

            if event.get("event").and_then(|e| e.get("end")).is_some() {
                let _ = send_msg(&mut ws_tx, &ServerMessage::Close { reason: None }).await;
                return "envd_end".to_string();
            }
        }
    }

    let _ = send_msg(&mut ws_tx, &ServerMessage::Close { reason: None }).await;
    finish_reason
}

/// Receives input/resize actions from the browser and forwards them to envd
/// via the unary `SendInput` and `Update` RPCs.  Cleans up the PTY when the
/// channel closes or cancellation is requested.  Returns a short reason string
/// for audit logs.
async fn envd_writer(
    state: AppState,
    sandbox_id: String,
    domain: String,
    pid_rx: oneshot::Receiver<i64>,
    mut action_rx: mpsc::Receiver<TerminalAction>,
    current_size: Arc<(AtomicU16, AtomicU16)>,
    cancel: CancellationToken,
) -> String {
    let pid = match pid_rx.await {
        Ok(p) => p,
        Err(_) => {
            tracing::debug!("envd_writer: pid sender dropped before start event");
            return "pid_sender_dropped".to_string();
        }
    };

    let mut finish_reason = "writer_finished".to_string();
    loop {
        let action = tokio::select! {
            _ = cancel.cancelled() => {
                finish_reason = "cancelled".to_string();
                break;
            },
            item = action_rx.recv() => match item {
                Some(a) => a,
                None => break,
            },
        };

        match action {
            TerminalAction::Input(bytes) => {
                let payload = json!({
                    "process": { "pid": pid },
                    "input": { "pty": BASE64.encode(&bytes) }
                });
                if let Err(e) =
                    send_envd_unary(&state, &sandbox_id, &domain, "SendInput", payload).await
                {
                    tracing::error!(error = %e, pid, "terminal SendInput to envd failed");
                    cancel.cancel();
                    finish_reason = "envd_action_error".to_string();
                    break;
                }
            }
            TerminalAction::Resize { cols, rows } => {
                current_size.0.store(cols, Ordering::Relaxed);
                current_size.1.store(rows, Ordering::Relaxed);
                let payload = json!({
                    "process": { "pid": pid },
                    "pty": { "size": { "rows": rows, "cols": cols } }
                });
                if let Err(e) =
                    send_envd_unary(&state, &sandbox_id, &domain, "Update", payload).await
                {
                    tracing::error!(error = %e, pid, "terminal Update to envd failed");
                    cancel.cancel();
                    finish_reason = "envd_action_error".to_string();
                    break;
                }
            }
        }
    }

    // Best-effort cleanup: kill the shell so we do not leak PTYs.
    let kill_payload = json!({
        "process": { "pid": pid },
        "signal": SIGNAL_SIGKILL
    });
    if let Err(e) = send_envd_unary(&state, &sandbox_id, &domain, "SendSignal", kill_payload).await
    {
        tracing::debug!(error = %e, pid, "failed to SIGKILL terminal process (may already be gone)");
    }

    finish_reason
}

/// Reads JSON messages from the browser and forwards them to the envd writer.
/// Returns a short reason string for audit logs.
async fn ws_reader_task(
    action_tx: mpsc::Sender<TerminalAction>,
    mut ws_rx: futures::stream::SplitStream<WebSocket>,
    last_activity: Arc<AtomicU64>,
    shutdown_reason: Arc<RwLock<Option<String>>>,
    cancel: CancellationToken,
) -> String {
    loop {
        let msg = tokio::select! {
            _ = cancel.cancelled() => {
                let reason = shutdown_reason.read().await.clone();
                return reason.unwrap_or_else(|| "cancelled".to_string());
            },
            item = ws_rx.next() => item,
        };

        match msg {
            Some(Ok(Message::Text(text))) => {
                let client_msg: ClientMessage = match serde_json::from_str(&text) {
                    Ok(m) => m,
                    Err(e) => {
                        tracing::debug!(error = %e, "ignoring invalid terminal client message");
                        continue;
                    }
                };

                let action = match client_msg {
                    ClientMessage::Input { data } => match BASE64.decode(data) {
                        Ok(bytes) => TerminalAction::Input(bytes),
                        Err(e) => {
                            tracing::debug!(error = %e, "ignoring invalid base64 terminal input");
                            continue;
                        }
                    },
                    ClientMessage::Resize { cols, rows } => TerminalAction::Resize { cols, rows },
                };

                last_activity.store(unix_now_secs(), Ordering::Relaxed);

                if action_tx.send(action).await.is_err() {
                    return "action_channel_closed".to_string();
                }
            }
            Some(Ok(Message::Close(_))) | None => return "client_closed".to_string(),
            Some(Ok(_)) => {}
            Some(Err(e)) => {
                tracing::debug!(error = %e, "WebSocket receive error");
                return "client_error".to_string();
            }
        }
    }
}

async fn idle_watcher(
    last_activity: Arc<AtomicU64>,
    shutdown_reason: Arc<RwLock<Option<String>>>,
    timeout: Duration,
    cancel: CancellationToken,
) -> String {
    let check_interval = timeout / 4;
    let check_interval = check_interval.max(Duration::from_secs(1));

    loop {
        tokio::select! {
            _ = cancel.cancelled() => return "cancelled".to_string(),
            _ = tokio::time::sleep(check_interval) => {}
        }

        let elapsed = Duration::from_secs(unix_now_secs() - last_activity.load(Ordering::Relaxed));
        if elapsed >= timeout {
            let mut reason = shutdown_reason.write().await;
            *reason = Some("idle_timeout".to_string());
            cancel.cancel();
            return "idle_timeout".to_string();
        }
    }
}

async fn keepalive_task(
    ping_tx: mpsc::Sender<()>,
    interval: Duration,
    cancel: CancellationToken,
) -> String {
    loop {
        tokio::select! {
            _ = cancel.cancelled() => return "cancelled".to_string(),
            _ = tokio::time::sleep(interval) => {}
        }

        if ping_tx.send(()).await.is_err() {
            return "ping_channel_closed".to_string();
        }
    }
}

async fn send_envd_unary(
    state: &AppState,
    sandbox_id: &str,
    domain: &str,
    method: &str,
    payload: serde_json::Value,
) -> AppResult<()> {
    let host = envd::envd_host(envd::ENVD_PORT, sandbox_id, domain);
    let auth_header = envd::basic_auth_header(
        &state.config.envd_auth_username,
        state.config.envd_auth_password.as_deref(),
    );
    let resp = state
        .http_client
        .post(envd::envd_process_url(method))
        .header("Host", host)
        .header("Content-Type", "application/json")
        .header("Connect-Protocol-Version", envd::CONNECT_PROTOCOL_VERSION)
        .header("Authorization", auth_header)
        .json(&payload)
        .send()
        .await
        .map_err(|e| {
            AppError::Internal(anyhow::anyhow!("envd {} request failed: {}", method, e))
        })?;

    if !resp.status().is_success() {
        let status = resp.status();
        return Err(AppError::Internal(anyhow::anyhow!(
            "envd {} returned HTTP {}",
            method,
            status,
        )));
    }

    Ok(())
}

async fn send_msg(ws_tx: &mut futures::stream::SplitSink<WebSocket, Message>, msg: &ServerMessage) {
    let text = match serde_json::to_string(msg) {
        Ok(t) => t,
        Err(e) => {
            tracing::error!(error = %e, "failed to serialize terminal server message");
            return;
        }
    };
    if let Err(e) = ws_tx.send(Message::Text(text)).await {
        tracing::debug!(error = %e, "failed to send terminal message to browser");
    }
}

async fn send_error(ws_tx: &mut futures::stream::SplitSink<WebSocket, Message>, message: &str) {
    send_msg(
        ws_tx,
        &ServerMessage::Error {
            message: message.to_string(),
        },
    )
    .await;
}

fn unix_now_secs() -> u64 {
    SystemTime::now()
        .duration_since(SystemTime::UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::models::{SandboxContainer, SandboxContainerState};

    #[test]
    fn basic_auth_header_default_root_no_password() {
        let header = envd::basic_auth_header("root", None);
        assert_eq!(header, "Basic cm9vdDo=");
    }

    #[test]
    fn basic_auth_header_with_password() {
        let header = envd::basic_auth_header("root", Some("secret"));
        assert_eq!(header, "Basic cm9vdDpzZWNyZXQ=");
    }

    #[test]
    fn select_running_container_matches_by_id_only() {
        let containers = vec![SandboxContainer {
            container_id: "cid-1".to_string(),
            name: "app".to_string(),
            state: SandboxContainerState::Running,
            image: "img".to_string(),
            cpu_count: 1,
            memory_mb: 256,
            started_at: None,
            kind: None,
        }];

        assert!(select_running_container(&containers, "cid-1").is_ok());
        assert!(select_running_container(&containers, "app").is_err());
    }

    #[test]
    fn select_running_container_rejects_missing_or_not_running() {
        let containers = vec![
            SandboxContainer {
                container_id: "cid-1".to_string(),
                name: "app".to_string(),
                state: SandboxContainerState::Running,
                image: "img".to_string(),
                cpu_count: 1,
                memory_mb: 256,
                started_at: None,
                kind: None,
            },
            SandboxContainer {
                container_id: "cid-2".to_string(),
                name: "sidecar".to_string(),
                state: SandboxContainerState::Paused,
                image: "img".to_string(),
                cpu_count: 1,
                memory_mb: 256,
                started_at: None,
                kind: None,
            },
        ];

        assert!(select_running_container(&containers, "missing").is_err());
        assert!(select_running_container(&containers, "sidecar").is_err());
    }

    #[test]
    fn build_start_payload_includes_container_id_when_selected() {
        let payload = build_start_payload(PtySize { cols: 80, rows: 24 }, Some("cid-1"));
        assert_eq!(payload["process"]["container_id"].as_str(), Some("cid-1"));
    }

    #[test]
    fn build_start_payload_omits_container_id_when_not_selected() {
        let payload = build_start_payload(PtySize { cols: 80, rows: 24 }, None);
        assert!(payload["process"]["container_id"].is_null());
    }
}
