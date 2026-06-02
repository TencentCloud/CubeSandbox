// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use axum::{
    extract::{Path, Query, State},
    http::StatusCode,
    response::IntoResponse,
    Json,
};

use crate::{
    error::AppResult,
    models::{
        V2TemplateBuildStart, V3BuildStatusQuery, V3TemplateBuildRequest,
    },
    state::AppState,
};

/// `POST /v3/templates` — register template + first build attempt.
pub async fn v3_create_template(
    State(state): State<AppState>,
    Json(body): Json<V3TemplateBuildRequest>,
) -> AppResult<impl IntoResponse> {
    let resp = state.services.templates.v3_create_template(body)?;
    Ok((StatusCode::ACCEPTED, Json(resp)))
}

/// `GET /templates/{templateID}/files/{hash}` — file-cache probe used by the
/// SDK before uploading build context tarballs. We always answer
/// `present=true` because the current CubeMaster pipeline only consumes
/// `from_image` references (no Dockerfile-from-context build yet).
pub async fn v3_get_files_hash(
    State(state): State<AppState>,
    Path((template_id, hash)): Path<(String, String)>,
) -> AppResult<impl IntoResponse> {
    let resp = state
        .services
        .templates
        .v3_get_file_upload(&template_id, &hash)?;
    Ok((StatusCode::CREATED, Json(resp)))
}

/// `POST /v2/templates/{templateID}/builds/{buildID}` — kick off the build.
pub async fn v2_trigger_build(
    State(state): State<AppState>,
    Path((template_id, build_id)): Path<(String, String)>,
    Json(body): Json<V2TemplateBuildStart>,
) -> AppResult<impl IntoResponse> {
    state
        .services
        .templates
        .v3_trigger_build(template_id, build_id, body)
        .await?;
    Ok(StatusCode::ACCEPTED)
}

/// `GET /templates/{templateID}/builds/{buildID}/status?logsOffset=N&limit=M`
pub async fn v3_get_build_status(
    State(state): State<AppState>,
    Path((template_id, build_id)): Path<(String, String)>,
    Query(params): Query<V3BuildStatusQuery>,
) -> AppResult<impl IntoResponse> {
    let info = state
        .services
        .templates
        .v3_get_build_status(&template_id, &build_id, params.logs_offset, params.limit)
        .await?;
    Ok((StatusCode::OK, Json(info)))
}
