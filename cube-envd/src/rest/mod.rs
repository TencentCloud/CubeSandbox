// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! REST endpoints: /health /init /envs /metrics /files.

pub mod files;
pub mod metrics;

use std::collections::HashMap;
use std::sync::Arc;

use axum::extract::State;
use axum::http::{HeaderMap, StatusCode};
use axum::response::IntoResponse;
use serde::Deserialize;

use crate::state::AppState;

/// GET /health — baseline: 204 with `Cache-Control: no-store`.
pub async fn health() -> impl IntoResponse {
    (
        StatusCode::NO_CONTENT,
        [(axum::http::header::CACHE_CONTROL, "no-store")],
    )
}

/// /init request body. CubeSandbox's Cubelet only ever sends `envVars`;
/// the remaining upstream fields are accepted and logged so foreign callers
/// are not broken, but they carry no behavior in cube-envd (declared MVP
/// differences: volumeMounts / hyperloopIP / caBundle / timestamp sync).
#[derive(Debug, Default, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct InitRequest {
    #[serde(default)]
    pub env_vars: Option<HashMap<String, String>>,
    #[serde(default)]
    pub access_token: Option<String>,
    #[serde(default)]
    pub default_user: Option<String>,
    #[serde(default)]
    pub default_workdir: Option<String>,
    #[serde(default)]
    pub volume_mounts: Option<serde_json::Value>,
    #[serde(default)]
    pub hyperloop_ip: Option<String>,
    #[serde(default)]
    pub timestamp: Option<String>,
    #[serde(default)]
    pub ca_bundle: Option<String>,
}

/// POST /init — merge env vars (baseline: accumulative), store the access
/// token if one is provided, 204.
pub async fn init(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    body: axum::body::Body,
) -> axum::response::Response {
    if check_token(&state, &headers).is_err() {
        return StatusCode::UNAUTHORIZED.into_response();
    }
    // Read with the same explicit cap the RPC unary surface uses instead of
    // relying on axum's default 2 MiB extractor limit: an /init carrying a
    // large caBundle (accepted-and-ignored fields) must not bounce with a
    // framework-shaped 413.
    let body = match axum::body::to_bytes(body, crate::server::MAX_UNARY_BODY).await {
        Ok(b) => b,
        Err(e) => {
            return (StatusCode::BAD_REQUEST, format!("read body: {e}")).into_response();
        }
    };
    let req: InitRequest = if body.is_empty() {
        InitRequest::default()
    } else {
        match serde_json::from_slice(&body) {
            Ok(r) => r,
            Err(e) => {
                return (StatusCode::BAD_REQUEST, format!("invalid init body: {e}")).into_response()
            }
        }
    };
    if let Some(vars) = req.env_vars {
        tracing::info!("init: merging {} env vars", vars.len());
        state.merge_env_vars(vars);
    } else {
        state
            .initialized
            .store(true, std::sync::atomic::Ordering::Relaxed);
    }
    if let Some(token) = req.access_token {
        tracing::info!("init: access token configured");
        state.set_access_token(token);
    }
    for (field, present) in [
        ("volumeMounts", req.volume_mounts.is_some()),
        ("hyperloopIP", req.hyperloop_ip.is_some()),
        ("caBundle", req.ca_bundle.is_some()),
        ("defaultUser", req.default_user.is_some()),
        ("defaultWorkdir", req.default_workdir.is_some()),
        ("timestamp", req.timestamp.is_some()),
    ] {
        if present {
            tracing::warn!("init: field '{field}' is accepted but ignored by cube-envd");
        }
    }
    no_store(StatusCode::NO_CONTENT).into_response()
}

/// GET /envs — the accumulated env-var store (includes E2B_SANDBOX).
pub async fn envs(State(state): State<Arc<AppState>>, headers: HeaderMap) -> impl IntoResponse {
    if check_token(&state, &headers).is_err() {
        return StatusCode::UNAUTHORIZED.into_response();
    }
    (
        [(axum::http::header::CACHE_CONTROL, "no-store")],
        axum::Json(state.env_vars()),
    )
        .into_response()
}

/// Baseline: health/init/envs/metrics all answer with Cache-Control: no-store.
pub(crate) fn no_store(status: StatusCode) -> impl IntoResponse {
    (status, [(axum::http::header::CACHE_CONTROL, "no-store")])
}

pub(crate) fn check_token(state: &AppState, headers: &HeaderMap) -> Result<(), ()> {
    let token = headers.get("x-access-token").and_then(|v| v.to_str().ok());
    state.check_access_token(token)
}
