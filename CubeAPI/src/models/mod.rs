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
}

/// Body for POST /templates (create from image).
#[derive(Debug, Deserialize, Validate, ToSchema)]
pub struct CreateTemplateRequest {
    #[serde(rename = "templateID")]
    #[validate(length(min = 1))]
    pub template_id: String,
    #[serde(rename = "instanceType", default)]
    pub instance_type: Option<String>,
    /// Container image reference, e.g. `registry.example.com/code:latest`.
    #[validate(length(min = 1))]
    pub image: String,
    /// Extra fields forwarded verbatim to CubeMaster.
    #[serde(flatten)]
    pub extra: serde_json::Map<String, serde_json::Value>,
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

// ─── Cluster & Nodes ───────────────────────────────────────────────────────

#[derive(Debug, Serialize, Default, ToSchema)]
pub struct ClusterOverview {
    #[serde(rename = "nodeCount")]
    pub node_count: usize,
    #[serde(rename = "healthyNodes")]
    pub healthy_nodes: usize,
    /// Total CPU across the cluster, expressed in CPU cores.
    #[serde(rename = "totalCpuCores")]
    pub total_cpu_cores: i64,
    /// Currently-allocatable CPU cores.
    #[serde(rename = "allocatableCpuCores")]
    pub allocatable_cpu_cores: i64,
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
    #[serde(rename = "cpuCores")]
    pub cpu_cores: i64,
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
    #[serde(rename = "heartbeatTime", skip_serializing_if = "Option::is_none")]
    pub heartbeat_time: Option<DateTime<Utc>>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub conditions: Vec<NodeConditionView>,
    #[serde(rename = "localTemplates", skip_serializing_if = "Vec::is_empty")]
    pub local_templates: Vec<String>,
}
