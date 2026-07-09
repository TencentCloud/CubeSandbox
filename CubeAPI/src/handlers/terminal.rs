// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use axum::{
    extract::{
        ws::{Message, WebSocket, WebSocketUpgrade},
        Path, Query, State,
    },
    http::{HeaderMap, StatusCode},
    response::IntoResponse,
};
use base64::{engine::general_purpose::STANDARD as BASE64, Engine as _};
use futures::{SinkExt, StreamExt};
use std::time::Duration;
use tokio::time::Instant;
use uuid::Uuid;

use crate::{
    error::{AppError, AppResult},
    logging::{LogEvent, LogLevel},
    models::{SandboxState, TerminalClientMessage, TerminalQuery, TerminalServerMessage},
    services::terminal::{
        parse_pty_event, ConnectJsonDecoder, PtyEvent, TerminalService, TerminalTarget,
    },
    state::AppState,
};

const DEFAULT_ROWS: u16 = 24;
const DEFAULT_COLS: u16 = 80;
const IDLE_TIMEOUT_SECS: u64 = 30 * 60;
const SESSION_HEADER: &str = "x-session-token";

pub async fn open_terminal(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    Query(query): Query<TerminalQuery>,
    headers: HeaderMap,
    ws: WebSocketUpgrade,
) -> AppResult<impl IntoResponse> {
    let actor = authorize_terminal(&state, &headers, &query, &sandbox_id).await?;
    let detail = state.services.sandboxes.get_sandbox(&sandbox_id).await?;
    ensure_terminal_ready(detail.state)?;

    let target = TerminalTarget {
        sandbox_id: sandbox_id.clone(),
        domain: detail
            .domain
            .clone()
            .unwrap_or_else(|| state.config.sandbox_domain.clone()),
        envd_access_token: detail.envd_access_token.clone(),
        container_id: query.container.clone(),
        process_base_url: None,
    };
    let (rows, cols) = terminal_size(&query);
    let session_id = new_session_id();
    let logger = state.logger.clone();
    let http = state.http_client.clone();
    let container_id = query.container.clone();

    Ok(ws.on_upgrade(move |socket| async move {
        run_terminal_socket(
            socket,
            TerminalSocketContext {
                service: TerminalService::new(http),
                target,
                rows,
                cols,
                session_id,
                actor,
                logger,
                container_id,
            },
        )
        .await;
    }))
}

struct TerminalSocketContext {
    service: TerminalService,
    target: TerminalTarget,
    rows: u16,
    cols: u16,
    session_id: String,
    actor: String,
    logger: crate::logging::ArcLogger,
    container_id: Option<String>,
}

async fn run_terminal_socket(socket: WebSocket, ctx: TerminalSocketContext) {
    let sandbox_id = ctx.target.sandbox_id.clone();
    let start_result = ctx.service.start(&ctx.target, ctx.rows, ctx.cols).await;
    let mut resp = match start_result {
        Ok(resp) => resp,
        Err(err) => {
            send_one_error(socket, format!("failed to start terminal: {}", err)).await;
            return;
        }
    };

    let (mut ws_tx, mut ws_rx) = socket.split();
    let mut decoder = ConnectJsonDecoder::new();
    let mut pid: Option<i64> = None;

    while pid.is_none() {
        let chunk = match resp.chunk().await {
            Ok(Some(chunk)) => chunk,
            Ok(None) => {
                let _ = send_server_message(
                    &mut ws_tx,
                    &TerminalServerMessage::Error {
                        message: "terminal stream closed before start".to_string(),
                    },
                )
                .await;
                return;
            }
            Err(err) => {
                let _ = send_server_message(
                    &mut ws_tx,
                    &TerminalServerMessage::Error {
                        message: err.to_string(),
                    },
                )
                .await;
                return;
            }
        };
        match decoder.push(&chunk) {
            Ok(messages) => {
                for message in messages {
                    match parse_pty_event(message) {
                        Ok(PtyEvent::Start { pid: started }) => {
                            pid = Some(started);
                            break;
                        }
                        Ok(_) => {}
                        Err(err) => {
                            let _ = send_server_message(
                                &mut ws_tx,
                                &TerminalServerMessage::Error {
                                    message: err.to_string(),
                                },
                            )
                            .await;
                            return;
                        }
                    }
                }
            }
            Err(err) => {
                let _ = send_server_message(
                    &mut ws_tx,
                    &TerminalServerMessage::Error {
                        message: err.to_string(),
                    },
                )
                .await;
                return;
            }
        }
    }

    let pid = pid.expect("checked above");
    ctx.logger
        .log(terminal_audit_event(
            "terminal.session.opened",
            &ctx.actor,
            &sandbox_id,
            &ctx.session_id,
            ctx.container_id.as_deref(),
        ))
        .await;

    let _ = send_server_message(
        &mut ws_tx,
        &TerminalServerMessage::Status {
            status: "ready".to_string(),
            session_id: ctx.session_id.clone(),
            pid: Some(pid),
        },
    )
    .await;

    let idle = tokio::time::sleep(idle_timeout_duration());
    tokio::pin!(idle);

    'session: loop {
        tokio::select! {
            _ = &mut idle => {
                let _ = send_server_message(&mut ws_tx, &TerminalServerMessage::Exit {
                    code: None,
                    message: Some("terminal session idle timeout".to_string()),
                }).await;
                break;
            }
            chunk = resp.chunk() => {
                let chunk = match chunk {
                    Ok(Some(chunk)) => chunk,
                    Ok(None) => break,
                    Err(err) => {
                        let _ = send_server_message(&mut ws_tx, &TerminalServerMessage::Error { message: err.to_string() }).await;
                        break;
                    }
                };
                match decoder.push(&chunk) {
                    Ok(messages) => {
                        let mut close_session = false;
                        for message in messages {
                            match parse_pty_event(message) {
                                Ok(PtyEvent::Output { data }) => {
                                    if send_server_message(&mut ws_tx, &TerminalServerMessage::Output { data: BASE64.encode(data) }).await.is_err() {
                                        close_session = true;
                                        break;
                                    }
                                }
                                Ok(PtyEvent::End { code, message }) => {
                                    let _ = send_server_message(&mut ws_tx, &TerminalServerMessage::Exit { code, message }).await;
                                    close_session = true;
                                    break;
                                }
                                Ok(_) => {}
                                Err(err) => {
                                    let _ = send_server_message(&mut ws_tx, &TerminalServerMessage::Error { message: err.to_string() }).await;
                                    close_session = true;
                                    break;
                                }
                            }
                        }
                        if close_session {
                            break 'session;
                        }
                    }
                    Err(err) => {
                        let _ = send_server_message(&mut ws_tx, &TerminalServerMessage::Error { message: err.to_string() }).await;
                        break;
                    }
                }
            }
            client_msg = ws_rx.next() => {
                let Some(Ok(client_msg)) = client_msg else { break; };
                idle.as_mut().reset(Instant::now() + idle_timeout_duration());
                match client_msg {
                    Message::Text(text) => {
                        match serde_json::from_str::<TerminalClientMessage>(&text) {
                            Ok(TerminalClientMessage::Input { data }) => {
                                if let Err(err) = ctx.service.send_input(&ctx.target, pid, data.as_bytes()).await {
                                    let _ = send_server_message(&mut ws_tx, &TerminalServerMessage::Error { message: err.to_string() }).await;
                                }
                            }
                            Ok(TerminalClientMessage::Resize { rows, cols }) => {
                                if let Err(err) = ctx.service.resize(&ctx.target, pid, rows, cols).await {
                                    ctx.logger
                                        .log(
                                            LogEvent::new(LogLevel::Warn, "terminal.resize.failed")
                                                .field("actor", &ctx.actor)
                                                .field("sandbox_id", &sandbox_id)
                                                .field("session_id", &ctx.session_id)
                                                .field("container_id", ctx.container_id.as_deref().unwrap_or("default"))
                                                .field("rows", rows.to_string())
                                                .field("cols", cols.to_string())
                                                .field("error", err.to_string()),
                                        )
                                        .await;
                                }
                            }
                            Ok(TerminalClientMessage::Close) => break,
                            Err(err) => {
                                let _ = send_server_message(&mut ws_tx, &TerminalServerMessage::Error { message: format!("invalid terminal message: {}", err) }).await;
                            }
                        }
                    }
                    Message::Close(_) => break,
                    Message::Ping(bytes) => {
                        let _ = ws_tx.send(Message::Pong(bytes)).await;
                    }
                    _ => {}
                }
            }
        }
    }

    let _ = ctx.service.kill(&ctx.target, pid).await;
    ctx.logger
        .log(terminal_audit_event(
            "terminal.session.closed",
            &ctx.actor,
            &sandbox_id,
            &ctx.session_id,
            ctx.container_id.as_deref(),
        ))
        .await;
}

fn terminal_size(query: &TerminalQuery) -> (u16, u16) {
    (
        query.rows.unwrap_or(DEFAULT_ROWS).max(1),
        query.cols.unwrap_or(DEFAULT_COLS).max(1),
    )
}

fn ensure_terminal_ready(state: SandboxState) -> AppResult<()> {
    if state == SandboxState::Running {
        return Ok(());
    }
    Err(AppError::Conflict(
        "terminal is available only while the sandbox is running".to_string(),
    ))
}

fn new_session_id() -> String {
    Uuid::new_v4().to_string()
}

fn idle_timeout_duration() -> Duration {
    Duration::from_secs(IDLE_TIMEOUT_SECS)
}

fn terminal_audit_event(
    event: &'static str,
    actor: &str,
    sandbox_id: &str,
    session_id: &str,
    container_id: Option<&str>,
) -> LogEvent {
    LogEvent::new(LogLevel::Info, event)
        .field("actor", actor)
        .field("sandbox_id", sandbox_id)
        .field("session_id", session_id)
        .field("container_id", container_id.unwrap_or("default"))
}

async fn send_one_error(mut socket: WebSocket, message: String) {
    let _ = socket
        .send(Message::Text(
            serde_json::to_string(&TerminalServerMessage::Error { message }).unwrap_or_else(|_| {
                "{\"type\":\"error\",\"message\":\"terminal error\"}".to_string()
            }),
        ))
        .await;
    let _ = socket.close().await;
}

async fn send_server_message(
    ws_tx: &mut futures::stream::SplitSink<WebSocket, Message>,
    message: &TerminalServerMessage,
) -> Result<(), axum::Error> {
    ws_tx
        .send(Message::Text(
            serde_json::to_string(message).expect("terminal message serializes"),
        ))
        .await
}

async fn authorize_terminal(
    state: &AppState,
    headers: &HeaderMap,
    query: &TerminalQuery,
    sandbox_id: &str,
) -> AppResult<String> {
    if let Some(store) = &state.agenthub_store {
        let token = headers
            .get(SESSION_HEADER)
            .and_then(|v| v.to_str().ok())
            .map(str::to_string)
            .or_else(|| query.session_token.clone())
            .filter(|v| !v.trim().is_empty())
            .ok_or_else(|| {
                AppError::Unauthorized("terminal login requires a valid session".to_string())
            })?;
        let username = store
            .validate_session(&token)
            .await
            .map_err(|e| AppError::Internal(anyhow::anyhow!("failed to validate session: {}", e)))?
            .ok_or_else(|| {
                AppError::Unauthorized("terminal session is invalid or expired".to_string())
            })?;
        return Ok(username);
    }

    if let Some(callback_url) = state
        .config
        .auth_callback_url
        .as_deref()
        .filter(|v| !v.is_empty())
    {
        let mut req = state
            .http_client
            .post(callback_url)
            .header(
                "X-Request-Path",
                format!("/sandboxes/{}/terminal", sandbox_id),
            )
            .header("X-Request-Method", "GET");
        if let Some(api_key) = query.api_key.as_deref().filter(|v| !v.is_empty()) {
            req = req.header("X-API-Key", api_key);
        } else if let Some(bearer) = query.bearer.as_deref().filter(|v| !v.is_empty()) {
            req = req.header("Authorization", format!("Bearer {}", bearer));
        } else {
            return Err(AppError::Unauthorized(
                "terminal login requires an API key or bearer token".to_string(),
            ));
        }
        let resp = req
            .send()
            .await
            .map_err(|e| AppError::Internal(anyhow::anyhow!("auth callback unreachable: {}", e)))?;
        if resp.status() != StatusCode::OK {
            return Err(AppError::Unauthorized(
                "terminal authorization rejected by callback".to_string(),
            ));
        }
        return Ok("api-user".to_string());
    }

    Ok("anonymous".to_string())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        config::ServerConfig,
        logging::{arc, noop::NoopLogger},
    };
    use axum::{
        extract::State,
        http::{HeaderMap, StatusCode},
        response::IntoResponse,
        routing::post,
        Router,
    };
    use std::sync::Arc;
    use tokio::sync::Mutex;

    #[derive(Clone, Default)]
    struct HeaderCapture {
        headers: Arc<Mutex<Option<HeaderMap>>>,
    }

    async fn spawn_server(app: Router) -> String {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .expect("listener should bind");
        let addr = listener.local_addr().expect("listener addr");
        tokio::spawn(async move {
            axum::serve(listener, app).await.expect("server should run");
        });
        format!("http://{}", addr)
    }

    fn terminal_query() -> TerminalQuery {
        TerminalQuery {
            rows: None,
            cols: None,
            container: None,
            session_token: None,
            api_key: None,
            bearer: None,
        }
    }

    async fn app_state(config: ServerConfig) -> AppState {
        AppState::new(config, arc(NoopLogger)).await
    }

    #[tokio::test]
    async fn authorize_terminal_rejects_missing_callback_credentials() {
        let mut config = ServerConfig::default();
        config.database_url = None;
        config.auth_callback_url = Some("http://127.0.0.1:9/auth".to_string());
        let state = app_state(config).await;

        let err = authorize_terminal(&state, &HeaderMap::new(), &terminal_query(), "sandbox-1")
            .await
            .expect_err("missing credentials should be rejected");

        assert!(matches!(err, AppError::Unauthorized(_)));
    }

    #[tokio::test]
    async fn authorize_terminal_uses_query_api_key_for_callback_auth() {
        async fn auth_handler(
            State(capture): State<HeaderCapture>,
            headers: HeaderMap,
        ) -> impl IntoResponse {
            *capture.headers.lock().await = Some(headers);
            StatusCode::OK
        }

        let capture = HeaderCapture::default();
        let auth_url = spawn_server(
            Router::new()
                .route("/auth", post(auth_handler))
                .with_state(capture.clone()),
        )
        .await;

        let mut config = ServerConfig::default();
        config.database_url = None;
        config.auth_callback_url = Some(format!("{}/auth", auth_url));
        let state = app_state(config).await;
        let mut query = terminal_query();
        query.api_key = Some("key-1".to_string());

        let actor = authorize_terminal(&state, &HeaderMap::new(), &query, "sandbox-1")
            .await
            .expect("callback auth should pass");

        assert_eq!(actor, "api-user");
        let headers = capture
            .headers
            .lock()
            .await
            .clone()
            .expect("auth callback should be called");
        assert_eq!(headers["x-api-key"], "key-1");
        assert_eq!(headers["x-request-path"], "/sandboxes/sandbox-1/terminal");
        assert_eq!(headers["x-request-method"], "GET");
    }

    #[test]
    fn ensure_terminal_ready_rejects_non_running_sandbox_state() {
        assert!(ensure_terminal_ready(SandboxState::Running).is_ok());
        let err = ensure_terminal_ready(SandboxState::Paused)
            .expect_err("paused sandbox should be rejected");
        assert!(matches!(err, AppError::Conflict(message) if message.contains("running")));
    }

    #[test]
    fn terminal_size_defaults_and_clamps_zero_dimensions() {
        let mut query = terminal_query();
        assert_eq!(terminal_size(&query), (DEFAULT_ROWS, DEFAULT_COLS));

        query.rows = Some(0);
        query.cols = Some(132);
        assert_eq!(terminal_size(&query), (1, 132));
    }

    #[test]
    fn idle_timeout_policy_is_thirty_minutes() {
        assert_eq!(idle_timeout_duration(), Duration::from_secs(30 * 60));
    }

    #[test]
    fn audit_events_include_actor_target_session_and_container() {
        let opened = terminal_audit_event(
            "terminal.session.opened",
            "alice",
            "sb-1",
            "session-1",
            Some("container-1"),
        );
        assert_eq!(opened.event, "terminal.session.opened");
        assert_eq!(opened.fields["actor"], serde_json::json!("alice"));
        assert_eq!(opened.fields["sandbox_id"], serde_json::json!("sb-1"));
        assert_eq!(opened.fields["session_id"], serde_json::json!("session-1"));
        assert_eq!(
            opened.fields["container_id"],
            serde_json::json!("container-1")
        );

        let closed = terminal_audit_event(
            "terminal.session.closed",
            "alice",
            "sb-1",
            "session-1",
            None,
        );
        assert_eq!(closed.fields["container_id"], serde_json::json!("default"));
    }

    #[test]
    fn new_terminal_sessions_get_independent_session_ids() {
        let first = new_session_id();
        let second = new_session_id();
        assert_ne!(first, second);
        assert!(Uuid::parse_str(&first).is_ok());
        assert!(Uuid::parse_str(&second).is_ok());
    }
}
