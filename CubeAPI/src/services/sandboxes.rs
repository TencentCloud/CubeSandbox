// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use std::collections::HashMap;

use uuid::Uuid;

use crate::{
    constants::ENVD_VERSION,
    cubemaster::{
        extract_template_id, CreateSandboxRequest, CubeMasterClient, CubeMasterError,
        CubeVSContext, DeleteSandboxRequest, ListSandboxRequest, SandboxInfo, SandboxLogsRequest,
        SandboxRefreshRequest, SandboxSnapshotRequest, SandboxStatus, SandboxTimeoutRequest,
        SandboxUpdateRequest,
    },
    error::{AppError, AppResult},
    models::{
        LogLevel as ModelLogLevel, NewSandbox, Sandbox, SandboxDetail, SandboxLog, SandboxLogEntry,
        SandboxLogs, SandboxLogsV2Response, SandboxNetworkConfig, SandboxState, SnapshotInfo,
    },
};

const RET_CODE_OK: i32 = 0;
const RET_CODE_HTTP_OK: i32 = 200;
const RET_CODE_NOT_FOUND: i32 = 130404;
const RET_CODE_CONFLICT: i32 = 130409;
const HOSTDIR_MOUNT_KEY: &str = "host-mount";
const HOSTDIR_MOUNT_LEGACY_KEY: &str = "hostdir-mount";

fn split_metadata_labels_and_hostdir_annotation(
    metadata: Option<HashMap<String, String>>,
    annotations: &mut HashMap<String, String>,
) -> Option<HashMap<String, String>> {
    metadata.map(|mut meta| {
        let host_mount = meta.remove(HOSTDIR_MOUNT_KEY);
        let legacy_hostdir_mount = meta.remove(HOSTDIR_MOUNT_LEGACY_KEY);
        if let Some(value) = host_mount.or(legacy_hostdir_mount) {
            annotations.insert(HOSTDIR_MOUNT_KEY.to_string(), value);
        }
        meta
    })
}

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

        resp.ret.into_result().map_err(internal_error)?;

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

        let labels = split_metadata_labels_and_hostdir_annotation(body.metadata, &mut annotations);

        let req = CreateSandboxRequest {
            request_id: new_request_id(),
            instance_type: self.instance_type.clone(),
            timeout: Some(body.timeout),
            annotations,
            labels,
            volumes: None,
            containers: vec![],
            exposed_ports: vec![],
            network_type: Some("tap".to_string()),
            cubevs_context: build_cubevs_context(body.allow_internet_access, body.network.as_ref()),
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
            .map_err(internal_error)?;

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
            .map_err(internal_error)?;

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
                .map_err(internal_error)?;

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

    pub async fn create_snapshot(
        &self,
        sandbox_id: &str,
        name: Option<String>,
    ) -> AppResult<SnapshotInfo> {
        let snapshot_name = name.unwrap_or_else(new_request_id);
        let names = vec![snapshot_name.clone()];
        let req = SandboxSnapshotRequest {
            request_id: new_request_id(),
            sandbox_id: sandbox_id.to_string(),
            instance_type: self.instance_type.clone(),
            name: Some(snapshot_name),
            names: names.clone(),
            sync: false,
            timeout: Some(60),
        };

        match self.cubemaster.create_sandbox_snapshot(&req).await {
            Ok(resp) => {
                resp.ret.into_result().map_err(|e| {
                    if e.is_not_found() {
                        AppError::NotFound(format!("sandbox {} not found", sandbox_id))
                    } else if e.is_conflict() {
                        AppError::Conflict(format!(
                            "sandbox {} has a snapshot operation in progress",
                            sandbox_id
                        ))
                    } else {
                        internal_error(e)
                    }
                })?;

                Ok(SnapshotInfo {
                    snapshot_id: resp.snapshot_id,
                    names: if resp.names.is_empty() {
                        names
                    } else {
                        resp.names
                    },
                })
            }
            Err(e) if e.is_endpoint_missing() => Ok(SnapshotInfo {
                snapshot_id: new_request_id(),
                names,
            }),
            Err(e) if e.is_not_found() => Err(AppError::NotFound(format!(
                "sandbox {} not found",
                sandbox_id
            ))),
            Err(e) => Err(internal_error(e)),
        }
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
                            line: l.line.clone(),
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
        start: Option<i64>,
        limit: i32,
    ) -> SandboxLogsRequest {
        SandboxLogsRequest {
            request_id: new_request_id(),
            sandbox_id: sandbox_id.to_string(),
            instance_type: self.instance_type.clone(),
            start,
            limit,
            source: "all".to_string(),
        }
    }
}

fn internal_error(error: impl std::fmt::Display) -> AppError {
    AppError::Internal(anyhow::anyhow!(error.to_string()))
}

fn sandbox_not_found_or_internal(e: CubeMasterError, sandbox_id: &str) -> AppError {
    if e.is_not_found() {
        AppError::NotFound(format!("sandbox {} not found", sandbox_id))
    } else {
        internal_error(e)
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
        return Err(AppError::Conflict(format!(
            "sandbox {} {}",
            sandbox_id, conflict_message
        )));
    }
    Err(AppError::Internal(anyhow::anyhow!(ret_msg)))
}

pub(crate) fn from_cubemaster_info(s: SandboxInfo) -> crate::models::ListedSandbox {
    use crate::models::ListedSandbox;

    let now = chrono::Utc::now();
    let template_id = extract_template_id(&s.template_id, &s.annotations, &s.labels);

    ListedSandbox {
        template_id,
        alias: None,
        sandbox_id: s.sandbox_id,
        client_id: s.host_id,
        started_at: s.started_at.unwrap_or(now),
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
    let mut fields = HashMap::new();
    fields.insert("source".to_string(), log.source);

    SandboxLogEntry {
        timestamp: log.timestamp,
        message: log.line,
        level: ModelLogLevel::Info,
        fields,
    }
}

fn new_request_id() -> String {
    Uuid::new_v4().to_string()
}

pub(crate) fn build_cubevs_context(
    allow_internet_access: Option<bool>,
    network: Option<&SandboxNetworkConfig>,
) -> Option<CubeVSContext> {
    let effective_allow = network
        .and_then(|n| n.allow_public_traffic)
        .or(allow_internet_access);
    let allow_out = network
        .and_then(|n| n.allow_out.clone())
        .unwrap_or_default();
    let deny_out = network.and_then(|n| n.deny_out.clone()).unwrap_or_default();

    if effective_allow.is_none() && allow_out.is_empty() && deny_out.is_empty() {
        return None;
    }

    Some(CubeVSContext {
        allow_internet_access: effective_allow,
        allow_out,
        deny_out,
    })
}

#[cfg(test)]
mod tests {
    use std::collections::HashMap;

    use super::{
        build_cubevs_context, filter_by_metadata, from_cubemaster_info,
        split_metadata_labels_and_hostdir_annotation,
    };
    use crate::cubemaster::SandboxInfo;
    use crate::models::SandboxNetworkConfig;

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
    fn split_metadata_moves_host_mount_to_annotation() {
        let mut annotations = HashMap::new();
        let labels = split_metadata_labels_and_hostdir_annotation(
            Some(HashMap::from([
                ("host-mount".to_string(), "mount-config".to_string()),
                ("user".to_string(), "alice".to_string()),
            ])),
            &mut annotations,
        )
        .expect("labels");

        assert_eq!(
            annotations.get("host-mount").map(String::as_str),
            Some("mount-config")
        );
        assert_eq!(labels.get("user").map(String::as_str), Some("alice"));
        assert!(!labels.contains_key("host-mount"));
    }

    #[test]
    fn split_metadata_accepts_legacy_hostdir_mount_and_prefers_canonical() {
        let mut annotations = HashMap::new();
        let labels = split_metadata_labels_and_hostdir_annotation(
            Some(HashMap::from([
                ("host-mount".to_string(), "canonical".to_string()),
                ("hostdir-mount".to_string(), "legacy".to_string()),
            ])),
            &mut annotations,
        )
        .expect("labels");

        assert_eq!(
            annotations.get("host-mount").map(String::as_str),
            Some("canonical")
        );
        assert!(!labels.contains_key("host-mount"));
        assert!(!labels.contains_key("hostdir-mount"));
    }

    #[test]
    fn network_context_prefers_explicit_network_config() {
        let context = build_cubevs_context(
            Some(false),
            Some(&SandboxNetworkConfig {
                allow_public_traffic: Some(true),
                allow_out: Some(vec!["github.com".to_string()]),
                deny_out: None,
                mask_request_host: None,
            }),
        )
        .expect("context should exist");

        assert_eq!(context.allow_internet_access, Some(true));
        assert_eq!(context.allow_out, vec!["github.com".to_string()]);
    }

    #[test]
    fn listed_sandbox_preserves_resources_from_cubemaster_list() {
        let listed = from_cubemaster_info(SandboxInfo {
            sandbox_id: "sb-1".to_string(),
            host_id: "host-1".to_string(),
            status: "running".to_string(),
            started_at: None,
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
}
