// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use crate::{
    logging::{LogEvent, LogLevel},
    state::AppState,
};
use axum::{extract::State, http::StatusCode, response::IntoResponse, Json};
use serde::Serialize;
use utoipa::ToSchema;

#[derive(Serialize, ToSchema)]
pub struct HealthResponse {
    pub status: &'static str,
    pub sandboxes: usize,
}

/// GET /health
#[utoipa::path(
    get,
    path = "/health",
    responses(
        (status = 200, description = "Health status", body = HealthResponse)
    )
)]
pub async fn health(State(state): State<AppState>) -> impl IntoResponse {
    tracing::debug!("health: ok");
    state
        .logger
        .log(LogEvent::new(LogLevel::Debug, "api.request").field("handler", "health"))
        .await;

    (
        StatusCode::OK,
        Json(HealthResponse {
            status: "ok",
            sandboxes: 0,
        }),
    )
}
