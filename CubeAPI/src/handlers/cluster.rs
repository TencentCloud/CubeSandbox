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
    http::StatusCode,
    response::IntoResponse,
    Json,
};

use crate::{
    error::AppResult,
    models::{ApiError, ClusterOverview, NodeView},
    state::AppState,
};

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
