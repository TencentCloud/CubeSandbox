// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use std::{sync::Arc, time::Duration};

use axum::{
    extract::{
        ws::{Message, WebSocket, WebSocketUpgrade},
        Path, Query, State,
    },
    http::HeaderMap,
    response::{IntoResponse, Response},
    Json,
};
use chrono::{DateTime, Utc};
use dashmap::DashMap;
use futures::{SinkExt, StreamExt};
use serde::{Deserialize, Serialize};
use tokio::time::{sleep, Instant};
use tokio_tungstenite::{connect_async, tungstenite::Message as MasterMessage};
use url::Url;
use utoipa::ToSchema;
use uuid::Uuid;

use crate::{
    error::{AppError, AppResult},
    handlers::auth::session_token,
    logging::{LogEvent, LogLevel},
    models::SandboxState,
    state::AppState,
};

const TERMINAL_SESSION_TTL_SECS: i64 = 60;

#[derive(Debug, Clone)]
struct TerminalSession {
    sandbox_id: String,
    container_id: String,
    operator: String,
    cols: u32,
    rows: u32,
    expires_at: Instant,
}

#[derive(Clone, Default)]
pub struct TerminalSessionStore {
    sessions: Arc<DashMap<String, TerminalSession>>,
}

impl TerminalSessionStore {
    fn insert(&self, session_id: String, session: TerminalSession) {
        let now = Instant::now();
        self.sessions
            .retain(|_, existing| existing.expires_at > now);
        self.sessions.insert(session_id, session);
    }

    fn take(&self, session_id: &str) -> Option<TerminalSession> {
        self.sessions.remove(session_id).map(|(_, session)| session)
    }

    fn take_for_sandbox(
        &self,
        session_id: &str,
        sandbox_id: &str,
    ) -> Result<TerminalSession, TakeTerminalSessionError> {
        let session = self
            .take(session_id)
            .ok_or(TakeTerminalSessionError::Missing)?;
        if session.expires_at <= Instant::now() {
            return Err(TakeTerminalSessionError::Expired(session));
        }
        if session.sandbox_id != sandbox_id {
            return Err(TakeTerminalSessionError::WrongSandbox(session));
        }
        Ok(session)
    }
}

enum TakeTerminalSessionError {
    Missing,
    Expired(TerminalSession),
    WrongSandbox(TerminalSession),
}

#[derive(Debug, Deserialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct CreateTerminalSessionRequest {
    #[serde(default, rename = "containerID")]
    pub container_id: Option<String>,
    #[serde(default)]
    pub cols: Option<u32>,
    #[serde(default)]
    pub rows: Option<u32>,
}

#[derive(Debug, Serialize, ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct CreateTerminalSessionResponse {
    #[serde(rename = "sessionID")]
    pub session_id: String,
    #[serde(rename = "websocketURL")]
    pub websocket_url: String,
    #[serde(rename = "containerID")]
    pub container_id: String,
    pub expires_at: DateTime<Utc>,
    pub idle_timeout_seconds: u64,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct TerminalWebSocketQuery {
    #[serde(rename = "sessionID")]
    pub session_id: String,
}

#[utoipa::path(
    post,
    path = "/sandboxes/{sandboxID}/terminal/sessions",
    request_body = CreateTerminalSessionRequest,
    params(("sandboxID" = String, Path, description = "Sandbox identifier")),
    responses(
        (status = 200, description = "One-time terminal session", body = CreateTerminalSessionResponse),
        (status = 400, description = "Container is not part of the sandbox"),
        (status = 404, description = "Sandbox not found"),
        (status = 409, description = "Sandbox is not running")
    ),
    tag = "sandboxes"
)]
pub async fn create_terminal_session(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    headers: HeaderMap,
    Json(body): Json<CreateTerminalSessionRequest>,
) -> AppResult<impl IntoResponse> {
    let operator = terminal_operator(&state, &headers).await;
    let detail = match state.services.sandboxes.get_sandbox(&sandbox_id).await {
        Ok(detail) => detail,
        Err(error) => {
            audit(
                &state,
                LogLevel::Warn,
                "terminal.session.rejected",
                &sandbox_id,
                body.container_id.as_deref().unwrap_or(""),
                "",
                &error.to_string(),
                Some(&operator),
            )
            .await;
            return Err(error);
        }
    };
    if detail.state != SandboxState::Running {
        audit(
            &state,
            LogLevel::Warn,
            "terminal.session.rejected",
            &sandbox_id,
            body.container_id.as_deref().unwrap_or(""),
            "",
            "sandbox is not running",
            Some(&operator),
        )
        .await;
        return Err(AppError::Conflict(format!(
            "sandbox {} must be running to open a terminal",
            sandbox_id
        )));
    }

    let containers = detail.containers.unwrap_or_default();
    let selected = match body.container_id.as_deref().map(str::trim) {
        Some(container_id) if !container_id.is_empty() => containers
            .iter()
            .find(|container| container.container_id == container_id),
        _ => containers
            .iter()
            .find(|container| {
                container.kind.as_deref() == Some("sandbox") || container.container_id == sandbox_id
            })
            .or_else(|| containers.first()),
    };
    let selected = match selected {
        Some(container) => container,
        None => {
            let reason = body
                .container_id
                .as_deref()
                .filter(|value| !value.trim().is_empty())
                .map(|container_id| {
                    format!(
                        "container {} does not belong to sandbox {}",
                        container_id, sandbox_id
                    )
                })
                .unwrap_or_else(|| format!("sandbox {} has no terminal container", sandbox_id));
            audit(
                &state,
                LogLevel::Warn,
                "terminal.session.rejected",
                &sandbox_id,
                body.container_id.as_deref().unwrap_or(""),
                "",
                &reason,
                Some(&operator),
            )
            .await;
            return Err(AppError::BadRequest(reason));
        }
    };

    let cols = body.cols.unwrap_or(80).clamp(1, 1000);
    let rows = body.rows.unwrap_or(24).clamp(1, 1000);
    let session_id = Uuid::new_v4().to_string();
    let expires_at = Utc::now() + chrono::Duration::seconds(TERMINAL_SESSION_TTL_SECS);
    state.terminal_sessions.insert(
        session_id.clone(),
        TerminalSession {
            sandbox_id: sandbox_id.clone(),
            container_id: selected.container_id.clone(),
            operator: operator.clone(),
            cols,
            rows,
            expires_at: Instant::now() + Duration::from_secs(TERMINAL_SESSION_TTL_SECS as u64),
        },
    );
    audit(
        &state,
        LogLevel::Info,
        "terminal.session.created",
        &sandbox_id,
        &selected.container_id,
        &session_id,
        "created",
        Some(&operator),
    )
    .await;

    Ok(Json(CreateTerminalSessionResponse {
        session_id: session_id.clone(),
        websocket_url: format!(
            "/cubeapi/v1/sandboxes/{}/terminal/ws?sessionID={}",
            sandbox_id, session_id
        ),
        container_id: selected.container_id.clone(),
        expires_at,
        idle_timeout_seconds: state.config.terminal_idle_timeout_secs,
    }))
}

pub async fn terminal_websocket(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    Query(query): Query<TerminalWebSocketQuery>,
    ws: WebSocketUpgrade,
) -> AppResult<Response> {
    let session = match state
        .terminal_sessions
        .take_for_sandbox(&query.session_id, &sandbox_id)
    {
        Ok(session) => session,
        Err(TakeTerminalSessionError::Missing) => {
            audit(
                &state,
                LogLevel::Warn,
                "terminal.websocket.rejected",
                &sandbox_id,
                "",
                &query.session_id,
                "invalid, expired, or already used session",
                None,
            )
            .await;
            return Err(AppError::Unauthorized(
                "invalid or already used terminal session".into(),
            ));
        }
        Err(TakeTerminalSessionError::Expired(session)) => {
            audit(
                &state,
                LogLevel::Warn,
                "terminal.websocket.rejected",
                &session.sandbox_id,
                &session.container_id,
                &query.session_id,
                "session expired",
                Some(&session.operator),
            )
            .await;
            return Err(AppError::Unauthorized("terminal session expired".into()));
        }
        Err(TakeTerminalSessionError::WrongSandbox(session)) => {
            audit(
                &state,
                LogLevel::Warn,
                "terminal.websocket.rejected",
                &session.sandbox_id,
                &session.container_id,
                &query.session_id,
                "sandbox mismatch",
                Some(&session.operator),
            )
            .await;
            return Err(AppError::BadRequest(
                "terminal session does not belong to this sandbox".into(),
            ));
        }
    };

    let state_for_upgrade = state.clone();
    let session_id = query.session_id;
    Ok(ws
        .on_upgrade(move |socket| async move {
            bridge_terminal(state_for_upgrade, socket, session, session_id).await;
        })
        .into_response())
}

async fn bridge_terminal(
    state: AppState,
    browser: WebSocket,
    session: TerminalSession,
    session_id: String,
) {
    let master_url = match master_terminal_url(&state, &session, &session_id) {
        Ok(url) => url,
        Err(error) => {
            let mut browser = browser;
            let _ = browser
                .send(Message::Text(terminal_notice("error", &error.to_string())))
                .await;
            audit(
                &state,
                LogLevel::Error,
                "terminal.websocket.closed",
                &session.sandbox_id,
                &session.container_id,
                &session_id,
                "invalid CubeMaster URL",
                Some(&session.operator),
            )
            .await;
            return;
        }
    };
    let (master, _) = match connect_async(master_url.as_str()).await {
        Ok(connection) => connection,
        Err(error) => {
            let mut browser = browser;
            let _ = browser
                .send(Message::Text(terminal_notice(
                    "error",
                    &format!("failed to connect terminal backend: {}", error),
                )))
                .await;
            audit(
                &state,
                LogLevel::Error,
                "terminal.websocket.closed",
                &session.sandbox_id,
                &session.container_id,
                &session_id,
                "CubeMaster connection failed",
                Some(&session.operator),
            )
            .await;
            return;
        }
    };

    audit(
        &state,
        LogLevel::Info,
        "terminal.websocket.connected",
        &session.sandbox_id,
        &session.container_id,
        &session_id,
        "connected",
        Some(&session.operator),
    )
    .await;

    let (mut browser_tx, mut browser_rx) = browser.split();
    let (mut master_tx, mut master_rx) = master.split();
    let idle_timeout = Duration::from_secs(state.config.terminal_idle_timeout_secs.max(1));
    let idle = sleep(idle_timeout);
    tokio::pin!(idle);
    let close_reason = loop {
        tokio::select! {
            browser_message = browser_rx.next() => {
                idle.as_mut().reset(Instant::now() + idle_timeout);
                match browser_message {
                    Some(Ok(Message::Binary(data))) => {
                        if master_tx.send(MasterMessage::Binary(data)).await.is_err() {
                            break "CubeMaster input closed";
                        }
                    }
                    Some(Ok(Message::Text(text))) => {
                        if master_tx.send(MasterMessage::Text(text.into())).await.is_err() {
                            break "CubeMaster control channel closed";
                        }
                    }
                    Some(Ok(Message::Ping(data))) => {
                        let _ = browser_tx.send(Message::Pong(data)).await;
                    }
                    Some(Ok(Message::Pong(_))) => {}
                    Some(Ok(Message::Close(_))) | None => break "client disconnected",
                    Some(Err(_)) => break "client websocket error",
                }
            }
            master_message = master_rx.next() => {
                idle.as_mut().reset(Instant::now() + idle_timeout);
                match master_message {
                    Some(Ok(MasterMessage::Binary(data))) => {
                        if browser_tx.send(Message::Binary(data)).await.is_err() {
                            break "client output closed";
                        }
                    }
                    Some(Ok(MasterMessage::Text(text))) => {
                        if browser_tx.send(Message::Text(text.to_string())).await.is_err() {
                            break "client control channel closed";
                        }
                    }
                    Some(Ok(MasterMessage::Ping(data))) => {
                        let _ = master_tx.send(MasterMessage::Pong(data)).await;
                    }
                    Some(Ok(MasterMessage::Pong(_))) | Some(Ok(MasterMessage::Frame(_))) => {}
                    Some(Ok(MasterMessage::Close(_))) | None => break "terminal backend disconnected",
                    Some(Err(_)) => break "terminal backend websocket error",
                }
            }
            _ = &mut idle => {
                let _ = browser_tx
                    .send(Message::Text(terminal_notice("close", "terminal closed after idle timeout")))
                    .await;
                break "idle timeout";
            }
        }
    };
    let _ = master_tx.send(MasterMessage::Close(None)).await;
    let _ = browser_tx.send(Message::Close(None)).await;
    audit(
        &state,
        LogLevel::Info,
        "terminal.websocket.closed",
        &session.sandbox_id,
        &session.container_id,
        &session_id,
        close_reason,
        Some(&session.operator),
    )
    .await;
}

fn master_terminal_url(
    state: &AppState,
    session: &TerminalSession,
    session_id: &str,
) -> anyhow::Result<Url> {
    let mut url = Url::parse(&state.config.cubemaster_url)?;
    let websocket_scheme = match url.scheme() {
        "https" => "wss",
        "http" => "ws",
        other => anyhow::bail!("unsupported CubeMaster URL scheme {}", other),
    };
    url.set_scheme(websocket_scheme)
        .map_err(|_| anyhow::anyhow!("failed to set CubeMaster WebSocket scheme"))?;
    url.set_path("/cube/sandbox/terminal");
    url.set_query(None);
    url.query_pairs_mut()
        .append_pair("sandbox_id", &session.sandbox_id)
        .append_pair("container_id", &session.container_id)
        .append_pair("request_id", session_id)
        .append_pair("cols", &session.cols.to_string())
        .append_pair("rows", &session.rows.to_string());
    Ok(url)
}

async fn terminal_operator(state: &AppState, headers: &HeaderMap) -> String {
    if let (Some(store), Some(token)) = (&state.agenthub_store, session_token(headers)) {
        if let Ok(Some(username)) = store.validate_session(&token).await {
            return username;
        }
    }
    if headers.contains_key("authorization") {
        "bearer-auth".to_string()
    } else if headers.contains_key("x-api-key") {
        "api-key-auth".to_string()
    } else {
        "anonymous".to_string()
    }
}

fn terminal_notice(kind: &str, message: &str) -> String {
    serde_json::json!({ "type": kind, "message": message }).to_string()
}

async fn audit(
    state: &AppState,
    level: LogLevel,
    event: &str,
    sandbox_id: &str,
    container_id: &str,
    session_id: &str,
    reason: &str,
    operator: Option<&str>,
) {
    state
        .logger
        .log(
            LogEvent::new(level, event)
                .field("sandbox_id", sandbox_id)
                .field("container_id", container_id)
                .field("session_id", session_id)
                .field("reason", reason)
                .field("operator", operator.unwrap_or("unknown")),
        )
        .await;
}

#[cfg(test)]
mod tests {
    use super::*;

    use crate::{
        config::ServerConfig,
        logging::{arc, noop::NoopLogger},
    };

    #[test]
    fn terminal_session_is_one_time() {
        let store = TerminalSessionStore::default();
        store.insert(
            "session".into(),
            TerminalSession {
                sandbox_id: "sandbox".into(),
                container_id: "container".into(),
                operator: "test".into(),
                cols: 80,
                rows: 24,
                expires_at: Instant::now() + Duration::from_secs(60),
            },
        );
        assert!(store.take("session").is_some());
        assert!(store.take("session").is_none());
    }

    #[test]
    fn terminal_session_rejects_missing_expired_and_wrong_sandbox() {
        let store = TerminalSessionStore::default();
        assert!(matches!(
            store.take_for_sandbox("missing", "sandbox"),
            Err(TakeTerminalSessionError::Missing)
        ));

        store.insert(
            "expired".into(),
            test_session("sandbox", Instant::now() - Duration::from_secs(1)),
        );
        assert!(matches!(
            store.take_for_sandbox("expired", "sandbox"),
            Err(TakeTerminalSessionError::Expired(_))
        ));

        store.insert(
            "wrong".into(),
            test_session("sandbox-a", Instant::now() + Duration::from_secs(60)),
        );
        assert!(matches!(
            store.take_for_sandbox("wrong", "sandbox-b"),
            Err(TakeTerminalSessionError::WrongSandbox(_))
        ));
        assert!(matches!(
            store.take_for_sandbox("wrong", "sandbox-a"),
            Err(TakeTerminalSessionError::Missing)
        ));
    }

    #[test]
    fn inserting_a_session_removes_expired_unused_grants() {
        let store = TerminalSessionStore::default();
        store.insert(
            "expired".into(),
            test_session("sandbox", Instant::now() - Duration::from_secs(1)),
        );
        store.insert(
            "active".into(),
            test_session("sandbox", Instant::now() + Duration::from_secs(60)),
        );
        assert_eq!(store.sessions.len(), 1);
        assert!(store.take("expired").is_none());
        assert!(store.take("active").is_some());
    }

    #[tokio::test]
    async fn idle_timeout_notifies_and_closes_browser_connection() {
        async fn quiet_master(ws: WebSocketUpgrade) -> Response {
            ws.on_upgrade(|mut socket| async move {
                while let Some(message) = socket.next().await {
                    if matches!(message, Ok(Message::Close(_)) | Err(_)) {
                        break;
                    }
                }
            })
        }

        let master_listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let master_address = master_listener.local_addr().unwrap();
        tokio::spawn(async move {
            axum::serve(
                master_listener,
                axum::Router::new()
                    .route("/cube/sandbox/terminal", axum::routing::get(quiet_master)),
            )
            .await
            .unwrap();
        });

        let mut config = ServerConfig::default();
        config.cubemaster_url = format!("http://{}", master_address);
        config.terminal_idle_timeout_secs = 1;
        let state = AppState::new(config, arc(NoopLogger)).await;
        state.terminal_sessions.insert(
            "idle-session".into(),
            test_session("sandbox", Instant::now() + Duration::from_secs(60)),
        );

        let api_listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let api_address = api_listener.local_addr().unwrap();
        tokio::spawn(async move {
            axum::serve(
                api_listener,
                axum::Router::new()
                    .route(
                        "/cubeapi/v1/sandboxes/:sandboxID/terminal/ws",
                        axum::routing::get(terminal_websocket),
                    )
                    .with_state(state),
            )
            .await
            .unwrap();
        });

        let url = format!(
            "ws://{}/cubeapi/v1/sandboxes/sandbox/terminal/ws?sessionID=idle-session",
            api_address
        );
        let (mut browser, _) = tokio_tungstenite::connect_async(url).await.unwrap();
        let notice = tokio::time::timeout(Duration::from_secs(3), browser.next())
            .await
            .expect("idle timeout notice was not sent")
            .expect("browser stream ended before idle notice")
            .expect("browser websocket failed before idle notice");
        let MasterMessage::Text(notice) = notice else {
            panic!("expected text idle notice, got {notice:?}");
        };
        let notice: serde_json::Value = serde_json::from_str(&notice).unwrap();
        assert_eq!(notice["type"], "close");
        assert_eq!(notice["message"], "terminal closed after idle timeout");

        let close = tokio::time::timeout(Duration::from_secs(1), browser.next())
            .await
            .expect("browser connection did not close after idle notice");
        assert!(matches!(close, None | Some(Ok(MasterMessage::Close(_)))));
    }

    fn test_session(sandbox_id: &str, expires_at: Instant) -> TerminalSession {
        TerminalSession {
            sandbox_id: sandbox_id.into(),
            container_id: "container".into(),
            operator: "test".into(),
            cols: 80,
            rows: 24,
            expires_at,
        }
    }
}
