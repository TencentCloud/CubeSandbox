// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

use axum::{extract::State, http::StatusCode, response::IntoResponse, Json};
use serde::Serialize;
use utoipa::ToSchema;

use crate::state::AppState;

#[derive(Debug, Serialize, ToSchema)]
pub struct RuntimeConfig {
    /// Max requests per second per API key (token-bucket).
    #[serde(rename = "rateLimitPerSec")]
    pub rate_limit_per_sec: u32,
    /// Whether auth callback is configured (true = auth enabled).
    #[serde(rename = "authEnabled")]
    pub auth_enabled: bool,
    /// Default sandbox domain.
    #[serde(rename = "sandboxDomain")]
    pub sandbox_domain: String,
    /// Default instance type.
    #[serde(rename = "instanceType")]
    pub instance_type: String,
}

/// GET /cubeapi/v1/config — read-only runtime configuration snapshot.
pub async fn get_config(State(state): State<AppState>) -> impl IntoResponse {
    let cfg = &state.config;
    (
        StatusCode::OK,
        Json(RuntimeConfig {
            rate_limit_per_sec: cfg.rate_limit_per_sec,
            auth_enabled: cfg.auth_callback_url.as_deref().is_some_and(|u| !u.is_empty()),
            sandbox_domain: cfg.sandbox_domain.clone(),
            instance_type: cfg.instance_type.clone(),
        }),
    )
}
