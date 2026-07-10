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
        ApiError, BatchCreateRequest, BatchDestroyRequest, ConnectSandbox, ListSandboxesQuery,
        ListSandboxesV2Query, NewSandbox, RefreshRequest, ResumedSandbox, Sandbox, SandboxDetail,
        SandboxLogsQuery, SandboxLogsV2Query, SandboxLogsV2Response, SetTimeoutRequest,
    },
    state::AppState,
};

// ─── POST /sandboxes/batch ────────────────────────────────────────────────────

pub async fn batch_create_sandboxes(
    State(state): State<AppState>,
    Json(body): Json<BatchCreateRequest>,
) -> AppResult<impl IntoResponse> {
    let count = body.requests.len();
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "batch_create_sandboxes")
                .field_value("count", count),
        )
        .await;

    let results = state
        .services
        .sandboxes
        .batch_create_sandboxes(body.requests)
        .await;

    let succeeded = results.iter().filter(|r| r.error.is_none()).count();
    let failed = results.len() - succeeded;
    tracing::info!(
        succeeded = succeeded,
        failed = failed,
        "batch_create_sandboxes: completed"
    );
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Info, "sandbox.batch_created")
                .field_value("succeeded", succeeded)
                .field_value("failed", failed),
        )
        .await;

    Ok((StatusCode::CREATED, Json(results)))
}

// ─── DELETE /sandboxes/batch ──────────────────────────────────────────────────

pub async fn batch_destroy_sandboxes(
    State(state): State<AppState>,
    Json(body): Json<BatchDestroyRequest>,
) -> AppResult<impl IntoResponse> {
    let count = body.sandbox_ids.len();
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "batch_destroy_sandboxes")
                .field_value("count", count),
        )
        .await;

    let results = state
        .services
        .sandboxes
        .batch_destroy_sandboxes(body.sandbox_ids)
        .await;

    let succeeded = results.iter().filter(|r| r.error.is_none()).count();
    let failed = results.len() - succeeded;
    tracing::info!(
        succeeded = succeeded,
        failed = failed,
        "batch_destroy_sandboxes: completed"
    );
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Info, "sandbox.batch_deleted")
                .field_value("succeeded", succeeded)
                .field_value("failed", failed),
        )
        .await;

    Ok(Json(results))
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

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        config::ServerConfig,
        logging::{arc, noop::NoopLogger},
        state::AppState,
    };
    use axum::{routing::{delete, post}, Json};
    use axum_test::TestServer;
    use serde_json::{json, Value};
    use std::sync::Arc;
    use tokio::sync::Mutex;

    async fn mock_cubemaster_server() -> (String, Arc<Mutex<Vec<Value>>>) {
        let captures: Arc<Mutex<Vec<Value>>> = Arc::new(Mutex::new(Vec::new()));
        let captured_for_create = captures.clone();
        let captured_for_delete = captures.clone();

        async fn create_handler(
            axum::extract::State(captured): axum::extract::State<Arc<Mutex<Vec<Value>>>>,
            Json(body): Json<Value>,
        ) -> Json<Value> {
            captured.lock().await.push(body.clone());
            let tpl = body
                .get("annotations")
                .and_then(|v| v.get("cube.master.appsnapshot.template.id"))
                .and_then(|v| v.as_str())
                .unwrap_or("");
            if tpl == "invalid-tpl" {
                return Json(json!({
                    "requestID": "req-err",
                    "ret": { "ret_code": 130404, "ret_msg": "template not found" }
                }));
            }
            Json(json!({
                "requestID": "req-ok",
                "sandbox_id": format!("sb-{}", uuid::Uuid::new_v4()),
                "ret": { "ret_code": 0, "ret_msg": "ok" },
                "ext_info": {}
            }))
        }

        async fn delete_handler(
            axum::extract::State(captured): axum::extract::State<Arc<Mutex<Vec<Value>>>>,
            Json(body): Json<Value>,
        ) -> Json<Value> {
            captured.lock().await.push(body.clone());
            let sid = body["sandbox_id"].as_str().unwrap_or("");
            if sid == "sb-missing" || sid == "sb-gone" {
                return Json(json!({
                    "requestID": "req-err",
                    "ret": { "ret_code": 130404, "ret_msg": "sandbox not found" }
                }));
            }
            Json(json!({
                "requestID": "req-ok",
                "sandbox_id": sid,
                "ret": { "ret_code": 0, "ret_msg": "ok" }
            }))
        }

        let app = axum::Router::new()
            .route("/cube/sandbox", post(create_handler))
            .route("/cube/sandbox", delete(delete_handler))
            .with_state(captured_for_create);

        let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .expect("listener should bind");
        let addr = listener.local_addr().expect("listener addr");
        tokio::spawn(async move {
            axum::serve(listener, app).await.expect("mock server should run");
        });
        (format!("http://{}", addr), captured_for_delete)
    }

    async fn test_app(cubemaster_url: &str) -> axum::Router {
        let mut config = ServerConfig::default();
        config.cubemaster_url = cubemaster_url.to_string();
        let state = AppState::new(config, arc(NoopLogger)).await;
        axum::Router::new()
            .route("/sandboxes/batch", post(batch_create_sandboxes))
            .route("/sandboxes/batch", delete(batch_destroy_sandboxes))
            .with_state(state)
    }

    #[tokio::test]
    async fn batch_create_all_success() {
        let (url, _captures) = mock_cubemaster_server().await;
        let server = TestServer::new(test_app(&url).await).expect("test server should build");

        let resp = server
            .post("/sandboxes/batch")
            .json(&json!({
                "requests": [
                    {"templateID": "tpl-a"},
                    {"templateID": "tpl-b"},
                    {"templateID": "tpl-c"}
                ]
            }))
            .await;

        resp.assert_status(StatusCode::CREATED);
        let results: Vec<Value> = resp.json();
        assert_eq!(results.len(), 3);
        for r in &results {
            assert!(r["sandboxID"].is_string(), "sandboxID missing: {r}");
            assert!(r["sandbox"].is_object(), "sandbox missing: {r}");
            assert!(r["error"].is_null(), "unexpected error: {r}");
        }
    }

    #[tokio::test]
    async fn batch_create_partial_failure() {
        let (url, _captures) = mock_cubemaster_server().await;
        let server = TestServer::new(test_app(&url).await).expect("test server should build");

        let resp = server
            .post("/sandboxes/batch")
            .json(&json!({
                "requests": [
                    {"templateID": "tpl-good"},
                    {"templateID": "invalid-tpl"},
                    {"templateID": "tpl-also-good"}
                ]
            }))
            .await;

        resp.assert_status(StatusCode::CREATED);
        let results: Vec<Value> = resp.json();
        assert_eq!(results.len(), 3);

        assert!(results[0]["sandboxID"].is_string(), "entry 0 should succeed");
        assert!(results[0]["error"].is_null(), "entry 0 should have no error");

        assert!(results[1]["sandboxID"].is_null(), "entry 1 should fail");
        assert!(results[1]["sandbox"].is_null(), "entry 1 sandbox should be null");
        assert!(
            results[1]["error"].as_str().is_some_and(|s| !s.is_empty()),
            "entry 1 should have error: {}",
            results[1]
        );

        assert!(results[2]["sandboxID"].is_string(), "entry 2 should succeed");
        assert!(results[2]["error"].is_null(), "entry 2 should have no error");
    }

    #[tokio::test]
    async fn batch_destroy_all_success() {
        let (url, _captures) = mock_cubemaster_server().await;
        let server = TestServer::new(test_app(&url).await).expect("test server should build");

        let resp = server
            .delete("/sandboxes/batch")
            .json(&json!({
                "sandboxIDs": ["sb-1", "sb-2", "sb-3"]
            }))
            .await;

        resp.assert_status_ok();
        let results: Vec<Value> = resp.json();
        assert_eq!(results.len(), 3);
        for r in &results {
            assert!(r["sandboxID"].is_string(), "sandboxID missing: {r}");
            assert!(r["error"].is_null(), "unexpected error: {r}");
        }
    }

    #[tokio::test]
    async fn batch_destroy_partial_failure() {
        let (url, _captures) = mock_cubemaster_server().await;
        let server = TestServer::new(test_app(&url).await).expect("test server should build");

        let resp = server
            .delete("/sandboxes/batch")
            .json(&json!({
                "sandboxIDs": ["sb-ok", "sb-missing", "sb-also-ok"]
            }))
            .await;

        resp.assert_status_ok();
        let results: Vec<Value> = resp.json();
        assert_eq!(results.len(), 3);

        assert!(results[0]["sandboxID"].is_string(), "entry 0 should succeed");
        assert!(results[0]["error"].is_null(), "entry 0 should have no error");

        assert!(
            results[1]["error"].as_str().is_some_and(|s| !s.is_empty()),
            "entry 1 should have error: {}",
            results[1]
        );

        assert!(results[2]["sandboxID"].is_string(), "entry 2 should succeed");
        assert!(results[2]["error"].is_null(), "entry 2 should have no error");
    }
}
