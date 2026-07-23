// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use axum::{
    extract::{Path, Query, State},
    http::{header::USER_AGENT, HeaderMap, StatusCode},
    response::IntoResponse,
    Json,
};
use chrono::{DateTime, Utc};
use validator::Validate;

use crate::{
    error::{AppError, AppResult},
    logging::{LogEvent, LogLevel},
    models::{
        ApiError, ConnectSandbox, ListSandboxesQuery, ListSandboxesV2Query, ListedSandbox,
        NewSandbox, RefreshRequest, ResumedSandbox, Sandbox, SandboxDetail, SandboxLogsQuery,
        SandboxLogsV2Query, SandboxLogsV2Response, SetTimeoutRequest,
    },
    state::AppState,
};

// CubeSandbox represents a never-timeout sandbox as `end_at=None`, which the
// public Cube response serializes by omitting `endAt`. The E2B Python SDK's
// generated SandboxDetail/ListedSandbox models require `endAt` to be a valid
// datetime string, so E2B SDK callers get a far-future sentinel at the handler
// boundary while Cube-native callers keep the no-deadline shape.
const E2B_NEVER_TIMEOUT_END_AT_RFC3339: &str = "9999-12-31T23:59:59Z";

fn is_e2b_sdk_request(headers: &HeaderMap) -> bool {
    // E2B SDKs identify API requests through User-Agent. Keep this compatibility
    // branch narrow so curl, Cube SDKs, and other clients preserve Cube's
    // native omitted-endAt semantics for never-timeout sandboxes.
    headers
        .get(USER_AGENT)
        .and_then(|value| value.to_str().ok())
        .map(|value| {
            let user_agent = value.to_ascii_lowercase();
            user_agent.contains("e2b-python-sdk/") || user_agent.contains("e2b-code-interpreter/")
        })
        .unwrap_or(false)
}

fn e2b_never_timeout_end_at() -> DateTime<Utc> {
    DateTime::parse_from_rfc3339(E2B_NEVER_TIMEOUT_END_AT_RFC3339)
        .expect("E2B never-timeout sentinel must be valid RFC3339")
        .with_timezone(&Utc)
}

fn apply_e2b_sandbox_detail_compat(headers: &HeaderMap, detail: &mut SandboxDetail) {
    if is_e2b_sdk_request(headers) && detail.end_at.is_none() {
        detail.end_at = Some(e2b_never_timeout_end_at());
    }
}

fn apply_e2b_listed_sandbox_compat(headers: &HeaderMap, sandboxes: &mut [ListedSandbox]) {
    if !is_e2b_sdk_request(headers) {
        return;
    }

    for sandbox in sandboxes {
        if sandbox.end_at.is_none() {
            sandbox.end_at = Some(e2b_never_timeout_end_at());
        }
    }
}

// ─── GET /sandboxes ───────────────────────────────────────────────────────────

#[utoipa::path(
    get,
    path = "/sandboxes",
    params(ListSandboxesQuery),
    responses(
        (status = 200, description = "Sandbox list", body = [crate::models::ListedSandbox]),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
pub async fn list_sandboxes(
    State(state): State<AppState>,
    headers: HeaderMap,
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
        Ok(mut list) => {
            apply_e2b_listed_sandbox_compat(&headers, &mut list);
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
    headers: HeaderMap,
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

    let mut list = state
        .services
        .sandboxes
        .list(
            params.metadata.as_deref(),
            params.state.as_deref(),
            params.limit,
        )
        .await?;
    apply_e2b_listed_sandbox_compat(&headers, &mut list);

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
    headers: HeaderMap,
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

    let mut detail = state.services.sandboxes.get_sandbox(&sandbox_id).await?;
    apply_e2b_sandbox_detail_compat(&headers, &mut detail);
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

#[utoipa::path(
    post,
    path = "/sandboxes",
    request_body = NewSandbox,
    responses(
        (status = 201, description = "Sandbox created", body = Sandbox),
        (status = 400, description = "Invalid request", body = ApiError),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
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
        (status = 408, description = "The existing standard API request timeout expired before the synchronous delete completed"),
        (status = 409, description = "Paused sandbox cannot be admitted for internal resume because node capacity or resource metadata is unavailable", body = ApiError),
        (status = 503, description = "Sandbox is pausing, another lifecycle operation is in progress, the Cubelet RPC has too little remaining time, or its internal resume could not be completed", body = ApiError,
            headers(
                ("Retry-After" = u64, description = "Seconds a client should wait before retrying DELETE")
            )
        ),
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
    body.validate()
        .map_err(|e| AppError::BadRequest(e.to_string()))?;
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

#[utoipa::path(
    post,
    path = "/sandboxes/{sandboxID}/connect",
    params(
        ("sandboxID" = String, Path, description = "Sandbox identifier")
    ),
    request_body = ConnectSandbox,
    responses(
        (status = 200, description = "Sandbox connection info", body = Sandbox),
        (status = 404, description = "Sandbox not found", body = ApiError),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
pub async fn connect_sandbox(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    Json(body): Json<ConnectSandbox>,
) -> AppResult<impl IntoResponse> {
    body.validate()
        .map_err(|e| AppError::BadRequest(e.to_string()))?;
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

#[utoipa::path(
    get,
    path = "/sandboxes/{sandboxID}/logs",
    params(
        ("sandboxID" = String, Path, description = "Sandbox identifier"),
        SandboxLogsQuery
    ),
    responses(
        (status = 200, description = "Sandbox logs (legacy shape)", body = crate::models::SandboxLogs),
        (status = 404, description = "Sandbox not found", body = ApiError),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
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

#[utoipa::path(
    post,
    path = "/sandboxes/{sandboxID}/timeout",
    params(
        ("sandboxID" = String, Path, description = "Sandbox identifier")
    ),
    request_body = SetTimeoutRequest,
    responses(
        (status = 204, description = "Timeout updated"),
        (status = 400, description = "Invalid timeout value", body = ApiError),
        (status = 404, description = "Sandbox not found", body = ApiError),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
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

#[utoipa::path(
    post,
    path = "/sandboxes/{sandboxID}/refreshes",
    params(
        ("sandboxID" = String, Path, description = "Sandbox identifier")
    ),
    request_body = RefreshRequest,
    responses(
        (status = 204, description = "Sandbox refreshed"),
        (status = 400, description = "Invalid duration value", body = ApiError),
        (status = 404, description = "Sandbox not found", body = ApiError),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
pub async fn refresh_sandbox(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    Json(body): Json<RefreshRequest>,
) -> AppResult<impl IntoResponse> {
    body.validate()
        .map_err(|e| AppError::BadRequest(e.to_string()))?;

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
    use super::{
        apply_e2b_listed_sandbox_compat, apply_e2b_sandbox_detail_compat,
        E2B_NEVER_TIMEOUT_END_AT_RFC3339, USER_AGENT,
    };
    use crate::models::{ListedSandbox, SandboxDetail, SandboxState};
    use axum::http::{HeaderMap, HeaderValue};
    use chrono::TimeZone;

    fn sandbox_detail_without_end_at() -> SandboxDetail {
        SandboxDetail {
            template_id: "tpl-test".to_string(),
            alias: None,
            sandbox_id: "sbx-test".to_string(),
            client_id: "node-test".to_string(),
            started_at: chrono::Utc
                .timestamp_opt(1_800_000_000, 0)
                .single()
                .expect("valid timestamp"),
            end_at: None,
            envd_version: "0.5.11".to_string(),
            envd_access_token: None,
            domain: Some("cube.app".to_string()),
            cpu_count: 2,
            memory_mb: 2000,
            disk_size_mb: Some(0),
            metadata: None,
            state: SandboxState::Running,
            volume_mounts: None,
        }
    }

    fn listed_sandbox_without_end_at() -> ListedSandbox {
        ListedSandbox {
            template_id: "tpl-test".to_string(),
            alias: None,
            sandbox_id: "sbx-test".to_string(),
            client_id: "node-test".to_string(),
            started_at: chrono::Utc
                .timestamp_opt(1_800_000_000, 0)
                .single()
                .expect("valid timestamp"),
            end_at: None,
            cpu_count: 2,
            memory_mb: 2000,
            disk_size_mb: Some(0),
            metadata: None,
            state: SandboxState::Running,
            envd_version: "0.5.11".to_string(),
            volume_mounts: None,
        }
    }

    fn e2b_headers() -> HeaderMap {
        let mut headers = HeaderMap::new();
        headers.insert(
            USER_AGENT,
            HeaderValue::from_static("e2b-python-sdk/2.34.0"),
        );
        headers
    }

    #[test]
    fn e2b_sandbox_detail_compat_adds_end_at_for_never_timeout() {
        let mut detail = sandbox_detail_without_end_at();

        apply_e2b_sandbox_detail_compat(&e2b_headers(), &mut detail);

        let json = serde_json::to_value(detail).expect("detail should serialize");
        assert_eq!(json["endAt"], E2B_NEVER_TIMEOUT_END_AT_RFC3339);
    }

    #[test]
    fn non_e2b_sandbox_detail_keeps_end_at_omitted_for_never_timeout() {
        let mut detail = sandbox_detail_without_end_at();

        apply_e2b_sandbox_detail_compat(&HeaderMap::new(), &mut detail);

        let json = serde_json::to_value(detail).expect("detail should serialize");
        assert!(!json
            .as_object()
            .expect("detail should serialize as object")
            .contains_key("endAt"));
    }

    #[test]
    fn e2b_listed_sandbox_compat_adds_end_at_for_never_timeout() {
        let mut sandboxes = vec![listed_sandbox_without_end_at()];

        apply_e2b_listed_sandbox_compat(&e2b_headers(), &mut sandboxes);

        let json = serde_json::to_value(&sandboxes[0]).expect("listed sandbox should serialize");
        assert_eq!(json["endAt"], E2B_NEVER_TIMEOUT_END_AT_RFC3339);
    }

    #[test]
    fn non_e2b_listed_sandbox_keeps_end_at_omitted_for_never_timeout() {
        let mut sandboxes = vec![listed_sandbox_without_end_at()];

        apply_e2b_listed_sandbox_compat(&HeaderMap::new(), &mut sandboxes);

        let json = serde_json::to_value(&sandboxes[0]).expect("listed sandbox should serialize");
        assert!(!json
            .as_object()
            .expect("listed sandbox should serialize as object")
            .contains_key("endAt"));
    }
}
