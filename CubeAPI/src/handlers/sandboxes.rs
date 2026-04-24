// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use axum::{
    extract::{Path, Query, State},
    http::StatusCode,
    response::IntoResponse,
    Json,
};
use std::collections::HashMap;
use uuid::Uuid;

use crate::{
    constants::ENVD_VERSION,
    cubemaster::{
        CreateSandboxRequest, CubeVSContext, DeleteSandboxRequest, ListSandboxRequest,
        SandboxLogsRequest, SandboxRefreshRequest, SandboxSnapshotRequest, SandboxTimeoutRequest,
        SandboxUpdateRequest,
    },
    error::{AppError, AppResult},
    logging::{LogEvent, LogLevel},
    models::{
        ConnectSandbox, CreateSnapshotRequest, ListSandboxesQuery, ListSandboxesV2Query,
        LogLevel as ModelLogLevel, NewSandbox, RefreshRequest, ResumedSandbox, Sandbox,
        SandboxDetail, SandboxLog, SandboxLogEntry, SandboxLogs, SandboxLogsQuery,
        SandboxLogsV2Query, SandboxLogsV2Response, SandboxState, SetTimeoutRequest, SnapshotInfo,
    },
    state::AppState,
};

// ─── Helpers ──────────────────────────────────────────────────────────────────

/// Build a ListedSandbox from a CubeMaster SandboxInfo record.
fn from_cubemaster_info(s: crate::cubemaster::SandboxInfo) -> crate::models::ListedSandbox {
    use crate::models::ListedSandbox;
    let now = chrono::Utc::now();
    let template_id = if !s.template_id.is_empty() {
        s.template_id.clone()
    } else {
        s.annotations
            .get("cube.master.appsnapshot.template.id")
            .cloned()
            .or_else(|| s.labels.get("cube.master.appsnapshot.template.id").cloned())
            .unwrap_or_default()
    };
    let state = match s.status.to_lowercase().as_str() {
        "paused" => SandboxState::Paused,
        _ => SandboxState::Running,
    };
    ListedSandbox {
        template_id,
        alias: None,
        sandbox_id: s.sandbox_id,
        client_id: s.host_id,
        started_at: s.started_at.unwrap_or(now),
        end_at: s.end_at.unwrap_or(now),
        cpu_count: s.cpu_count,
        memory_mb: s.memory_mb,
        // Always populate diskSizeMB so the field is present in JSON.
        // The e2b SDK treats diskSizeMB as required and raises KeyError if absent.
        // SandboxInfo does not carry disk_size_mb, so we default to 0.
        disk_size_mb: Some(0),
        metadata: if s.labels.is_empty() {
            None
        } else {
            Some(s.labels)
        },
        state,
        envd_version: ENVD_VERSION.to_string(),
        volume_mounts: None,
    }
}

/// Filter sandboxes by URL-encoded metadata, e.g. "user=abc&app=prod".
fn filter_by_metadata(metadata: Option<&HashMap<String, String>>, query: Option<&str>) -> bool {
    let Some(q) = query else { return true };
    let Some(meta) = metadata else { return false };
    for pair in q.split('&') {
        if let Some((k, v)) = pair.split_once('=') {
            if meta.get(k).map_or(true, |mv| mv != v) {
                return false;
            }
        }
    }
    true
}

// ─── GET /sandboxes ───────────────────────────────────────────────────────────

pub async fn list_sandboxes(
    State(state): State<AppState>,
    Query(params): Query<ListSandboxesQuery>,
) -> AppResult<impl IntoResponse> {
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "list_sandboxes")
                .field("metadata_filter", params.metadata.as_deref().unwrap_or("")),
        )
        .await;

    let req = ListSandboxRequest {
        request_id: Uuid::new_v4().to_string(),
        instance_type: state.config.instance_type.clone(),
        start_idx: Some(0),
        size: Some(200),
        host_id: None,
        filter: None,
    };

    match state.cubemaster.list_sandboxes(&req).await {
        Ok(resp) => {
            resp.ret
                .into_result()
                .map_err(|e| AppError::Internal(anyhow::anyhow!(e.to_string())))?;
            let list: Vec<_> = resp
                .sandboxes
                .into_iter()
                .map(from_cubemaster_info)
                .filter(|sb| filter_by_metadata(sb.metadata.as_ref(), params.metadata.as_deref()))
                .collect();
            state
                .logger
                .log(
                    LogEvent::new(LogLevel::Info, "api.response")
                        .field("handler", "list_sandboxes")
                        .field_value("count", list.len()),
                )
                .await;
            Ok(Json(list))
        }
        Err(e) => {
            let msg = e.to_string();
            tracing::error!(error = %msg, "list_sandboxes: cubemaster error");
            state
                .logger
                .log(
                    LogEvent::new(LogLevel::Error, "api.error")
                        .field("handler", "list_sandboxes")
                        .field("error", &msg),
                )
                .await;
            Err(AppError::Internal(anyhow::anyhow!(msg)))
        }
    }
}

// ─── GET /v2/sandboxes ────────────────────────────────────────────────────────

pub async fn list_sandboxes_v2(
    State(state): State<AppState>,
    Query(params): Query<ListSandboxesV2Query>,
) -> AppResult<impl IntoResponse> {
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "list_sandboxes_v2")
                .field("state_filter", params.state.as_deref().unwrap_or(""))
                .field_value("limit", params.limit),
        )
        .await;

    let req = ListSandboxRequest {
        request_id: Uuid::new_v4().to_string(),
        instance_type: state.config.instance_type.clone(),
        start_idx: Some(0),
        size: Some(params.limit.max(1)),
        host_id: None,
        filter: None,
    };

    match state.cubemaster.list_sandboxes(&req).await {
        Ok(resp) => {
            resp.ret
                .into_result()
                .map_err(|e| AppError::Internal(anyhow::anyhow!(e.to_string())))?;

            let state_filter: Option<SandboxState> =
                params.state.as_deref().and_then(|s| match s {
                    "running" => Some(SandboxState::Running),
                    "paused" => Some(SandboxState::Paused),
                    _ => None,
                });

            let list: Vec<_> = resp
                .sandboxes
                .into_iter()
                .map(from_cubemaster_info)
                .filter(|sb| filter_by_metadata(sb.metadata.as_ref(), params.metadata.as_deref()))
                .filter(|sb| state_filter.as_ref().map_or(true, |sf| &sb.state == sf))
                .collect();

            state
                .logger
                .log(
                    LogEvent::new(LogLevel::Info, "api.response")
                        .field("handler", "list_sandboxes_v2")
                        .field_value("count", list.len()),
                )
                .await;
            Ok(Json(list))
        }
        Err(e) => Err(AppError::Internal(anyhow::anyhow!(e.to_string()))),
    }
}

// ─── GET /sandboxes/:sandboxID ────────────────────────────────────────────────

pub async fn get_sandbox(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
) -> AppResult<impl IntoResponse> {
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "get_sandbox")
                .field("sandbox_id", &sandbox_id),
        )
        .await;

    match state
        .cubemaster
        .get_sandbox(&sandbox_id, &state.config.instance_type)
        .await
    {
        Ok(resp) => {
            if resp.ret.ret_code != 0 && resp.ret.ret_code != 200 {
                if resp.ret.ret_code == 130404 {
                    return Err(AppError::NotFound(format!(
                        "sandbox {} not found",
                        sandbox_id
                    )));
                }
                return Err(AppError::Internal(anyhow::anyhow!("{}", resp.ret.ret_msg)));
            }
            let d = match resp.into_first_sandbox(&state.config.instance_type) {
                Some(d) => d,
                None => {
                    return Err(AppError::NotFound(format!(
                        "sandbox {} not found",
                        sandbox_id
                    )))
                }
            };
            let now = chrono::Utc::now();
            let state_val = match d.status {
                crate::cubemaster::SandboxStatus::Paused => SandboxState::Paused,
                crate::cubemaster::SandboxStatus::Running => SandboxState::Running,
                _ => SandboxState::Running,
            };
            let detail = SandboxDetail {
                template_id: d.template_id,
                alias: None,
                sandbox_id: d.sandbox_id,
                client_id: d.host_id,
                started_at: d.started_at.unwrap_or(now),
                end_at: d.end_at.unwrap_or(now),
                envd_version: ENVD_VERSION.to_string(),
                envd_access_token: None,
                domain: Some(state.config.sandbox_domain.clone()),
                cpu_count: d.cpu_count,
                memory_mb: d.memory_mb,
                disk_size_mb: Some(d.disk_size_mb),
                metadata: None,
                state: state_val,
                volume_mounts: None,
            };
            state
                .logger
                .log(
                    LogEvent::new(LogLevel::Info, "api.response")
                        .field("handler", "get_sandbox")
                        .field("sandbox_id", &sandbox_id),
                )
                .await;
            Ok(Json(detail))
        }
        Err(e) if e.is_not_found() => Err(AppError::NotFound(format!(
            "sandbox {} not found",
            sandbox_id
        ))),
        Err(e) => Err(AppError::Internal(anyhow::anyhow!(e.to_string()))),
    }
}

// ─── POST /sandboxes ──────────────────────────────────────────────────────────

pub async fn create_sandbox(
    State(state): State<AppState>,
    Json(body): Json<NewSandbox>,
) -> AppResult<impl IntoResponse> {
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "create_sandbox")
                .field("template_id", &body.template_id)
                .field_value("timeout", body.timeout),
        )
        .await;

    let mut annotations: HashMap<String, String> = HashMap::new();
    annotations.insert(
        "cube.master.appsnapshot.template.id".to_string(),
        body.template_id.clone(),
    );
    annotations.insert(
        "cube.master.appsnapshot.template.version".to_string(),
        "v2".to_string(),
    );

    // Extract host-dir mount config from metadata into annotations so that
    // CubeMaster's injectHostDirMounts() can pick it up.
    // The key "host-mount" is a CubeMaster-internal annotation; callers pass
    // it via E2B metadata because the E2B protocol has no native volume field.
    const HOSTDIR_MOUNT_KEY: &str = "host-mount";
    let labels: Option<HashMap<String, String>> = body.metadata.map(|mut meta| {
        if let Some(v) = meta.remove(HOSTDIR_MOUNT_KEY) {
            annotations.insert(HOSTDIR_MOUNT_KEY.to_string(), v);
        }
        meta
    });

    let req = CreateSandboxRequest {
        request_id: Uuid::new_v4().to_string(),
        instance_type: state.config.instance_type.clone(),
        timeout: Some(body.timeout),
        annotations,
        labels,
        volumes: None,
        containers: vec![],
        exposed_ports: vec![],
        network_type: Some("tap".to_string()),
        cubevs_context: build_cubevs_context(body.allow_internet_access, body.network.as_ref()),
    };

    let resp = state.cubemaster.create_sandbox(&req).await.map_err(|e| {
        tracing::error!(error = %e, "create_sandbox: cubemaster error");
        if e.is_not_found() {
            AppError::BadRequest(format!("template {} not found", body.template_id))
        } else {
            AppError::Internal(anyhow::anyhow!(e.to_string()))
        }
    })?;
    resp.ret
        .into_result()
        .map_err(|e| {
            if e.is_not_found() {
                AppError::BadRequest(format!("template {} not found", body.template_id))
            } else {
                AppError::Internal(anyhow::anyhow!(e.to_string()))
            }
        })?;

    let sandbox_id = resp.sandbox_id.clone();

    tracing::info!(sandbox_id = %sandbox_id, template_id = %body.template_id, "create_sandbox: success");
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Info, "sandbox.created")
                .field("sandbox_id", &sandbox_id)
                .field("template_id", &body.template_id),
        )
        .await;

    Ok((
        StatusCode::CREATED,
        Json(Sandbox {
            template_id: body.template_id,
            sandbox_id,
            alias: None,
            client_id: resp.request_id,
            envd_version: ENVD_VERSION.to_string(),
            envd_access_token: None,
            traffic_access_token: None,
            domain: Some(state.config.sandbox_domain.clone()),
        }),
    ))
}

// ─── DELETE /sandboxes/:sandboxID ─────────────────────────────────────────────

pub async fn kill_sandbox(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
) -> AppResult<impl IntoResponse> {
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "kill_sandbox")
                .field("sandbox_id", &sandbox_id),
        )
        .await;

    let req = DeleteSandboxRequest {
        request_id: Uuid::new_v4().to_string(),
        sandbox_id: sandbox_id.clone(),
        instance_type: state.config.instance_type.clone(),
        filter: None,
        sync: Some(true),
        annotations: None,
    };

    match state.cubemaster.delete_sandbox(&req).await {
        Ok(resp) => {
            if let Err(e) = resp.ret.into_result() {
                if e.is_not_found() {
                    return Err(AppError::NotFound(format!(
                        "sandbox {} not found",
                        sandbox_id
                    )));
                }
                return Err(AppError::Internal(anyhow::anyhow!(e.to_string())));
            }
        }
        Err(e) => return Err(AppError::Internal(anyhow::anyhow!(e.to_string()))),
    }

    tracing::info!(sandbox_id = %sandbox_id, "kill_sandbox: success");
    state
        .logger
        .log(LogEvent::new(LogLevel::Info, "sandbox.deleted").field("sandbox_id", &sandbox_id))
        .await;
    Ok(StatusCode::NO_CONTENT)
}

// ─── POST /sandboxes/:sandboxID/pause ─────────────────────────────────────────

pub async fn pause_sandbox(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
) -> AppResult<impl IntoResponse> {
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "pause_sandbox")
                .field("sandbox_id", &sandbox_id),
        )
        .await;
    tracing::info!(sandbox_id = %sandbox_id, "pause sandbox request");
    let req = SandboxUpdateRequest {
        request_id: Uuid::new_v4().to_string(),
        sandbox_id: sandbox_id.clone(),
        instance_type: state.config.instance_type.clone(),
        action: "pause".to_string(),
        timeout: None,
    };

    let resp = state
        .cubemaster
        .update_sandbox(&req)
        .await
        .map_err(|e| AppError::Internal(anyhow::anyhow!(e.to_string())))?;
    if resp.ret.ret_code != 0 && resp.ret.ret_code != 200 {
        if resp.ret.ret_code == 130404 {
            return Err(AppError::NotFound(format!(
                "sandbox {} not found",
                sandbox_id
            )));
        }
        if resp.ret.ret_code == 130409 {
            return Err(AppError::Conflict(format!(
                "sandbox {} cannot be paused",
                sandbox_id
            )));
        }
        return Err(AppError::Internal(anyhow::anyhow!("{}", resp.ret.ret_msg)));
    }

    tracing::info!(sandbox_id = %sandbox_id, "pause_sandbox: success");
    state
        .logger
        .log(LogEvent::new(LogLevel::Info, "sandbox.paused").field("sandbox_id", &sandbox_id))
        .await;
    Ok(StatusCode::NO_CONTENT)
}

// ─── POST /sandboxes/:sandboxID/resume ────────────────────────────────────────

pub async fn resume_sandbox(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    Json(body): Json<ResumedSandbox>,
) -> AppResult<impl IntoResponse> {
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "resume_sandbox")
                .field("sandbox_id", &sandbox_id)
                .field_value("timeout", body.timeout),
        )
        .await;
    tracing::info!(sandbox_id = %sandbox_id, "resume sandbox request");
    let req = SandboxUpdateRequest {
        request_id: Uuid::new_v4().to_string(),
        sandbox_id: sandbox_id.clone(),
        instance_type: state.config.instance_type.clone(),
        action: "resume".to_string(),
        timeout: Some(body.timeout),
    };

    let resp = state
        .cubemaster
        .update_sandbox(&req)
        .await
        .map_err(|e| AppError::Internal(anyhow::anyhow!(e.to_string())))?;
    if resp.ret.ret_code != 0 && resp.ret.ret_code != 200 {
        if resp.ret.ret_code == 130404 {
            return Err(AppError::NotFound(format!(
                "sandbox {} not found",
                sandbox_id
            )));
        }
        if resp.ret.ret_code == 130409 {
            return Err(AppError::Conflict(format!(
                "sandbox {} is already running",
                sandbox_id
            )));
        }
        return Err(AppError::Internal(anyhow::anyhow!("{}", resp.ret.ret_msg)));
    }

    let get_resp = state
        .cubemaster
        .get_sandbox(&sandbox_id, &state.config.instance_type)
        .await
        .map_err(|e| AppError::Internal(anyhow::anyhow!(e.to_string())))?;
    if get_resp.ret.ret_code != 0 && get_resp.ret.ret_code != 200 {
        return Err(AppError::Internal(anyhow::anyhow!(
            "{}",
            get_resp.ret.ret_msg
        )));
    }
    let d = get_resp
        .into_first_sandbox(&state.config.instance_type)
        .ok_or_else(|| AppError::Internal(anyhow::anyhow!("get_sandbox returned empty data")))?;

    tracing::info!(sandbox_id = %sandbox_id, "resume_sandbox: success");
    state
        .logger
        .log(LogEvent::new(LogLevel::Info, "sandbox.resumed").field("sandbox_id", &sandbox_id))
        .await;

    Ok((
        StatusCode::CREATED,
        Json(Sandbox {
            template_id: d.template_id,
            sandbox_id: sandbox_id.clone(),
            alias: None,
            client_id: d.host_id,
            envd_version: ENVD_VERSION.to_string(),
            envd_access_token: None,
            traffic_access_token: None,
            domain: Some(state.config.sandbox_domain.clone()),
        }),
    ))
}

// ─── POST /sandboxes/:sandboxID/connect ───────────────────────────────────────

pub async fn connect_sandbox(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    Json(body): Json<ConnectSandbox>,
) -> AppResult<impl IntoResponse> {
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "connect_sandbox")
                .field("sandbox_id", &sandbox_id)
                .field_value("timeout", body.timeout),
        )
        .await;
    tracing::info!("connect request");
    let get_resp = state
        .cubemaster
        .get_sandbox(&sandbox_id, &state.config.instance_type)
        .await
        .map_err(|e| AppError::Internal(anyhow::anyhow!(e.to_string())))?;
    if get_resp.ret.ret_code != 0 && get_resp.ret.ret_code != 200 {
        return Err(AppError::Internal(anyhow::anyhow!(
            "{}",
            get_resp.ret.ret_msg
        )));
    }
    let mut d = get_resp
        .into_first_sandbox(&state.config.instance_type)
        .ok_or_else(|| AppError::NotFound(format!("sandbox {} not found", sandbox_id)))?;

    tracing::info!(sandbox_id = %sandbox_id, status = ?d.status, "connect_sandbox: fetched sandbox");

    if d.status == crate::cubemaster::SandboxStatus::Paused {
        let req = SandboxUpdateRequest {
            request_id: Uuid::new_v4().to_string(),
            sandbox_id: sandbox_id.clone(),
            instance_type: state.config.instance_type.clone(),
            action: "resume".to_string(),
            timeout: Some(body.timeout),
        };
        let resp = state
            .cubemaster
            .update_sandbox(&req)
            .await
            .map_err(|e| AppError::Internal(anyhow::anyhow!(e.to_string())))?;
        if resp.ret.ret_code != 0 && resp.ret.ret_code != 200 {
            if resp.ret.ret_code == 130404 {
                return Err(AppError::NotFound(format!(
                    "sandbox {} not found",
                    sandbox_id
                )));
            }
            if resp.ret.ret_code == 130409 {
                return Err(AppError::Conflict(format!(
                    "sandbox {} is already running",
                    sandbox_id
                )));
            }
            return Err(AppError::Internal(anyhow::anyhow!("{}", resp.ret.ret_msg)));
        }
        let get_after = state
            .cubemaster
            .get_sandbox(&sandbox_id, &state.config.instance_type)
            .await
            .map_err(|e| AppError::Internal(anyhow::anyhow!(e.to_string())))?;
        if get_after.ret.ret_code != 0 && get_after.ret.ret_code != 200 {
            return Err(AppError::Internal(anyhow::anyhow!(
                "{}",
                get_after.ret.ret_msg
            )));
        }
        d = get_after
            .into_first_sandbox(&state.config.instance_type)
            .ok_or_else(|| AppError::Internal(anyhow::anyhow!("get_sandbox returned empty data")))?;
    }

    Ok((
        StatusCode::OK,
        Json(Sandbox {
            template_id: d.template_id.clone(),
            sandbox_id: sandbox_id.clone(),
            alias: None,
            client_id: d.host_id.clone(),
            envd_version: ENVD_VERSION.to_string(),
            envd_access_token: None,
            traffic_access_token: None,
            domain: Some(state.config.sandbox_domain.clone()),
        }),
    ))
}

// ─── POST /sandboxes/:sandboxID/snapshots ─────────────────────────────────────

pub async fn create_snapshot(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    Json(body): Json<CreateSnapshotRequest>,
) -> AppResult<impl IntoResponse> {
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "create_snapshot")
                .field("sandbox_id", &sandbox_id)
                .field("name", body.name.as_deref().unwrap_or("")),
        )
        .await;

    let snapshot_name = body
        .name
        .clone()
        .unwrap_or_else(|| Uuid::new_v4().to_string());
    let names = body
        .name
        .clone()
        .map(|n| vec![n])
        .unwrap_or_else(|| vec![snapshot_name.clone()]);

    let req = SandboxSnapshotRequest {
        request_id: Uuid::new_v4().to_string(),
        sandbox_id: sandbox_id.clone(),
        instance_type: state.config.instance_type.clone(),
        name: Some(snapshot_name.clone()),
        names: names.clone(),
        sync: false,
        timeout: Some(60),
    };

    match state.cubemaster.create_sandbox_snapshot(&req).await {
        Ok(resp) => {
            if let Err(e) = resp.ret.into_result() {
                if e.is_not_found() {
                    return Err(AppError::NotFound(format!(
                        "sandbox {} not found",
                        sandbox_id
                    )));
                }
                if e.is_conflict() {
                    return Err(AppError::Conflict(format!(
                        "sandbox {} has a snapshot operation in progress",
                        sandbox_id
                    )));
                }
                return Err(AppError::Internal(anyhow::anyhow!(e.to_string())));
            }
            let snapshot_id = resp.snapshot_id;
            let names = if resp.names.is_empty() {
                names
            } else {
                resp.names
            };
            tracing::info!(sandbox_id = %sandbox_id, snapshot_id = %snapshot_id, "create_snapshot: success");
            state
                .logger
                .log(
                    LogEvent::new(LogLevel::Info, "sandbox.snapshot.created")
                        .field("sandbox_id", &sandbox_id)
                        .field("snapshot_id", &snapshot_id),
                )
                .await;
            Ok((
                StatusCode::CREATED,
                Json(SnapshotInfo { snapshot_id, names }),
            ))
        }
        Err(ref e) if e.is_endpoint_missing() => {
            tracing::warn!(
                sandbox_id = %sandbox_id,
                "create_snapshot: CubeMaster POST /cube/sandbox/snapshot not available yet"
            );
            // Return a placeholder snapshot_id so clients aren't broken
            let snapshot_id = Uuid::new_v4().to_string();
            state
                .logger
                .log(
                    LogEvent::new(LogLevel::Warn, "sandbox.snapshot.placeholder")
                        .field("sandbox_id", &sandbox_id)
                        .field("snapshot_id", &snapshot_id)
                        .field("reason", "cubemaster endpoint not yet implemented"),
                )
                .await;
            Ok((
                StatusCode::CREATED,
                Json(SnapshotInfo { snapshot_id, names }),
            ))
        }
        Err(e) if e.is_not_found() => Err(AppError::NotFound(format!(
            "sandbox {} not found",
            sandbox_id
        ))),
        Err(e) => Err(AppError::Internal(anyhow::anyhow!(e.to_string()))),
    }
}

// ─── GET /sandboxes/:sandboxID/logs ───────────────────────────────────────────

pub async fn get_sandbox_logs(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    Query(params): Query<SandboxLogsQuery>,
) -> AppResult<impl IntoResponse> {
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "get_sandbox_logs")
                .field("sandbox_id", &sandbox_id)
                .field_value("limit", params.limit),
        )
        .await;

    let req = SandboxLogsRequest {
        request_id: Uuid::new_v4().to_string(),
        sandbox_id: sandbox_id.clone(),
        instance_type: state.config.instance_type.clone(),
        start: params.start,
        limit: params.limit,
        source: "all".to_string(),
    };

    match state.cubemaster.get_sandbox_logs(&req).await {
        Ok(resp) => {
            if let Err(e) = resp.ret.into_result() {
                if e.is_not_found() {
                    return Err(AppError::NotFound(format!(
                        "sandbox {} not found",
                        sandbox_id
                    )));
                }
                return Err(AppError::Internal(anyhow::anyhow!(e.to_string())));
            }
            let logs = SandboxLogs {
                logs: resp
                    .logs
                    .iter()
                    .map(|l| SandboxLog {
                        timestamp: l.timestamp,
                        line: l.line.clone(),
                    })
                    .collect(),
                log_entries: resp
                    .logs
                    .iter()
                    .map(|l| SandboxLogEntry {
                        timestamp: l.timestamp,
                        message: l.line.clone(),
                        level: ModelLogLevel::Info,
                        fields: {
                            let mut m = std::collections::HashMap::new();
                            m.insert("source".to_string(), l.source.clone());
                            m
                        },
                    })
                    .collect(),
            };
            state
                .logger
                .log(
                    LogEvent::new(LogLevel::Info, "api.response")
                        .field("handler", "get_sandbox_logs")
                        .field("sandbox_id", &sandbox_id)
                        .field_value("count", logs.logs.len()),
                )
                .await;
            Ok(Json(logs))
        }
        Err(ref e) if e.is_endpoint_missing() => {
            tracing::warn!(
                sandbox_id = %sandbox_id,
                "get_sandbox_logs: CubeMaster POST /cube/sandbox/logs not available yet"
            );
            Ok(Json(SandboxLogs {
                logs: vec![SandboxLog {
                    timestamp: chrono::Utc::now(),
                    line: format!("(log streaming not yet available — CubeMaster endpoint pending, see docs/cubemaster-api-requirements.md §1.6)"),
                }],
                log_entries: vec![],
            }))
        }
        Err(e) if e.is_not_found() => Err(AppError::NotFound(format!(
            "sandbox {} not found",
            sandbox_id
        ))),
        Err(e) => Err(AppError::Internal(anyhow::anyhow!(e.to_string()))),
    }
}

// ─── GET /v2/sandboxes/:sandboxID/logs ────────────────────────────────────────

pub async fn get_sandbox_logs_v2(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    Query(params): Query<SandboxLogsV2Query>,
) -> AppResult<impl IntoResponse> {
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "get_sandbox_logs_v2")
                .field("sandbox_id", &sandbox_id)
                .field_value("limit", params.limit),
        )
        .await;

    let req = SandboxLogsRequest {
        request_id: Uuid::new_v4().to_string(),
        sandbox_id: sandbox_id.clone(),
        instance_type: state.config.instance_type.clone(),
        start: params.cursor,
        limit: params.limit,
        source: "all".to_string(),
    };

    match state.cubemaster.get_sandbox_logs(&req).await {
        Ok(resp) => {
            if let Err(e) = resp.ret.into_result() {
                if e.is_not_found() {
                    return Err(AppError::NotFound(format!(
                        "sandbox {} not found",
                        sandbox_id
                    )));
                }
                return Err(AppError::Internal(anyhow::anyhow!(e.to_string())));
            }
            let logs = SandboxLogsV2Response {
                logs: resp
                    .logs
                    .iter()
                    .map(|l| SandboxLogEntry {
                        timestamp: l.timestamp,
                        message: l.line.clone(),
                        level: ModelLogLevel::Info,
                        fields: {
                            let mut m = std::collections::HashMap::new();
                            m.insert("source".to_string(), l.source.clone());
                            m
                        },
                    })
                    .collect(),
            };
            state
                .logger
                .log(
                    LogEvent::new(LogLevel::Info, "api.response")
                        .field("handler", "get_sandbox_logs_v2")
                        .field("sandbox_id", &sandbox_id)
                        .field_value("count", logs.logs.len()),
                )
                .await;
            Ok(Json(logs))
        }
        Err(ref e) if e.is_endpoint_missing() => {
            tracing::warn!(
                sandbox_id = %sandbox_id,
                "get_sandbox_logs_v2: CubeMaster endpoint not available yet"
            );
            Ok(Json(SandboxLogsV2Response {
                logs: vec![SandboxLogEntry {
                    timestamp: chrono::Utc::now(),
                    message:
                        "(log streaming pending — see docs/cubemaster-api-requirements.md §1.6)"
                            .to_string(),
                    level: ModelLogLevel::Info,
                    fields: std::collections::HashMap::new(),
                }],
            }))
        }
        Err(e) if e.is_not_found() => Err(AppError::NotFound(format!(
            "sandbox {} not found",
            sandbox_id
        ))),
        Err(e) => Err(AppError::Internal(anyhow::anyhow!(e.to_string()))),
    }
}

// ─── POST /sandboxes/:sandboxID/timeout ───────────────────────────────────────

pub async fn set_sandbox_timeout(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    Json(body): Json<SetTimeoutRequest>,
) -> AppResult<impl IntoResponse> {
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "set_sandbox_timeout")
                .field("sandbox_id", &sandbox_id)
                .field_value("timeout", body.timeout),
        )
        .await;

    let req = SandboxTimeoutRequest {
        request_id: Uuid::new_v4().to_string(),
        sandbox_id: sandbox_id.clone(),
        instance_type: state.config.instance_type.clone(),
        timeout: body.timeout,
    };

    let resp = state
        .cubemaster
        .set_sandbox_timeout(&req)
        .await
        .map_err(|e| AppError::Internal(anyhow::anyhow!(e.to_string())))?;
    resp.ret.into_result().map_err(|e| {
        if e.is_not_found() {
            AppError::NotFound(format!("sandbox {} not found", sandbox_id))
        } else {
            AppError::Internal(anyhow::anyhow!(e.to_string()))
        }
    })?;

    tracing::info!(sandbox_id = %sandbox_id, timeout = body.timeout, "set_sandbox_timeout: success");
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Info, "sandbox.timeout.updated")
                .field("sandbox_id", &sandbox_id)
                .field_value("timeout", body.timeout),
        )
        .await;
    Ok(StatusCode::NO_CONTENT)
}

// ─── POST /sandboxes/:sandboxID/refreshes ─────────────────────────────────────

pub async fn refresh_sandbox(
    State(state): State<AppState>,
    Path(sandbox_id): Path<String>,
    Json(body): Json<RefreshRequest>,
) -> AppResult<impl IntoResponse> {
    let duration = body.duration.unwrap_or(0);
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Debug, "api.request")
                .field("handler", "refresh_sandbox")
                .field("sandbox_id", &sandbox_id)
                .field_value("duration", duration),
        )
        .await;

    let req = SandboxRefreshRequest {
        request_id: Uuid::new_v4().to_string(),
        sandbox_id: sandbox_id.clone(),
        instance_type: state.config.instance_type.clone(),
        duration,
    };

    let resp = state
        .cubemaster
        .refresh_sandbox(&req)
        .await
        .map_err(|e| AppError::Internal(anyhow::anyhow!(e.to_string())))?;
    resp.ret.into_result().map_err(|e| {
        if e.is_not_found() {
            AppError::NotFound(format!("sandbox {} not found", sandbox_id))
        } else {
            AppError::Internal(anyhow::anyhow!(e.to_string()))
        }
    })?;

    tracing::info!(sandbox_id = %sandbox_id, duration = duration, "refresh_sandbox: success");
    state
        .logger
        .log(
            LogEvent::new(LogLevel::Info, "sandbox.refreshed")
                .field("sandbox_id", &sandbox_id)
                .field_value("duration", duration),
        )
        .await;
    Ok(StatusCode::NO_CONTENT)
}

// ─── Helpers ────────────────────────────────────────────────────────────────

/// Build a CubeVSContext from the two network-related fields in NewSandbox:
/// - `allow_internet_access`: top-level shorthand
/// - `network`: full SandboxNetworkConfig (allowPublicTraffic, allowOut, denyOut)
///
/// Merge rule: `network` fields take precedence; `allow_internet_access` is a
/// fallback for `allowPublicTraffic` when `network` is absent or doesn't set it.
fn build_cubevs_context(
    allow_internet_access: Option<bool>,
    network: Option<&crate::models::SandboxNetworkConfig>,
) -> Option<CubeVSContext> {
    let effective_allow = network
        .and_then(|n| n.allow_public_traffic)
        .or(allow_internet_access);
    let allow_out = network
        .and_then(|n| n.allow_out.clone())
        .unwrap_or_default();
    let deny_out = network
        .and_then(|n| n.deny_out.clone())
        .unwrap_or_default();

    if effective_allow.is_none() && allow_out.is_empty() && deny_out.is_empty() {
        return None;
    }

    Some(CubeVSContext {
        allow_internet_access: effective_allow,
        allow_out,
        deny_out,
    })
}
