// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! Template handlers — thin forwarder to CubeMaster `/cube/template*` endpoints.

use axum::{
    extract::{Path, Query, State},
    http::{HeaderMap, HeaderValue, StatusCode},
    response::IntoResponse,
    Json,
};
use serde::Deserialize;

use crate::{
    error::AppResult,
    models::{
        ApiError, CreateTemplateRequest, ListTemplatesQuery, RebuildTemplateRequest,
        TemplateBuildJob, TemplateCompatAdoptResponseView, TemplateCompatMatrixView,
        TemplateDetail, TemplateNameLookupResponse, TemplateSummary, UpdateTemplateRequest,
    },
    state::AppState,
};
use validator::Validate;

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

// ─── GET /templates/lookup ──────────────────────────────────────────────────

#[derive(Debug, Deserialize)]
pub struct TemplateNameLookupQuery {
    pub name: String,
}

#[utoipa::path(
    get,
    path = "/templates/lookup",
    params(
        ("name" = String, Query, description = "Human-readable template display name")
    ),
    responses(
        (status = 200, description = "Name resolves to a template", body = TemplateNameLookupResponse),
        (status = 400, description = "Invalid or ambiguous template name", body = ApiError),
        (status = 404, description = "Template name not found", body = ApiError),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
pub async fn lookup_template_name(
    State(state): State<AppState>,
    Query(query): Query<TemplateNameLookupQuery>,
) -> AppResult<impl IntoResponse> {
    let template_id = state
        .services
        .templates
        .lookup_template_name(&query.name)
        .await?;
    Ok((
        StatusCode::OK,
        Json(TemplateNameLookupResponse { template_id }),
    ))
}

// ─── GET /templates/:templateID ───────────────────────────────────────────────

#[utoipa::path(
    get,
    path = "/templates/{templateID}",
    params(
        ("templateID" = String, Path, description = "Template identifier (tpl-*) or human-readable display name")
    ),
    responses(
        (status = 200, description = "Template detail", body = TemplateDetail),
        (status = 400, description = "Invalid or ambiguous template name", body = ApiError),
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

// ─── GET /templates/compat ────────────────────────────────────────────────────

#[utoipa::path(
    get,
    path = "/templates/compat",
    responses(
        (status = 200, description = "Template compatibility matrix", body = TemplateCompatMatrixView),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
pub async fn template_compat(State(state): State<AppState>) -> AppResult<impl IntoResponse> {
    let matrix = state.services.templates.compat_matrix().await?;
    Ok((StatusCode::OK, Json(matrix)))
}

// ─── POST /templates/compat/:templateID/adopt-baseline ────────────────────────

#[utoipa::path(
    post,
    path = "/templates/compat/{templateID}/adopt-baseline",
    params(
        ("templateID" = String, Path, description = "Template identifier (tpl-*) or human-readable display name")
    ),
    responses(
        (status = 200, description = "Adopted UNKNOWN replicas to current baseline", body = TemplateCompatAdoptResponseView),
        (status = 400, description = "Invalid or ambiguous template name", body = ApiError),
        (status = 404, description = "Template not found", body = ApiError),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
pub async fn adopt_template_compat_baseline(
    State(state): State<AppState>,
    Path(template_id): Path<String>,
) -> AppResult<impl IntoResponse> {
    let updated = state
        .services
        .templates
        .adopt_compat_baseline(template_id)
        .await?;
    Ok((
        StatusCode::OK,
        Json(TemplateCompatAdoptResponseView { updated }),
    ))
}

// ─── POST /templates ──────────────────────────────────────────────────────────

#[utoipa::path(
    post,
    path = "/templates",
    request_body = CreateTemplateRequest,
    responses(
        (status = 202, description = "Template build accepted", body = TemplateBuildJob),
        (status = 400, description = "Invalid request", body = ApiError),
        (status = 409, description = "Name already in use", body = ApiError),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
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

#[utoipa::path(
    patch,
    path = "/templates/{templateID}",
    params(
        ("templateID" = String, Path, description = "Template identifier (tpl-*) or human-readable display name")
    ),
    request_body = UpdateTemplateRequest,
    responses(
        (status = 200, description = "Updated template display name", body = TemplateDetail),
        (status = 400, description = "Invalid name", body = ApiError),
        (status = 404, description = "Template not found", body = ApiError),
        (status = 409, description = "Name already in use", body = ApiError),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
pub async fn update_template(
    State(state): State<AppState>,
    Path(template_id): Path<String>,
    Json(body): Json<UpdateTemplateRequest>,
) -> AppResult<impl IntoResponse> {
    let detail = state
        .services
        .templates
        .update_template_name(template_id, body)
        .await?;
    Ok((StatusCode::OK, Json(detail)))
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
    // Both branches return `204 No Content` so callers see a single, REST-
    // conventional response shape regardless of whether `templateID`
    // resolves to a snapshot or a regular template (Bug 2).  The snapshot
    // branch additionally exposes the operation id via a response header so
    // audit trails / debugging can still correlate the deletion with its
    // CubeMaster job, but no body is returned.
    // Snapshot IDs use the snap- prefix; template display names must not, so
    // only treat snap-* paths as snapshot deletion when the snapshot exists.
    if template_id.trim().to_ascii_lowercase().starts_with("snap-")
        && state.services.snapshots.has_snapshot(&template_id).await?
    {
        let resp = state.services.snapshots.delete(&template_id).await?;
        let mut headers = HeaderMap::new();
        if let Ok(value) = HeaderValue::from_str(&resp.operation_id) {
            headers.insert("x-operation-id", value);
        }
        reverse_sync_agenthub_template(&state, &template_id).await;
        return Ok((StatusCode::NO_CONTENT, headers).into_response());
    }

    state
        .services
        .templates
        .delete_template(template_id.clone(), params.instance_type, params.sync)
        .await?;

    reverse_sync_agenthub_template(&state, &template_id).await;

    Ok(StatusCode::NO_CONTENT.into_response())
}

// reverse_sync_agenthub_template best-effort soft-deletes any AgentHub template
// registration backed by the just-deleted infrastructure template/snapshot
// (FIX-5b, L15/H5). It keeps the AgentHub registry from referencing a snapshot
// that no longer exists. Failures are logged, never propagated, so they cannot
// block the primary deletion.
async fn reverse_sync_agenthub_template(state: &AppState, id: &str) {
    let Some(store) = state.agenthub_store.as_ref() else {
        return;
    };
    match store
        .find_template_ids_by_template_or_source_snapshot(id)
        .await
    {
        Ok(template_ids) => {
            for template_id in template_ids {
                if let Err(e) = store.soft_delete_template(&template_id).await {
                    tracing::warn!(
                        "reverse sync: failed to soft-delete AgentHub template {}: {}",
                        template_id,
                        e
                    );
                }
            }
        }
        Err(e) => {
            tracing::warn!("reverse sync: query AgentHub templates failed: {}", e);
        }
    }
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
    Path((template_id, build_id)): Path<(String, String)>,
    Query(_params): Query<BuildLogsQuery>,
) -> AppResult<impl IntoResponse> {
    let logs = state
        .services
        .templates
        .get_template_build_logs(&template_id, &build_id)
        .await?;
    Ok((StatusCode::OK, Json(logs)))
}
