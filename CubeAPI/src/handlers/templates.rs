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
    error::{AppError, AppResult},
    models::{
        ApiError, CreateTemplateRequest, ListTemplatesQuery, RebuildTemplateRequest,
        TemplateAliasLookupResponse, TemplateBuildJob, TemplateBuildStatus,
        TemplateCompatAdoptResponseView, TemplateCompatMatrixView, TemplateDetail, TemplateSummary,
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

// ─── GET /templates/aliases/:alias ───────────────────────────────────────────

#[utoipa::path(
    get,
    path = "/templates/aliases/{alias}",
    params(
        ("alias" = String, Path, description = "Template alias")
    ),
    responses(
        (status = 200, description = "Template alias lookup", body = TemplateAliasLookupResponse),
        (status = 400, description = "Invalid alias", body = ApiError),
        (status = 404, description = "Template alias not found", body = ApiError),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
pub async fn get_template_by_alias(
    State(state): State<AppState>,
    Path(alias): Path<String>,
) -> AppResult<impl IntoResponse> {
    let out = state
        .services
        .templates
        .get_template_by_alias(&alias)
        .await?;
    Ok((StatusCode::OK, Json(out)))
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
        ("templateID" = String, Path, description = "Template identifier")
    ),
    responses(
        (status = 200, description = "Adopted UNKNOWN replicas to current baseline", body = TemplateCompatAdoptResponseView),
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
        (status = 202, description = "Template build job accepted", body = TemplateBuildJob),
        (status = 400, description = "Invalid request (bad alias, missing image, etc.)", body = ApiError),
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

#[utoipa::path(
    post,
    path = "/templates/{templateID}",
    params(
        ("templateID" = String, Path, description = "Template identifier")
    ),
    request_body = RebuildTemplateRequest,
    responses(
        (status = 202, description = "Rebuild job accepted", body = TemplateBuildJob),
        (status = 404, description = "Template not found", body = ApiError),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
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
        ("templateID" = String, Path, description = "Template identifier")
    ),
    responses(
        (status = 501, description = "Not implemented; use POST /templates/{id} to rebuild", body = ApiError)
    )
)]
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

#[utoipa::path(
    delete,
    path = "/templates/{templateID}",
    params(
        ("templateID" = String, Path, description = "Template identifier or alias"),
        ("instance_type" = Option<String>, Query, description = "CubeMaster instance_type filter"),
        ("sync" = Option<bool>, Query, description = "Wait for deletion to complete before returning")
    ),
    responses(
        (status = 204, description = "Template deleted"),
        (status = 404, description = "Template not found", body = ApiError),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
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
    if state.services.snapshots.has_snapshot(&template_id).await? {
        let resp = state.services.snapshots.delete(&template_id).await?;
        let mut headers = HeaderMap::new();
        if let Ok(value) = HeaderValue::from_str(&resp.operation_id) {
            headers.insert("x-operation-id", value);
        }
        return Ok((StatusCode::NO_CONTENT, headers).into_response());
    }
    // Alias resolution happens at the CubeMaster layer (deleteTemplate
    // calls resolveTemplateIdentifierFn). CubeAPI no longer performs
    // AgentHub reverse-sync (removed when this branch was rebased onto
    // master, which includes the #984 refactoring).
    state
        .services
        .templates
        .delete_template(template_id.clone(), params.instance_type, params.sync)
        .await?;
    Ok(StatusCode::NO_CONTENT.into_response())
}

// ─── POST /templates/:templateID/builds/:buildID ──────────────────────────────

#[utoipa::path(
    post,
    path = "/templates/{templateID}/builds/{buildID}",
    params(
        ("templateID" = String, Path, description = "Template identifier"),
        ("buildID" = String, Path, description = "Build identifier")
    ),
    responses(
        (status = 202, description = "Build started", body = TemplateBuildJob),
        (status = 404, description = "Template or build not found", body = ApiError),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
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

#[utoipa::path(
    get,
    path = "/templates/{templateID}/builds/{buildID}/status",
    params(
        ("templateID" = String, Path, description = "Template identifier"),
        ("buildID" = String, Path, description = "Build identifier")
    ),
    responses(
        (status = 200, description = "Build status", body = TemplateBuildStatus),
        (status = 404, description = "Template or build not found", body = ApiError),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
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

#[utoipa::path(
    get,
    path = "/templates/{templateID}/builds/{buildID}/logs",
    params(
        ("templateID" = String, Path, description = "Template identifier"),
        ("buildID" = String, Path, description = "Build identifier")
    ),
    responses(
        (status = 200, description = "Build logs (JSON)"),
        (status = 404, description = "Build not found", body = ApiError),
        (status = 500, description = "Unexpected backend error", body = ApiError)
    )
)]
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
