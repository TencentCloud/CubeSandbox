// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use utoipa::{IntoParams, ToSchema};
use validator::Validate;

// ─── Common ────────────────────────────────────────────────────────────────

#[derive(Debug, Serialize, Deserialize, ToSchema)]
pub struct ApiError {
    pub code: i32,
    pub message: String,
}

impl ApiError {
    pub fn new(code: i32, message: impl Into<String>) -> Self {
        Self {
            code,
            message: message.into(),
        }
    }
}

// ─── Sandbox shared types ──────────────────────────────────────────────────

pub type SandboxMetadata = HashMap<String, String>;
pub type EnvVars = HashMap<String, String>;

/// State of the sandbox (running | paused)
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq, ToSchema)]
#[serde(rename_all = "lowercase")]
pub enum SandboxState {
    Running,
    Paused,
    Pausing,
}

/// Network configuration for sandbox egress/ingress control.
#[derive(Debug, Clone, Serialize, Deserialize, Default, ToSchema)]
pub struct SandboxNetworkConfig {
    #[serde(rename = "allowPublicTraffic", skip_serializing_if = "Option::is_none")]
    pub allow_public_traffic: Option<bool>,
    #[serde(
        rename = "allowOut",
        alias = "allow_out",
        skip_serializing_if = "Option::is_none"
    )]
    pub allow_out: Option<Vec<String>>,
    #[serde(
        rename = "denyOut",
        alias = "deny_out",
        skip_serializing_if = "Option::is_none"
    )]
    pub deny_out: Option<Vec<String>>,
    #[serde(rename = "maskRequestHost", skip_serializing_if = "Option::is_none")]
    pub mask_request_host: Option<String>,
    /// L7 egress rules, evaluated first-match-wins in list order.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub rules: Option<Vec<EgressRule>>,
}

/// L7 egress rule: match conditions + action (allow/deny, audit, credential injection).
#[derive(Debug, Clone, Serialize, Deserialize, ToSchema)]
pub struct EgressRule {
    /// Human-readable label used for audit logging.
    pub name: String,
    pub r#match: EgressRuleMatch,
    pub action: EgressRuleAction,
}

/// Rule match conditions. All fields optional; empty match matches any request.
///
/// Multi-field semantics: AND across fields, OR within `method`.
/// Comparisons on sni/host/scheme are case-insensitive.
#[derive(Debug, Clone, Serialize, Deserialize, Default, ToSchema)]
pub struct EgressRuleMatch {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub sni: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub host: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub method: Option<Vec<String>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub path: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub scheme: Option<String>,
}

/// Rule action.
///
/// - `allow=true`: pass the request through; optional credential injection.
/// - `allow=false`: reject (HTTP 403); `inject` is ignored if set.
/// - `audit` defaults to `"metadata"` server-side when omitted.
#[derive(Debug, Clone, Serialize, Deserialize, ToSchema)]
pub struct EgressRuleAction {
    pub allow: bool,
    /// One of `"full"`, `"metadata"`, `"none"`.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub audit: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub inject: Option<Vec<EgressRuleInject>>,
}

/// Credential injection. Only honored when `Action.allow=true` and the
/// request is HTTPS with matching SNI/Host (downstream enforces).
#[derive(Debug, Clone, Serialize, Deserialize, ToSchema)]
pub struct EgressRuleInject {
    pub header: String,
    pub secret: String,
    /// Template containing `${SECRET}`; defaults to `"${SECRET}"` when omitted.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub format: Option<String>,
}

/// Sandbox lifecycle configuration. Mirrors the e2b SDK's `lifecycle` object —
/// see https://e2b.dev/docs/sandbox/auto-resume for the canonical reference.
///
/// `on_timeout` decides what happens when the sandbox idle timer fires; the
/// historical default is "kill" (delete the sandbox) which matches today's
/// behaviour. `auto_resume` only takes effect when `on_timeout = "pause"` —
/// it tells the proxy/sidecar to wake a paused sandbox up automatically when
/// activity arrives, instead of returning an error.
#[derive(Debug, Clone, Default, Serialize, Deserialize, ToSchema)]
pub struct SandboxLifecycleConfig {
    /// "kill" (default) | "pause".
    #[serde(rename = "onTimeout", alias = "on_timeout", default)]
    pub on_timeout: SandboxOnTimeout,

    /// Auto-resume on activity. Defaults to false. Only meaningful when
    /// `on_timeout` is set to "pause".
    #[serde(rename = "autoResume", alias = "auto_resume", default)]
    pub auto_resume: bool,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, ToSchema)]
#[serde(rename_all = "lowercase")]
pub enum SandboxOnTimeout {
    Kill,
    Pause,
}

impl Default for SandboxOnTimeout {
    fn default() -> Self {
        Self::Kill
    }
}

/// Volume mount inside the sandbox.
#[derive(Debug, Clone, Serialize, Deserialize, ToSchema)]
pub struct SandboxVolumeMount {
    pub name: String,
    pub path: String,
}

// ─── Sandbox — create request ──────────────────────────────────────────────

/// Request body for POST /sandboxes
/// Field names match exactly what the E2B SDK sends.
/// Rule: ID abbreviations → uppercase (templateID, sandboxID, envVars);
///       allow_internet_access is a known SDK snake_case quirk;
///       lifecycle is a nested object — see SandboxLifecycleConfig.
/// `envVars` is the canonical field name; `envs` is accepted as a compatibility
/// alias for E2B SDK callers.
#[derive(Debug, Deserialize, Validate, ToSchema)]
#[allow(dead_code)]
pub struct NewSandbox {
    #[serde(rename = "templateID")]
    pub template_id: String,

    /// Optional idle TTL in seconds. See docs/guide/lifecycle.md.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub timeout: Option<i32>,

    /// Sandbox lifecycle configuration. Maps to e2b's `lifecycle` object so
    /// callers that already speak e2b can pass through unchanged. Absent
    /// (None) means today's behaviour: idle sandboxes are killed.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub lifecycle: Option<SandboxLifecycleConfig>,

    #[serde(skip_serializing_if = "Option::is_none")]
    pub secure: Option<bool>,

    /// SDK sends this as snake_case (known quirk).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub allow_internet_access: Option<bool>,

    #[serde(skip_serializing_if = "Option::is_none")]
    pub network: Option<SandboxNetworkConfig>,

    #[serde(skip_serializing_if = "Option::is_none")]
    pub metadata: Option<SandboxMetadata>,

    #[serde(
        rename = "distributionScope",
        alias = "distribution_scope",
        skip_serializing_if = "Option::is_none"
    )]
    pub distribution_scope: Option<Vec<String>>,

    #[serde(
        alias = "envs",
        rename = "envVars",
        skip_serializing_if = "Option::is_none"
    )]
    pub env_vars: Option<EnvVars>,

    #[serde(skip_serializing_if = "Option::is_none")]
    pub mcp: Option<serde_json::Value>,

    #[serde(rename = "volumeMounts", skip_serializing_if = "Option::is_none")]
    pub volume_mounts: Option<Vec<SandboxVolumeMount>>,
}

// ─── Sandbox — create / connect response ──────────────────────────────────

/// Response for POST /sandboxes and POST /sandboxes/{id}/connect.
/// All ID abbreviations uppercase per E2B OpenAPI spec.
#[derive(Debug, Serialize, Deserialize, ToSchema)]
pub struct Sandbox {
    #[serde(rename = "templateID")]
    pub template_id: String,

    #[serde(rename = "sandboxID")]
    pub sandbox_id: String,

    #[serde(skip_serializing_if = "Option::is_none")]
    pub alias: Option<String>,

    #[serde(rename = "clientID")]
    pub client_id: String,

    #[serde(rename = "envdVersion")]
    pub envd_version: String,

    #[serde(rename = "envdAccessToken", skip_serializing_if = "Option::is_none")]
    pub envd_access_token: Option<String>,

    #[serde(rename = "trafficAccessToken", skip_serializing_if = "Option::is_none")]
    pub traffic_access_token: Option<String>,

    #[serde(skip_serializing_if = "Option::is_none")]
    pub domain: Option<String>,
}

// ─── Sandbox — list / detail responses ────────────────────────────────────

/// One entry in GET /sandboxes (RunningSandbox in OpenAPI spec).
#[derive(Debug, Serialize, Deserialize, ToSchema)]
pub struct ListedSandbox {
    #[serde(rename = "templateID")]
    pub template_id: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub alias: Option<String>,
    #[serde(rename = "sandboxID")]
    pub sandbox_id: String,
    #[serde(rename = "clientID")]
    pub client_id: String,
    #[serde(rename = "startedAt")]
    pub started_at: DateTime<Utc>,
    /// Projected next-timeout instant. Omitted for never-timeout sandboxes
    /// (no deadline) rather than being misreported as equal to startedAt.
    #[serde(rename = "endAt", skip_serializing_if = "Option::is_none")]
    pub end_at: Option<DateTime<Utc>>,
    #[serde(rename = "cpuCount")]
    pub cpu_count: i32,
    #[serde(rename = "memoryMB")]
    pub memory_mb: i32,
    #[serde(rename = "diskSizeMB", skip_serializing_if = "Option::is_none")]
    pub disk_size_mb: Option<i32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub metadata: Option<SandboxMetadata>,
    pub state: SandboxState,
    #[serde(rename = "envdVersion")]
    pub envd_version: String,
    #[serde(rename = "volumeMounts", skip_serializing_if = "Option::is_none")]
    pub volume_mounts: Option<Vec<SandboxVolumeMount>>,
}

/// Detailed sandbox info returned by GET /sandboxes/{sandboxID}.
#[derive(Debug, Serialize, Deserialize, ToSchema)]
pub struct SandboxDetail {
    #[serde(rename = "templateID")]
    pub template_id: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub alias: Option<String>,
    #[serde(rename = "sandboxID")]
    pub sandbox_id: String,
    #[serde(rename = "clientID")]
    pub client_id: String,
    #[serde(rename = "startedAt")]
    pub started_at: DateTime<Utc>,
    /// Projected next-timeout instant. Omitted for never-timeout sandboxes
    /// (no deadline) rather than being misreported as equal to startedAt.
    #[serde(rename = "endAt", skip_serializing_if = "Option::is_none")]
    pub end_at: Option<DateTime<Utc>>,
    #[serde(rename = "envdVersion")]
    pub envd_version: String,
    #[serde(rename = "envdAccessToken", skip_serializing_if = "Option::is_none")]
    pub envd_access_token: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub domain: Option<String>,
    #[serde(rename = "cpuCount")]
    pub cpu_count: i32,
    #[serde(rename = "memoryMB")]
    pub memory_mb: i32,
    #[serde(rename = "diskSizeMB", skip_serializing_if = "Option::is_none")]
    pub disk_size_mb: Option<i32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub metadata: Option<SandboxMetadata>,
    pub state: SandboxState,
    #[serde(rename = "volumeMounts", skip_serializing_if = "Option::is_none")]
    pub volume_mounts: Option<Vec<SandboxVolumeMount>>,
}

// ─── Sandbox — pause/resume/connect/snapshot ──────────────────────────────

/// Request body for POST /sandboxes/{id}/resume (deprecated).
#[derive(Debug, Deserialize, ToSchema)]
#[allow(dead_code)]
pub struct ResumedSandbox {
    /// Idle timeout in seconds; None when the client did not send one.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub timeout: Option<i32>,
    #[serde(rename = "autoPause", default)]
    pub auto_pause: bool,
}

/// Request body for POST /sandboxes/{id}/connect.
#[derive(Debug, Deserialize, Validate, ToSchema)]
pub struct ConnectSandbox {
    /// Idle timeout in seconds; None when the client did not send one.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub timeout: Option<i32>,
}

/// Request body for POST /sandboxes/{id}/snapshots.
#[derive(Debug, Deserialize, ToSchema)]
pub struct CreateSnapshotRequest {
    pub name: Option<String>,
}

/// Response for POST /sandboxes/{id}/snapshots.
#[derive(Debug, Serialize, ToSchema)]
pub struct SnapshotInfo {
    #[serde(rename = "snapshotID")]
    pub snapshot_id: String,
    pub names: Vec<String>,
}

/// Query parameters for GET /snapshots.
#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
pub struct ListSnapshotsQuery {
    /// Filter by originating sandbox ID.
    #[serde(rename = "sandboxID")]
    pub sandbox_id: Option<String>,
    /// Max items per page (default 100, max 100).
    pub limit: Option<i32>,
    /// Pagination cursor from previous response header x-next-token.
    #[serde(rename = "nextToken")]
    pub next_token: Option<String>,
}

/// One entry in the GET /snapshots list.
#[derive(Debug, Serialize, ToSchema)]
pub struct SnapshotListItem {
    #[serde(rename = "snapshotID")]
    pub snapshot_id: String,
    pub names: Vec<String>,
    pub status: String,
    #[serde(rename = "originSandboxID", skip_serializing_if = "Option::is_none")]
    pub origin_sandbox_id: Option<String>,
    #[serde(rename = "createdAt", skip_serializing_if = "Option::is_none")]
    pub created_at: Option<DateTime<Utc>>,
    #[serde(rename = "updatedAt", skip_serializing_if = "Option::is_none")]
    pub updated_at: Option<DateTime<Utc>>,
}

/// Request body for POST /sandboxes/{id}/rollback.
#[derive(Debug, Deserialize, ToSchema)]
pub struct RollbackRequest {
    #[serde(rename = "snapshotID")]
    pub snapshot_id: String,
}

/// Response for POST /sandboxes/{id}/rollback after synchronous completion.
#[derive(Debug, Serialize, ToSchema)]
pub struct RollbackResponse {
    #[serde(rename = "sandboxID")]
    pub sandbox_id: String,
    #[serde(rename = "snapshotID")]
    pub snapshot_id: String,
    #[serde(rename = "operationID")]
    pub operation_id: String,
    pub status: String,
}

/// Response for DELETE /templates/{templateID} when the target is a snapshot.
#[derive(Debug, Serialize, ToSchema)]
pub struct DeleteSnapshotResponse {
    #[serde(rename = "templateID")]
    pub template_id: String,
    #[serde(rename = "operationID")]
    pub operation_id: String,
    pub status: String,
}

// ─── Sandbox — logs ────────────────────────────────────────────────────────

#[derive(Debug, Serialize, Deserialize, Clone, ToSchema)]
#[serde(rename_all = "lowercase")]
pub enum LogLevel {
    Debug,
    Info,
    Warn,
    Error,
}

/// Single raw log line — matches E2B SandboxLog schema (timestamp + line).
#[derive(Debug, Serialize, Deserialize, ToSchema)]
pub struct SandboxLog {
    pub timestamp: DateTime<Utc>,
    pub line: String,
}

/// Structured log entry (v2 logs).
#[derive(Debug, Serialize, Deserialize, ToSchema)]
pub struct SandboxLogEntry {
    pub timestamp: DateTime<Utc>,
    pub message: String,
    pub level: LogLevel,
    pub fields: HashMap<String, String>,
}

/// Legacy log response — matches E2B SandboxLogs schema.
#[derive(Debug, Serialize, Deserialize, ToSchema)]
pub struct SandboxLogs {
    pub logs: Vec<SandboxLog>,
    #[serde(rename = "logEntries")]
    pub log_entries: Vec<SandboxLogEntry>,
}

/// v2 log response.
#[derive(Debug, Serialize, Deserialize, ToSchema)]
pub struct SandboxLogsV2Response {
    pub logs: Vec<SandboxLogEntry>,
}

/// Query params for v1 sandbox logs.
#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
pub struct SandboxLogsQuery {
    pub start: Option<i64>,
    #[serde(default = "default_log_limit")]
    pub limit: i32,
}

/// Query params for v2 sandbox logs.
#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
#[allow(dead_code)]
pub struct SandboxLogsV2Query {
    pub cursor: Option<i64>,
    #[serde(default = "default_log_limit")]
    pub limit: i32,
    pub direction: Option<String>,
}

fn default_log_limit() -> i32 {
    1000
}

// ─── Sandbox — timeout / refresh ──────────────────────────────────────────

/// Request body for POST /sandboxes/{id}/timeout
#[derive(Debug, Deserialize, Validate, ToSchema)]
pub struct SetTimeoutRequest {
    #[validate(custom(function = "validate_timeout_value"))]
    pub timeout: i32,
}

/// Validates the timeout value for SetTimeoutRequest.
///
/// Accepts:
/// - `>= 0`: normal TTL in seconds (0 = immediate expire)
/// - `-1`:  never-timeout sentinel (see `NEVER_TIMEOUT` in SDK)
///
/// Rejects all other negative values.
fn validate_timeout_value(timeout: i32) -> Result<(), validator::ValidationError> {
    if timeout >= 0 || timeout == -1 {
        Ok(())
    } else {
        Err(validator::ValidationError::new(
            "timeout_must_be_non_negative_or_never",
        ))
    }
}

/// Request body for POST /sandboxes/{id}/refreshes
#[derive(Debug, Deserialize, Validate, ToSchema)]
pub struct RefreshRequest {
    #[validate(range(min = 0, max = 3600))]
    pub duration: Option<i32>,
}

// ─── Sandbox — list query ──────────────────────────────────────────────────

/// Query params for GET /sandboxes.
#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
pub struct ListSandboxesQuery {
    pub metadata: Option<String>,
}

/// Query params for GET /v2/sandboxes.
#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
#[allow(dead_code)]
pub struct ListSandboxesV2Query {
    pub metadata: Option<String>,
    pub state: Option<String>,
    #[serde(rename = "nextToken")]
    pub next_token: Option<String>,
    #[serde(default = "default_page_limit")]
    pub limit: i32,
}

fn default_page_limit() -> i32 {
    100
}

#[cfg(test)]
mod tests {
    use super::{NewSandbox, SandboxNetworkConfig, SetTimeoutRequest};
    use validator::Validate;

    #[test]
    fn set_timeout_request_rejects_invalid_negative_values() {
        // Only -1 (NEVER_TIMEOUT) is accepted as a valid negative value.
        for timeout in [-2, -100, -999] {
            let req = SetTimeoutRequest { timeout };
            assert!(
                req.validate().is_err(),
                "timeout={timeout} should be rejected (only -1 is valid negative)"
            );
        }
    }

    #[test]
    fn set_timeout_request_accepts_never_timeout() {
        let req = SetTimeoutRequest { timeout: -1 };
        req.validate()
            .unwrap_or_else(|e| panic!("NEVER_TIMEOUT (-1) should be valid: {e}"));
    }

    #[test]
    fn set_timeout_request_accepts_zero_and_positive() {
        for timeout in [0, 60, 3600] {
            let req = SetTimeoutRequest { timeout };
            req.validate()
                .unwrap_or_else(|e| panic!("timeout={timeout} should be valid: {e}"));
        }
    }

    #[test]
    fn sandbox_network_config_accepts_snake_case_policy_fields() {
        let cfg: SandboxNetworkConfig = serde_json::from_value(serde_json::json!({
            "allow_out": ["api.example.com", "8.8.8.8"],
            "deny_out": ["0.0.0.0/0"]
        }))
        .expect("network config should deserialize");

        assert_eq!(
            cfg.allow_out,
            Some(vec!["api.example.com".to_string(), "8.8.8.8".to_string()])
        );
        assert_eq!(cfg.deny_out, Some(vec!["0.0.0.0/0".to_string()]));
    }

    #[test]
    fn new_sandbox_accepts_e2b_envs_alias() {
        let req: NewSandbox = serde_json::from_value(serde_json::json!({
            "templateID": "tpl-1",
            "envs": {
                "CUBE_TEST_ENV": "value"
            }
        }))
        .expect("new sandbox request should deserialize");

        assert_eq!(
            req.env_vars
                .as_ref()
                .and_then(|envs| envs.get("CUBE_TEST_ENV"))
                .map(String::as_str),
            Some("value")
        );
    }
}

// ─── Templates ─────────────────────────────────────────────────────────────

/// Query params for GET /templates.
#[derive(Debug, Deserialize, Default, IntoParams)]
#[into_params(parameter_in = Query)]
#[allow(dead_code)]
pub struct ListTemplatesQuery {
    /// Optional CubeMaster instance_type filter (currently no server-side filter;
    /// reserved for future use).
    pub instance_type: Option<String>,
}

/// Summary row returned by GET /templates.
#[derive(Debug, Serialize, Deserialize, Clone, ToSchema)]
pub struct TemplateSummary {
    #[serde(rename = "templateID")]
    pub template_id: String,
    #[serde(rename = "instanceType", skip_serializing_if = "Option::is_none")]
    pub instance_type: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub version: Option<String>,
    pub status: String,
    #[serde(rename = "lastError", skip_serializing_if = "Option::is_none")]
    pub last_error: Option<String>,
    #[serde(rename = "createdAt", skip_serializing_if = "Option::is_none")]
    pub created_at: Option<String>,
    #[serde(rename = "imageInfo", skip_serializing_if = "Option::is_none")]
    pub image_info: Option<String>,
    /// Latest create/rebuild job id for the template.
    #[serde(rename = "jobID", skip_serializing_if = "Option::is_none")]
    pub job_id: Option<String>,
}

/// Detailed template response (GET /templates/:id).
#[derive(Debug, Serialize, ToSchema)]
pub struct TemplateDetail {
    #[serde(rename = "templateID")]
    pub template_id: String,
    #[serde(rename = "instanceType", skip_serializing_if = "Option::is_none")]
    pub instance_type: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub version: Option<String>,
    pub status: String,
    #[serde(rename = "lastError", skip_serializing_if = "Option::is_none")]
    pub last_error: Option<String>,
    pub replicas: Vec<serde_json::Value>,
    #[serde(rename = "createRequest", skip_serializing_if = "Option::is_none")]
    pub create_request: Option<serde_json::Value>,
    /// Network type used when the template was created, e.g. "tap".
    #[serde(rename = "networkType", skip_serializing_if = "Option::is_none")]
    pub network_type: Option<String>,
    /// Whether public internet access is allowed for sandboxes from this template.
    #[serde(
        rename = "allowInternetAccess",
        skip_serializing_if = "Option::is_none"
    )]
    pub allow_internet_access: Option<bool>,
    /// Latest create/rebuild job id for the template.
    #[serde(rename = "jobID", skip_serializing_if = "Option::is_none")]
    pub job_id: Option<String>,
}

/// Body for POST /templates (create from image).
#[derive(Debug, Deserialize, Validate, ToSchema)]
pub struct CreateTemplateRequest {
    /// Deprecated and ignored. Template IDs are always generated server-side
    /// with the `tpl-` prefix; clients must use the returned `templateID`.
    #[serde(rename = "templateID", default)]
    #[allow(dead_code)]
    pub template_id: String,
    #[serde(rename = "instanceType", default)]
    pub instance_type: Option<String>,
    /// Container image reference, e.g. `registry.example.com/code:latest`.
    #[validate(length(min = 1))]
    pub image: String,
    /// Writable layer size for the rootfs, e.g. "1G".
    #[serde(rename = "writableLayerSize", default)]
    pub writable_layer_size: Option<String>,
    /// Ports the container listens on.
    #[serde(rename = "exposedPorts", default)]
    pub exposed_ports: Option<Vec<u16>>,
    /// HTTP probe port.
    #[serde(rename = "probePort", default)]
    pub probe_port: Option<u16>,
    /// HTTP probe path, e.g. "/health". Defaults to "/health" when `probePort` is set.
    #[serde(rename = "probePath", default)]
    pub probe_path: Option<String>,
    /// CPU in millicores, e.g. 2000 means 2000m.
    #[serde(default)]
    pub cpu: Option<u32>,
    /// Memory in MiB, e.g. 2000.
    #[serde(default)]
    pub memory: Option<u32>,
    /// Environment variables as "KEY=VALUE" strings.
    #[serde(default)]
    pub env: Option<Vec<String>>,
    /// Allow internet (public) access.
    #[serde(rename = "allowInternetAccess", default)]
    pub allow_internet_access: Option<bool>,
    /// Network mode, e.g. "tap".
    #[serde(rename = "networkType", default)]
    pub network_type: Option<String>,
    /// Limit template distribution to these node IDs or host IPs.
    #[serde(default)]
    pub nodes: Option<Vec<String>>,
    /// Registry username for private source images.
    #[serde(rename = "registryUsername", default)]
    pub registry_username: Option<String>,
    /// Registry password for private source images.
    #[serde(rename = "registryPassword", default)]
    pub registry_password: Option<String>,
    /// Override container ENTRYPOINT.
    #[serde(default)]
    pub command: Option<Vec<String>>,
    /// Override container CMD args.
    #[serde(default)]
    pub args: Option<Vec<String>>,
    /// Container DNS nameservers.
    #[serde(default)]
    pub dns: Option<Vec<String>>,
    /// Allowed outbound CIDRs for CubeVS egress policy.
    #[serde(rename = "allowOut", default)]
    pub allow_out: Option<Vec<String>>,
    /// Denied outbound CIDRs for CubeVS egress policy.
    #[serde(rename = "denyOut", default)]
    pub deny_out: Option<Vec<String>>,
    /// Enable ivshmem when building the template snapshot.
    #[serde(rename = "enableIvshmem", default)]
    pub enable_ivshmem: Option<bool>,
    /// Whether CubeMaster bakes the CubeEgress root CA into the template rootfs.
    /// Omitted or null defaults to true on CubeMaster.
    #[serde(rename = "with_cube_ca", default)]
    pub with_cube_ca: Option<bool>,
}

/// Body for POST /templates/:id (rebuild).
#[derive(Debug, Deserialize, ToSchema)]
pub struct RebuildTemplateRequest {
    #[serde(flatten)]
    pub extra: serde_json::Map<String, serde_json::Value>,
}

/// Job envelope returned by create / rebuild.
#[derive(Debug, Serialize, ToSchema)]
pub struct TemplateBuildJob {
    #[serde(rename = "jobID")]
    pub job_id: String,
    #[serde(rename = "templateID")]
    pub template_id: String,
    pub status: String,
    pub phase: String,
    pub progress: i32,
    #[serde(rename = "errorMessage", skip_serializing_if = "String::is_empty")]
    pub error_message: String,
}

/// Response for GET /templates/:id/builds/:bid/status
#[derive(Debug, Serialize, ToSchema)]
pub struct TemplateBuildStatus {
    #[serde(rename = "buildID")]
    pub build_id: String,
    #[serde(rename = "templateID")]
    pub template_id: String,
    pub status: String,
    pub progress: i32,
    pub message: String,
}

#[derive(Debug, Serialize, Default, ToSchema)]
pub struct TemplateCompatSummaryView {
    #[serde(rename = "staleTemplates")]
    pub stale_templates: i32,
    #[serde(rename = "staleReplicas")]
    pub stale_replicas: i32,
    #[serde(rename = "affectedNodes")]
    pub affected_nodes: i32,
    #[serde(rename = "missingReplicas")]
    pub missing_replicas: i32,
    #[serde(rename = "unknownReplicas")]
    pub unknown_replicas: i32,
}

#[derive(Debug, Serialize, Default, ToSchema)]
pub struct TemplateNodeCompatView {
    #[serde(rename = "nodeID")]
    pub node_id: String,
    #[serde(rename = "nodeIP", skip_serializing_if = "Option::is_none")]
    pub node_ip: Option<String>,
    #[serde(rename = "compatStatus")]
    pub compat_status: String,
    #[serde(
        rename = "boundGuestImageVersion",
        skip_serializing_if = "Option::is_none"
    )]
    pub bound_guest_image_version: Option<String>,
    #[serde(
        rename = "currentGuestImageVersion",
        skip_serializing_if = "Option::is_none"
    )]
    pub current_guest_image_version: Option<String>,
    #[serde(rename = "boundAgentVersion", skip_serializing_if = "Option::is_none")]
    pub bound_agent_version: Option<String>,
    #[serde(
        rename = "currentAgentVersion",
        skip_serializing_if = "Option::is_none"
    )]
    pub current_agent_version: Option<String>,
    #[serde(rename = "boundKernelVersion", skip_serializing_if = "Option::is_none")]
    pub bound_kernel_version: Option<String>,
    #[serde(
        rename = "currentKernelVersion",
        skip_serializing_if = "Option::is_none"
    )]
    pub current_kernel_version: Option<String>,
}

#[derive(Debug, Serialize, Default, ToSchema)]
pub struct TemplateCompatRowView {
    #[serde(rename = "templateID")]
    pub template_id: String,
    #[serde(rename = "instanceType", skip_serializing_if = "Option::is_none")]
    pub instance_type: Option<String>,
    pub overall: String,
    pub nodes: Vec<TemplateNodeCompatView>,
}

#[derive(Debug, Serialize, Default, ToSchema)]
pub struct TemplateCompatMatrixView {
    pub summary: TemplateCompatSummaryView,
    pub templates: Vec<TemplateCompatRowView>,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct TemplateCompatAdoptResponseView {
    pub updated: i32,
}

// ─── Cluster & Nodes ───────────────────────────────────────────────────────

#[derive(Debug, Serialize, Default, ToSchema)]
pub struct ClusterOverview {
    #[serde(rename = "nodeCount")]
    pub node_count: usize,
    #[serde(rename = "healthyNodes")]
    pub healthy_nodes: usize,
    /// Total CPU across the cluster, expressed in millicpu.
    #[serde(rename = "totalCpuMilli")]
    pub total_cpu_milli: i64,
    /// Currently-allocatable CPU in millicpu.
    #[serde(rename = "allocatableCpuMilli")]
    pub allocatable_cpu_milli: i64,
    /// Total memory in MiB.
    #[serde(rename = "totalMemoryMB")]
    pub total_memory_mb: i64,
    /// Currently-allocatable memory in MiB.
    #[serde(rename = "allocatableMemoryMB")]
    pub allocatable_memory_mb: i64,
    /// Sum of every node's maximum MVM slots.
    #[serde(rename = "maxMvmSlots")]
    pub max_mvm_slots: i64,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct NodeResourcesView {
    /// CPU capacity or availability expressed in millicpu.
    #[serde(rename = "cpuMilli")]
    pub cpu_milli: i64,
    #[serde(rename = "memoryMB")]
    pub memory_mb: i64,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct NodeConditionView {
    #[serde(rename = "type")]
    pub kind: String,
    pub status: String,
    #[serde(rename = "lastHeartbeatTime", skip_serializing_if = "Option::is_none")]
    pub last_heartbeat_time: Option<DateTime<Utc>>,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub reason: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub message: String,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct NodeView {
    #[serde(rename = "nodeID")]
    pub node_id: String,
    #[serde(rename = "hostIP")]
    pub host_ip: String,
    #[serde(rename = "instanceType", skip_serializing_if = "String::is_empty")]
    pub instance_type: String,
    pub healthy: bool,
    pub capacity: NodeResourcesView,
    pub allocatable: NodeResourcesView,
    /// Percentage (0-100) of CPU currently in use.
    #[serde(rename = "cpuSaturation")]
    pub cpu_saturation: f32,
    /// Percentage (0-100) of memory currently in use.
    #[serde(rename = "memorySaturation")]
    pub memory_saturation: f32,
    #[serde(rename = "maxMvmSlots")]
    pub max_mvm_slots: i64,
    /// CPU quota in millicores assigned to this node.
    #[serde(rename = "quotaCpu")]
    pub quota_cpu: i64,
    /// Memory quota in MiB assigned to this node.
    #[serde(rename = "quotaMemMB")]
    pub quota_mem_mb: i64,
    /// Max concurrent sandbox-create operations on this node.
    #[serde(rename = "createConcurrentNum")]
    pub create_concurrent_num: i64,
    #[serde(rename = "heartbeatTime", skip_serializing_if = "Option::is_none")]
    pub heartbeat_time: Option<DateTime<Utc>>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub conditions: Vec<NodeConditionView>,
    #[serde(rename = "localTemplates", skip_serializing_if = "Vec::is_empty")]
    pub local_templates: Vec<String>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub versions: Vec<ComponentVersionView>,
}

/// One component's version on a node.
#[derive(Debug, Serialize, ToSchema, Clone, Default)]
pub struct ComponentVersionView {
    pub component: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub version: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub commit: String,
    #[serde(rename = "buildTime", skip_serializing_if = "String::is_empty")]
    pub build_time: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub source: String,
}

/// Control-plane reference version (the cluster's target version).
#[derive(Debug, Serialize, ToSchema, Clone, Default)]
pub struct ControlPlaneVersionView {
    pub version: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub commit: String,
    #[serde(rename = "buildTime", skip_serializing_if = "String::is_empty")]
    pub build_time: String,
}

/// A group of nodes that report the same version of a component.
#[derive(Debug, Serialize, ToSchema, Clone, Default)]
pub struct ComponentVersionGroupView {
    pub version: String,
    pub nodes: Vec<String>,
}

/// Per-component aggregation across all nodes.
#[derive(Debug, Serialize, ToSchema, Clone, Default)]
pub struct ComponentMatrixRowView {
    pub component: String,
    #[serde(rename = "declaredVersion", skip_serializing_if = "String::is_empty")]
    pub declared_version: String,
    #[serde(rename = "declaredVersions", skip_serializing_if = "Vec::is_empty")]
    pub declared_versions: Vec<String>,
    pub consistent: bool,
    pub versions: Vec<ComponentVersionGroupView>,
}

/// A single component version on a single node, with release declaration membership.
#[derive(Debug, Serialize, ToSchema, Clone, Default)]
pub struct NodeComponentEntryView {
    pub component: String,
    pub version: String,
    pub declared: bool,
}

/// Per-node view of the version matrix.
#[derive(Debug, Serialize, ToSchema, Clone, Default)]
pub struct NodeVersionRowView {
    #[serde(rename = "nodeID")]
    pub node_id: String,
    pub healthy: bool,
    pub components: Vec<NodeComponentEntryView>,
}

/// Full node x component version matrix.
#[derive(Debug, Serialize, ToSchema, Clone, Default)]
pub struct VersionMatrixView {
    #[serde(rename = "controlPlane")]
    pub control_plane: ControlPlaneVersionView,
    pub components: Vec<ComponentMatrixRowView>,
    pub nodes: Vec<NodeVersionRowView>,
}
