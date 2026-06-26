// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
//! Example HTTP handlers.
//!
//! Exposes three endpoints:
//!
//! - `GET  /cubeapi/v1/examples`               — list visible examples
//! - `GET  /cubeapi/v1/examples/:scenario/:file` — fetch source code
//! - `POST /cubeapi/v1/examples/run`            — run an example script
//!
//! All business logic (template resolution, dependency install, subprocess
//! spawn) lives in [`crate::services::examples::ExampleService`].

use axum::{extract::State, http::StatusCode, response::IntoResponse, Json};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use crate::{error::AppResult, examples::TopologyGraph, state::AppState};

// ─── Request / Response models ────────────────────────────────────────────────

#[derive(Serialize, Clone, ToSchema)]
pub struct ExampleMeta {
    /// Stable identifier. Format: `"<scenario>:<file-id>"`.
    pub id: String,
    pub scenario: String,
    pub filename: String,
    pub title: String,
    pub description: String,
    pub category: String,
    /// Source language: `python` | `go` | `bash` | `markdown`.
    pub language: String,
    /// Associated store catalog item ID.
    pub store_item_id: Option<String>,
}

#[derive(Deserialize, ToSchema)]
pub struct RunExampleRequest {
    pub id: String,
    /// Optional template ID override (highest priority).
    pub template_id: Option<String>,
    /// Optional language override (informational; interpreter is chosen by
    /// file extension, not this field).
    #[allow(dead_code)]
    pub language: Option<String>,
    /// When present, the handler runs this body instead of the on-disk file.
    pub code: Option<String>,
    /// Optional CubeAPI URL override.
    pub api_url: Option<String>,
    /// Optional CubeProxy node IP override.
    pub proxy_node_ip: Option<String>,
}

#[derive(Serialize, Clone, ToSchema)]
pub struct RunExampleResponse {
    pub stdout: String,
    pub stderr: String,
    pub exit_code: i32,
    pub success: bool,
    pub elapsed_ms: u64,
    pub topology: TopologyGraph,
    pub ran_edited: bool,
}

// ─── GET /cubeapi/v1/examples ─────────────────────────────────────────────────

/// List all visible example scripts. Hidden scenarios (AI / LLM demos) are
/// filtered out at the source.
pub async fn list_examples(State(state): State<AppState>) -> AppResult<impl IntoResponse> {
    Ok(Json(state.services.examples.list_visible()))
}

// ─── GET /cubeapi/v1/examples/:scenario/:file ─────────────────────────────────

/// Get the source code of a single example script.
pub async fn get_example_source(
    State(state): State<AppState>,
    axum::extract::Path((scenario, file)): axum::extract::Path<(String, String)>,
) -> AppResult<impl IntoResponse> {
    match state.services.examples.get_source(&scenario, &file).await {
        Ok(body) => Ok((StatusCode::OK, Json(body)).into_response()),
        Err(e) => Ok(e.into_response()),
    }
}

// ─── POST /cubeapi/v1/examples/run ───────────────────────────────────────────

/// Run an example script in a subprocess and return stdout / stderr and the
/// topology graph for the scenario.
pub async fn run_example(
    State(state): State<AppState>,
    Json(req): Json<RunExampleRequest>,
) -> AppResult<impl IntoResponse> {
    let result = state
        .services
        .examples
        .run(
            req,
            &state.services.templates,
            state.agenthub_store.as_ref(),
        )
        .await;

    match result {
        Ok(resp) => Ok(Json(resp).into_response()),
        Err(e) => Ok(e.into_response()),
    }
}
