// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! Template handlers — thin forwarder to CubeMaster `/cube/template*` endpoints.

use axum::{
    extract::{Path, Query, State},
    http::StatusCode,
    response::IntoResponse,
    Json,
};
use serde::Deserialize;

use crate::{
    error::{AppError, AppResult},
    models::{
        ApiError, CreateTemplateRequest, ListTemplatesQuery, RebuildTemplateRequest,
        TemplateDetail, TemplateSummary,
    },
    state::AppState,
};

// ─── GET /templates ───────────────────────────────────────────────────────────

#[utoipa::path(
    get,
    path = "/templates",
    params(ListTemplatesQuery),
    responses(
        (status = 200, description = "Template list", body = [TemplateSummary]),
        (status = 404, description = "Template endpoint unavailable", body = ApiError),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
pub async fn list_templates(
    State(state): State<AppState>,
    Query(_params): Query<ListTemplatesQuery>,
) -> AppResult<impl IntoResponse> {
    let items = state.services.templates.list_templates().await?;
    Ok((StatusCode::OK, Json(items)))
}

// ─── GET /templates/:templateID ───────────────────────────────────────────────

#[utoipa::path(
    get,
    path = "/templates/{templateID}",
    params(
        ("templateID" = String, Path, description = "Template identifier")
    ),
    responses(
        (status = 200, description = "Template detail", body = TemplateDetail),
        (status = 404, description = "Template not found", body = ApiError),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
pub async fn get_template(
    State(state): State<AppState>,
    Path(template_id): Path<String>,
) -> AppResult<impl IntoResponse> {
    let detail = state.services.templates.get_template(&template_id).await?;
    Ok((StatusCode::OK, Json(detail)))
}

// ─── POST /templates ──────────────────────────────────────────────────────────

pub async fn create_template(
    State(state): State<AppState>,
    Json(body): Json<CreateTemplateRequest>,
) -> AppResult<impl IntoResponse> {
    let job = state.services.templates.create_template(body).await?;
    Ok((StatusCode::ACCEPTED, Json(job)))
}

// ─── POST /templates/:templateID (rebuild) ────────────────────────────────────

pub async fn rebuild_template(
    State(state): State<AppState>,
    Path(template_id): Path<String>,
    Json(body): Json<RebuildTemplateRequest>,
) -> AppResult<impl IntoResponse> {
    let job = state
        .services
        .templates
        .rebuild_template(template_id, body)
        .await?;
    Ok((StatusCode::ACCEPTED, Json(job)))
}

// ─── PATCH /templates/:templateID ─────────────────────────────────────────────

pub async fn update_template(
    State(_): State<AppState>,
    Path(_template_id): Path<String>,
    _body: Json<serde_json::Value>,
) -> AppResult<impl IntoResponse> {
    // CubeMaster exposes no dedicated PATCH; clients should use POST
    // /templates/:id (rebuild) or DELETE + re-create.
    Err::<(), _>(AppError::NotImplemented(
        "template metadata update is not supported; use POST /templates/{id} to rebuild"
            .to_string(),
    ))
}

// ─── DELETE /templates/:templateID ────────────────────────────────────────────

#[derive(Debug, Deserialize, Default)]
pub struct DeleteTemplateQuery {
    #[serde(default)]
    pub instance_type: Option<String>,
    #[serde(default)]
    pub sync: Option<bool>,
}

pub async fn delete_template(
    State(state): State<AppState>,
    Path(template_id): Path<String>,
    Query(params): Query<DeleteTemplateQuery>,
) -> AppResult<impl IntoResponse> {
    state
        .services
        .templates
        .delete_template(template_id, params.instance_type, params.sync)
        .await?;

    Ok(StatusCode::NO_CONTENT)
}

// ─── POST /templates/:templateID/builds/:buildID ──────────────────────────────

pub async fn start_template_build(
    State(state): State<AppState>,
    Path((template_id, _build_id)): Path<(String, String)>,
) -> AppResult<impl IntoResponse> {
    let job = state
        .services
        .templates
        .start_template_build(template_id)
        .await?;
    Ok((StatusCode::ACCEPTED, Json(job)))
}

// ─── GET /templates/:templateID/builds/:buildID/status ────────────────────────

#[derive(Debug, Deserialize)]
pub struct BuildStatusQuery {
    #[serde(default)]
    #[allow(dead_code)]
    pub logs_offset: i32,
}

pub async fn get_template_build_status(
    State(state): State<AppState>,
    Path((template_id, build_id)): Path<(String, String)>,
    Query(_params): Query<BuildStatusQuery>,
) -> AppResult<impl IntoResponse> {
    let out = state
        .services
        .templates
        .get_template_build_status(&template_id, &build_id)
        .await?;
    Ok((StatusCode::OK, Json(out)))
}

// ─── GET /templates/:templateID/builds/:buildID/logs ─────────────────────────

#[derive(Debug, Deserialize)]
pub struct BuildLogsQuery {
    #[serde(default)]
    #[allow(dead_code)]
    pub offset: i32,
    #[serde(default = "default_log_limit")]
    #[allow(dead_code)]
    pub limit: i32,
}
fn default_log_limit() -> i32 {
    100
}

pub async fn get_template_build_logs(
    State(state): State<AppState>,
    Path((_template_id, build_id)): Path<(String, String)>,
    Query(_params): Query<BuildLogsQuery>,
) -> AppResult<impl IntoResponse> {
    let logs = state
        .services
        .templates
        .get_template_build_logs(&build_id)
        .await?;
    Ok((StatusCode::OK, Json(logs)))
}
