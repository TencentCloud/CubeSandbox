// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use std::collections::HashMap;

use uuid::Uuid;

use super::validate_allow_out_domains_require_deny_all;
use crate::{
    constants::ENVD_VERSION,
    cubemaster::{
        datetime_from_unix_nanos, extract_template_id, ContainerSpec, CreateSandboxRequest,
        CubeEgressRule, CubeEgressRuleAction, CubeEgressRuleInject, CubeEgressRuleMatch,
        CubeMasterClient, CubeMasterError, CubeNetworkConfig, DeleteSandboxRequest, EnvVar,
        ImageSpec, ListSandboxRequest, SandboxInfo, SandboxLogsRequest, SandboxRefreshRequest,
        SandboxStatus, SandboxTimeoutRequest, SandboxUpdateRequest,
    },
    error::{AppError, AppResult},
    models::{
        EgressRule, EnvVars, LogLevel as ModelLogLevel, NewSandbox, Sandbox, SandboxDetail,
        SandboxLog, SandboxLogEntry, SandboxLogs, SandboxLogsV2Response, SandboxNetworkConfig,
        SandboxState,
    },
};

const RET_CODE_OK: i32 = 0;
const RET_CODE_HTTP_OK: i32 = 200;
const RET_CODE_NOT_FOUND: i32 = 130404;
const RET_CODE_CONFLICT: i32 = 130409;
const HOSTDIR_MOUNT_KEY: &str = "host-mount";

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
        let end_at = summary
            .as_ref()
            .and_then(|s| s.end_at.as_ref().cloned())
            .or(d.end_at)
            .unwrap_or(started_at);

        Ok(SandboxDetail {
            template_id: d.template_id,
            alias: None,
            sandbox_id: d.sandbox_id,
            client_id: d.host_id,
            started_at,
            end_at,
            envd_version: ENVD_VERSION.to_string(),
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
        let template_id = body.template_id.clone();
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

        let labels = body.metadata.map(|mut meta| {
            if let Some(value) = meta.remove(HOSTDIR_MOUNT_KEY) {
                annotations.insert(HOSTDIR_MOUNT_KEY.to_string(), value);
            }
            meta
        });

        let cube_network_config =
            build_cube_network_config(body.allow_internet_access, body.network.as_ref())?;

        // Derive the two CubeMaster-side bools from the e2b-shaped lifecycle
        // object. Absent lifecycle keeps today's behaviour: idle sandboxes
        // are killed (auto_pause = false), and auto_resume defaults off.
        let (auto_pause, auto_resume) = body
            .lifecycle
            .as_ref()
            .map(|lc| {
                use crate::models::SandboxOnTimeout;
                (
                    matches!(lc.on_timeout, SandboxOnTimeout::Pause),
                    lc.auto_resume,
                )
            })
            .unwrap_or((false, false));

        let req = CreateSandboxRequest {
            request_id: new_request_id(),
            instance_type: self.instance_type.clone(),
            timeout: Some(body.timeout),
            annotations,
            labels,
            distribution_scope: body.distribution_scope,
            volumes: None,
            containers: build_env_container(body.env_vars),
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

        Ok(self.sandbox_response(template_id, resp.sandbox_id, resp.request_id))
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
            .map_err(internal_error)?;

        resp.ret
            .into_result()
            .map_err(|e| sandbox_not_found_or_internal(e, sandbox_id))?;

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

    pub async fn resume_sandbox(&self, sandbox_id: &str, timeout: i32) -> AppResult<Sandbox> {
        let resp = self
            .cubemaster
            .update_sandbox(&self.build_update_request(sandbox_id, "resume", Some(timeout)))
            .await
            .map_err(|e| map_update_cubemaster_err(e, sandbox_id))?;

        ensure_update_result(
            resp.ret.ret_code,
            resp.ret.ret_msg,
            sandbox_id,
            "is already running",
        )?;

        let d = self.fetch_sandbox_detail(sandbox_id).await?;
        Ok(self.sandbox_response(d.template_id, sandbox_id.to_string(), d.host_id))
    }

    pub async fn connect_sandbox(&self, sandbox_id: &str, timeout: i32) -> AppResult<Sandbox> {
        let mut d = self.fetch_sandbox_detail(sandbox_id).await?;

        if d.status == SandboxStatus::Paused {
            let resp = self
                .cubemaster
                .update_sandbox(&self.build_update_request(sandbox_id, "resume", Some(timeout)))
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

        Ok(self.sandbox_response(d.template_id, sandbox_id.to_string(), d.host_id))
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
    ) -> Sandbox {
        Sandbox {
            template_id,
            sandbox_id,
            alias: None,
            client_id,
            envd_version: ENVD_VERSION.to_string(),
            envd_access_token: None,
            traffic_access_token: None,
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

pub(crate) fn from_cubemaster_info(s: SandboxInfo) -> crate::models::ListedSandbox {
    use crate::models::ListedSandbox;

    let now = chrono::Utc::now();
    let template_id = extract_template_id(&s.template_id, &s.annotations, &s.labels);

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
        end_at: s.end_at.unwrap_or(now),
        cpu_count: s.cpu_count,
        memory_mb: s.memory_mb,
        disk_size_mb: Some(0),
        metadata: optional_metadata(s.labels),
        state: sandbox_state_from_str(&s.status),
        envd_version: ENVD_VERSION.to_string(),
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

    if allow_internet_access.is_none()
        && allow_out.is_empty()
        && deny_out.is_empty()
        && rules.is_empty()
    {
        return Ok(None);
    }

    Ok(Some(CubeNetworkConfig {
        allow_internet_access,
        allow_out,
        deny_out,
        rules,
    }))
}

/// Build the single-container payload that carries the user-supplied env vars
/// down to CubeMaster.
///
/// Each sandbox is modelled as one container. The env vars are attached to that
/// container's `envs` and every other field is left empty:
///   - The empty `image` is overwritten with the template image by CubeMaster's
///     `applyTemplateToContainer`, after which Cubelet resolves the image config.
///   - Empty `command`/`args` fall back to the image `Entrypoint`/`Cmd` inside
///     Cubelet's `command.WithProcessArgs`, so the entrypoint (cube-entrypoint.sh)
///     still runs and envd still starts.
/// Env vars are validated before being forwarded:
///   - A key must be a usable name: non-empty, with no surrounding whitespace,
///     no `=` and no control characters (see [`is_valid_env_key`]).
///   - A value must be non-empty, not whitespace-only, and contain no NUL byte
///     (a NUL would truncate the `KEY=VALUE` C string at execve).
///   - Reserved names a sandbox owner must not set are dropped. `LD_PRELOAD`,
///     `LD_AUDIT`, `LD_LIBRARY_PATH`, `GCONV_PATH` and `GCONV_MODULES` can
///     inject or hijack code into every dynamically linked process in the
///     sandbox (including envd) — via the dynamic linker or glibc's gconv
///     mechanism — so they are blocked at the API boundary as defence-in-depth.
///     The check is intentionally case-sensitive: glibc's dynamic linker is
///     case-sensitive, so lowercase variants such as `ld_preload` are inert.
///
/// Merge / override order (this is why image-owned vars stay protected, and why
/// the reserved names above are not): CubeMaster concatenates the user envs with
/// the template envs (`append(ctr.Envs, templateCtr.Envs...)`, where the template
/// envs are a copy of the image ENV), and Cubelet's `env.GenOpt` feeds first the
/// image ENV and then the container envs into containerd's `oci.WithEnv`.
/// `oci.WithEnv` dedupes by key keeping the last value, so on a key collision
/// the template/image value wins — a user env var cannot override image-owned
/// vars such as `ENVD_*` or `PATH`. `LD_PRELOAD`/`LD_AUDIT` are not in the image
/// ENV, so no template copy shadows them; they are dropped here instead. Note
/// this differs from E2B's "user value wins" semantics for colliding keys;
/// flipping the CubeMaster append order is tracked as a follow-up.
///
/// With no env vars left after validation we return an empty list, preserving
/// the prior behaviour where `containers` was an empty `vec![]`.
pub(crate) fn build_env_container(env_vars: Option<EnvVars>) -> Vec<ContainerSpec> {
    let Some(vars) = env_vars else {
        return Vec::new();
    };
    // Reserved env var names a sandbox owner must not set via the API.
    // Sorted; kept to pure code-injection vectors with no legitimate use.
    const RESERVED_ENV_KEYS: &[&str] = &[
        "GCONV_MODULES",
        "GCONV_PATH",
        "LD_AUDIT",
        "LD_LIBRARY_PATH",
        "LD_PRELOAD",
    ];

    // One pass: collect the entries we drop (so a misconfigured caller is not
    // silently surprised) while building the forwarded list.
    let mut dropped_reserved: Vec<String> = Vec::new();
    let mut dropped_invalid: Vec<String> = Vec::new();
    let envs: Vec<EnvVar> = vars
        .into_iter()
        .filter_map(|(key, value)| {
            if RESERVED_ENV_KEYS.contains(&key.as_str()) {
                dropped_reserved.push(key);
                return None;
            }
            if !is_valid_env_key(&key) || value.contains('\0') {
                dropped_invalid.push(key);
                return None;
            }
            if value.trim().is_empty() {
                // Empty or whitespace-only values carry no usable value.
                return None;
            }
            Some(EnvVar { key, value })
        })
        .collect();
    if !dropped_reserved.is_empty() {
        tracing::warn!(
            dropped = ?dropped_reserved,
            "dropping reserved env var names from sandbox create request; \
             LD_PRELOAD/LD_AUDIT/LD_LIBRARY_PATH (dynamic linker) and \
             GCONV_PATH/GCONV_MODULES (glibc gconv) can inject or hijack code \
             into every dynamically linked process in the sandbox and are not \
             allowed"
        );
    }
    if !dropped_invalid.is_empty() {
        tracing::warn!(
            dropped = ?dropped_invalid,
            "dropping env var entries with an invalid key or value (empty/blank \
             key, key with '=' or a control character, or value with a NUL) \
             from sandbox create request"
        );
    }
    if envs.is_empty() {
        return Vec::new();
    }
    vec![ContainerSpec {
        name: None,
        image: ImageSpec {
            image: String::new(),
            storage_media: None,
        },
        command: None,
        args: None,
        working_dir: None,
        resources: None,
        envs: Some(envs),
        volume_mounts: None,
        dns_config: None,
        r_limit: None,
        security_context: None,
        probe: None,
        annotations: None,
    }]
}

/// A permissive, correctness-focused check for an env var name.
///
/// We deliberately do *not* restrict the key to the POSIX `[A-Za-z_][A-Za-z0-9_]*`
/// shape: real-world names use lowercase, digits, dots and dashes, and rejecting
/// them would break legitimate callers. We only reject names that cannot round-trip
/// into the container or are meaningless: an empty or whitespace-only key, one with
/// leading/trailing whitespace (which would not match the reserved-name check after
/// a potential downstream trim), one containing `=` (env entries are serialised as
/// `KEY=VALUE` and downstream code splits on the first `=`), or one containing a
/// control character (e.g. a newline, which would break the `KEY=VALUE` line
/// format).
fn is_valid_env_key(key: &str) -> bool {
    !key.is_empty()
        && key.trim() == key
        && !key.contains('=')
        && !key.chars().any(|c| c.is_control())
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

    use super::{
        build_cube_network_config, build_env_container, filter_by_metadata, from_cubemaster_info,
        is_valid_env_key,
    };
    use crate::cubemaster::{CreateSandboxRequest, ListSandboxResponse, SandboxInfo};
    use crate::models::{
        EgressRule, EgressRuleAction, EgressRuleInject, EgressRuleMatch, SandboxNetworkConfig,
        SandboxState,
    };

    #[test]
    fn env_container_is_empty_when_no_env_vars() {
        // No env vars at all (the SDK omits the field) and an explicitly empty
        // map both keep the prior `containers: vec![]` behaviour.
        assert!(build_env_container(None).is_empty());
        assert!(build_env_container(Some(HashMap::new())).is_empty());
    }

    #[test]
    fn env_container_wraps_each_var_as_an_envvar() {
        let vars = HashMap::from([
            ("API_KEY".to_string(), "sk-123".to_string()),
            ("DEBUG".to_string(), "true".to_string()),
        ]);

        let containers = build_env_container(Some(vars));
        assert_eq!(containers.len(), 1);

        let container = &containers[0];
        // The image is intentionally empty: CubeMaster's template merge fills it
        // from the resolved template. command/args stay unset so Cubelet falls
        // back to the image entrypoint.
        assert!(container.image.image.is_empty());
        assert!(container.command.is_none());
        assert!(container.args.is_none());

        let envs = container.envs.as_ref().expect("envs should be set");
        assert_eq!(envs.len(), 2);
        let lookup: HashMap<&str, &str> = envs
            .iter()
            .map(|e| (e.key.as_str(), e.value.as_str()))
            .collect();
        assert_eq!(lookup.get("API_KEY").copied(), Some("sk-123"));
        assert_eq!(lookup.get("DEBUG").copied(), Some("true"));
    }

    #[test]
    fn env_container_drops_reserved_and_empty_entries() {
        // Reserved names (incl. GCONV_*), a whitespace-padded reserved name
        // (would bypass a naive exact match), whitespace-only/invalid keys,
        // and empty/whitespace-only/NUL values are filtered out; only valid
        // entries survive.
        let vars = HashMap::from([
            ("API_KEY".to_string(), "sk-123".to_string()),
            ("LD_PRELOAD".to_string(), "/tmp/x.so".to_string()),
            (" LD_AUDIT".to_string(), "/tmp/y.so".to_string()),
            ("LD_LIBRARY_PATH".to_string(), "/tmp/hijack".to_string()),
            ("GCONV_PATH".to_string(), "/tmp/gconv".to_string()),
            ("GCONV_MODULES".to_string(), "/tmp/mods".to_string()),
            ("".to_string(), "no-key".to_string()),
            ("   ".to_string(), "ws-key".to_string()),
            ("EMPTY".to_string(), "".to_string()),
            ("BLANK".to_string(), "   \t ".to_string()),
            ("NULLVAL".to_string(), "a\0b".to_string()),
            ("lower_var".to_string(), "ok".to_string()),
        ]);

        let containers = build_env_container(Some(vars));
        assert_eq!(containers.len(), 1);
        let container = &containers[0];
        // Spot-check the contract this function owns: a single container whose
        // image is left empty (CubeMaster fills it) and whose envs survive. We
        // don't exhaustively assert every `None` field — that would break on any
        // unrelated ContainerSpec change.
        assert!(container.image.image.is_empty());
        let envs = container.envs.as_ref().expect("envs should be set");
        assert_eq!(envs.len(), 2);
        let lookup: HashMap<&str, &str> = envs
            .iter()
            .map(|e| (e.key.as_str(), e.value.as_str()))
            .collect();
        assert_eq!(lookup.get("API_KEY").copied(), Some("sk-123"));
        assert_eq!(lookup.get("lower_var").copied(), Some("ok"));
    }

    #[test]
    fn env_container_returns_empty_when_all_entries_dropped() {
        // Non-empty input where every entry is filtered out must yield an empty
        // list (the early-return path), not a container carrying empty envs.
        let vars = HashMap::from([
            ("LD_PRELOAD".to_string(), "/tmp/x.so".to_string()),
            ("GCONV_PATH".to_string(), "/tmp/gconv".to_string()),
            ("EMPTY".to_string(), "".to_string()),
            ("BAD=KEY".to_string(), "v".to_string()),
        ]);
        assert!(build_env_container(Some(vars)).is_empty());
    }

    #[test]
    fn env_container_drops_entries_with_an_invalid_key() {
        // Keys with '=' corrupt the downstream KEY=VALUE split, and keys with
        // control characters break the line format; a permissive-but-correct
        // check drops them while keeping ordinary names (incl. lowercase/dash).
        let vars = HashMap::from([
            ("FOO=BAR".to_string(), "x".to_string()),
            ("BAD\nKEY".to_string(), "y".to_string()),
            ("\0NULL".to_string(), "z".to_string()),
            ("dash-key".to_string(), "kept".to_string()),
        ]);

        let containers = build_env_container(Some(vars));
        assert_eq!(containers.len(), 1);
        let envs = containers[0].envs.as_ref().expect("envs should be set");
        assert_eq!(envs.len(), 1);
        assert_eq!(envs[0].key, "dash-key");
        assert_eq!(envs[0].value, "kept");
    }

    #[test]
    fn is_valid_env_key_accepts_ordinary_names() {
        // Deliberately permissive: lowercase, digits, dashes, dots and unicode
        // are all fine — only structural breakers (=, control chars) are rejected.
        assert!(is_valid_env_key("API_KEY"));
        assert!(is_valid_env_key("lower_var"));
        assert!(is_valid_env_key("dash-key"));
        assert!(is_valid_env_key("app.db.url"));
        assert!(is_valid_env_key("1startsWithDigit"));
        assert!(!is_valid_env_key(""));
        assert!(!is_valid_env_key(" "));
        assert!(!is_valid_env_key("   "));
        assert!(!is_valid_env_key(" PADDED"));
        assert!(!is_valid_env_key("PADDED "));
        assert!(!is_valid_env_key("FOO=BAR"));
        assert!(!is_valid_env_key("BAD\nKEY"));
        assert!(!is_valid_env_key("with\0null"));
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
}
