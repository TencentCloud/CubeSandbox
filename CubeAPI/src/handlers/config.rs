// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

use axum::{extract::State, http::StatusCode, response::IntoResponse, Json};
use serde::Serialize;
use utoipa::ToSchema;

use crate::state::AppState;

#[derive(Debug, Serialize, ToSchema)]
pub struct RuntimeConfig {
    /// Public-facing API endpoint URL (e.g. "http://1.2.3.4:8080/cubeapi/v1").
    #[serde(rename = "apiEndpoint")]
    pub api_endpoint: String,
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

/// Build the public-facing API endpoint URL.
///
/// Priority:
///   1. `CUBE_API_PUBLIC_HOST` env var — hostname, host:port, or full URL.
///      Scheme "http://" is prepended automatically when missing.
///   2. Bind address with "0.0.0.0" replaced by "127.0.0.1".
fn public_api_endpoint(bind: &str) -> String {
    if let Ok(v) = std::env::var("CUBE_API_PUBLIC_HOST") {
        let v = v.trim().to_string();
        if !v.is_empty() {
            let with_scheme = if v.starts_with("http://") || v.starts_with("https://") {
                v
            } else {
                format!("http://{v}")
            };
            let base = with_scheme.trim_end_matches('/');
            return if base.ends_with("/cubeapi/v1") {
                base.to_string()
            } else {
                format!("{base}/cubeapi/v1")
            };
        }
    }
    let bind_addr = bind.replace("0.0.0.0", "127.0.0.1");
    format!("http://{bind_addr}/cubeapi/v1")
}

/// GET /cubeapi/v1/config — read-only runtime configuration snapshot.
pub async fn get_config(State(state): State<AppState>) -> impl IntoResponse {
    let cfg = &state.config;
    (
        StatusCode::OK,
        Json(RuntimeConfig {
            api_endpoint: public_api_endpoint(&cfg.bind),
            rate_limit_per_sec: cfg.rate_limit_per_sec,
            auth_enabled: cfg
                .auth_callback_url
                .as_deref()
                .is_some_and(|u| !u.is_empty()),
            sandbox_domain: cfg.sandbox_domain.clone(),
            instance_type: cfg.instance_type.clone(),
        }),
    )
}
