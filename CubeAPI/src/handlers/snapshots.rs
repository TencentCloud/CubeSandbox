// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use axum::{
    extract::{Path, Query, State},
    http::{HeaderMap, HeaderValue, StatusCode},
    response::IntoResponse,
    Json,
};

use crate::{
    error::AppResult,
    models::{ApiError, CreateSnapshotRequest, ListSnapshotsQuery, RollbackRequest, SnapshotInfo},
    state::AppState,
};

// ── POST /sandboxes/:sandboxID/snapshots ──────────────────────────────────

pub async fn create_snapshot(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    Json(body): Json<CreateSnapshotRequest>,
) -> AppResult<impl IntoResponse> {
    tracing::debug!(sandbox_id = %sandbox_id, name = ?body.name, "create_snapshot");

    let info: SnapshotInfo = state
        .services
        .snapshots
        .create(&sandbox_id, body.name)
        .await?;

    tracing::info!(
        sandbox_id = %sandbox_id,
        snapshot_id = %info.snapshot_id,
        "create_snapshot: success"
    );

    Ok((StatusCode::CREATED, Json(info)))
}

// ── GET /snapshots ────────────────────────────────────────────────────────

#[utoipa::path(
    get,
    path = "/snapshots",
    params(ListSnapshotsQuery),
    responses(
        (status = 200, description = "List of snapshots", body = [crate::models::SnapshotListItem]),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
pub async fn list_snapshots(
    State(state): State<AppState>,
    Query(params): Query<ListSnapshotsQuery>,
) -> AppResult<impl IntoResponse> {
    tracing::debug!(
        sandbox_id = ?params.sandbox_id,
        limit = ?params.limit,
        next_token = ?params.next_token,
        "list_snapshots"
    );

    let (items, next_token) = state
        .services
        .snapshots
        .list(
            params.sandbox_id.as_deref(),
            params.limit,
            params.next_token.as_deref(),
        )
        .await?;

    let mut headers = HeaderMap::new();
    if !next_token.is_empty() {
        if let Ok(v) = HeaderValue::from_str(&next_token) {
            headers.insert("x-next-token", v);
        }
    }

    Ok((StatusCode::OK, headers, Json(items)))
}

// Snapshot deletion is exposed exclusively through `DELETE /templates/{id}`
// (see `handlers::templates::delete_template`).  We intentionally do NOT
// publish a separate `delete_snapshot` handler / route because:
//   * having a second entry point invites divergent response shapes (Bug 5
//     observed exactly that — the dead handler returned `204` while the
//     dispatcher in `delete_template` returned `200` + JSON);
//   * the "is this id a snapshot?" lookup belongs in one place, not two.
// If a dedicated `/snapshots/{id}` DELETE endpoint is ever needed, route it
// to `services::SnapshotService::delete` and return `204 No Content` to stay
// aligned with the template path.

// ── POST /sandboxes/:sandboxID/rollback ───────────────────────────────────

pub async fn rollback_sandbox(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    Json(body): Json<RollbackRequest>,
) -> AppResult<impl IntoResponse> {
    tracing::debug!(
        sandbox_id = %sandbox_id,
        snapshot_id = %body.snapshot_id,
        "rollback_sandbox"
    );

    let resp = state
        .services
        .snapshots
        .rollback(&sandbox_id, &body.snapshot_id)
        .await?;

    tracing::info!(
        sandbox_id = %sandbox_id,
        snapshot_id = %body.snapshot_id,
        operation_id = %resp.operation_id,
        "rollback_sandbox: success"
    );

    Ok((StatusCode::OK, Json(resp)))
}
