// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! Cluster & node read-only handlers, powering the Dashboard Overview /
//! Nodes pages.
//!
//! Data is sourced from CubeMaster's `/internal/meta/nodes` endpoint and
//! normalised for UI consumption (CPU reported in cores, memory in MiB,
//! saturation ratios as percentages).

use axum::{
    extract::{Path, State},
    http::HeaderMap,
    http::StatusCode,
    response::IntoResponse,
    Json,
};
use serde::Deserialize;

use crate::{
    error::AppResult,
    models::{ApiError, ClusterOverview, NodeView, VersionMatrixView},
    state::AppState,
};

const SESSION_HEADER: &str = "x-session-token";

/// Request body for the node isolation endpoint. The audit identity is never
/// taken from the body; it is derived server-side from the authenticated
/// principal (see `resolve_operator`).
#[derive(Debug, Deserialize, utoipa::ToSchema)]
#[serde(rename_all = "camelCase")]
pub struct SetNodeIsolationBody {
    pub isolated: bool,
    #[serde(default)]
    pub reason: Option<String>,
}

/// Derive a trustworthy operator identity for the audit trail. Resolves the
/// WebUI session token to its username when possible; otherwise records
/// "unknown". Deliberately ignores any client-supplied operator field so the
/// audit cannot be forged.
async fn resolve_operator(state: &AppState, headers: &HeaderMap) -> String {
    let token = headers
        .get(SESSION_HEADER)
        .and_then(|v| v.to_str().ok())
        .map(|v| v.trim().to_string())
        .filter(|v| !v.is_empty());
    if let (Some(store), Some(token)) = (&state.agenthub_store, token) {
        if let Ok(Some(username)) = store.validate_session(&token).await {
            return username;
        }
    }
    "unknown".to_string()
}

// ─── GET /cluster/overview ────────────────────────────────────────────────

#[utoipa::path(
    get,
    path = "/cluster/overview",
    responses(
        (status = 200, description = "Cluster capacity overview", body = ClusterOverview),
        (status = 404, description = "Cluster endpoint unavailable", body = ApiError),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
pub async fn cluster_overview(State(state): State<AppState>) -> AppResult<impl IntoResponse> {
    let overview = state.services.cluster.cluster_overview().await?;
    Ok((StatusCode::OK, Json(overview)))
}

// ─── GET /nodes ───────────────────────────────────────────────────────────

#[utoipa::path(
    get,
    path = "/nodes",
    responses(
        (status = 200, description = "Node list", body = [NodeView]),
        (status = 404, description = "Node inventory unavailable", body = ApiError),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
pub async fn list_nodes(State(state): State<AppState>) -> AppResult<impl IntoResponse> {
    let views = state.services.cluster.list_nodes().await?;
    Ok((StatusCode::OK, Json(views)))
}

// ─── GET /nodes/:id ───────────────────────────────────────────────────────

#[utoipa::path(
    get,
    path = "/nodes/{nodeID}",
    params(
        ("nodeID" = String, Path, description = "Node identifier")
    ),
    responses(
        (status = 200, description = "Node detail", body = NodeView),
        (status = 404, description = "Node not found", body = ApiError),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
pub async fn get_node(
    State(state): State<AppState>,
    Path(node_id): Path<String>,
) -> AppResult<impl IntoResponse> {
    let node = state.services.cluster.get_node(&node_id).await?;
    Ok((StatusCode::OK, Json(node)))
}

// ─── POST /nodes/:id/isolation ────────────────────────────────────────────

#[utoipa::path(
    post,
    path = "/nodes/{nodeID}/isolation",
    params(
        ("nodeID" = String, Path, description = "Node identifier")
    ),
    request_body = inline(SetNodeIsolationBody),
    responses(
        (status = 200, description = "Updated node detail", body = NodeView),
        (status = 400, description = "Invalid request", body = ApiError),
        (status = 404, description = "Node not found", body = ApiError),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
pub async fn set_node_isolation(
    State(state): State<AppState>,
    Path(node_id): Path<String>,
    headers: HeaderMap,
    Json(body): Json<SetNodeIsolationBody>,
) -> AppResult<impl IntoResponse> {
    let operator = resolve_operator(&state, &headers).await;
    let node = state
        .services
        .cluster
        .set_node_isolation(&node_id, &operator, body.isolated, body.reason)
        .await?;
    Ok((StatusCode::OK, Json(node)))
}

// ─── GET /cluster/versions ────────────────────────────────────────────────

#[utoipa::path(
    get,
    path = "/cluster/versions",
    responses(
        (status = 200, description = "Cluster component version matrix", body = VersionMatrixView),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
pub async fn cluster_versions(State(state): State<AppState>) -> AppResult<impl IntoResponse> {
    let matrix = state.services.cluster.version_matrix().await?;
    Ok((StatusCode::OK, Json(matrix)))
}
