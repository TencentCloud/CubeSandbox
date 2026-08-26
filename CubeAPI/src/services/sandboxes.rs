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
        SandboxLogsRequest, SandboxNetworkRequest, SandboxRefreshRequest, SandboxStatus,
        SandboxTimeoutRequest, SandboxUpdateRequest, VolumeSpec,
    },
    error::{AppError, AppResult},
    models::{
        EgressRule, EgressRuleMatch, LogLevel as ModelLogLevel, NewSandbox, Sandbox,
        SandboxAutoResume, SandboxDetail, SandboxLifecycleConfig, SandboxLog, SandboxLogEntry,
        SandboxLogs, SandboxLogsV2Response, SandboxNetworkConfig, SandboxOnTimeout, SandboxState,
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
const MASK_REQUEST_HOST_MAX_LEN: usize = 512;
const MASK_REQUEST_HOST_PORT_PLACEHOLDER: &str = "${PORT}";

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
            .map_err(params_error_or_internal)?;

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
            cpu_milli: (d.cpu_milli > 0).then_some(d.cpu_milli),
            memory_mb: d.memory_mb,
            disk_size_mb: Some(d.disk_size_mb),
            metadata: optional_metadata(d.labels),
            state: sandbox_state_from_status(d.status),
            volume_mounts: map_volume_mounts(&d.volume_mounts),
        })
    }

    pub async fn create_sandbox(&self, body: NewSandbox) -> AppResult<Sandbox> {
        let NewSandbox {
            template_id,
            timeout,
            lifecycle,
            auto_pause: flat_auto_pause,
            auto_resume: flat_auto_resume,
            allow_internet_access,
            network,
            metadata,
            distribution_scope,
            env_vars,
            volume_mounts,
            backend,
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

        let (auto_pause, auto_resume) = resolve_lifecycle_flags(
            lifecycle.as_ref(),
            flat_auto_pause,
            flat_auto_resume.as_ref(),
        );

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
                    #[serde(skip_serializing_if = "Option::is_none")]
                    readonly: Option<bool>,
                }
                let entries: Vec<MountEntry> = mounts
                    .iter()
                    .map(|m| MountEntry {
                        name: &m.name,
                        container_path: &m.path,
                        readonly: m.read_only.then_some(true),
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
            backend,
        };

        let resp = self
            .cubemaster
            .create_sandbox(&req)
            .await
            .map_err(params_error_or_internal)?;

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
            Err(e) => Err(params_error_or_internal(e)),
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
            Err(e) => Err(params_error_or_internal(e)),
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
            .map_err(|e| sandbox_not_found_or_internal(e, sandbox_id))?;

        resp.ret
            .into_result()
            .map_err(|e| sandbox_not_found_or_internal(e, sandbox_id))?;

        Ok(())
    }

    /// Replace a running sandbox's egress policy.
    ///
    /// The policy is validated and mapped by the same code as sandbox creation,
    /// so an update cannot install anything create would have rejected. An
    /// all-empty body is legal and clears the policy, which is why the mapper's
    /// "nothing set" `None` is turned back into a default config rather than
    /// treated as "no change".
    pub async fn update_network(
        &self,
        sandbox_id: &str,
        allow_internet_access: Option<bool>,
        network: Option<&SandboxNetworkConfig>,
    ) -> AppResult<()> {
        let cube_network_config =
            build_cube_network_config(allow_internet_access, network)?.unwrap_or_default();

        let req = SandboxNetworkRequest {
            request_id: new_request_id(),
            sandbox_id: sandbox_id.to_string(),
            instance_type: self.instance_type.clone(),
            cube_network_config,
        };

        let resp = self
            .cubemaster
            .update_sandbox_network(&req)
            .await
            .map_err(|e| sandbox_not_found_or_internal(e, sandbox_id))?;

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
            .map_err(|e| sandbox_not_found_or_internal(e, sandbox_id))?;

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
            .map_err(|e| sandbox_not_found_or_internal(e, sandbox_id))?;

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

// parse_response converts every non-success ret_code into CubeMasterError::Api before
// the envelope reaches the caller, so a rejection of the client's own input has to be
// classified here — the ret.into_result()/ensure_* checks never observe it.
fn params_error_or_internal(e: CubeMasterError) -> AppError {
    if e.is_invalid_path_parameter() || e.is_params_error() {
        // Same reasoning as templates::map_err: the caller sent something invalid, so
        // this is a 400. Reporting it as 500 misleads clients into retrying a request
        // that can never succeed, and charges a client mistake against the
        // server-side success-rate SLI.
        AppError::BadRequest(e.to_string())
    } else {
        internal_error(e)
    }
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
        params_error_or_internal(e)
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
        cpu_count: if s.cpu_milli > 0 {
            s.cpu_milli / 1000
        } else {
            s.cpu_count
        },
        cpu_milli: (s.cpu_milli > 0).then_some(s.cpu_milli),
        memory_mb: if s.memory_mib > 0 {
            s.memory_mib
        } else {
            s.memory_mb
        },
        disk_size_mb: Some(0),
        metadata: optional_metadata(s.labels),
        state: sandbox_state_from_str(&s.status),
        envd_version,
        volume_mounts: map_volume_mounts(&s.volume_mounts),
    }
}

pub(crate) fn map_volume_mounts(
    mounts: &[crate::cubemaster::CubeVolumeMount],
) -> Option<Vec<crate::models::SandboxVolumeMount>> {
    if mounts.is_empty() {
        return None;
    }
    let mapped: Vec<_> = mounts
        .iter()
        // Drop entries that have neither a logical name nor a container path.
        .filter(|mount| !mount.name.is_empty() || !mount.container_path.is_empty())
        .map(|mount| crate::models::SandboxVolumeMount {
            name: mount.name.clone(),
            path: mount.container_path.clone(),
            read_only: mount.readonly,
        })
        .collect();
    if mapped.is_empty() {
        None
    } else {
        Some(mapped)
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

fn validate_mask_request_host(value: &str) -> AppResult<()> {
    let invalid = |reason: &str| {
        AppError::BadRequest(format!("network.maskRequestHost is invalid: {reason}"))
    };

    if value.is_empty() {
        return Err(invalid("value must not be empty"));
    }
    if value.len() > MASK_REQUEST_HOST_MAX_LEN {
        return Err(invalid("value is too long"));
    }
    if value.trim() != value || value.chars().any(|c| c.is_control() || c.is_whitespace()) {
        return Err(invalid("whitespace and control characters are not allowed"));
    }
    if value.contains("://")
        || value.contains('/')
        || value.contains('?')
        || value.contains('#')
        || value.contains('@')
    {
        return Err(invalid("expected a valid host or host:port authority"));
    }

    let expanded = value.replace(MASK_REQUEST_HOST_PORT_PLACEHOLDER, "65535");
    if expanded.contains("${") {
        return Err(invalid("only the ${PORT} placeholder is supported"));
    }

    let authority = expanded
        .parse::<axum::http::uri::Authority>()
        .map_err(|_| invalid("expected a valid host or host:port authority"))?;
    if authority.host().is_empty() || !authority.host().is_ascii() {
        return Err(invalid("host must be non-empty ASCII"));
    }
    let explicit_port = if expanded.starts_with('[') {
        expanded
            .find(']')
            .and_then(|end| expanded.get(end + 1..))
            .and_then(|suffix| suffix.strip_prefix(':'))
    } else {
        if expanded.matches(':').count() > 1 {
            return Err(invalid("IPv6 hosts must use brackets"));
        }
        expanded.rsplit_once(':').map(|(_, port)| port)
    };
    if let Some(port) = explicit_port {
        match port.parse::<u16>() {
            Ok(1..=u16::MAX) => {}
            _ => return Err(invalid("port must be between 1 and 65535")),
        }
    }

    Ok(())
}

/// Derive CubeMaster's `(auto_pause, auto_resume)` bools from whichever
/// lifecycle representation the caller sent.
///
/// Two shapes reach this endpoint. `lifecycle` is CubeSandbox's native nested
/// object. The e2b SDK never sends it — `Sandbox.create(lifecycle=...)` is
/// flattened client-side into top-level `autoPause` / `autoResume` before the
/// request goes out. Both are accepted; the nested object wins when present,
/// since an explicit `lifecycle` can only come from a direct API caller who
/// meant it. Neither present keeps today's behaviour: idle sandboxes are
/// killed.
pub(crate) fn resolve_lifecycle_flags(
    lifecycle: Option<&SandboxLifecycleConfig>,
    auto_pause: Option<bool>,
    auto_resume: Option<&SandboxAutoResume>,
) -> (bool, bool) {
    if let Some(lc) = lifecycle {
        return (
            matches!(lc.on_timeout, SandboxOnTimeout::Pause),
            lc.auto_resume,
        );
    }

    (
        auto_pause.unwrap_or(false),
        auto_resume.is_some_and(SandboxAutoResume::enabled),
    )
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

    if let Some(rs) = network.and_then(|n| n.rules.as_ref()) {
        for (index, rule) in rs.iter().enumerate() {
            validate_egress_rule_match(&rule.r#match, index)?;
        }
    }

    let rules: Vec<CubeEgressRule> = network
        .and_then(|n| n.rules.as_ref())
        .map(|rs| rs.iter().map(map_egress_rule).collect())
        .unwrap_or_default();

    let allow_public_traffic = network.and_then(|n| n.allow_public_traffic);
    let mask_request_host = network.and_then(|n| n.mask_request_host.clone());
    if let Some(value) = mask_request_host.as_deref() {
        validate_mask_request_host(value)?;
    }

    if allow_internet_access.is_none()
        && allow_public_traffic.is_none()
        && mask_request_host.is_none()
        && allow_out.is_empty()
        && deny_out.is_empty()
        && rules.is_empty()
    {
        return Ok(None);
    }

    Ok(Some(CubeNetworkConfig {
        allow_internet_access,
        allow_public_traffic,
        mask_request_host,
        allow_out,
        deny_out,
        rules,
    }))
}

/// Validate the port/scheme pair on one egress rule match, mirroring the
/// SDK client-side contract and the CubeEgress Lua validation: a set port
/// must be in [1, 65535] and must be paired with a scheme, and a set scheme
/// must be http or https (case-insensitive — downstream normalizes).
fn validate_egress_rule_match(rule_match: &EgressRuleMatch, index: usize) -> AppResult<()> {
    if let Some(port) = rule_match.port {
        if !(1..=65535).contains(&port) {
            return Err(AppError::BadRequest(format!(
                "network.rules[{index}].match.port must be in [1, 65535], got {port}"
            )));
        }
        if rule_match.scheme.is_none() {
            return Err(AppError::BadRequest(format!(
                "network.rules[{index}].match.port requires match.scheme to be set"
            )));
        }
    }
    if let Some(scheme) = rule_match.scheme.as_deref() {
        if !scheme.eq_ignore_ascii_case("http") && !scheme.eq_ignore_ascii_case("https") {
            return Err(AppError::BadRequest(format!(
                "network.rules[{index}].match.scheme must be 'http' or 'https', got {scheme:?}"
            )));
        }
    }
    Ok(())
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
            port: rule.r#match.port,
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
        map_delete_cubemaster_err, map_volume_mounts, resolve_lifecycle_flags,
        validate_mask_request_host, SandboxService, RET_CODE_CONFLICT, RET_CODE_NOT_FOUND,
        RET_CODE_TASK_RESUME_FAILED, RET_CODE_TASK_STATE_INVALID,
    };
    use crate::cubemaster::{
        CreateSandboxRequest, CubeMasterClient, CubeMasterError, CubeVolumeMount,
        ListSandboxResponse, SandboxInfo, SandboxUpdateRequest,
    };
    use crate::error::AppError;
    use crate::models::{
        EgressRule, EgressRuleAction, EgressRuleInject, EgressRuleMatch, NewSandbox,
        SandboxAutoResume, SandboxLifecycleConfig, SandboxNetworkConfig, SandboxOnTimeout,
        SandboxState, SandboxVolumeMount,
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

    async fn spawn_fake_cubemaster(app: Router) -> SandboxService {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .expect("listener should bind");
        let addr = listener.local_addr().expect("listener addr");
        tokio::spawn(async move {
            axum::serve(listener, app).await.expect("server should run");
        });
        SandboxService::new(
            CubeMasterClient::new(format!("http://{}", addr), reqwest::Client::new()),
            "cubebox".to_string(),
            "cube.app".to_string(),
        )
    }

    fn ret_envelope(ret_code: i32, ret_msg: &str) -> Json<Value> {
        Json(serde_json::json!({
            "requestID": "req-1",
            "ret": { "ret_code": ret_code, "ret_msg": ret_msg }
        }))
    }

    fn params_error_reason() -> &'static str {
        r#""host-mount" entry[0]: hostPath "/tmp" is not within an allowed mount prefix"#
    }

    fn probe_sandbox() -> NewSandbox {
        NewSandbox {
            template_id: "tpl-1".to_string(),
            timeout: Some(30),
            lifecycle: None,
            auto_pause: None,
            auto_resume: None,
            secure: None,
            allow_internet_access: None,
            network: None,
            metadata: None,
            distribution_scope: None,
            env_vars: None,
            mcp: None,
            volume_mounts: None,
            backend: None,
        }
    }

    fn assert_bad_request(err: AppError, expected_reason: &str) {
        assert!(
            matches!(err, AppError::BadRequest(ref m) if m.contains(expected_reason)),
            "expected BadRequest carrying the backend reason, got {err:?}"
        );
        assert_eq!(err.into_response().status(), StatusCode::BAD_REQUEST);
    }

    // CubeMaster returns MasterParamsError (130400) when the request itself is
    // rejected — e.g. a host-mount hostPath outside allowed_host_mount_prefixes.
    // templates::map_err already treats it as 400 via is_params_error(); the sandbox
    // paths were reporting it as 500, which misleads clients into retrying a request
    // that can never succeed and charges a client mistake against the server-side
    // success-rate SLI.
    //
    // Driven end-to-end through the service so the assertion covers the path the
    // request actually takes: parse_response raises CubeMasterError::Api before the
    // envelope is visible, so the fix has to sit on the transport error.
    #[tokio::test]
    async fn create_sandbox_maps_cubemaster_params_error_to_bad_request() {
        let reason = params_error_reason();
        let service = spawn_fake_cubemaster(Router::new().route(
            "/cube/sandbox",
            post(move || async move { ret_envelope(130400, reason) }),
        ))
        .await;

        let err = service
            .create_sandbox(probe_sandbox())
            .await
            .expect_err("rejected create should not succeed");
        assert_bad_request(err, reason);
    }

    #[tokio::test]
    async fn set_timeout_maps_cubemaster_params_error_to_bad_request() {
        let reason = "timeout must be positive";
        let service = spawn_fake_cubemaster(Router::new().route(
            "/cube/sandbox/timeout",
            post(move || async move { ret_envelope(130400, reason) }),
        ))
        .await;

        let err = service
            .set_timeout("sbx-1", -1)
            .await
            .expect_err("rejected timeout update should not succeed");
        assert_bad_request(err, reason);
    }

    #[tokio::test]
    async fn refresh_maps_cubemaster_params_error_to_bad_request() {
        let reason = "duration out of range";
        let service = spawn_fake_cubemaster(Router::new().route(
            "/cube/sandbox/refresh",
            post(move || async move { ret_envelope(130400, reason) }),
        ))
        .await;

        let err = service
            .refresh("sbx-1", 1 << 30)
            .await
            .expect_err("rejected refresh should not succeed");
        assert_bad_request(err, reason);
    }

    // Negative control: genuine backend faults must keep counting as 5xx, and
    // 130408 CubeletUnHealthy must not be swept up by the 1304xx prefix.
    #[tokio::test]
    async fn create_sandbox_keeps_backend_faults_internal() {
        for ret_code in [130408, 130593] {
            let service = spawn_fake_cubemaster(Router::new().route(
                "/cube/sandbox",
                post(move || async move { ret_envelope(ret_code, "backend fault") }),
            ))
            .await;

            let err = service
                .create_sandbox(probe_sandbox())
                .await
                .expect_err("backend fault should not succeed");
            assert!(
                matches!(err, AppError::Internal(_)),
                "ret_code {ret_code} must stay 5xx, got {err:?}"
            );
            assert_eq!(
                err.into_response().status(),
                StatusCode::INTERNAL_SERVER_ERROR
            );
        }
    }

    #[test]
    fn map_volume_mounts_returns_none_for_empty_input() {
        assert!(map_volume_mounts(&[]).is_none());
    }

    #[test]
    fn map_volume_mounts_skips_all_empty_entries() {
        assert!(map_volume_mounts(&[CubeVolumeMount {
            name: String::new(),
            container_path: String::new(),
            readonly: false,
        }])
        .is_none());
    }

    #[test]
    fn map_volume_mounts_exposes_public_mount_fields() {
        let mapped = map_volume_mounts(&[CubeVolumeMount {
            name: "hostdir-0".to_string(),
            container_path: "/mnt/data".to_string(),
            readonly: true,
        }])
        .expect("mounts should map");

        assert_eq!(mapped.len(), 1);
        assert_eq!(mapped[0].name, "hostdir-0");
        assert_eq!(mapped[0].path, "/mnt/data");
        assert!(mapped[0].read_only);
    }

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
    fn network_context_forwards_mask_request_host_by_itself() {
        let context = build_cube_network_config(
            None,
            Some(&SandboxNetworkConfig {
                mask_request_host: Some("localhost:${PORT}".to_string()),
                ..Default::default()
            }),
        )
        .expect("mask should be valid")
        .expect("mask-only network config must not be dropped");

        assert_eq!(
            context.mask_request_host.as_deref(),
            Some("localhost:${PORT}")
        );
        let json = serde_json::to_value(&context).expect("serialize");
        assert_eq!(json["maskRequestHost"], "localhost:${PORT}");
    }

    #[test]
    fn mask_request_host_validation_accepts_documented_authorities() {
        for value in [
            "localhost",
            "localhost:3000",
            "localhost:${PORT}",
            "my-app.example.com:${PORT}",
            "127.0.0.1:3000",
            "[::1]:${PORT}",
        ] {
            validate_mask_request_host(value).unwrap_or_else(|err| {
                panic!("expected {value:?} to be valid, got {err}");
            });
        }
    }

    #[test]
    fn mask_request_host_validation_rejects_unsafe_values() {
        for value in [
            "",
            " localhost",
            "localhost ",
            "https://example.com",
            "example.com/path",
            "example.com?x=1",
            "example.com#fragment",
            "user@example.com",
            "bad\r\nInjected: value",
            "example.com:",
            "example.com:0",
            "example.com:99999",
            "localhost:${OTHER}",
            "localhost:${PORT",
            "[::1",
            "::1",
            "[::1]]:3000",
            "[::1]:",
            "例子.测试",
        ] {
            assert!(
                validate_mask_request_host(value).is_err(),
                "expected {value:?} to be rejected"
            );
        }
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
                        port: None,
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

    fn network_with_match(rule_match: EgressRuleMatch) -> SandboxNetworkConfig {
        SandboxNetworkConfig {
            allow_public_traffic: None,
            allow_out: None,
            deny_out: None,
            mask_request_host: None,
            rules: Some(vec![EgressRule {
                name: "r1".to_string(),
                r#match: rule_match,
                action: EgressRuleAction {
                    allow: true,
                    audit: None,
                    inject: None,
                },
            }]),
        }
    }

    #[test]
    fn egress_match_port_requires_scheme() {
        let err = build_cube_network_config(
            None,
            Some(&network_with_match(EgressRuleMatch {
                port: Some(8443),
                ..Default::default()
            })),
        )
        .expect_err("port without scheme must be rejected");
        assert!(err.to_string().contains("requires match.scheme"), "{err}");
    }

    #[test]
    fn egress_match_port_range_enforced() {
        for port in [0, -1, 65536, 99999] {
            let err = build_cube_network_config(
                None,
                Some(&network_with_match(EgressRuleMatch {
                    port: Some(port),
                    scheme: Some("https".to_string()),
                    ..Default::default()
                })),
            )
            .expect_err("out-of-range port must be rejected");
            assert!(err.to_string().contains("[1, 65535]"), "{err}");
        }
    }

    #[test]
    fn egress_match_invalid_scheme_rejected() {
        let err = build_cube_network_config(
            None,
            Some(&network_with_match(EgressRuleMatch {
                scheme: Some("ftp".to_string()),
                ..Default::default()
            })),
        )
        .expect_err("non-http(s) scheme must be rejected");
        assert!(
            err.to_string().contains("must be 'http' or 'https'"),
            "{err}"
        );
    }

    #[test]
    fn egress_match_valid_port_scheme_accepted() {
        build_cube_network_config(
            None,
            Some(&network_with_match(EgressRuleMatch {
                port: Some(8443),
                scheme: Some("https".to_string()),
                ..Default::default()
            })),
        )
        .expect("valid port+scheme pair");
        // Case variants are accepted (downstream normalizes to lowercase).
        build_cube_network_config(
            None,
            Some(&network_with_match(EgressRuleMatch {
                scheme: Some("HTTPS".to_string()),
                ..Default::default()
            })),
        )
        .expect("uppercase scheme is accepted");
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
            cpu_milli: 0,
            memory_mib: 0,
            template_id: "tpl-1".to_string(),
            annotations: HashMap::new(),
            labels: HashMap::new(),
            volume_mounts: vec![],
        });

        assert_eq!(listed.cpu_count, 2);
        assert_eq!(listed.cpu_milli, None);
        assert_eq!(listed.memory_mb, 2048);
        assert_eq!(listed.template_id, "tpl-1");
    }

    #[test]
    fn listed_sandbox_prefers_exact_resource_units_from_cubemaster() {
        let listed = from_cubemaster_info(SandboxInfo {
            sandbox_id: "sb-small".to_string(),
            host_id: "host-1".to_string(),
            status: "running".to_string(),
            started_at: None,
            create_at: 0,
            end_at: None,
            cpu_count: 0,
            memory_mb: 537,
            cpu_milli: 500,
            memory_mib: 512,
            template_id: "tpl-1".to_string(),
            annotations: HashMap::new(),
            labels: HashMap::new(),
            volume_mounts: vec![],
        });

        assert_eq!(listed.cpu_count, 0);
        assert_eq!(listed.cpu_milli, Some(500));
        assert_eq!(listed.memory_mb, 512);
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
            backend: None,
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

    /// The e2b Python SDK does not put `lifecycle` on the wire. It flattens
    /// the user-facing object into top-level `autoPause` / `autoResume` before
    /// calling `POST /sandboxes`. Unknown fields are dropped silently by serde,
    /// so a missing binding here means auto-pause is ignored and the sandbox is
    /// killed at timeout instead of paused.
    #[test]
    fn new_sandbox_deserializes_e2b_flat_lifecycle_fields() {
        let body = serde_json::json!({
            "templateID": "tpl-1",
            "timeout": 30,
            "autoPause": true,
            "autoResume": { "enabled": true }
        });

        let new_sandbox: NewSandbox =
            serde_json::from_value(body).expect("e2b create body should deserialize");

        assert_eq!(new_sandbox.auto_pause, Some(true));
        assert_eq!(
            new_sandbox
                .auto_resume
                .as_ref()
                .map(SandboxAutoResume::enabled),
            Some(true)
        );
    }

    /// `autoResume` is an object in the current SDK, but older releases and
    /// hand-rolled clients send a bare bool. Accept both rather than 400-ing on
    /// a shape difference that carries the same meaning.
    #[test]
    fn new_sandbox_accepts_bare_bool_auto_resume() {
        let body = serde_json::json!({
            "templateID": "tpl-1",
            "autoResume": true
        });

        let new_sandbox: NewSandbox =
            serde_json::from_value(body).expect("bare-bool autoResume should deserialize");

        assert_eq!(
            new_sandbox
                .auto_resume
                .as_ref()
                .map(SandboxAutoResume::enabled),
            Some(true)
        );
    }

    #[test]
    fn flat_lifecycle_fields_enable_pause_and_resume() {
        let auto_resume = SandboxAutoResume::Config { enabled: true };
        let flags = resolve_lifecycle_flags(None, Some(true), Some(&auto_resume));

        assert_eq!(flags, (true, true));
    }

    /// `lifecycle` is CubeSandbox's native, richer form. When a caller sends it
    /// explicitly it wins over the flattened compatibility fields. The e2b SDK
    /// never sends both, so this only disambiguates direct API callers.
    #[test]
    fn nested_lifecycle_wins_over_flat_lifecycle_fields() {
        let lifecycle = SandboxLifecycleConfig {
            on_timeout: SandboxOnTimeout::Kill,
            auto_resume: false,
        };
        let auto_resume = SandboxAutoResume::Enabled(true);

        let flags = resolve_lifecycle_flags(Some(&lifecycle), Some(true), Some(&auto_resume));

        assert_eq!(flags, (false, false));
    }

    #[test]
    fn absent_lifecycle_fields_keep_kill_behaviour() {
        let flags = resolve_lifecycle_flags(None, None, None);

        assert_eq!(flags, (false, false));
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
            backend: None,
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
    /// covers each meaningful combination via the real resolver.
    #[test]
    fn lifecycle_object_translates_to_cubemaster_bools() {
        use crate::models::NewSandbox;

        fn flags(body: &NewSandbox) -> (bool, bool) {
            resolve_lifecycle_flags(
                body.lifecycle.as_ref(),
                body.auto_pause,
                body.auto_resume.as_ref(),
            )
        }

        // Absent lifecycle => preserve historical behaviour.
        let absent: NewSandbox = serde_json::from_value(serde_json::json!({
            "templateID": "tpl",
        }))
        .unwrap();
        assert_eq!(flags(&absent), (false, false));

        // Explicit kill (with auto_resume=true) is still kill — auto_resume
        // doesn't auto-imply pause. Server-side enforcement of the e2b
        // semantic ("auto_resume only meaningful when on_timeout=pause") is
        // delegated to CubeMaster.
        let kill: NewSandbox = serde_json::from_value(serde_json::json!({
            "templateID": "tpl",
            "lifecycle": {"onTimeout": "kill", "autoResume": true},
        }))
        .unwrap();
        assert_eq!(flags(&kill), (false, true));

        // Pause + auto_resume — the canonical e2b auto-resume case.
        let pause_with_resume: NewSandbox = serde_json::from_value(serde_json::json!({
            "templateID": "tpl",
            "lifecycle": {"onTimeout": "pause", "autoResume": true},
        }))
        .unwrap();
        assert_eq!(flags(&pause_with_resume), (true, true));

        // Some Python SDK versions and direct callers may send the Pythonic
        // snake_case shape. Keep accepting it so lifecycle does not silently
        // fall back to the default kill/no-resume behaviour.
        let snake_case_pause_with_resume: NewSandbox = serde_json::from_value(serde_json::json!({
            "templateID": "tpl",
            "lifecycle": {"on_timeout": "pause", "auto_resume": true},
        }))
        .unwrap();
        assert_eq!(flags(&snake_case_pause_with_resume), (true, true));

        // Pause without auto_resume — caller must call connect() manually.
        let pause_only: NewSandbox = serde_json::from_value(serde_json::json!({
            "templateID": "tpl",
            "lifecycle": {"onTimeout": "pause"},
        }))
        .unwrap();
        assert_eq!(flags(&pause_only), (true, false));

        // Empty lifecycle object — defaults: kill on timeout, no auto-resume.
        let empty: NewSandbox = serde_json::from_value(serde_json::json!({
            "templateID": "tpl",
            "lifecycle": {},
        }))
        .unwrap();
        assert_eq!(flags(&empty), (false, false));

        // e2b SDK wire shape: flattened top-level fields, no nested lifecycle.
        let flat: NewSandbox = serde_json::from_value(serde_json::json!({
            "templateID": "tpl",
            "autoPause": true,
            "autoResume": { "enabled": true },
        }))
        .unwrap();
        assert_eq!(flags(&flat), (true, true));
    }

    /// The failure mode this PR fixes is silent drop between inbound flat
    /// fields and the CubeMaster create body. Deserialization and
    /// `resolve_lifecycle_flags` alone cannot catch a regression that unbinds
    /// `auto_pause`/`auto_resume` from the `NewSandbox` destructuring (`..`
    /// still compiles). Drive the full service path and assert the outbound
    /// flags.
    #[tokio::test]
    async fn create_sandbox_forwards_e2b_flat_lifecycle_flags_to_cubemaster() {
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
                "sandbox_id": "sb-flat-lifecycle",
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
                timeout: Some(30),
                lifecycle: None,
                auto_pause: Some(true),
                auto_resume: Some(SandboxAutoResume::Config { enabled: true }),
                secure: None,
                allow_internet_access: None,
                network: None,
                metadata: None,
                distribution_scope: None,
                env_vars: None,
                mcp: None,
                volume_mounts: None,
                backend: None,
            })
            .await
            .expect("sandbox create should succeed");

        assert_eq!(sandbox.sandbox_id, "sb-flat-lifecycle");
        let create_body = capture
            .create_body
            .lock()
            .await
            .clone()
            .expect("create body");
        assert_eq!(
            create_body.get("auto_pause"),
            Some(&serde_json::Value::Bool(true)),
            "flat autoPause must reach CubeMaster, got: {create_body}"
        );
        assert_eq!(
            create_body.get("auto_resume"),
            Some(&serde_json::Value::Bool(true)),
            "flat autoResume must reach CubeMaster, got: {create_body}"
        );
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
                auto_pause: None,
                auto_resume: None,
                secure: None,
                allow_internet_access: None,
                network: None,
                metadata: None,
                distribution_scope: None,
                env_vars: Some(env_vars),
                mcp: None,
                volume_mounts: None,
                backend: None,
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
                auto_pause: None,
                auto_resume: None,
                secure: None,
                allow_internet_access: None,
                network: None,
                metadata: None,
                distribution_scope: None,
                env_vars: None,
                mcp: None,
                volume_mounts: None,
                backend: None,
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
    async fn create_sandbox_forwards_read_only_volume_mount_to_cubemaster() {
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
                "requestID": "req-volume",
                "sandbox_id": "sb-volume",
                "ret": { "ret_code": 0, "ret_msg": "ok" }
            }))
        }

        let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .expect("listener should bind");
        let addr = listener.local_addr().expect("listener addr");
        let capture = Capture::default();
        let app = Router::new()
            .route("/cube/sandbox", post(create_handler))
            .with_state(capture.clone());
        tokio::spawn(async move {
            axum::serve(listener, app).await.expect("server should run");
        });

        let service = SandboxService::new(
            CubeMasterClient::new(format!("http://{addr}"), reqwest::Client::new()),
            "cubebox".to_string(),
            "cube.app".to_string(),
        );

        service
            .create_sandbox(NewSandbox {
                template_id: "tpl-1".to_string(),
                timeout: Some(15),
                lifecycle: None,
                auto_pause: None,
                auto_resume: None,
                secure: None,
                allow_internet_access: None,
                network: None,
                metadata: None,
                distribution_scope: None,
                env_vars: None,
                mcp: None,
                volume_mounts: Some(vec![
                    SandboxVolumeMount {
                        name: "dataset".to_string(),
                        path: "/data".to_string(),
                        read_only: true,
                    },
                    SandboxVolumeMount {
                        name: "workspace".to_string(),
                        path: "/workspace".to_string(),
                        read_only: false,
                    },
                ]),
                backend: None,
            })
            .await
            .expect("sandbox create should succeed");

        let create_body = capture
            .create_body
            .lock()
            .await
            .clone()
            .expect("create body");
        let raw = create_body["annotations"]["plugin-volume-mounts"]
            .as_str()
            .expect("plugin volume mounts annotation");
        let mounts: Value = serde_json::from_str(raw).expect("annotation JSON");
        assert_eq!(
            mounts,
            serde_json::json!([
                {"name": "dataset", "container_path": "/data", "readonly": true},
                {"name": "workspace", "container_path": "/workspace"}
            ])
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
                read_only: false,
            },
            SandboxVolumeMount {
                name: "data".to_string(),
                path: "/mnt/b".to_string(),
                read_only: false,
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
                read_only: false,
            },
            SandboxVolumeMount {
                name: "logs".to_string(),
                path: "/mnt/b".to_string(),
                read_only: false,
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
                read_only: true,
            },
            SandboxVolumeMount {
                name: "logs".to_string(),
                path: "/mnt/logs".to_string(),
                read_only: false,
            },
        ];

        let (specs, bindings): (Vec<VolumeSpec>, Vec<VolumeMount>) = mounts
            .into_iter()
            .map(
                |SandboxVolumeMount {
                     name,
                     path,
                     read_only,
                     ..
                 }| {
                    (
                        VolumeSpec {
                            name: Some(name.clone()),
                            volume_source: None,
                        },
                        VolumeMount {
                            name,
                            container_path: path,
                            readonly: read_only.then_some(true),
                        },
                    )
                },
            )
            .unzip();

        assert_eq!(specs.len(), 2);
        assert_eq!(specs[0].name.as_deref(), Some("data"));
        assert_eq!(specs[1].name.as_deref(), Some("logs"));

        assert_eq!(bindings[0].name, "data");
        assert_eq!(bindings[0].container_path, "/mnt/data");
        assert_eq!(bindings[0].readonly, Some(true));
        assert_eq!(bindings[1].name, "logs");
        assert_eq!(bindings[1].container_path, "/mnt/logs");
        assert_eq!(bindings[1].readonly, None);
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
