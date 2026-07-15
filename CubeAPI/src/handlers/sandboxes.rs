// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use axum::{
    extract::{Path, Query, State},
    http::StatusCode,
    response::IntoResponse,
    Json,
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

fn sandbox_resumed_event(sandbox_id: &str, template_id: &str) -> LogEvent {
    LogEvent::new(LogLevel::Info, "sandbox.resumed")
        .field("sandbox_id", sandbox_id)
        .field("template_id", template_id)
}

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
        .log(sandbox_resumed_event(&sandbox_id, &sandbox.template_id))
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
    let outcome = state
        .services
        .sandboxes
        .connect_sandbox_with_outcome(&sandbox_id, body.timeout)
        .await?;
    if outcome.resume_performed {
        state
            .logger
            .log(sandbox_resumed_event(
                &sandbox_id,
                &outcome.sandbox.template_id,
            ))
            .await;
    }
    Ok((StatusCode::OK, Json(outcome.sandbox)))
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

#[cfg(test)]
mod tests {
    use std::{
        collections::VecDeque,
        sync::{
            atomic::{AtomicUsize, Ordering},
            Arc, Mutex,
        },
    };

    use async_trait::async_trait;
    use axum::{
        extract::State,
        http::StatusCode,
        routing::{get, post},
        Json, Router,
    };
    use axum_test::TestServer;

    use crate::{
        config::ServerConfig,
        cubemaster::CubeMasterClient,
        logging::{arc, LogEvent, LogLevel, Logger},
        routes::build_router,
        services::AppServices,
        state::AppState,
    };

    type MockResponse = (StatusCode, serde_json::Value);

    #[derive(Clone)]
    struct MockCubeMasterState {
        info_responses: Arc<Mutex<VecDeque<MockResponse>>>,
        update_responses: Arc<Mutex<VecDeque<MockResponse>>>,
        update_calls: Arc<AtomicUsize>,
    }

    #[derive(Clone, Default)]
    struct RecordingLogger {
        events: Arc<Mutex<Vec<LogEvent>>>,
    }

    impl RecordingLogger {
        fn events_named(&self, name: &str) -> Vec<LogEvent> {
            self.events
                .lock()
                .unwrap()
                .iter()
                .filter(|event| event.event == name)
                .cloned()
                .collect()
        }
    }

    #[async_trait]
    impl Logger for RecordingLogger {
        async fn log(&self, event: LogEvent) {
            self.events.lock().unwrap().push(event);
        }

        fn name(&self) -> &'static str {
            "recording"
        }
    }

    async fn mock_sandbox_info(
        State(state): State<MockCubeMasterState>,
    ) -> (StatusCode, Json<serde_json::Value>) {
        let (status, body) = state
            .info_responses
            .lock()
            .unwrap()
            .pop_front()
            .expect("unexpected sandbox detail request");
        (status, Json(body))
    }

    async fn mock_sandbox_update(
        State(state): State<MockCubeMasterState>,
    ) -> (StatusCode, Json<serde_json::Value>) {
        state.update_calls.fetch_add(1, Ordering::SeqCst);
        let (status, body) = state
            .update_responses
            .lock()
            .unwrap()
            .pop_front()
            .expect("unexpected sandbox update request");
        (status, Json(body))
    }

    async fn spawn_mock_cubemaster(
        info_responses: Vec<MockResponse>,
        update_responses: Vec<MockResponse>,
    ) -> (String, MockCubeMasterState) {
        let state = MockCubeMasterState {
            info_responses: Arc::new(Mutex::new(info_responses.into())),
            update_responses: Arc::new(Mutex::new(update_responses.into())),
            update_calls: Arc::new(AtomicUsize::new(0)),
        };
        let app = Router::new()
            .route("/cube/sandbox/info", get(mock_sandbox_info))
            .route("/cube/sandbox/update", post(mock_sandbox_update))
            .with_state(state.clone());
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();

        tokio::spawn(async move {
            let _ = axum::serve(listener, app).await;
        });

        (format!("http://{address}"), state)
    }

    async fn test_server(base_url: String, logger: RecordingLogger) -> TestServer {
        let config = ServerConfig {
            cubemaster_url: base_url.clone(),
            database_url: None,
            ..ServerConfig::default()
        };
        let mut state = AppState::new(config, arc(logger)).await;
        let client = reqwest::Client::builder().no_proxy().build().unwrap();
        let services = AppServices::new(
            state.config.as_ref(),
            CubeMasterClient::new(base_url, client.clone()),
        );
        state.http_client = client;
        state.services = services;
        TestServer::new(build_router(state)).expect("router should build")
    }

    fn sandbox_detail_response(status: i32) -> MockResponse {
        (
            StatusCode::OK,
            serde_json::json!({
                "requestID": "req-detail",
                "ret": { "ret_code": 0, "ret_msg": "ok" },
                "data": [{
                    "sandbox_id": "sandbox-123",
                    "host_id": "host-1",
                    "status": status,
                    "template_id": "template-456",
                    "annotations": {},
                    "labels": {},
                    "containers": []
                }]
            }),
        )
    }

    fn successful_update_response() -> MockResponse {
        (
            StatusCode::OK,
            serde_json::json!({
                "ret": { "ret_code": 200, "ret_msg": "ok" }
            }),
        )
    }

    fn failed_update_response() -> MockResponse {
        (
            StatusCode::OK,
            serde_json::json!({
                "ret": { "ret_code": 130409, "ret_msg": "resume conflict" }
            }),
        )
    }

    fn unavailable_response() -> MockResponse {
        (
            StatusCode::SERVICE_UNAVAILABLE,
            serde_json::json!({ "message": "temporarily unavailable" }),
        )
    }

    #[tokio::test]
    async fn resume_emits_existing_resumed_payload() {
        let (base_url, mock) = spawn_mock_cubemaster(
            vec![sandbox_detail_response(1)],
            vec![successful_update_response()],
        )
        .await;
        let logger = RecordingLogger::default();
        let server = test_server(base_url, logger.clone()).await;

        server
            .post("/sandboxes/sandbox-123/resume")
            .json(&serde_json::json!({ "timeout": 300 }))
            .await
            .assert_status(StatusCode::CREATED);

        assert_eq!(mock.update_calls.load(Ordering::SeqCst), 1);
        let events = logger.events_named("sandbox.resumed");
        assert_eq!(events.len(), 1);
        assert_eq!(events[0].level, LogLevel::Info);
        assert_eq!(events[0].fields.len(), 2);
        assert_eq!(events[0].fields["sandbox_id"], "sandbox-123");
        assert_eq!(events[0].fields["template_id"], "template-456");
    }

    #[tokio::test]
    async fn connect_emits_resumed_after_successful_resume_operation() {
        let (base_url, mock) = spawn_mock_cubemaster(
            vec![sandbox_detail_response(5), sandbox_detail_response(1)],
            vec![successful_update_response()],
        )
        .await;
        let logger = RecordingLogger::default();
        let server = test_server(base_url, logger.clone()).await;

        server
            .post("/sandboxes/sandbox-123/connect")
            .json(&serde_json::json!({ "timeout": 300 }))
            .await
            .assert_status_ok();

        assert_eq!(mock.update_calls.load(Ordering::SeqCst), 1);
        let events = logger.events_named("sandbox.resumed");
        assert_eq!(events.len(), 1);
        assert_eq!(events[0].fields.len(), 2);
        assert_eq!(events[0].fields["sandbox_id"], "sandbox-123");
        assert_eq!(events[0].fields["template_id"], "template-456");
    }

    #[tokio::test]
    async fn connect_does_not_emit_resumed_for_running_sandbox() {
        let (base_url, mock) =
            spawn_mock_cubemaster(vec![sandbox_detail_response(1)], vec![]).await;
        let logger = RecordingLogger::default();
        let server = test_server(base_url, logger.clone()).await;

        server
            .post("/sandboxes/sandbox-123/connect")
            .json(&serde_json::json!({ "timeout": 300 }))
            .await
            .assert_status_ok();

        assert_eq!(mock.update_calls.load(Ordering::SeqCst), 0);
        assert!(logger.events_named("sandbox.resumed").is_empty());
    }

    #[tokio::test]
    async fn connect_does_not_emit_resumed_when_update_fails() {
        let (base_url, mock) = spawn_mock_cubemaster(
            vec![sandbox_detail_response(5)],
            vec![failed_update_response()],
        )
        .await;
        let logger = RecordingLogger::default();
        let server = test_server(base_url, logger.clone()).await;

        let response = server
            .post("/sandboxes/sandbox-123/connect")
            .json(&serde_json::json!({ "timeout": 300 }))
            .await;

        response.assert_status(StatusCode::CONFLICT);
        assert_eq!(mock.update_calls.load(Ordering::SeqCst), 1);
        assert!(logger.events_named("sandbox.resumed").is_empty());
    }

    #[tokio::test]
    async fn connect_does_not_emit_resumed_when_follow_up_detail_fails() {
        let (base_url, mock) = spawn_mock_cubemaster(
            vec![sandbox_detail_response(5), unavailable_response()],
            vec![successful_update_response()],
        )
        .await;
        let logger = RecordingLogger::default();
        let server = test_server(base_url, logger.clone()).await;

        let response = server
            .post("/sandboxes/sandbox-123/connect")
            .json(&serde_json::json!({ "timeout": 300 }))
            .await;

        response.assert_status(StatusCode::INTERNAL_SERVER_ERROR);
        assert_eq!(mock.update_calls.load(Ordering::SeqCst), 1);
        assert!(logger.events_named("sandbox.resumed").is_empty());
    }
}
