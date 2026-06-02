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
    #[serde(rename = "allowOut", skip_serializing_if = "Option::is_none")]
    pub allow_out: Option<Vec<String>>,
    #[serde(rename = "denyOut", skip_serializing_if = "Option::is_none")]
    pub deny_out: Option<Vec<String>>,
    #[serde(rename = "maskRequestHost", skip_serializing_if = "Option::is_none")]
    pub mask_request_host: Option<String>,
}

/// Auto-resume configuration for paused sandboxes.
#[derive(Debug, Clone, Serialize, Deserialize, ToSchema)]
pub struct SandboxAutoResumeConfig {
    pub enabled: bool,
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
/// Rule: ID abbreviations → uppercase (templateID, sandboxID, envVars, autoPause);
///       allow_internet_access is a known SDK snake_case quirk.
#[derive(Debug, Deserialize, Validate, ToSchema)]
#[allow(dead_code)]
pub struct NewSandbox {
    #[serde(rename = "templateID")]
    pub template_id: String,

    #[validate(range(min = 0))]
    #[serde(default = "default_timeout")]
    pub timeout: i32,

    #[serde(rename = "autoPause", default)]
    pub auto_pause: bool,

    #[serde(rename = "autoResume", skip_serializing_if = "Option::is_none")]
    pub auto_resume: Option<SandboxAutoResumeConfig>,

    #[serde(skip_serializing_if = "Option::is_none")]
    pub secure: Option<bool>,

    /// SDK sends this as snake_case (known quirk).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub allow_internet_access: Option<bool>,

    #[serde(skip_serializing_if = "Option::is_none")]
    pub network: Option<SandboxNetworkConfig>,

    #[serde(skip_serializing_if = "Option::is_none")]
    pub metadata: Option<SandboxMetadata>,

    #[serde(rename = "envVars", skip_serializing_if = "Option::is_none")]
    pub env_vars: Option<EnvVars>,

    #[serde(skip_serializing_if = "Option::is_none")]
    pub mcp: Option<serde_json::Value>,

    #[serde(rename = "volumeMounts", skip_serializing_if = "Option::is_none")]
    pub volume_mounts: Option<Vec<SandboxVolumeMount>>,
}

fn default_timeout() -> i32 {
    15
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
    #[serde(rename = "endAt")]
    pub end_at: DateTime<Utc>,
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
    #[serde(rename = "endAt")]
    pub end_at: DateTime<Utc>,
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
    #[serde(default = "default_timeout")]
    pub timeout: i32,
    #[serde(rename = "autoPause", default)]
    pub auto_pause: bool,
}

/// Request body for POST /sandboxes/{id}/connect.
#[derive(Debug, Deserialize, Validate, ToSchema)]
pub struct ConnectSandbox {
    #[validate(range(min = 0))]
    pub timeout: i32,
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
    #[validate(range(min = 0))]
    pub timeout: i32,
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
}

/// Body for POST /templates (create from image).
///
/// Two mutually exclusive modes are supported on the same endpoint, matching
/// both the **CubeSandbox-native** and **E2B-standard** template build flows:
///
/// 1. CubeSandbox-native (`image` is provided): CubeMaster will pull
///    `image` from an external OCI registry and build the rootfs directly.
///    All extra fields (`exposed_ports`, `cpu`, `memory`, ...) override the
///    image defaults.
///
/// 2. E2B-standard (`dockerfile` is provided): the server allocates a
///    `templateID` + `buildID`, returns a short-lived push credential, and the
///    client (`e2b template build`) pushes the locally-built image to the
///    bundled OCI registry. The actual rootfs build is then triggered by
///    `POST /templates/{tid}/builds/{bid}`.
///
/// Field naming follows the E2B SDK conventions where they collide with
/// CubeSandbox legacy fields (camelCase for IDs, snake_case for
/// `start_cmd`/`ready_cmd`).
#[derive(Debug, Deserialize, Validate, Clone, ToSchema)]
#[allow(dead_code)]
pub struct CreateTemplateRequest {
    // ── Common fields (both modes) ─────────────────────────────────────────
    /// Deprecated and ignored. Template IDs are always generated server-side
    /// with the `tpl-` prefix; clients must use the returned `templateID`.
    #[serde(rename = "templateID", default)]
    #[allow(dead_code)]
    pub template_id: String,
    #[serde(rename = "instanceType", default)]
    pub instance_type: Option<String>,

    /// Optional human-readable alias (E2B field: `alias`).
    #[serde(default)]
    pub alias: Option<String>,

    /// E2B `teamID`. Currently only logged; reserved for multi-tenant rollout.
    #[serde(rename = "teamID", default)]
    pub team_id: Option<String>,

    /// Container image reference (CubeSandbox-native mode), e.g.
    /// `registry.example.com/code:latest`. Mutually exclusive with `dockerfile`.
    #[serde(default)]
    pub image: Option<String>,

    /// Inline Dockerfile content (E2B-standard mode). Currently NOT built
    /// server-side — the client is expected to build & push the image locally
    /// using the credentials returned by this endpoint. Stored verbatim for
    /// future in-cluster builds.
    #[serde(default)]
    pub dockerfile: Option<String>,

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

    /// CPU in millicores (legacy CubeSandbox field).
    #[serde(default)]
    pub cpu: Option<u32>,
    /// Memory in MiB (legacy CubeSandbox field).
    #[serde(default)]
    pub memory: Option<u32>,

    /// E2B-style integer CPU count (cores). Mapped to `cpu * 1000` millicores
    /// when `cpu` is not supplied.
    #[serde(rename = "cpuCount", default)]
    pub cpu_count: Option<u32>,

    /// E2B-style memory in MiB. Mapped to `memory` when the legacy field is
    /// not supplied.
    #[serde(rename = "memoryMB", default)]
    pub memory_mb: Option<u32>,

    /// Environment variables as "KEY=VALUE" strings (legacy CubeSandbox).
    #[serde(default)]
    pub env: Option<Vec<String>>,

    /// E2B-style env-vars map. Merged into `env` when present.
    #[serde(rename = "envVars", default)]
    pub env_vars: Option<HashMap<String, String>>,

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

    /// E2B-style `startCmd`: shell command to execute inside the container
    /// once the rootfs is mounted. Mapped to CubeMaster `args`.
    #[serde(rename = "startCmd", alias = "start_cmd", default)]
    pub start_cmd: Option<String>,

    /// E2B-style `readyCmd`: shell command used as readiness probe.
    /// Translated into a CubeMaster `Probe.Exec` when `probe_port` is empty.
    #[serde(rename = "readyCmd", alias = "ready_cmd", default)]
    pub ready_cmd: Option<String>,
}

/// Body for POST /templates/:id (rebuild).
#[derive(Debug, Deserialize, ToSchema)]
pub struct RebuildTemplateRequest {
    #[serde(flatten)]
    pub extra: serde_json::Map<String, serde_json::Value>,
}

/// Job envelope returned by create / rebuild.
///
/// E2B's CLI expects (besides the bare job state):
///   - `buildID`     — opaque token that subsequent `/builds/{buildID}/...`
///                     calls use to refer to *this* attempt.
///   - `uploadUrl`   — URL the CLI should `docker push` to.
///   - `registry`    — credentials matched against `Authorization` on /v2/*.
///
/// All of these are emitted as *Optional* so existing CubeSandbox clients,
/// which only look at `templateID`/`status`, continue to deserialize.
#[derive(Debug, Serialize, ToSchema, Default)]
pub struct TemplateBuildJob {
    #[serde(rename = "jobID")]
    pub job_id: String,
    #[serde(rename = "templateID")]
    pub template_id: String,
    /// E2B-required identifier of this build attempt. Equals `jobID` when
    /// CubeMaster returns one; otherwise a server-side uuid.
    #[serde(rename = "buildID")]
    pub build_id: String,
    pub status: String,
    pub phase: String,
    pub progress: i32,
    #[serde(rename = "errorMessage", skip_serializing_if = "String::is_empty")]
    pub error_message: String,

    /// E2B-style `uploadUrl`: where the CLI should push the locally-built
    /// dockerfile image. Same as `registry.url` for convenience.
    #[serde(rename = "uploadUrl", skip_serializing_if = "Option::is_none")]
    pub upload_url: Option<String>,

    /// Registry credentials advertised to E2B clients.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub registry: Option<RegistryCredential>,
}

/// Short-lived push credential returned alongside a new template build.
#[derive(Debug, Serialize, Clone, ToSchema)]
pub struct RegistryCredential {
    /// Full base URL of the registry endpoint, e.g. `https://cube.example.com`.
    pub url: String,
    /// Repository the client should push to, e.g. `e2b/tpl-abc:bld-001`.
    pub repository: String,
    /// Username for `docker login` / Basic auth.
    pub username: String,
    /// Password for `docker login` / Basic auth.
    pub password: String,
}

/// Response for GET /templates/:id/builds/:bid/status
///
/// E2B's CLI polls this endpoint with `?logsOffset=N` and expects:
///   - `status`        : "building" | "ready" | "error" | "uploading" | ...
///   - `logs: string[]`: the new lines added since the previous offset.
#[derive(Debug, Serialize, ToSchema, Default)]
pub struct TemplateBuildStatus {
    #[serde(rename = "buildID")]
    pub build_id: String,
    #[serde(rename = "templateID")]
    pub template_id: String,
    pub status: String,
    pub progress: i32,
    pub message: String,
    /// Incremental log lines starting from the offset given in the query.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub logs: Vec<String>,
    /// Offset to send back next round to receive only newer lines.
    #[serde(rename = "logsOffset", skip_serializing_if = "Option::is_none")]
    pub logs_offset: Option<i32>,
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

// ─── E2B V3 protocol (real e2b SDK contract) ──────────────────────────────
//
// The e2b Python/JS SDK calls this trio of endpoints (camelCase JSON):
//
//   1. POST /v3/templates                      → register, returns
//                                                {templateID, buildID, ...}
//   2. GET  /templates/{tid}/files/{hash}      → resolve cache, returns
//                                                {present, url?}
//   3. POST /v2/templates/{tid}/builds/{bid}   → trigger build, body has
//                                                fromImage / startCmd /
//                                                readyCmd / steps / ...
//   4. GET  /templates/{tid}/builds/{bid}/status?logsOffset=N&limit=M
//                                              → poll, returns
//                                                {buildID, templateID,
//                                                 status, logs[], logEntries[]}
#[derive(Debug, Deserialize, Default, ToSchema)]
#[allow(dead_code)]
pub struct V3TemplateBuildRequest {
    /// New-style "name" or "name:tag". The SDK *prefers* this over `alias`.
    #[serde(default)]
    pub name: Option<String>,
    /// Deprecated. Some older SDKs still send this.
    #[serde(default)]
    pub alias: Option<String>,
    /// Tag list to attach to the resulting build.
    #[serde(default)]
    pub tags: Option<Vec<String>>,
    /// CPU cores (whole number).
    #[serde(rename = "cpuCount", default)]
    pub cpu_count: Option<u32>,
    /// Memory in MiB.
    #[serde(rename = "memoryMB", default)]
    pub memory_mb: Option<u32>,
    /// Team identifier — currently only logged.
    #[serde(rename = "teamID", default)]
    pub team_id: Option<String>,
}

/// Response for `POST /v3/templates` — must match `TemplateRequestResponseV3`
/// exactly: the SDK calls `from_dict` and **fails fast on missing keys**.
#[derive(Debug, Serialize, ToSchema)]
pub struct V3TemplateBuildResponse {
    #[serde(rename = "templateID")]
    pub template_id: String,
    #[serde(rename = "buildID")]
    pub build_id: String,
    pub names: Vec<String>,
    pub aliases: Vec<String>,
    pub tags: Vec<String>,
    pub public: bool,
}

/// Response for `GET /templates/{tid}/files/{hash}` — the SDK only checks
/// `present`/`url` and (when `present=false`) PUTs the tarball to `url`.
#[derive(Debug, Serialize, ToSchema)]
pub struct V3TemplateFileUpload {
    pub present: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub url: Option<String>,
}

/// Body for `POST /v2/templates/{tid}/builds/{bid}` — the moment the build is
/// actually dispatched to CubeMaster.
#[derive(Debug, Deserialize, Default, ToSchema)]
#[allow(dead_code)]
pub struct V2TemplateBuildStart {
    /// Skip-cache flag.
    #[serde(default)]
    pub force: Option<bool>,
    /// External base image (CubeMaster `SourceImageRef`).
    #[serde(rename = "fromImage", default)]
    pub from_image: Option<String>,
    /// Optional registry credential block (AWS/GCP/General). Stored verbatim
    /// for now; CubeMaster doesn't yet consume it.
    #[serde(rename = "fromImageRegistry", default)]
    pub from_image_registry: Option<serde_json::Value>,
    /// Reuse another already-built CubeSandbox template as the base.
    #[serde(rename = "fromTemplate", default)]
    pub from_template: Option<String>,
    /// E2B `readyCmd` — translated into CubeMaster `Probe.Exec`.
    #[serde(rename = "readyCmd", default)]
    pub ready_cmd: Option<String>,
    /// E2B `startCmd` — translated into container `args`.
    #[serde(rename = "startCmd", default)]
    pub start_cmd: Option<String>,
    /// Multi-step build instructions (RUN/COPY/ENV/...). Currently only used
    /// for hashing & log breadcrumbs; full Dockerfile-equivalent semantics
    /// require the in-cluster builder (Phase 4).
    #[serde(default)]
    pub steps: Option<Vec<serde_json::Value>>,
}

/// Response for `GET /templates/{tid}/builds/{bid}/status` — must round-trip
/// to E2B's `TemplateBuildInfo` to satisfy the SDK's strict `from_dict`.
#[derive(Debug, Serialize, ToSchema, Default)]
pub struct V3TemplateBuildInfo {
    #[serde(rename = "buildID")]
    pub build_id: String,
    #[serde(rename = "templateID")]
    pub template_id: String,
    /// One of: "waiting" | "building" | "ready" | "error".
    pub status: String,
    /// Plain log lines (already filtered by `logsOffset`).
    pub logs: Vec<String>,
    /// Structured log entries — same content with timestamps + level.
    #[serde(rename = "logEntries")]
    pub log_entries: Vec<V3BuildLogEntry>,
    /// Failure reason payload (only when `status == "error"`).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub reason: Option<V3BuildStatusReason>,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct V3BuildLogEntry {
    pub timestamp: DateTime<Utc>,
    pub message: String,
    /// "debug" | "info" | "warn" | "error"
    pub level: String,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct V3BuildStatusReason {
    #[serde(rename = "stepIndex", skip_serializing_if = "Option::is_none")]
    pub step_index: Option<i32>,
    pub message: String,
}

/// Query string for `GET /v3` status endpoint.
#[derive(Debug, Deserialize, Default, IntoParams)]
#[into_params(parameter_in = Query)]
#[allow(dead_code)]
pub struct V3BuildStatusQuery {
    #[serde(rename = "logsOffset", alias = "logs_offset", default)]
    pub logs_offset: i32,
    #[serde(default = "default_v3_log_limit")]
    pub limit: i32,
    #[serde(default)]
    pub level: Option<String>,
}

fn default_v3_log_limit() -> i32 {
    100
}

