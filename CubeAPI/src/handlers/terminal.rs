// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
//! Authenticated WebSocket bridge for interactive Cubelet terminals.

use std::time::{Duration, Instant};

use axum::{
    extract::{
        ws::{Message as BrowserMessage, WebSocket, WebSocketUpgrade},
        Path, State,
    },
    http::HeaderMap,
    response::{IntoResponse, Response},
    Json,
};
use futures::{SinkExt, StreamExt};
use serde::{Deserialize, Serialize};
use tokio_tungstenite::{
    connect_async,
    tungstenite::{client::IntoClientRequest, http::HeaderValue, Message as NodeMessage},
};
use uuid::Uuid;

use crate::{
    error::{AppError, AppResult},
    handlers::auth::require_webui_session,
    logging::{LogEvent, LogLevel},
    services::sandboxes::TerminalContainer,
    state::AppState,
    terminal_sessions::TerminalTicket,
};

const TERMINAL_PROXY_HEADER: &str = "x-cube-terminal-token";

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct TerminalInfoResponse {
    pub enabled: bool,
    pub reason: Option<String>,
    pub containers: Vec<TerminalContainerResponse>,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct TerminalContainerResponse {
    pub id: String,
    pub name: String,
}

impl From<TerminalContainer> for TerminalContainerResponse {
    fn from(container: TerminalContainer) -> Self {
        Self {
            id: container.id,
            name: container.name,
        }
    }
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CreateTerminalSessionRequest {
    pub container_id: Option<String>,
    pub shell: Option<String>,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct CreateTerminalSessionResponse {
    pub ticket: String,
    pub websocket_path: String,
    pub expires_in_secs: u64,
}

pub async fn terminal_info(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
) -> AppResult<impl IntoResponse> {
    if state.config.terminal_proxy_token.is_none() {
        return Ok(Json(TerminalInfoResponse {
            enabled: false,
            reason: Some("terminal service is not configured".to_string()),
            containers: Vec::new(),
        }));
    }
    let (_, containers) = state
        .services
        .sandboxes
        .terminal_target(&sandbox_id, None)
        .await?;
    Ok(Json(TerminalInfoResponse {
        enabled: true,
        reason: None,
        containers: containers.into_iter().map(Into::into).collect(),
    }))
}

pub async fn create_terminal_session(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    headers: HeaderMap,
    Json(body): Json<CreateTerminalSessionRequest>,
) -> AppResult<impl IntoResponse> {
    let proxy_token = state.config.terminal_proxy_token.as_ref().ok_or_else(|| {
        AppError::NotImplemented(
            "terminal service is not configured; set CUBE_TERMINAL_PROXY_TOKEN".to_string(),
        )
    })?;
    if proxy_token.trim().is_empty() {
        return Err(AppError::NotImplemented(
            "terminal service is not configured".to_string(),
        ));
    }
    let username = require_webui_session(&state, &headers).await?;
    let shell = validated_shell(body.shell.as_deref())?;
    let (target, _) = state
        .services
        .sandboxes
        .terminal_target(&sandbox_id, body.container_id.as_deref())
        .await?;

    let ticket = Uuid::new_v4().simple().to_string();
    state.terminal_tickets.insert(
        ticket.clone(),
        TerminalTicket {
            sandbox_id: target.sandbox_id,
            container_id: target.container_id.clone(),
            host_ip: target.host_ip,
            shell,
            username: username.clone(),
            expires_at: Instant::now() + Duration::from_secs(state.config.terminal_ticket_ttl_secs),
        },
    );
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Info, "terminal.session.requested")
                .field("sandbox_id", &sandbox_id)
                .field("container_id", &target.container_id)
                .field(
                    "operator",
                    username.unwrap_or_else(|| "anonymous".to_string()),
                ),
        )
        .await;

    Ok(Json(CreateTerminalSessionResponse {
        websocket_path: format!(
            "/cubeapi/v1/sandboxes/{}/terminal/sessions/{}",
            urlencoding::encode(&sandbox_id),
            ticket
        ),
        ticket,
        expires_in_secs: state.config.terminal_ticket_ttl_secs,
    }))
}

pub async fn connect_terminal(
    State(state): State<AppState>,
    Path((sandbox_id, ticket)): Path<(String, String)>,
    websocket: WebSocketUpgrade,
) -> AppResult<Response> {
    let session = state.terminal_tickets.take(&ticket).ok_or_else(|| {
        AppError::Unauthorized("terminal ticket is invalid, expired, or already used".to_string())
    })?;
    if session.sandbox_id != sandbox_id {
        return Err(AppError::Unauthorized(
            "terminal ticket does not match sandbox".to_string(),
        ));
    }
    let proxy_token = state.config.terminal_proxy_token.clone().ok_or_else(|| {
        AppError::NotImplemented("terminal service is not configured".to_string())
    })?;
    let port = state.config.terminal_port;
    let logger = state.logger.clone();

    Ok(websocket
        .on_upgrade(move |socket| async move {
            let outcome = relay_terminal(socket, &session, port, &proxy_token).await;
            let level = if outcome.is_ok() {
                LogLevel::Info
            } else {
                LogLevel::Warn
            };
            logger
                .log(
                    LogEvent::new(level, "terminal.session.closed")
                        .field("sandbox_id", &session.sandbox_id)
                        .field("container_id", &session.container_id)
                        .field(
                            "operator",
                            session.username.unwrap_or_else(|| "anonymous".to_string()),
                        )
                        .field(
                            "outcome",
                            outcome
                                .as_ref()
                                .err()
                                .map(ToString::to_string)
                                .unwrap_or_else(|| "closed".to_string()),
                        ),
                )
                .await;
        })
        .into_response())
}

async fn relay_terminal(
    browser: WebSocket,
    session: &TerminalTicket,
    terminal_port: u16,
    proxy_token: &str,
) -> Result<(), String> {
    let url = format!(
        "ws://{}:{}/v1/terminal?sandbox_id={}&container_id={}&shell={}",
        session.host_ip,
        terminal_port,
        urlencoding::encode(&session.sandbox_id),
        urlencoding::encode(&session.container_id),
        urlencoding::encode(&session.shell),
    );
    let mut request = url
        .into_client_request()
        .map_err(|error| format!("invalid Cubelet terminal URL: {error}"))?;
    request.headers_mut().insert(
        TERMINAL_PROXY_HEADER,
        HeaderValue::from_str(proxy_token)
            .map_err(|_| "invalid terminal proxy token".to_string())?,
    );
    let (node, _) = connect_async(request)
        .await
        .map_err(|error| format!("failed connecting to Cubelet terminal: {error}"))?;
    let (mut browser_tx, mut browser_rx) = browser.split();
    let (mut node_tx, mut node_rx) = node.split();

    let browser_to_node = async {
        while let Some(message) = browser_rx.next().await {
            let message = message.map_err(|error| error.to_string())?;
            match message {
                BrowserMessage::Text(text) => node_tx
                    .send(NodeMessage::Text(text.to_string().into()))
                    .await
                    .map_err(|error| error.to_string())?,
                BrowserMessage::Binary(bytes) => node_tx
                    .send(NodeMessage::Binary(bytes.to_vec().into()))
                    .await
                    .map_err(|error| error.to_string())?,
                BrowserMessage::Ping(bytes) => node_tx
                    .send(NodeMessage::Ping(bytes.to_vec().into()))
                    .await
                    .map_err(|error| error.to_string())?,
                BrowserMessage::Pong(bytes) => node_tx
                    .send(NodeMessage::Pong(bytes.to_vec().into()))
                    .await
                    .map_err(|error| error.to_string())?,
                BrowserMessage::Close(_) => {
                    let _ = node_tx.send(NodeMessage::Close(None)).await;
                    break;
                }
            }
        }
        Ok::<(), String>(())
    };
    let node_to_browser = async {
        while let Some(message) = node_rx.next().await {
            let message = message.map_err(|error| error.to_string())?;
            match message {
                NodeMessage::Text(text) => browser_tx
                    .send(BrowserMessage::Text(text.to_string().into()))
                    .await
                    .map_err(|error| error.to_string())?,
                NodeMessage::Binary(bytes) => browser_tx
                    .send(BrowserMessage::Binary(bytes.to_vec().into()))
                    .await
                    .map_err(|error| error.to_string())?,
                NodeMessage::Ping(bytes) => browser_tx
                    .send(BrowserMessage::Ping(bytes.to_vec().into()))
                    .await
                    .map_err(|error| error.to_string())?,
                NodeMessage::Pong(bytes) => browser_tx
                    .send(BrowserMessage::Pong(bytes.to_vec().into()))
                    .await
                    .map_err(|error| error.to_string())?,
                NodeMessage::Close(_) => {
                    let _ = browser_tx.send(BrowserMessage::Close(None)).await;
                    break;
                }
                NodeMessage::Frame(_) => {}
            }
        }
        Ok::<(), String>(())
    };
    tokio::select! {
        result = browser_to_node => result,
        result = node_to_browser => result,
    }
}

fn validated_shell(value: Option<&str>) -> AppResult<String> {
    let shell = value.unwrap_or("/bin/sh").trim();
    match shell {
        "/bin/sh" | "/bin/bash" => Ok(shell.to_string()),
        _ => Err(AppError::BadRequest(
            "shell must be /bin/sh or /bin/bash".to_string(),
        )),
    }
}

#[cfg(test)]
mod tests {
    use super::validated_shell;

    #[test]
    fn only_allows_expected_shells() {
        assert_eq!(validated_shell(None).unwrap(), "/bin/sh");
        assert_eq!(validated_shell(Some("/bin/bash")).unwrap(), "/bin/bash");
        assert!(validated_shell(Some("/bin/zsh")).is_err());
        assert!(validated_shell(Some("/bin/sh -c whoami")).is_err());
    }
}
