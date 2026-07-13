// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use axum::{
    extract::{
        ws::{Message, WebSocket, WebSocketUpgrade},
        Path, Query, State,
    },
    http::{header::SEC_WEBSOCKET_PROTOCOL, HeaderMap, StatusCode},
    response::IntoResponse,
    Json,
};
use futures::{SinkExt, StreamExt};
use serde::Deserialize;
use tokio_tungstenite::{
    connect_async,
    tungstenite::{client::IntoClientRequest, protocol::Message as TungsteniteMessage},
};
use validator::Validate;

use crate::{
    error::{AppError, AppResult},
    logging::{LogEvent, LogLevel},
    models::{
        ApiError, ConnectSandbox, ListSandboxesQuery, ListSandboxesV2Query, NewSandbox,
        RefreshRequest, ResumedSandbox, Sandbox, SandboxDetail, SandboxLogsQuery,
        SandboxLogsV2Query, SandboxLogsV2Response, SetTimeoutRequest,
    },
    state::AppState,
};

// ─── GET /sandboxes ───────────────────────────────────────────────────────────

pub async fn list_sandboxes(
    State(state): State<AppState>,
    Query(params): Query<ListSandboxesQuery>,
) -> AppResult<impl IntoResponse> {
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "list_sandboxes")
                .field("metadata_filter", params.metadata.as_deref().unwrap_or("")),
        )
        .await;

    match state
        .services
        .sandboxes
        .list(params.metadata.as_deref(), None, 200)
        .await
    {
        Ok(list) => {
            state
                .logger
                .log(
                    LogEvent::new(LogLevel::Info, "api.response")
                        .field("handler", "list_sandboxes")
                        .field_value("count", list.len()),
                )
                .await;
            Ok(Json(list))
        }
        Err(error) => {
            let message = error.to_string();
            tracing::error!(error = %message, "list_sandboxes: service error");
            state
                .logger
                .log(
                    LogEvent::new(LogLevel::Error, "api.error")
                        .field("handler", "list_sandboxes")
                        .field("error", &message),
                )
                .await;
            Err(error)
        }
    }
}

// ─── GET /v2/sandboxes ────────────────────────────────────────────────────────

#[utoipa::path(
    get,
    path = "/v2/sandboxes",
    params(ListSandboxesV2Query),
    responses(
        (status = 200, description = "Sandbox list", body = [crate::models::ListedSandbox]),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
pub async fn list_sandboxes_v2(
    State(state): State<AppState>,
    Query(params): Query<ListSandboxesV2Query>,
) -> AppResult<impl IntoResponse> {
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "list_sandboxes_v2")
                .field("state_filter", params.state.as_deref().unwrap_or(""))
                .field_value("limit", params.limit),
        )
        .await;

    let list = state
        .services
        .sandboxes
        .list(
            params.metadata.as_deref(),
            params.state.as_deref(),
            params.limit,
        )
        .await?;

    state
        .logger
        .log(
            LogEvent::new(LogLevel::Info, "api.response")
                .field("handler", "list_sandboxes_v2")
                .field_value("count", list.len()),
        )
        .await;
    Ok(Json(list))
}

// ─── GET /sandboxes/:sandboxID ────────────────────────────────────────────────

#[utoipa::path(
    get,
    path = "/sandboxes/{sandboxID}",
    params(
        ("sandboxID" = String, Path, description = "Sandbox identifier")
    ),
    responses(
        (status = 200, description = "Sandbox detail", body = SandboxDetail),
        (status = 404, description = "Sandbox not found", body = ApiError),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
pub async fn get_sandbox(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
) -> AppResult<impl IntoResponse> {
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "get_sandbox")
                .field("sandbox_id", &sandbox_id),
        )
        .await;

    let detail = state.services.sandboxes.get_sandbox(&sandbox_id).await?;
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Info, "api.response")
                .field("handler", "get_sandbox")
                .field("sandbox_id", &sandbox_id),
        )
        .await;
    Ok(Json(detail))
}

// ─── GET /sandboxes/:sandboxID/terminal/ws ────────────────────────────────

const TERMINAL_MAX_FRAME_SIZE: usize = 64 * 1024;

/// Upgrades an authenticated browser terminal session and proxies it to the
/// private CubeMaster terminal endpoint. The browser never receives the
/// CubeMaster address or the gateway secret.
pub async fn sandbox_terminal(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    headers: HeaderMap,
    ws: WebSocketUpgrade,
) -> AppResult<impl IntoResponse> {
    let operator = terminal_operator(&state, &headers).await?;
    let gateway_token = state.config.terminal_gateway_token.clone().ok_or_else(|| {
        crate::error::AppError::ServiceUnavailable("terminal gateway is not configured".to_string())
    })?;
    if gateway_token.trim().is_empty() {
        return Err(crate::error::AppError::ServiceUnavailable(
            "terminal gateway is not configured".to_string(),
        ));
    }
    Ok(ws
        .protocols(["cube-terminal"])
        .max_message_size(TERMINAL_MAX_FRAME_SIZE)
        .max_frame_size(TERMINAL_MAX_FRAME_SIZE)
        .on_upgrade(move |socket| async move {
            proxy_terminal(socket, state, sandbox_id, gateway_token, operator).await;
        }))
}

#[derive(Debug, Deserialize, PartialEq)]
#[serde(rename_all = "camelCase")]
struct TerminalOpenFrame {
    #[serde(rename = "type")]
    frame_type: String,
    sandbox_id: String,
    container_id: String,
    #[serde(default)]
    cols: u32,
    #[serde(default)]
    rows: u32,
}

fn terminal_open_target(
    message: &Message,
    expected_sandbox_id: &str,
) -> Result<TerminalOpenFrame, &'static str> {
    let Message::Text(payload) = message else {
        return Err("first terminal frame must be JSON text");
    };
    let frame: TerminalOpenFrame =
        serde_json::from_str(payload).map_err(|_| "invalid terminal open frame")?;
    if frame.frame_type != "open" || frame.container_id.is_empty() {
        return Err("first terminal frame must open a container");
    }
    if frame.sandbox_id != expected_sandbox_id {
        return Err("terminal sandbox does not match request path");
    }
    Ok(frame)
}

fn sanitized_terminal_open_message(frame: &TerminalOpenFrame) -> TungsteniteMessage {
    TungsteniteMessage::Text(
        serde_json::json!({
            "type": "open",
            "sandboxId": frame.sandbox_id,
            "containerId": frame.container_id,
            "cols": frame.cols,
            "rows": frame.rows,
        })
        .to_string(),
    )
}

fn terminal_error(message: &str) -> Message {
    Message::Text(
        serde_json::json!({"type": "error", "message": message})
            .to_string()
            .into(),
    )
}

async fn terminal_operator(state: &AppState, headers: &HeaderMap) -> AppResult<String> {
    let Some(store) = &state.agenthub_store else {
        return Err(crate::error::AppError::ServiceUnavailable(
            "terminal requires WebUI session authentication".to_string(),
        ));
    };
    let protocol = headers
        .get(SEC_WEBSOCKET_PROTOCOL)
        .and_then(|value| value.to_str().ok())
        .unwrap_or("");
    let token = protocol
        .split(',')
        .map(str::trim)
        .find(|value| *value != "cube-terminal")
        .filter(|value| !value.is_empty());
    let Some(token) = token else {
        return Err(crate::error::AppError::Unauthorized(
            "terminal session token is required".to_string(),
        ));
    };
    store
        .validate_session(token)
        .await
        .map_err(|error| {
            crate::error::AppError::Internal(anyhow::anyhow!(
                "failed to validate terminal session: {error}"
            ))
        })?
        .ok_or_else(|| {
            crate::error::AppError::Unauthorized(
                "terminal session is invalid or expired".to_string(),
            )
        })
}

async fn proxy_terminal(
    browser: WebSocket,
    state: AppState,
    sandbox_id: String,
    gateway_token: String,
    operator: String,
) {
    let master_url = state
        .config
        .cubemaster_url
        .replace("https://", "wss://")
        .replace("http://", "ws://");
    let url = format!("{master_url}/cube/sandbox/terminal/ws");
    let mut request = match url.into_client_request() {
        Ok(request) => request,
        Err(error) => {
            tracing::error!(%error, sandbox_id, "terminal: invalid CubeMaster websocket URL");
            return;
        }
    };
    match gateway_token.parse() {
        Ok(value) => {
            request
                .headers_mut()
                .insert("x-cube-terminal-gateway", value);
        }
        Err(_) => {
            tracing::error!(sandbox_id, "terminal: invalid terminal gateway token");
            return;
        }
    }
    let (master, _) = match connect_async(request).await {
        Ok(connection) => connection,
        Err(error) => {
            tracing::warn!(%error, sandbox_id, operator, "terminal: CubeMaster connection failed");
            return;
        }
    };
    let (mut browser_tx, mut browser_rx) = browser.split();
    let (mut master_tx, mut master_rx) = master.split();
    let mut container_id = None;
    loop {
        tokio::select! {
            incoming = browser_rx.next() => match incoming {
                Some(Ok(message)) => {
                    let opening = if container_id.is_none() {
                        match terminal_open_target(&message, &sandbox_id) {
                            Ok(frame) => Some(frame),
                            Err(error) => {
                                let _ = browser_tx.send(terminal_error(error)).await;
                                break;
                            }
                        }
                    } else {
                        None
                    };
                    let outgoing = opening
                        .as_ref()
                        .map(sanitized_terminal_open_message)
                        .or_else(|| to_master_message(message));
                    match outgoing {
                        Some(message) => {
                            if master_tx.send(message).await.is_err() { break; }
                            if let Some(opening) = opening {
                                state.logger.log(
                                    LogEvent::new(LogLevel::Info, "terminal.session.open")
                                        .field("sandbox_id", &sandbox_id)
                                        .field("container_id", &opening.container_id)
                                        .field("operator", &operator),
                                ).await;
                                container_id = Some(opening.container_id);
                            }
                        },
                        None => break,
                    }
                },
                _ => break,
            },
            incoming = master_rx.next() => match incoming {
                Some(Ok(message)) => match to_browser_message(message) {
                    Some(message) => if browser_tx.send(message).await.is_err() { break; },
                    None => break,
                },
                _ => break,
            },
        }
    }
    if let Some(container_id) = container_id {
        state
            .logger
            .log(
                LogEvent::new(LogLevel::Info, "terminal.session.close")
                    .field("sandbox_id", &sandbox_id)
                    .field("container_id", &container_id)
                    .field("operator", &operator),
            )
            .await;
    }
}

fn to_master_message(message: Message) -> Option<TungsteniteMessage> {
    match message {
        Message::Text(value) => Some(TungsteniteMessage::Text(value)),
        Message::Binary(value) => Some(TungsteniteMessage::Binary(value)),
        Message::Ping(value) => Some(TungsteniteMessage::Ping(value)),
        Message::Pong(value) => Some(TungsteniteMessage::Pong(value)),
        Message::Close(_) => None,
    }
}

fn to_browser_message(message: TungsteniteMessage) -> Option<Message> {
    match message {
        TungsteniteMessage::Text(value) => Some(Message::Text(value.into())),
        TungsteniteMessage::Binary(value) => Some(Message::Binary(value.into())),
        TungsteniteMessage::Ping(value) => Some(Message::Ping(value.into())),
        TungsteniteMessage::Pong(value) => Some(Message::Pong(value.into())),
        TungsteniteMessage::Close(_) => None,
        TungsteniteMessage::Frame(_) => None,
    }
}

#[cfg(test)]
mod terminal_tests {
    use super::*;

    #[test]
    fn terminal_open_frame_must_match_request_sandbox() {
        let matching =
            Message::Text(r#"{"type":"open","sandboxId":"sandbox-1","containerId":"main"}"#.into());
        assert_eq!(
            terminal_open_target(&matching, "sandbox-1")
                .unwrap()
                .container_id,
            "main"
        );

        let mismatched =
            Message::Text(r#"{"type":"open","sandboxId":"sandbox-2","containerId":"main"}"#.into());
        assert_eq!(
            terminal_open_target(&mismatched, "sandbox-1"),
            Err("terminal sandbox does not match request path")
        );
    }

    #[test]
    fn terminal_first_frame_must_be_open_json_text() {
        let input = Message::Text(r#"{"type":"input","data":"whoami"}"#.into());
        assert!(terminal_open_target(&input, "sandbox-1").is_err());
        assert!(terminal_open_target(&Message::Binary(vec![1, 2, 3].into()), "sandbox-1").is_err());
    }

    #[test]
    fn terminal_open_frame_forwards_only_allowed_fields() {
        let message = Message::Text(
            r#"{"type":"open","sandboxId":"sandbox-1","containerId":"main","cols":120,"rows":40,"args":["/bin/bash"],"env":["TOKEN=secret"]}"#
                .into(),
        );
        let frame = terminal_open_target(&message, "sandbox-1").unwrap();
        let TungsteniteMessage::Text(sanitized) = sanitized_terminal_open_message(&frame) else {
            panic!("sanitized terminal open frame must be text");
        };
        let payload: serde_json::Value = serde_json::from_str(&sanitized).unwrap();

        assert_eq!(payload["cols"], 120);
        assert_eq!(payload["rows"], 40);
        assert!(payload.get("args").is_none());
        assert!(payload.get("env").is_none());
    }
}

// ─── POST /sandboxes ──────────────────────────────────────────────────────────

pub async fn create_sandbox(
    State(state): State<AppState>,
    Json(body): Json<NewSandbox>,
) -> AppResult<impl IntoResponse> {
    let template_id = body.template_id.clone();
    let timeout = body.timeout;
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "create_sandbox")
                .field("template_id", &template_id)
                .field_value("timeout", timeout),
        )
        .await;

    let created = state.services.sandboxes.create_sandbox(body).await?;
    let sandbox_id = created.sandbox_id.clone();

    tracing::info!(sandbox_id = %sandbox_id, template_id = %template_id, "create_sandbox: success");
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Info, "sandbox.created")
                .field("sandbox_id", &sandbox_id)
                .field("template_id", &template_id),
        )
        .await;

    Ok((StatusCode::CREATED, Json(created)))
}

// ─── DELETE /sandboxes/:sandboxID ─────────────────────────────────────────────

#[utoipa::path(
    delete,
    path = "/sandboxes/{sandboxID}",
    params(
        ("sandboxID" = String, Path, description = "Sandbox identifier")
    ),
    responses(
        (status = 204, description = "Sandbox deleted"),
        (status = 404, description = "Sandbox not found", body = ApiError),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
pub async fn kill_sandbox(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
) -> AppResult<impl IntoResponse> {
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "kill_sandbox")
                .field("sandbox_id", &sandbox_id),
        )
        .await;

    state.services.sandboxes.kill_sandbox(&sandbox_id).await?;

    tracing::info!(sandbox_id = %sandbox_id, "kill_sandbox: success");
    state
        .logger
        .log(LogEvent::new(LogLevel::Info, "sandbox.deleted").field("sandbox_id", &sandbox_id))
        .await;
    Ok(StatusCode::NO_CONTENT)
}

// ─── POST /sandboxes/:sandboxID/pause ─────────────────────────────────────────

#[utoipa::path(
    post,
    path = "/sandboxes/{sandboxID}/pause",
    params(
        ("sandboxID" = String, Path, description = "Sandbox identifier")
    ),
    responses(
        (status = 204, description = "Sandbox paused"),
        (status = 404, description = "Sandbox not found", body = ApiError),
        (status = 409, description = "Sandbox cannot be paused", body = ApiError),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
pub async fn pause_sandbox(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
) -> AppResult<impl IntoResponse> {
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "pause_sandbox")
                .field("sandbox_id", &sandbox_id),
        )
        .await;
    tracing::info!(sandbox_id = %sandbox_id, "pause sandbox request");
    state.services.sandboxes.pause_sandbox(&sandbox_id).await?;

    tracing::info!(sandbox_id = %sandbox_id, "pause_sandbox: success");
    state
        .logger
        .log(LogEvent::new(LogLevel::Info, "sandbox.paused").field("sandbox_id", &sandbox_id))
        .await;
    Ok(StatusCode::NO_CONTENT)
}

// ─── POST /sandboxes/:sandboxID/resume ────────────────────────────────────────

#[utoipa::path(
    post,
    path = "/sandboxes/{sandboxID}/resume",
    params(
        ("sandboxID" = String, Path, description = "Sandbox identifier")
    ),
    request_body = ResumedSandbox,
    responses(
        (status = 201, description = "Sandbox resumed", body = Sandbox),
        (status = 404, description = "Sandbox not found", body = ApiError),
        (status = 409, description = "Sandbox is already running", body = ApiError),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
pub async fn resume_sandbox(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    Json(body): Json<ResumedSandbox>,
) -> AppResult<impl IntoResponse> {
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "resume_sandbox")
                .field("sandbox_id", &sandbox_id)
                .field_value("timeout", body.timeout),
        )
        .await;
    tracing::info!(sandbox_id = %sandbox_id, "resume sandbox request");
    let sandbox = state
        .services
        .sandboxes
        .resume_sandbox(&sandbox_id, body.timeout)
        .await?;

    tracing::info!(sandbox_id = %sandbox_id, "resume_sandbox: success");
    state
        .logger
        .log(LogEvent::new(LogLevel::Info, "sandbox.resumed").field("sandbox_id", &sandbox_id))
        .await;

    Ok((StatusCode::CREATED, Json(sandbox)))
}

// ─── POST /sandboxes/:sandboxID/connect ───────────────────────────────────────

pub async fn connect_sandbox(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    Json(body): Json<ConnectSandbox>,
) -> AppResult<impl IntoResponse> {
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "connect_sandbox")
                .field("sandbox_id", &sandbox_id)
                .field_value("timeout", body.timeout),
        )
        .await;
    tracing::info!("connect request");
    let sandbox = state
        .services
        .sandboxes
        .connect_sandbox(&sandbox_id, body.timeout)
        .await?;
    Ok((StatusCode::OK, Json(sandbox)))
}

// ─── GET /sandboxes/:sandboxID/logs ───────────────────────────────────────────

pub async fn get_sandbox_logs(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    Query(params): Query<SandboxLogsQuery>,
) -> AppResult<impl IntoResponse> {
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "get_sandbox_logs")
                .field("sandbox_id", &sandbox_id)
                .field_value("limit", params.limit),
        )
        .await;

    let logs = state
        .services
        .sandboxes
        .get_logs(&sandbox_id, params.start, params.limit)
        .await?;
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Info, "api.response")
                .field("handler", "get_sandbox_logs")
                .field("sandbox_id", &sandbox_id)
                .field_value("count", logs.logs.len()),
        )
        .await;
    Ok(Json(logs))
}

// ─── GET /v2/sandboxes/:sandboxID/logs ────────────────────────────────────────

#[utoipa::path(
    get,
    path = "/v2/sandboxes/{sandboxID}/logs",
    params(
        ("sandboxID" = String, Path, description = "Sandbox identifier"),
        SandboxLogsV2Query
    ),
    responses(
        (status = 200, description = "Structured sandbox logs", body = SandboxLogsV2Response),
        (status = 404, description = "Sandbox not found", body = ApiError),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
pub async fn get_sandbox_logs_v2(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    Query(params): Query<SandboxLogsV2Query>,
) -> AppResult<impl IntoResponse> {
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "get_sandbox_logs_v2")
                .field("sandbox_id", &sandbox_id)
                .field_value("limit", params.limit),
        )
        .await;

    let logs = state
        .services
        .sandboxes
        .get_logs_v2(&sandbox_id, params.cursor, params.limit)
        .await?;
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Info, "api.response")
                .field("handler", "get_sandbox_logs_v2")
                .field("sandbox_id", &sandbox_id)
                .field_value("count", logs.logs.len()),
        )
        .await;
    Ok(Json(logs))
}

// ─── POST /sandboxes/:sandboxID/timeout ───────────────────────────────────────

pub async fn set_sandbox_timeout(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    Json(body): Json<SetTimeoutRequest>,
) -> AppResult<impl IntoResponse> {
    body.validate()
        .map_err(|e| AppError::BadRequest(e.to_string()))?;

    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "set_sandbox_timeout")
                .field("sandbox_id", &sandbox_id)
                .field_value("timeout", body.timeout),
        )
        .await;

    state
        .services
        .sandboxes
        .set_timeout(&sandbox_id, body.timeout)
        .await?;

    tracing::info!(sandbox_id = %sandbox_id, timeout = body.timeout, "set_sandbox_timeout: success");
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Info, "sandbox.timeout.updated")
                .field("sandbox_id", &sandbox_id)
                .field_value("timeout", body.timeout),
        )
        .await;
    Ok(StatusCode::NO_CONTENT)
}

// ─── POST /sandboxes/:sandboxID/refreshes ─────────────────────────────────────

pub async fn refresh_sandbox(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    Json(body): Json<RefreshRequest>,
) -> AppResult<impl IntoResponse> {
    let duration = body.duration.unwrap_or(0);
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "refresh_sandbox")
                .field("sandbox_id", &sandbox_id)
                .field_value("duration", duration),
        )
        .await;

    state
        .services
        .sandboxes
        .refresh(&sandbox_id, duration)
        .await?;

    tracing::info!(sandbox_id = %sandbox_id, duration = duration, "refresh_sandbox: success");
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Info, "sandbox.refreshed")
                .field("sandbox_id", &sandbox_id)
                .field_value("duration", duration),
        )
        .await;
    Ok(StatusCode::NO_CONTENT)
}
