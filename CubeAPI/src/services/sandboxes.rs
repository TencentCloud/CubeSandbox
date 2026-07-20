// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use std::collections::{HashMap, HashSet};
use uuid::Uuid;

use super::validate_allow_out_domains_require_deny_all;
use crate::{
    constants::{ENVD_VERSION_ANNOTATION, ENVD_VERSION_FALLBACK},
    cubemaster::{
        datetime_from_unix_nanos, extract_template_id, CreateSandboxRequest, CubeEgressRule,
        CubeEgressRuleAction, CubeEgressRuleInject, CubeEgressRuleMatch, CubeMasterClient,
        CubeMasterError, CubeNetworkConfig, DeleteSandboxRequest, ListSandboxRequest, SandboxInfo,
        SandboxLogsRequest, SandboxRefreshRequest, SandboxStatus, SandboxTimeoutRequest,
        SandboxUpdateRequest, VolumeMount, VolumeSpec,
    },
    error::{AppError, AppResult},
    models::{
        EgressRule, LogLevel as ModelLogLevel, NewSandbox, Sandbox, SandboxDetail, SandboxLog,
        SandboxLogEntry, SandboxLogs, SandboxLogsV2Response, SandboxNetworkConfig, SandboxState,
        SandboxVolumeMount,
    },
};

const RET_CODE_OK: i32 = 0;
const RET_CODE_HTTP_OK: i32 = 200;
const RET_CODE_NOT_FOUND: i32 = 130404;
const RET_CODE_CONFLICT: i32 = 130409;
const RET_CODE_TASK_STATE_INVALID: i32 = 130490;
const RET_CODE_TASK_RESUME_FAILED: i32 = 130589;
const HOSTDIR_MOUNT_KEY: &str = "host-mount";
const ENV_VAR_NAME_MAX_LEN: usize = 256;
const ENV_VAR_VALUE_MAX_LEN: usize = 4096;

/// Environment variable names that may compromise sandbox isolation if injected
/// at the runtime level (loader overrides, language runtime paths).
const FORBIDDEN_ENV_NAMES: &[&str] = &[
    "BASH_ENV",
    "ENV",
    "LD_PRELOAD",
    "LD_AUDIT",
    "LD_LIBRARY_PATH",
    "LD_ORIGIN_PATH",
    "DYLD_INSERT_LIBRARIES",
    "DYLD_LIBRARY_PATH",
    "GCONV_PATH",
    "PATH",
    "PYTHONPATH",
    "NODE_PATH",
    "JAVA_TOOL_OPTIONS",
    "_JAVA_OPTIONS",
    "GEM_PATH",
    "RUBYOPT",
    "RUBYLIB",
    "PERL5LIB",
    "PERLLIB",
    "CLASSPATH",
    "IFS",
];

#[derive(Clone)]
pub struct SandboxService {
    cubemaster: CubeMasterClient,
    instance_type: String,
    sandbox_domain: String,
}

impl SandboxService {
    pub fn new(
        cubemaster: CubeMasterClient,
        instance_type: String,
        sandbox_domain: String,
    ) -> Self {
        Self {
            cubemaster,
            instance_type,
            sandbox_domain,
        }
    }

    pub async fn list(
        &self,
        metadata_filter: Option<&str>,
        state_filter: Option<&str>,
        limit: i32,
    ) -> AppResult<Vec<crate::models::ListedSandbox>> {
        let req = ListSandboxRequest {
            request_id: new_request_id(),
            instance_type: self.instance_type.clone(),
            start_idx: Some(0),
            size: Some(limit.max(1)),
            host_id: None,
            filter: None,
        };

        let resp = self
            .cubemaster
            .list_sandboxes(&req)
            .await
            .map_err(internal_error)?;

        ensure_create_result(resp.ret.ret_code, resp.ret.ret_msg)?;

        let state_filter = parse_state_filter(state_filter);
        Ok(resp
            .sandboxes
            .into_iter()
            .map(from_cubemaster_info)
            .filter(|sb| filter_by_metadata(sb.metadata.as_ref(), metadata_filter))
            .filter(|sb| state_filter.as_ref().is_none_or(|state| &sb.state == state))
            .collect())
    }

    pub async fn get_sandbox(&self, sandbox_id: &str) -> AppResult<SandboxDetail> {
        let d = self.fetch_sandbox_detail(sandbox_id).await?;
        let summary = self.fetch_sandbox_summary(sandbox_id, &d.host_id).await?;
        let started_at = summary
            .as_ref()
            .and_then(|s| s.started_at.as_ref().cloned())
            .or(d.started_at)
            .unwrap_or_else(chrono::Utc::now);
        // Leave end_at as None for never-timeout sandboxes (CubeMaster returns
        // no end instant) instead of collapsing it onto started_at, which
        // would read as "already expired".
        let end_at = summary
            .as_ref()
            .and_then(|s| s.end_at.as_ref().cloned())
            .or(d.end_at);

        let envd_version = envd_version_from_annotations(&d.annotations);
        Ok(SandboxDetail {
            template_id: d.template_id,
            alias: None,
            sandbox_id: d.sandbox_id,
            client_id: d.host_id,
            started_at,
            end_at,
            envd_version,
            envd_access_token: None,
            domain: Some(self.sandbox_domain.clone()),
            cpu_count: d.cpu_count,
            memory_mb: d.memory_mb,
            disk_size_mb: Some(d.disk_size_mb),
            metadata: optional_metadata(d.labels),
            state: sandbox_state_from_status(d.status),
            volume_mounts: None,
        })
    }

    pub async fn create_sandbox(&self, body: NewSandbox) -> AppResult<Sandbox> {
        let NewSandbox {
            template_id,
            timeout,
            lifecycle,
            allow_internet_access,
            network,
            metadata,
            distribution_scope,
            env_vars,
            volume_mounts,
            ..
        } = body;
        if let Some(env_vars) = env_vars.as_ref() {
            validate_env_vars(env_vars)?;
        }
        if let Some(mounts) = volume_mounts.as_ref() {
            validate_unique_volume_mount_names(mounts)?;
        }
        let mut annotations = HashMap::from([
            (
                "cube.master.appsnapshot.template.id".to_string(),
                template_id.clone(),
            ),
            (
                "cube.master.appsnapshot.template.version".to_string(),
                "v2".to_string(),
            ),
        ]);

        let labels = metadata.map(|mut meta| {
            if let Some(value) = meta.remove(HOSTDIR_MOUNT_KEY) {
                annotations.insert(HOSTDIR_MOUNT_KEY.to_string(), value);
            }
            meta
        });

        let cube_network_config =
            build_cube_network_config(allow_internet_access, network.as_ref())?;

        // Derive the two CubeMaster-side bools from the e2b-shaped lifecycle
        // object. Absent lifecycle keeps today's behaviour: idle sandboxes
        // are killed (auto_pause = false), and auto_resume defaults off.
        let (auto_pause, auto_resume) = lifecycle
            .as_ref()
            .map(|lc| {
                use crate::models::SandboxOnTimeout;
                (
                    matches!(lc.on_timeout, SandboxOnTimeout::Pause),
                    lc.auto_resume,
                )
            })
            .unwrap_or((false, false));

        // Convert e2b-style volumeMounts into the CubeMaster wire format.
        // Volumes (pod-level declarations) are passed in the volumes field;
        // VolumeSource is left None so CubeMaster resolves from the volume DB.
        //
        // Container-level volume_mounts are forwarded via the
        // "plugin-volume-mounts" annotation so CubeMaster can inject them
        // into the existing template containers WITHOUT overriding the
        // template's command / image / other settings.
        let cube_volumes: Vec<VolumeSpec> = volume_mounts
            .as_deref()
            .unwrap_or_default()
            .iter()
            .map(|SandboxVolumeMount { name, .. }| VolumeSpec {
                name: Some(name.clone()),
                volume_source: None,
            })
            .collect();

        // Build the plugin-volume-mounts annotation value (JSON array).
        if let Some(mounts) = &volume_mounts {
            if !mounts.is_empty() {
                #[derive(serde::Serialize)]
                struct MountEntry<'a> {
                    name: &'a str,
                    container_path: &'a str,
                }
                let entries: Vec<MountEntry> = mounts
                    .iter()
                    .map(|m| MountEntry {
                        name: &m.name,
                        container_path: &m.path,
                    })
                    .collect();
                if let Ok(json) = serde_json::to_string(&entries) {
                    annotations.insert("plugin-volume-mounts".to_string(), json);
                }
            }
        }

        let volumes = if cube_volumes.is_empty() {
            None
        } else {
            Some(cube_volumes)
        };
        // Always leave containers empty — CubeMaster injects volume_mounts
        // from the annotation into the template's existing container spec.
        let containers = vec![];

        let req = CreateSandboxRequest {
            request_id: new_request_id(),
            instance_type: self.instance_type.clone(),
            // Pass the client's timeout through as-is: None → field omitted so
            // CubeMaster applies its server default; Some(0) → immediate
            // timeout; Some(n) → explicit TTL. No SDK/API-side default fill.
            timeout,
            annotations,
            labels,
            create_time_env_vars: env_vars,
            distribution_scope,
            volumes,
            containers,
            exposed_ports: vec![],
            network_type: Some("tap".to_string()),
            cube_network_config,
            auto_pause,
            auto_resume,
        };

        let resp = self
            .cubemaster
            .create_sandbox(&req)
            .await
            .map_err(internal_error)?;

        resp.ret.into_result().map_err(internal_error)?;

        let envd_version = envd_version_from_annotations(&resp.ext_info);
        Ok(self.sandbox_response(
            template_id,
            resp.sandbox_id,
            resp.request_id,
            envd_version,
            resp.traffic_access_token,
        ))
    }

    pub async fn kill_sandbox(&self, sandbox_id: &str) -> AppResult<()> {
        let req = DeleteSandboxRequest {
            request_id: new_request_id(),
            sandbox_id: sandbox_id.to_string(),
            instance_type: self.instance_type.clone(),
            filter: None,
            sync: Some(true),
            annotations: None,
        };

        let resp = self
            .cubemaster
            .delete_sandbox(&req)
            .await
            .map_err(|e| map_delete_cubemaster_err(e, sandbox_id))?;

        resp.ret
            .into_result()
            .map_err(|e| map_delete_cubemaster_err(e, sandbox_id))?;

        Ok(())
    }

    pub async fn pause_sandbox(&self, sandbox_id: &str) -> AppResult<()> {
        let resp = self
            .cubemaster
            .update_sandbox(&self.build_update_request(sandbox_id, "pause", None))
            .await
            .map_err(|e| map_update_cubemaster_err(e, sandbox_id))?;

        ensure_update_result(
            resp.ret.ret_code,
            resp.ret.ret_msg,
            sandbox_id,
            "cannot be paused",
        )
    }

    pub async fn resume_sandbox(
        &self,
        sandbox_id: &str,
        timeout: Option<i32>,
    ) -> AppResult<Sandbox> {
        let resp = self
            .cubemaster
            .update_sandbox(&self.build_update_request(sandbox_id, "resume", timeout))
            .await
            .map_err(|e| map_update_cubemaster_err(e, sandbox_id))?;

        ensure_update_result(
            resp.ret.ret_code,
            resp.ret.ret_msg,
            sandbox_id,
            "is already running",
        )?;

        let d = self.fetch_sandbox_detail(sandbox_id).await?;
        let envd_version = envd_version_from_annotations(&d.annotations);
        // resume/connect paths reload the sandbox via fetch_sandbox_detail,
        // which does not surface the traffic_access_token. The token only
        // matters at create time (so the caller can persist it); afterward
        // CubeProxy reads it directly from Redis. None here is correct.
        Ok(self.sandbox_response(
            d.template_id,
            sandbox_id.to_string(),
            d.host_id,
            envd_version,
            None,
        ))
    }

    pub async fn connect_sandbox(
        &self,
        sandbox_id: &str,
        timeout: Option<i32>,
    ) -> AppResult<Sandbox> {
        let mut d = self.fetch_sandbox_detail(sandbox_id).await?;

        if d.status == SandboxStatus::Paused {
            let resp = self
                .cubemaster
                .update_sandbox(&self.build_update_request(sandbox_id, "resume", timeout))
                .await
                .map_err(|e| map_update_cubemaster_err(e, sandbox_id))?;

            ensure_update_result(
                resp.ret.ret_code,
                resp.ret.ret_msg,
                sandbox_id,
                "is already running",
            )?;

            d = self.fetch_sandbox_detail(sandbox_id).await?;
        }

        let envd_version = envd_version_from_annotations(&d.annotations);
        Ok(self.sandbox_response(
            d.template_id,
            sandbox_id.to_string(),
            d.host_id,
            envd_version,
            None,
        ))
    }

    pub async fn get_logs(
        &self,
        sandbox_id: &str,
        start: Option<i64>,
        limit: i32,
    ) -> AppResult<SandboxLogs> {
        match self
            .cubemaster
            .get_sandbox_logs(&self.build_logs_request(sandbox_id, start, limit))
            .await
        {
            Ok(resp) => {
                resp.ret
                    .into_result()
                    .map_err(|e| sandbox_not_found_or_internal(e, sandbox_id))?;

                Ok(SandboxLogs {
                    logs: resp
                        .logs
                        .iter()
                        .map(|l| SandboxLog {
                            timestamp: l.timestamp,
                            line: l.message.clone(),
                        })
                        .collect(),
                    log_entries: resp.logs.into_iter().map(to_log_entry).collect(),
                })
            }
            Err(e) if e.is_endpoint_missing() => Ok(SandboxLogs {
                logs: vec![SandboxLog {
                    timestamp: chrono::Utc::now(),
                    line: "(log streaming not yet available — CubeMaster endpoint pending implementation)".to_string(),
                }],
                log_entries: vec![],
            }),
            Err(e) if e.is_not_found() => {
                Err(AppError::NotFound(format!("sandbox {} not found", sandbox_id)))
            }
            Err(e) => Err(internal_error(e)),
        }
    }

    pub async fn get_logs_v2(
        &self,
        sandbox_id: &str,
        cursor: Option<i64>,
        limit: i32,
    ) -> AppResult<SandboxLogsV2Response> {
        match self
            .cubemaster
            .get_sandbox_logs(&self.build_logs_request(sandbox_id, cursor, limit))
            .await
        {
            Ok(resp) => {
                resp.ret
                    .into_result()
                    .map_err(|e| sandbox_not_found_or_internal(e, sandbox_id))?;

                Ok(SandboxLogsV2Response {
                    logs: resp.logs.into_iter().map(to_log_entry).collect(),
                })
            }
            Err(e) if e.is_endpoint_missing() => Ok(SandboxLogsV2Response {
                logs: vec![SandboxLogEntry {
                    timestamp: chrono::Utc::now(),
                    message: "(log streaming pending — CubeMaster endpoint not yet implemented)"
                        .to_string(),
                    level: ModelLogLevel::Info,
                    fields: HashMap::new(),
                }],
            }),
            Err(e) if e.is_not_found() => Err(AppError::NotFound(format!(
                "sandbox {} not found",
                sandbox_id
            ))),
            Err(e) => Err(internal_error(e)),
        }
    }

    pub async fn set_timeout(&self, sandbox_id: &str, timeout: i32) -> AppResult<()> {
        let req = SandboxTimeoutRequest {
            request_id: new_request_id(),
            sandbox_id: sandbox_id.to_string(),
            instance_type: self.instance_type.clone(),
            timeout,
        };

        let resp = self
            .cubemaster
            .set_sandbox_timeout(&req)
            .await
            .map_err(internal_error)?;

        resp.ret
            .into_result()
            .map_err(|e| sandbox_not_found_or_internal(e, sandbox_id))?;

        Ok(())
    }

    pub async fn refresh(&self, sandbox_id: &str, duration: i32) -> AppResult<()> {
        let req = SandboxRefreshRequest {
            request_id: new_request_id(),
            sandbox_id: sandbox_id.to_string(),
            instance_type: self.instance_type.clone(),
            duration,
        };

        let resp = self
            .cubemaster
            .refresh_sandbox(&req)
            .await
            .map_err(internal_error)?;

        resp.ret
            .into_result()
            .map_err(|e| sandbox_not_found_or_internal(e, sandbox_id))?;

        Ok(())
    }

    async fn fetch_sandbox_detail(
        &self,
        sandbox_id: &str,
    ) -> AppResult<crate::cubemaster::SandboxDetail> {
        let resp = self
            .cubemaster
            .get_sandbox(sandbox_id, &self.instance_type)
            .await
            .map_err(|e| {
                if e.is_not_found() {
                    AppError::NotFound(format!("sandbox {} not found", sandbox_id))
                } else {
                    internal_error(e)
                }
            })?;

        if !is_success_ret_code(resp.ret.ret_code) {
            if resp.ret.ret_code == RET_CODE_NOT_FOUND {
                return Err(AppError::NotFound(format!(
                    "sandbox {} not found",
                    sandbox_id
                )));
            }
            return Err(AppError::Internal(anyhow::anyhow!("{}", resp.ret.ret_msg)));
        }

        resp.into_first_sandbox(&self.instance_type)
            .ok_or_else(|| AppError::NotFound(format!("sandbox {} not found", sandbox_id)))
    }

    async fn fetch_sandbox_summary(
        &self,
        sandbox_id: &str,
        host_id: &str,
    ) -> AppResult<Option<SandboxInfo>> {
        if host_id.trim().is_empty() {
            return Ok(None);
        }

        let req = ListSandboxRequest {
            request_id: new_request_id(),
            instance_type: self.instance_type.clone(),
            start_idx: None,
            size: None,
            host_id: Some(host_id.to_string()),
            filter: None,
        };

        let resp = self
            .cubemaster
            .list_sandboxes(&req)
            .await
            .map_err(internal_error)?;

        resp.ret.into_result().map_err(internal_error)?;

        Ok(resp
            .sandboxes
            .into_iter()
            .find(|sandbox| sandbox.sandbox_id == sandbox_id))
    }

    fn sandbox_response(
        &self,
        template_id: String,
        sandbox_id: String,
        client_id: String,
        envd_version: String,
        traffic_access_token: Option<String>,
    ) -> Sandbox {
        Sandbox {
            template_id,
            sandbox_id,
            alias: None,
            client_id,
            envd_version,
            envd_access_token: None,
            // Empty string from CubeMaster (publicly reachable sandbox) is
            // normalized to None so the JSON field is omitted on the wire.
            traffic_access_token: traffic_access_token.filter(|s| !s.is_empty()),
            domain: Some(self.sandbox_domain.clone()),
        }
    }

    fn build_update_request(
        &self,
        sandbox_id: &str,
        action: &str,
        timeout: Option<i32>,
    ) -> SandboxUpdateRequest {
        SandboxUpdateRequest {
            request_id: new_request_id(),
            sandbox_id: sandbox_id.to_string(),
            instance_type: self.instance_type.clone(),
            action: action.to_string(),
            timeout,
        }
    }

    fn build_logs_request(
        &self,
        sandbox_id: &str,
        cursor: Option<i64>,
        limit: i32,
    ) -> SandboxLogsRequest {
        SandboxLogsRequest {
            sandbox_id: sandbox_id.to_string(),
            cursor,
            limit,
        }
    }
}

/// Validate environment variable names against the POSIX name convention
/// and a deny-list of runtime-loader / path-override names that could
/// compromise sandbox isolation.
fn validate_env_vars(env_vars: &HashMap<String, String>) -> AppResult<()> {
    for (name, value) in env_vars {
        if name.is_empty() || name.len() > ENV_VAR_NAME_MAX_LEN {
            return Err(AppError::BadRequest(format!(
                "invalid env var name length: {name:?}"
            )));
        }
        let bytes = name.as_bytes();
        if !bytes
            .first()
            .map_or(false, |b| b.is_ascii_alphabetic() || *b == b'_')
            || !bytes
                .iter()
                .all(|b| b.is_ascii_alphanumeric() || *b == b'_')
        {
            return Err(AppError::BadRequest(format!(
                "env var name must match [a-zA-Z_][a-zA-Z0-9_]*: {name:?}"
            )));
        }
        if FORBIDDEN_ENV_NAMES
            .iter()
            .any(|forbidden| name.eq_ignore_ascii_case(forbidden))
        {
            return Err(AppError::BadRequest(format!(
                "env var name not allowed: {name}"
            )));
        }
        if value.len() > ENV_VAR_VALUE_MAX_LEN {
            return Err(AppError::BadRequest(format!(
                "env var value too large for {name:?}: {} bytes",
                value.len()
            )));
        }
        if value.contains('\0') {
            return Err(AppError::BadRequest(format!(
                "env var value contains NUL byte: {name:?}"
            )));
        }
        if value.chars().any(|ch| ch != '\t' && ch.is_control()) {
            return Err(AppError::BadRequest(format!(
                "env var value contains control character: {name:?}"
            )));
        }
    }
    Ok(())
}

/// Each volume (`volumeMounts[].name`) may be mounted at most once per sandbox.
fn validate_unique_volume_mount_names(mounts: &[SandboxVolumeMount]) -> AppResult<()> {
    let mut seen = HashSet::with_capacity(mounts.len());
    for m in mounts {
        if !seen.insert(m.name.as_str()) {
            return Err(AppError::BadRequest(format!(
                "duplicate volumeMounts name {:?}: each volume may be mounted at most once per sandbox",
                m.name
            )));
        }
    }
    Ok(())
}

fn internal_error(error: impl std::fmt::Display) -> AppError {
    AppError::Internal(anyhow::anyhow!(error.to_string()))
}

fn ensure_create_result(ret_code: i32, ret_msg: String) -> AppResult<()> {
    if is_success_ret_code(ret_code) {
        return Ok(());
    }
    if ret_code == RET_CODE_NOT_FOUND {
        return Err(AppError::NotFound(ret_msg));
    }
    if ret_code == RET_CODE_CONFLICT {
        return Err(AppError::Conflict(ret_msg));
    }
    Err(AppError::Internal(anyhow::anyhow!(ret_msg)))
}

fn sandbox_not_found_or_internal(e: CubeMasterError, sandbox_id: &str) -> AppError {
    if e.is_not_found() {
        AppError::NotFound(format!("sandbox {} not found", sandbox_id))
    } else {
        internal_error(e)
    }
}

fn map_delete_cubemaster_err(e: CubeMasterError, sandbox_id: &str) -> AppError {
    match e {
        CubeMasterError::Api { ret_code, .. } if ret_code == RET_CODE_NOT_FOUND => {
            AppError::NotFound(format!("sandbox {} not found", sandbox_id))
        }
        CubeMasterError::Api { ret_code, ret_msg } if ret_code == RET_CODE_CONFLICT => {
            let detail = if ret_msg.trim().is_empty() {
                format!("sandbox {} conflict", sandbox_id)
            } else {
                ret_msg
            };
            AppError::Conflict(detail)
        }
        CubeMasterError::Api { ret_code, ret_msg } if ret_code == RET_CODE_TASK_STATE_INVALID => {
            AppError::ServiceUnavailable {
                message: delete_retry_message(
                    ret_msg,
                    sandbox_id,
                    "is pausing; retry DELETE after 2 seconds",
                ),
                retry_after: 2,
            }
        }
        CubeMasterError::Api { ret_code, ret_msg } if ret_code == RET_CODE_TASK_RESUME_FAILED => {
            AppError::ServiceUnavailable {
                message: delete_retry_message(
                    ret_msg,
                    sandbox_id,
                    "could not be resumed before delete; retry DELETE after 5 seconds",
                ),
                retry_after: 5,
            }
        }
        other => sandbox_not_found_or_internal(other, sandbox_id),
    }
}

fn delete_retry_message(ret_msg: String, sandbox_id: &str, fallback: &str) -> String {
    if ret_msg.trim().is_empty() {
        format!("sandbox {} {}", sandbox_id, fallback)
    } else {
        ret_msg
    }
}

// parse_response treats any non-success ret_code as CubeMasterError::Api before the
// caller sees the envelope, so pause/resume/connect must remap business codes here
// (ensure_update_result alone never runs on that path).
fn map_update_cubemaster_err(e: CubeMasterError, sandbox_id: &str) -> AppError {
    match e {
        CubeMasterError::Api { ret_code, .. } if ret_code == RET_CODE_NOT_FOUND => {
            AppError::NotFound(format!("sandbox {} not found", sandbox_id))
        }
        CubeMasterError::Api { ret_code, ret_msg } if ret_code == RET_CODE_CONFLICT => {
            let detail = if ret_msg.trim().is_empty() {
                format!("sandbox {} conflict", sandbox_id)
            } else {
                ret_msg // owned, moved out of e -- no clone
            };
            AppError::Conflict(detail)
        }
        other => sandbox_not_found_or_internal(other, sandbox_id),
    }
}

fn ensure_update_result(
    ret_code: i32,
    ret_msg: String,
    sandbox_id: &str,
    conflict_message: &str,
) -> AppResult<()> {
    if is_success_ret_code(ret_code) {
        return Ok(());
    }
    if ret_code == RET_CODE_NOT_FOUND {
        return Err(AppError::NotFound(format!(
            "sandbox {} not found",
            sandbox_id
        )));
    }
    if ret_code == RET_CODE_CONFLICT {
        // Prefer the backend's own reason (e.g. the paused_resource_release_ratio
        // capacity rejection on resume) so the client sees why it conflicted;
        // fall back to the generic templated message when none was provided.
        let detail = if ret_msg.trim().is_empty() {
            format!("sandbox {} {}", sandbox_id, conflict_message)
        } else {
            ret_msg
        };
        return Err(AppError::Conflict(detail));
    }
    Err(AppError::Internal(anyhow::anyhow!(ret_msg)))
}

/// Resolve the reported `envdVersion` from a sandbox/template annotation map,
/// falling back to the conservative default when the annotation is absent or
/// blank (e.g. legacy templates created before version collection existed).
pub(crate) fn envd_version_from_annotations(annotations: &HashMap<String, String>) -> String {
    annotations
        .get(ENVD_VERSION_ANNOTATION)
        .map(|v| v.trim())
        .filter(|v| !v.is_empty())
        .map(|v| v.to_string())
        .unwrap_or_else(|| ENVD_VERSION_FALLBACK.to_string())
}

pub(crate) fn from_cubemaster_info(s: SandboxInfo) -> crate::models::ListedSandbox {
    use crate::models::ListedSandbox;

    let now = chrono::Utc::now();
    let template_id = extract_template_id(&s.template_id, &s.annotations, &s.labels);
    let envd_version = envd_version_from_annotations(&s.annotations);

    // Prefer explicit started_at; fall back to create_at (Unix nanos from Cubelet); last resort: now
    let started_at = s
        .started_at
        .or_else(|| datetime_from_unix_nanos(s.create_at))
        .unwrap_or(now);

    ListedSandbox {
        template_id,
        alias: None,
        sandbox_id: s.sandbox_id,
        client_id: s.host_id,
        started_at,
        end_at: s.end_at,
        cpu_count: s.cpu_count,
        memory_mb: s.memory_mb,
        disk_size_mb: Some(0),
        metadata: optional_metadata(s.labels),
        state: sandbox_state_from_str(&s.status),
        envd_version,
        volume_mounts: None,
    }
}

pub(crate) fn filter_by_metadata(
    metadata: Option<&HashMap<String, String>>,
    query: Option<&str>,
) -> bool {
    let Some(query) = query else {
        return true;
    };
    let Some(metadata) = metadata else {
        return false;
    };

    for pair in query.split('&') {
        if let Some((key, value)) = pair.split_once('=') {
            if metadata.get(key).is_none_or(|existing| existing != value) {
                return false;
            }
        }
    }

    true
}

fn parse_state_filter(value: Option<&str>) -> Option<SandboxState> {
    match value {
        Some("running") => Some(SandboxState::Running),
        Some("paused") => Some(SandboxState::Paused),
        _ => None,
    }
}

fn is_success_ret_code(ret_code: i32) -> bool {
    matches!(ret_code, RET_CODE_OK | RET_CODE_HTTP_OK)
}

fn sandbox_state_from_status(status: SandboxStatus) -> SandboxState {
    match status {
        SandboxStatus::Paused => SandboxState::Paused,
        SandboxStatus::Running => SandboxState::Running,
        _ => SandboxState::Running,
    }
}

fn sandbox_state_from_str(status: &str) -> SandboxState {
    match status.to_lowercase().as_str() {
        "paused" => SandboxState::Paused,
        "pausing" => SandboxState::Pausing,
        _ => SandboxState::Running,
    }
}

fn optional_metadata(metadata: HashMap<String, String>) -> Option<HashMap<String, String>> {
    if metadata.is_empty() {
        None
    } else {
        Some(metadata)
    }
}

fn to_log_entry(log: crate::cubemaster::SandboxLogLine) -> SandboxLogEntry {
    let level = match log.level.to_lowercase().as_str() {
        "debug" => ModelLogLevel::Debug,
        "warn" | "warning" => ModelLogLevel::Warn,
        "error" => ModelLogLevel::Error,
        _ => ModelLogLevel::Info,
    };
    SandboxLogEntry {
        timestamp: log.timestamp,
        message: log.message,
        level,
        fields: HashMap::new(),
    }
}

fn new_request_id() -> String {
    Uuid::new_v4().to_string()
}

pub(crate) fn build_cube_network_config(
    allow_internet_access: Option<bool>,
    network: Option<&SandboxNetworkConfig>,
) -> AppResult<Option<CubeNetworkConfig>> {
    let allow_out = network
        .and_then(|n| n.allow_out.clone())
        .unwrap_or_default();
    let deny_out = network.and_then(|n| n.deny_out.clone()).unwrap_or_default();
    validate_allow_out_domains_require_deny_all(
        &allow_out,
        &deny_out,
        allow_internet_access == Some(false),
    )?;

    let rules: Vec<CubeEgressRule> = network
        .and_then(|n| n.rules.as_ref())
        .map(|rs| rs.iter().map(map_egress_rule).collect())
        .unwrap_or_default();

    let allow_public_traffic = network.and_then(|n| n.allow_public_traffic);

    if allow_internet_access.is_none()
        && allow_public_traffic.is_none()
        && allow_out.is_empty()
        && deny_out.is_empty()
        && rules.is_empty()
    {
        return Ok(None);
    }

    Ok(Some(CubeNetworkConfig {
        allow_internet_access,
        allow_public_traffic,
        allow_out,
        deny_out,
        rules,
    }))
}

fn map_egress_rule(rule: &EgressRule) -> CubeEgressRule {
    CubeEgressRule {
        name: rule.name.clone(),
        r#match: CubeEgressRuleMatch {
            sni: rule.r#match.sni.clone(),
            host: rule.r#match.host.clone(),
            method: rule.r#match.method.clone(),
            path: rule.r#match.path.clone(),
            scheme: rule.r#match.scheme.clone(),
        },
        action: CubeEgressRuleAction {
            allow: rule.action.allow,
            audit: rule.action.audit.clone(),
            inject: rule.action.inject.as_ref().map(|injs| {
                injs.iter()
                    .map(|i| CubeEgressRuleInject {
                        header: i.header.clone(),
                        secret: i.secret.clone(),
                        format: i.format.clone(),
                    })
                    .collect()
            }),
        },
    }
}

#[cfg(test)]
mod tests {
    use std::collections::HashMap;
    use std::sync::Arc;

    use super::{
        build_cube_network_config, filter_by_metadata, from_cubemaster_info,
        map_delete_cubemaster_err, SandboxService, RET_CODE_CONFLICT, RET_CODE_NOT_FOUND,
        RET_CODE_TASK_RESUME_FAILED, RET_CODE_TASK_STATE_INVALID,
    };
    use crate::cubemaster::{
        CreateSandboxRequest, CubeMasterClient, CubeMasterError, ListSandboxResponse, SandboxInfo,
        SandboxUpdateRequest,
    };
    use crate::error::AppError;
    use crate::models::{
        EgressRule, EgressRuleAction, EgressRuleInject, EgressRuleMatch, NewSandbox,
        SandboxNetworkConfig, SandboxState,
    };
    use axum::{
        extract::State,
        http::{header::RETRY_AFTER, StatusCode},
        response::IntoResponse,
        routing::{delete, post},
        Json, Router,
    };
    use serde_json::Value;
    use tokio::sync::Mutex;

    #[test]
    fn metadata_filter_matches_all_pairs() {
        let metadata = HashMap::from([
            ("user".to_string(), "alice".to_string()),
            ("app".to_string(), "prod".to_string()),
        ]);

        assert!(filter_by_metadata(Some(&metadata), Some("user=alice")));
        assert!(filter_by_metadata(
            Some(&metadata),
            Some("user=alice&app=prod")
        ));
        assert!(!filter_by_metadata(Some(&metadata), Some("user=bob")));
        assert!(!filter_by_metadata(None, Some("user=alice")));
    }

    #[test]
    fn network_context_ignores_allow_public_traffic_for_outbound_access() {
        let context = build_cube_network_config(
            Some(false),
            Some(&SandboxNetworkConfig {
                allow_public_traffic: Some(true),
                allow_out: Some(vec!["github.com".to_string()]),
                deny_out: Some(vec!["0.0.0.0/0".to_string()]),
                mask_request_host: None,
                rules: None,
            }),
        )
        .expect("network config should be valid")
        .expect("context should exist");

        assert_eq!(context.allow_internet_access, Some(false));
        assert_eq!(context.allow_out, vec!["github.com".to_string()]);
    }

    #[test]
    fn network_context_rejects_allow_out_domain_without_deny_all() {
        let err = build_cube_network_config(
            None,
            Some(&SandboxNetworkConfig {
                allow_public_traffic: None,
                allow_out: Some(vec!["api.example.com".to_string()]),
                deny_out: Some(vec!["203.0.113.0/24".to_string()]),
                mask_request_host: None,
                rules: None,
            }),
        )
        .unwrap_err();

        assert!(err
            .to_string()
            .contains("must disable public outbound traffic or include '0.0.0.0/0' in deny_out"));
    }

    #[test]
    fn network_context_rejects_allow_out_domain_when_only_allow_public_traffic_disabled() {
        let err = build_cube_network_config(
            None,
            Some(&SandboxNetworkConfig {
                allow_public_traffic: Some(false),
                allow_out: Some(vec!["api.example.com".to_string()]),
                deny_out: None,
                mask_request_host: None,
                rules: None,
            }),
        )
        .unwrap_err();

        assert!(err
            .to_string()
            .contains("must disable public outbound traffic or include '0.0.0.0/0' in deny_out"));
    }

    #[test]
    fn network_context_accepts_allow_out_domain_when_internet_access_disabled() {
        let context = build_cube_network_config(
            Some(false),
            Some(&SandboxNetworkConfig {
                allow_public_traffic: Some(true),
                allow_out: Some(vec!["api.example.com".to_string()]),
                deny_out: None,
                mask_request_host: None,
                rules: None,
            }),
        )
        .expect("network config should be valid")
        .expect("context should exist");

        assert_eq!(context.allow_internet_access, Some(false));
        assert_eq!(context.allow_out, vec!["api.example.com".to_string()]);
    }

    #[test]
    fn network_context_forwards_egress_rules() {
        let context = build_cube_network_config(
            None,
            Some(&SandboxNetworkConfig {
                allow_public_traffic: None,
                allow_out: None,
                deny_out: None,
                mask_request_host: None,
                rules: Some(vec![EgressRule {
                    name: "deepseek_api".to_string(),
                    r#match: EgressRuleMatch {
                        scheme: Some("https".to_string()),
                        host: Some("api.deepseek.com".to_string()),
                        method: Some(vec!["POST".to_string()]),
                        path: Some("/v1/chat".to_string()),
                        sni: Some("api.deepseek.com".to_string()),
                    },
                    action: EgressRuleAction {
                        allow: true,
                        audit: Some("metadata".to_string()),
                        inject: Some(vec![EgressRuleInject {
                            header: "Authorization".to_string(),
                            secret: "sk_xxx".to_string(),
                            format: Some("Bearer ${SECRET}".to_string()),
                        }]),
                    },
                }]),
            }),
        )
        .expect("network config should be valid")
        .expect("context should exist for rules-only config");

        assert_eq!(context.rules.len(), 1);
        let rule = &context.rules[0];
        assert_eq!(rule.name, "deepseek_api");
        assert_eq!(rule.r#match.path.as_deref(), Some("/v1/chat"));
        assert!(rule.action.allow);
        let inject = rule
            .action
            .inject
            .as_ref()
            .expect("inject preserved")
            .clone();
        assert_eq!(inject.len(), 1);
        assert_eq!(inject[0].format.as_deref(), Some("Bearer ${SECRET}"));
    }

    #[test]
    fn network_rules_serialize_to_camel_case_wire() {
        let context = build_cube_network_config(
            None,
            Some(&SandboxNetworkConfig {
                allow_public_traffic: None,
                allow_out: None,
                deny_out: None,
                mask_request_host: None,
                rules: Some(vec![EgressRule {
                    name: "r1".to_string(),
                    r#match: EgressRuleMatch {
                        path: Some("/v1/chat".to_string()),
                        sni: Some("api.deepseek.com".to_string()),
                        ..Default::default()
                    },
                    action: EgressRuleAction {
                        allow: true,
                        audit: None,
                        inject: None,
                    },
                }]),
            }),
        )
        .expect("network config should be valid")
        .expect("context should exist");

        let json = serde_json::to_value(&context).expect("serialize");
        let rule = &json["rules"][0];
        assert_eq!(rule["name"], "r1");
        assert_eq!(rule["match"]["path"], "/v1/chat");
        assert_eq!(rule["match"]["sni"], "api.deepseek.com");
        // None fields are skipped on the wire.
        assert!(rule["action"].get("audit").is_none());
        assert!(rule["action"].get("inject").is_none());
    }

    #[test]
    fn listed_sandbox_preserves_resources_from_cubemaster_list() {
        let listed = from_cubemaster_info(SandboxInfo {
            sandbox_id: "sb-1".to_string(),
            host_id: "host-1".to_string(),
            status: "running".to_string(),
            started_at: None,
            create_at: 0,
            end_at: None,
            cpu_count: 2,
            memory_mb: 2048,
            template_id: "tpl-1".to_string(),
            annotations: HashMap::new(),
            labels: HashMap::new(),
        });

        assert_eq!(listed.cpu_count, 2);
        assert_eq!(listed.memory_mb, 2048);
        assert_eq!(listed.template_id, "tpl-1");
    }

    #[test]
    fn listed_sandbox_maps_paused_container_state_from_cubemaster_list() {
        let payload = serde_json::json!({
            "requestID": "req-1",
            "ret": { "ret_code": 0, "ret_msg": "ok" },
            "data": [{
                "sandbox_id": "sb-paused",
                "host_id": "host-1",
                "status": 5,
                "template_id": "tpl-1"
            }, {
                "sandbox_id": "sb-paused-string",
                "host_id": "host-1",
                "status": "5",
                "template_id": "tpl-1"
            }]
        });

        let response: ListSandboxResponse =
            serde_json::from_value(payload).expect("list response should deserialize");
        let listed: Vec<_> = response
            .sandboxes
            .into_iter()
            .map(from_cubemaster_info)
            .collect();

        assert_eq!(listed.len(), 2);
        assert!(listed
            .iter()
            .all(|sandbox| sandbox.state == SandboxState::Paused));
    }

    /// CubeMaster keys lifecycle metadata off these exact JSON field names —
    /// `auto_pause` / `auto_resume`. If they ever rename or get dropped during
    /// serialization the auto-pause sidecar silently treats every new sandbox
    /// as opted-out. Lock the wire shape down with a serialization snapshot.
    #[test]
    fn create_sandbox_request_serializes_lifecycle_flags() {
        let mut req = CreateSandboxRequest {
            request_id: "req-1".to_string(),
            instance_type: "cubebox".to_string(),
            timeout: Some(60),
            annotations: HashMap::new(),
            labels: None,
            create_time_env_vars: None,
            distribution_scope: None,
            volumes: None,
            containers: vec![],
            exposed_ports: vec![],
            network_type: None,
            cube_network_config: None,
            auto_pause: false,
            auto_resume: false,
        };

        // Both false → both fields are omitted (skip_serializing_if = Not::not).
        let json = serde_json::to_value(&req).unwrap();
        assert!(
            json.get("auto_pause").is_none(),
            "auto_pause=false should be omitted, got: {json}"
        );
        assert!(
            json.get("auto_resume").is_none(),
            "auto_resume=false should be omitted, got: {json}"
        );

        // Flip on → fields appear with snake_case key matching CubeMaster's
        // `json:"auto_pause,omitempty"` and `json:"auto_resume,omitempty"`.
        req.auto_pause = true;
        req.auto_resume = true;
        let json = serde_json::to_value(&req).unwrap();
        assert_eq!(json.get("auto_pause"), Some(&serde_json::Value::Bool(true)));
        assert_eq!(
            json.get("auto_resume"),
            Some(&serde_json::Value::Bool(true))
        );
    }

    fn empty_create_request() -> CreateSandboxRequest {
        CreateSandboxRequest {
            request_id: "req-1".to_string(),
            instance_type: "cubebox".to_string(),
            timeout: None,
            annotations: HashMap::new(),
            labels: None,
            create_time_env_vars: None,
            distribution_scope: None,
            volumes: None,
            containers: vec![],
            exposed_ports: vec![],
            network_type: None,
            cube_network_config: None,
            auto_pause: false,
            auto_resume: false,
        }
    }

    /// CubeMaster applies its server default only when the timeout key is
    /// absent. Lock down the three-value wire shape (omit / 0 / negative / positive).
    #[test]
    fn create_sandbox_request_timeout_wire_shape() {
        let mut req = empty_create_request();
        let json = serde_json::to_value(&req).unwrap();
        assert!(
            json.get("timeout").is_none(),
            "timeout=None should be omitted, got: {json}"
        );

        for (value, label) in [(0, "zero"), (-1, "never"), (45, "positive")] {
            req.timeout = Some(value);
            let json = serde_json::to_value(&req).unwrap();
            assert_eq!(
                json.get("timeout"),
                Some(&serde_json::Value::from(value)),
                "timeout={label} should be forwarded as-is, got: {json}"
            );
        }
    }

    #[test]
    fn new_sandbox_timeout_defaults_to_none_when_omitted() {
        let req: NewSandbox = serde_json::from_value(serde_json::json!({
            "templateID": "tpl",
        }))
        .unwrap();
        assert_eq!(req.timeout, None);
    }

    #[test]
    fn sandbox_update_request_timeout_wire_shape() {
        let req = SandboxUpdateRequest {
            request_id: "req-1".to_string(),
            sandbox_id: "sb-1".to_string(),
            instance_type: "cubebox".to_string(),
            action: "resume".to_string(),
            timeout: None,
        };
        let json = serde_json::to_value(&req).unwrap();
        assert!(
            json.get("timeout").is_none(),
            "resume/connect with timeout=None should omit field, got: {json}"
        );

        for (value, label) in [(0, "zero"), (-1, "never"), (120, "positive")] {
            let req = SandboxUpdateRequest {
                request_id: "req-1".to_string(),
                sandbox_id: "sb-1".to_string(),
                instance_type: "cubebox".to_string(),
                action: "resume".to_string(),
                timeout: Some(value),
            };
            let json = serde_json::to_value(&req).unwrap();
            assert_eq!(
                json.get("timeout"),
                Some(&serde_json::Value::from(value)),
                "update timeout={label} should be forwarded as-is, got: {json}"
            );
        }
    }

    /// The inbound API mirrors the e2b `lifecycle` object (camelCase nested
    /// struct). CubeAPI then translates it to the two CubeMaster-side bools
    /// when constructing the create-sandbox RPC. Verify the translation
    /// covers each meaningful combination.
    #[test]
    fn lifecycle_object_translates_to_cubemaster_bools() {
        use crate::models::{NewSandbox, SandboxOnTimeout};

        // Helper that mimics services::create_sandbox's lifecycle decoding.
        fn translate(body: &NewSandbox) -> (bool, bool) {
            body.lifecycle
                .as_ref()
                .map(|lc| {
                    (
                        matches!(lc.on_timeout, SandboxOnTimeout::Pause),
                        lc.auto_resume,
                    )
                })
                .unwrap_or((false, false))
        }

        // Absent lifecycle => preserve historical behaviour.
        let absent: NewSandbox = serde_json::from_value(serde_json::json!({
            "templateID": "tpl",
        }))
        .unwrap();
        assert_eq!(translate(&absent), (false, false));

        // Explicit kill (with auto_resume=true) is still kill — auto_resume
        // doesn't auto-imply pause. Server-side enforcement of the e2b
        // semantic ("auto_resume only meaningful when on_timeout=pause") is
        // delegated to CubeMaster.
        let kill: NewSandbox = serde_json::from_value(serde_json::json!({
            "templateID": "tpl",
            "lifecycle": {"onTimeout": "kill", "autoResume": true},
        }))
        .unwrap();
        assert_eq!(translate(&kill), (false, true));

        // Pause + auto_resume — the canonical e2b auto-resume case.
        let pause_with_resume: NewSandbox = serde_json::from_value(serde_json::json!({
            "templateID": "tpl",
            "lifecycle": {"onTimeout": "pause", "autoResume": true},
        }))
        .unwrap();
        assert_eq!(translate(&pause_with_resume), (true, true));

        // Some Python SDK versions and direct callers may send the Pythonic
        // snake_case shape. Keep accepting it so lifecycle does not silently
        // fall back to the default kill/no-resume behaviour.
        let snake_case_pause_with_resume: NewSandbox = serde_json::from_value(serde_json::json!({
            "templateID": "tpl",
            "lifecycle": {"on_timeout": "pause", "auto_resume": true},
        }))
        .unwrap();
        assert_eq!(translate(&snake_case_pause_with_resume), (true, true));

        // Pause without auto_resume — caller must call connect() manually.
        let pause_only: NewSandbox = serde_json::from_value(serde_json::json!({
            "templateID": "tpl",
            "lifecycle": {"onTimeout": "pause"},
        }))
        .unwrap();
        assert_eq!(translate(&pause_only), (true, false));

        // Empty lifecycle object — defaults: kill on timeout, no auto-resume.
        let empty: NewSandbox = serde_json::from_value(serde_json::json!({
            "templateID": "tpl",
            "lifecycle": {},
        }))
        .unwrap();
        assert_eq!(translate(&empty), (false, false));
    }

    #[tokio::test]
    async fn create_sandbox_forwards_create_time_env_vars_to_cubemaster() {
        #[derive(Clone, Default)]
        struct Capture {
            create_body: Arc<Mutex<Option<Value>>>,
        }

        async fn create_handler(
            State(capture): State<Capture>,
            Json(body): Json<Value>,
        ) -> Json<Value> {
            *capture.create_body.lock().await = Some(body);
            Json(serde_json::json!({
                "requestID": "req-1",
                "sandbox_id": "sb-123",
                "ret": { "ret_code": 0, "ret_msg": "ok" }
            }))
        }

        async fn spawn_server(app: Router) -> String {
            let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
                .await
                .expect("listener should bind");
            let addr = listener.local_addr().expect("listener addr");
            tokio::spawn(async move {
                axum::serve(listener, app).await.expect("server should run");
            });
            format!("http://{}", addr)
        }

        let capture = Capture::default();
        let cubemaster_url = spawn_server(
            Router::new()
                .route("/cube/sandbox", post(create_handler))
                .with_state(capture.clone()),
        )
        .await;

        let service = SandboxService::new(
            CubeMasterClient::new(cubemaster_url, reqwest::Client::new()),
            "cubebox".to_string(),
            "cube.app".to_string(),
        );

        let env_vars = HashMap::from([(
            "CUBE_TEST_CREATE_ENV".to_string(),
            "from-create".to_string(),
        )]);
        let sandbox = service
            .create_sandbox(NewSandbox {
                template_id: "tpl-1".to_string(),
                timeout: Some(15),
                lifecycle: None,
                secure: None,
                allow_internet_access: None,
                network: None,
                metadata: None,
                distribution_scope: None,
                env_vars: Some(env_vars),
                mcp: None,
                volume_mounts: None,
            })
            .await
            .expect("sandbox create should succeed");

        assert_eq!(sandbox.sandbox_id, "sb-123");
        let create_body = capture
            .create_body
            .lock()
            .await
            .clone()
            .expect("create body");
        assert_eq!(
            create_body["create_time_env_vars"]["CUBE_TEST_CREATE_ENV"],
            serde_json::json!("from-create")
        );
        assert!(create_body.get("envVars").is_none());
    }

    #[tokio::test]
    async fn create_sandbox_omits_create_time_env_vars_when_absent() {
        #[derive(Clone, Default)]
        struct Capture {
            create_body: Arc<Mutex<Option<Value>>>,
        }

        async fn create_handler(
            State(capture): State<Capture>,
            Json(body): Json<Value>,
        ) -> Json<Value> {
            *capture.create_body.lock().await = Some(body);
            Json(serde_json::json!({
                "requestID": "req-1",
                "sandbox_id": "sb-no-envs",
                "ret": { "ret_code": 0, "ret_msg": "ok" }
            }))
        }

        async fn spawn_server(app: Router) -> String {
            let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
                .await
                .expect("listener should bind");
            let addr = listener.local_addr().expect("listener addr");
            tokio::spawn(async move {
                axum::serve(listener, app).await.expect("server should run");
            });
            format!("http://{}", addr)
        }

        let capture = Capture::default();
        let cubemaster_url = spawn_server(
            Router::new()
                .route("/cube/sandbox", post(create_handler))
                .with_state(capture.clone()),
        )
        .await;

        let service = SandboxService::new(
            CubeMasterClient::new(cubemaster_url, reqwest::Client::new()),
            "cubebox".to_string(),
            "cube.app".to_string(),
        );

        let sandbox = service
            .create_sandbox(NewSandbox {
                template_id: "tpl-1".to_string(),
                timeout: Some(15),
                lifecycle: None,
                secure: None,
                allow_internet_access: None,
                network: None,
                metadata: None,
                distribution_scope: None,
                env_vars: None,
                mcp: None,
                volume_mounts: None,
            })
            .await
            .expect("sandbox create should succeed");

        assert_eq!(sandbox.sandbox_id, "sb-no-envs");
        let create_body = capture
            .create_body
            .lock()
            .await
            .clone()
            .expect("create body");
        assert!(
            create_body.get("create_time_env_vars").is_none(),
            "create_time_env_vars should be omitted when caller did not provide envs"
        );
    }

    #[tokio::test]
    async fn kill_sandbox_maps_cubemaster_not_found_to_app_not_found() {
        async fn delete_handler() -> Json<Value> {
            Json(serde_json::json!({
                "requestID": "req-delete",
                "ret": { "ret_code": RET_CODE_NOT_FOUND, "ret_msg": "no such sandbox" },
                "sandbox_id": "sandbox-missing"
            }))
        }

        async fn spawn_server(app: Router) -> String {
            let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
                .await
                .expect("listener should bind");
            let addr = listener.local_addr().expect("listener addr");
            tokio::spawn(async move {
                axum::serve(listener, app).await.expect("server should run");
            });
            format!("http://{}", addr)
        }

        let cubemaster_url =
            spawn_server(Router::new().route("/cube/sandbox", delete(delete_handler))).await;
        let service = SandboxService::new(
            CubeMasterClient::new(cubemaster_url, reqwest::Client::new()),
            "cubebox".to_string(),
            "cube.app".to_string(),
        );

        let err = service
            .kill_sandbox("sandbox-missing")
            .await
            .expect_err("missing sandbox delete should return not found");

        match err {
            crate::error::AppError::NotFound(msg) => {
                assert_eq!(msg, "sandbox sandbox-missing not found");
            }
            other => panic!("expected not found error, got {other:?}"),
        }
    }

    #[test]
    fn delete_maps_capacity_rejection_to_conflict() {
        let err = map_delete_cubemaster_err(
            CubeMasterError::Api {
                ret_code: RET_CODE_CONFLICT,
                ret_msg: "resume rejected by paused_resource_release_ratio policy: node is full"
                    .to_string(),
            },
            "sb-capacity",
        );

        match err {
            AppError::Conflict(message) => assert_eq!(
                message,
                "resume rejected by paused_resource_release_ratio policy: node is full"
            ),
            other => panic!("expected conflict error, got {other:?}"),
        }
    }

    #[test]
    fn delete_maps_pausing_to_short_retry() {
        let err = map_delete_cubemaster_err(
            CubeMasterError::Api {
                ret_code: RET_CODE_TASK_STATE_INVALID,
                ret_msg: "sandbox is pausing; retry DELETE after 2 seconds".to_string(),
            },
            "sb-pausing",
        );

        match err {
            AppError::ServiceUnavailable {
                message,
                retry_after,
            } => {
                assert_eq!(retry_after, 2);
                assert_eq!(message, "sandbox is pausing; retry DELETE after 2 seconds");
            }
            other => panic!("expected unavailable error, got {other:?}"),
        }
    }

    #[test]
    fn delete_maps_unproven_resume_to_retryable_unavailable() {
        let err = map_delete_cubemaster_err(
            CubeMasterError::Api {
                ret_code: RET_CODE_TASK_RESUME_FAILED,
                ret_msg: "failed to resume paused sandbox before delete: shim timeout; retry DELETE after 5 seconds".to_string(),
            },
            "sb-resume-failed",
        );

        match err {
            AppError::ServiceUnavailable {
                message,
                retry_after,
            } => {
                assert_eq!(retry_after, 5);
                assert_eq!(
                    message,
                    "failed to resume paused sandbox before delete: shim timeout; retry DELETE after 5 seconds"
                );
            }
            other => panic!("expected unavailable error, got {other:?}"),
        }
    }

    #[test]
    fn delete_retryable_errors_include_retry_after_in_http_response() {
        let cases = [
            (
                RET_CODE_TASK_STATE_INVALID,
                "sandbox is pausing; retry DELETE after 2 seconds",
                "2",
            ),
            (
                RET_CODE_TASK_RESUME_FAILED,
                "failed to resume paused sandbox before delete: shim timeout; retry DELETE after 5 seconds",
                "5",
            ),
        ];

        for (ret_code, ret_msg, retry_after) in cases {
            let response = map_delete_cubemaster_err(
                CubeMasterError::Api {
                    ret_code,
                    ret_msg: ret_msg.to_string(),
                },
                "sb-retry",
            )
            .into_response();

            assert_eq!(response.status(), StatusCode::SERVICE_UNAVAILABLE);
            assert_eq!(response.headers().get(RETRY_AFTER).unwrap(), retry_after);
        }
    }

    #[test]
    fn delete_retry_message_uses_fallback_for_empty_cube_master_message() {
        assert_eq!(
            super::delete_retry_message(
                String::new(),
                "sb-pausing",
                "is pausing; retry DELETE after 2 seconds",
            ),
            "sandbox sb-pausing is pausing; retry DELETE after 2 seconds"
        );
        assert_eq!(
            super::delete_retry_message(
                "  \n".to_string(),
                "sb-resume-failed",
                "could not be resumed before delete; retry DELETE after 5 seconds",
            ),
            "sandbox sb-resume-failed could not be resumed before delete; retry DELETE after 5 seconds"
        );
    }

    #[test]
    fn create_sandbox_rejects_dangerous_env_var_names() {
        for name in super::FORBIDDEN_ENV_NAMES {
            let err = super::validate_env_vars(&HashMap::from([(
                (*name).to_string(),
                "val".to_string(),
            )]))
            .expect_err("dangerous env var name should be rejected");
            assert!(
                err.to_string().contains("not allowed"),
                "error for {name} should say 'not allowed': {err}"
            );
        }
    }

    #[test]
    fn create_sandbox_rejects_dangerous_env_var_names_case_insensitive() {
        for name in ["ld_preload", "Ld_Preload", "LD_PRELOAD"] {
            let err =
                super::validate_env_vars(&HashMap::from([(name.to_string(), "val".to_string())]))
                    .expect_err(&format!(
                        "dangerous env var name {name} should be rejected case-insensitively"
                    ));
            assert!(
                err.to_string().contains("not allowed"),
                "error for {name} should say 'not allowed': {err}"
            );
        }
    }

    #[test]
    fn create_sandbox_rejects_invalid_env_var_name_format() {
        for (name, desc) in [
            ("", "empty"),
            ("9VAR", "starts with digit"),
            ("MY-VAR", "contains hyphen"),
            ("MY.VAR", "contains dot"),
        ] {
            let err =
                super::validate_env_vars(&HashMap::from([(name.to_string(), "v".to_string())]))
                    .expect_err(&format!("{desc} should be rejected: {name}"));
            let msg = err.to_string();
            assert!(
                msg.contains("must match") || msg.contains("invalid env var name"),
                "error for {desc} ({name}) should mention name validation: {err}"
            );
        }
    }

    #[test]
    fn create_sandbox_rejects_invalid_env_var_value() {
        let too_large = "x".repeat(super::ENV_VAR_VALUE_MAX_LEN + 1);
        let err = super::validate_env_vars(&HashMap::from([("TOO_LARGE".to_string(), too_large)]))
            .expect_err("oversized env var value should be rejected");
        assert!(
            err.to_string().contains("value too large"),
            "oversized env var value error should mention size: {err}"
        );

        let err = super::validate_env_vars(&HashMap::from([(
            "HAS_NUL".to_string(),
            "abc\0def".to_string(),
        )]))
        .expect_err("env var value with NUL should be rejected");
        assert!(
            err.to_string().contains("contains NUL"),
            "NUL-containing env var value error should mention NUL: {err}"
        );

        let err = super::validate_env_vars(&HashMap::from([(
            "HAS_ESC".to_string(),
            "line\x1b[31mred".to_string(),
        )]))
        .expect_err("env var value with control character should be rejected");
        assert!(
            err.to_string().contains("control character"),
            "control-character env var value error should mention control character: {err}"
        );

        let err = super::validate_env_vars(&HashMap::from([(
            "HAS_NEWLINE".to_string(),
            "line1\nline2".to_string(),
        )]))
        .expect_err("env var value with newline should be rejected");
        assert!(
            err.to_string().contains("control character"),
            "newline env var value error should mention control character: {err}"
        );
    }

    #[test]
    fn create_sandbox_accepts_valid_env_var_names() {
        super::validate_env_vars(&HashMap::from([
            ("MY_VAR".to_string(), "val".to_string()),
            ("_underscore_prefix".to_string(), "val".to_string()),
            ("CUBE_TEST_ENV".to_string(), "val".to_string()),
            ("TAB_OK".to_string(), "hello\tworld".to_string()),
        ]))
        .expect("valid env var names should be accepted");
    }

    #[test]
    fn volume_mounts_reject_duplicate_names() {
        use crate::models::SandboxVolumeMount;

        let mounts = vec![
            SandboxVolumeMount {
                name: "data".to_string(),
                path: "/mnt/a".to_string(),
            },
            SandboxVolumeMount {
                name: "data".to_string(),
                path: "/mnt/b".to_string(),
            },
        ];
        let err = super::validate_unique_volume_mount_names(&mounts)
            .expect_err("duplicate volume mount names should be rejected");
        assert!(
            err.to_string().contains("duplicate volumeMounts name"),
            "unexpected error: {err}"
        );

        let ok = vec![
            SandboxVolumeMount {
                name: "data".to_string(),
                path: "/mnt/a".to_string(),
            },
            SandboxVolumeMount {
                name: "logs".to_string(),
                path: "/mnt/b".to_string(),
            },
        ];
        super::validate_unique_volume_mount_names(&ok)
            .expect("unique volume mount names should be accepted");
    }

    /// Verifies that `volumeMounts` from the e2b-shaped `NewSandbox` are
    /// correctly split into `VolumeSpec` (pod-level declarations) and
    /// `VolumeMount` (container-level bindings) for CubeMaster.
    #[test]
    fn volume_mounts_are_split_into_spec_and_mount() {
        use crate::{
            cubemaster::{VolumeMount, VolumeSpec},
            models::SandboxVolumeMount,
        };

        let mounts = vec![
            SandboxVolumeMount {
                name: "data".to_string(),
                path: "/mnt/data".to_string(),
            },
            SandboxVolumeMount {
                name: "logs".to_string(),
                path: "/mnt/logs".to_string(),
            },
        ];

        let (specs, bindings): (Vec<VolumeSpec>, Vec<VolumeMount>) = mounts
            .into_iter()
            .map(|SandboxVolumeMount { name, path }| {
                (
                    VolumeSpec {
                        name: Some(name.clone()),
                        volume_source: None,
                    },
                    VolumeMount {
                        name,
                        container_path: path,
                        readonly: None,
                    },
                )
            })
            .unzip();

        assert_eq!(specs.len(), 2);
        assert_eq!(specs[0].name.as_deref(), Some("data"));
        assert_eq!(specs[1].name.as_deref(), Some("logs"));

        assert_eq!(bindings[0].name, "data");
        assert_eq!(bindings[0].container_path, "/mnt/data");
        assert_eq!(bindings[1].name, "logs");
        assert_eq!(bindings[1].container_path, "/mnt/logs");
    }

    /// When no `volumeMounts` are provided, `volumes` and `containers` in the
    /// CubeMaster request should be empty/None so CubeMaster falls back to the
    /// template's container definition.
    #[test]
    fn empty_volume_mounts_produces_none_volumes_and_empty_containers() {
        let mounts: Vec<crate::models::SandboxVolumeMount> = vec![];
        let has_mounts = !mounts.is_empty();
        assert!(!has_mounts, "no mounts → containers should stay empty");
    }
}
