// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
// Volume API handlers — aligned with the E2B openapi.yml `/volumes` spec.
// Each handler delegates to VolumeService which in turn calls CubeMaster.

use axum::{
    extract::{Path, State},
    http::StatusCode,
    response::IntoResponse,
    Json,
};

use crate::{
    error::AppResult,
    models::{ApiError, NewVolume, Volume, VolumeAndToken, MAX_VOLUME_NAME_LEN},
    state::AppState,
};

// ── GET /volumes ──────────────────────────────────────────────────────────

/// List all volumes.
///
/// Returns `200 OK` with `[Volume]` on success.
#[utoipa::path(
    get,
    path = "/volumes",
    responses(
        (status = 200, description = "Successfully listed all volumes",  body = [Volume]),
        (status = 401, description = "Authentication error",              body = ApiError),
        (status = 500, description = "Server error",                      body = ApiError),
    ),
    security(("ApiKeyAuth" = []))
)]
pub async fn list_volumes(State(state): State<AppState>) -> AppResult<impl IntoResponse> {
    tracing::debug!("list_volumes");
    let items = state.services.volumes.list().await?;
    Ok((StatusCode::OK, Json(items)))
}

// ── POST /volumes ─────────────────────────────────────────────────────────

/// Create a new volume.
///
/// Returns `201 Created` with `VolumeAndToken` on success.
#[utoipa::path(
    post,
    path = "/volumes",
    request_body = NewVolume,
    responses(
        (status = 201, description = "Successfully created a new volume", body = VolumeAndToken),
        (status = 400, description = "Bad request",                        body = ApiError),
        (status = 401, description = "Authentication error",               body = ApiError),
        (status = 500, description = "Server error",                       body = ApiError),
    ),
    security(("ApiKeyAuth" = []))
)]
pub async fn create_volume(
    State(state): State<AppState>,
    Json(body): Json<NewVolume>,
) -> AppResult<impl IntoResponse> {
    tracing::debug!(name = %body.name, driver = ?body.driver, "create_volume");

    if !body.name.is_empty() && !body.name_is_valid() {
        return Err(crate::error::AppError::BadRequest(format!(
            "volume name must match ^[a-zA-Z0-9_-]+$ and be at most {MAX_VOLUME_NAME_LEN} characters"
        )));
    }

    let vol = state
        .services
        .volumes
        .create(body.name, body.driver)
        .await?;

    tracing::info!(volume_id = %vol.volume_id, "create_volume: success");
    Ok((StatusCode::CREATED, Json(vol)))
}

// ── GET /volumes/{volumeID} ───────────────────────────────────────────────

/// Get info for a single volume (includes auth token).
///
/// Returns `200 OK` with `VolumeAndToken` on success.
#[utoipa::path(
    get,
    path = "/volumes/{volumeID}",
    params(
        ("volumeID" = String, Path, description = "Identifier of the volume")
    ),
    responses(
        (status = 200, description = "Successfully retrieved a volume", body = VolumeAndToken),
        (status = 401, description = "Authentication error",            body = ApiError),
        (status = 404, description = "Not found",                       body = ApiError),
        (status = 500, description = "Server error",                    body = ApiError),
    ),
    security(("ApiKeyAuth" = []))
)]
pub async fn get_volume(
    State(state): State<AppState>,
    Path(volume_id): Path<String>,
) -> AppResult<impl IntoResponse> {
    tracing::debug!(volume_id = %volume_id, "get_volume");
    let vol = state.services.volumes.get(&volume_id).await?;
    Ok((StatusCode::OK, Json(vol)))
}

// ── DELETE /volumes/{volumeID} ────────────────────────────────────────────

/// Delete a volume.
///
/// Returns `204 No Content` on success.
#[utoipa::path(
    delete,
    path = "/volumes/{volumeID}",
    params(
        ("volumeID" = String, Path, description = "Identifier of the volume")
    ),
    responses(
        (status = 204, description = "Successfully deleted a volume"),
        (status = 401, description = "Authentication error", body = ApiError),
        (status = 404, description = "Not found",            body = ApiError),
        (status = 500, description = "Server error",         body = ApiError),
    ),
    security(("ApiKeyAuth" = []))
)]
pub async fn delete_volume(
    State(state): State<AppState>,
    Path(volume_id): Path<String>,
) -> AppResult<impl IntoResponse> {
    tracing::debug!(volume_id = %volume_id, "delete_volume");
    state.services.volumes.delete(&volume_id).await?;
    Ok(StatusCode::NO_CONTENT)
}
