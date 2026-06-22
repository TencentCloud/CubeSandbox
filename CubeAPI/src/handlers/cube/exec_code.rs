// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! `POST /cube/sandboxes/{sandboxID}/exec-code`
//!
//! Cube-specific extension (NOT part of the e2b API). Executes a snippet of
//! Python or Bash inside a running sandbox. The transport / parsing logic
//! lives in [`crate::services::exec::ExecService`]; this handler only resolves
//! the target sandbox, delegates to the service and records request logs.

use axum::{
    extract::{Path, State},
    response::IntoResponse,
    Json,
};

use crate::{
    error::AppResult,
    logging::{LogEvent, LogLevel},
    models::ExecCodeRequest,
    state::AppState,
};

#[utoipa::path(
    post,
    path = "/cube/sandboxes/{sandboxID}/exec-code",
    params(
        ("sandboxID" = String, Path, description = "Sandbox identifier")
    ),
    request_body = ExecCodeRequest,
    responses(
        (status = 200, description = "Code execution result", body = crate::models::ExecCodeResponse),
        (status = 400, description = "Invalid request", body = crate::models::ApiError),
        (status = 404, description = "Sandbox not found", body = crate::models::ApiError),
        (status = 500, description = "Unexpected backend error", body = crate::models::ApiError)
    )
)]
pub async fn exec_code(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    Json(body): Json<ExecCodeRequest>,
) -> AppResult<impl IntoResponse> {
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "exec_code")
                .field("sandbox_id", &sandbox_id)
                .field("language", &body.language),
        )
        .await;

    // Resolve sandbox to obtain its domain.
    let detail = state.services.sandboxes.get_sandbox(&sandbox_id).await?;
    let domain = detail
        .domain
        .unwrap_or_else(|| state.config.sandbox_domain.clone());

    let resp = state
        .services
        .exec
        .exec_code(&sandbox_id, &domain, &body)
        .await?;

    state
        .logger
        .log(
            LogEvent::new(LogLevel::Info, "sandbox.exec_code")
                .field("sandbox_id", &sandbox_id)
                .field("language", &body.language)
                .field_value("exit_code", resp.exit_code)
                .field_value("elapsed_ms", resp.elapsed_ms),
        )
        .await;

    Ok(Json(resp))
}
