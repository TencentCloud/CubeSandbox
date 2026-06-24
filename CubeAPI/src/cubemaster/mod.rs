// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

/// cubemaster — thin HTTP client wrapping CubeMaster REST API.
///
/// Existing APIs (✅ implemented on CubeMaster):
///   - POST   /cube/sandbox          create sandbox
///   - DELETE /cube/sandbox          delete sandbox
///   - POST   /cube/sandbox/list     list sandboxes
///
/// Implemented on CubeMaster (see pkg/service/sandbox/types):
///   - GET    /cube/sandbox/info       get single sandbox detail (query: sandbox_id, instance_type)
///   - POST   /cube/sandbox/update     update sandbox (action: "pause" | "resume")
/// Implemented on CubeMaster (snapshot APIs):
///   - POST   /cube/snapshot                          create runtime snapshot (synchronous terminal result)
///   - GET    /cube/snapshot                          list snapshots (paginated)
///   - DELETE /cube/snapshot/{snapshot_id}            delete snapshot (synchronous terminal result)
///   - POST   /cube/sandbox/{sandbox_id}/rollback     rollback sandbox to snapshot (synchronous terminal result)
///   - GET    /cube/operation/{operation_id}          query operation/audit record (not required for snapshot completion)
///
/// New APIs required (❌ not yet on CubeMaster — pending implementation):
///   - POST   /cube/sandbox/timeout    set absolute TTL
///   - POST   /cube/sandbox/refresh    extend TTL by delta
///   - POST   /cube/sandbox/logs       fetch sandbox logs
///   - POST   /cube/sandbox/commit     commit sandbox → template image
///   - GET    /cube/template/build/{id}/status  build status poll
///   - DELETE /cube/template           delete template
///   - POST   /cube/template/list      list templates
///   - GET    /cube/template/{id}      get single template
use chrono::{DateTime, Utc};
use serde::de::Deserializer;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;

const TEMPLATE_ID_LABEL_KEY: &str = "cube.master.appsnapshot.template.id";

// ─── Client ────────────────────────────────────────────────────────────────

/// Lightweight wrapper around a shared `reqwest::Client`.
/// Clone is O(1) — the inner client holds an `Arc` to the connection pool.
#[derive(Clone)]
pub struct CubeMasterClient {
    inner: reqwest::Client,
    base_url: String,
}

impl CubeMasterClient {
    /// Create a client pointing at `base_url` (e.g. `"http://10.0.0.1:8080"`).
    pub fn new(base_url: impl Into<String>, http_client: reqwest::Client) -> Self {
        Self {
            inner: http_client,
            base_url: base_url.into().trim_end_matches('/').to_string(),
        }
    }

    // ── Sandbox: existing APIs ─────────────────────────────────────────────

    /// POST /cube/sandbox — create a new sandbox.
    pub async fn create_sandbox(
        &self,
        req: &CreateSandboxRequest,
    ) -> Result<CreateSandboxResponse, CubeMasterError> {
        let url = format!("{}/cube/sandbox", self.base_url);
        let resp = self
            .inner
            .post(&url)
            .json(req)
            .send()
            .await
            .map_err(CubeMasterError::Http)?;
        parse_response(resp).await
    }

    /// DELETE /cube/sandbox — destroy a sandbox.
    pub async fn delete_sandbox(
        &self,
        req: &DeleteSandboxRequest,
    ) -> Result<DeleteSandboxResponse, CubeMasterError> {
        let url = format!("{}/cube/sandbox", self.base_url);
        let resp = self
            .inner
            .delete(&url)
            .json(req)
            .send()
            .await
            .map_err(CubeMasterError::Http)?;
        parse_response(resp).await
    }

    /// POST /cube/sandbox/list — list sandboxes (paginated by host).
    pub async fn list_sandboxes(
        &self,
        req: &ListSandboxRequest,
    ) -> Result<ListSandboxResponse, CubeMasterError> {
        let url = format!("{}/cube/sandbox/list", self.base_url);
        let resp = self
            .inner
            .post(&url)
            .json(req)
            .send()
            .await
            .map_err(CubeMasterError::Http)?;
        parse_response(resp).await
    }

    // ── Sandbox: new APIs (require CubeMaster implementation) ─────────────
    // These APIs are not yet available on CubeMaster; structs are pre-defined for future integration.

    /// GET /cube/sandbox/info — fetch a single sandbox's real-time status.
    /// Query: sandbox_id, instance_type. See CubeMaster pkg/service/sandbox/types.
    pub async fn get_sandbox(
        &self,
        sandbox_id: &str,
        instance_type: &str,
    ) -> Result<GetSandboxResponse, CubeMasterError> {
        let url = format!("{}/cube/sandbox/info", self.base_url);
        let resp = self
            .inner
            .get(&url)
            .query(&[("sandbox_id", sandbox_id), ("instance_type", instance_type)])
            .send()
            .await
            .map_err(CubeMasterError::Http)?;
        parse_response(resp).await
    }

    /// POST /cube/sandbox/update — pause or resume a sandbox (action: "pause" | "resume").
    pub async fn update_sandbox(
        &self,
        req: &SandboxUpdateRequest,
    ) -> Result<SandboxUpdateResponse, CubeMasterError> {
        let url = format!("{}/cube/sandbox/update", self.base_url);
        let resp = self
            .inner
            .post(&url)
            .json(req)
            .send()
            .await
            .map_err(CubeMasterError::Http)?;
        parse_response(resp).await
    }

    /// POST /cube/sandbox/timeout — set absolute TTL for a sandbox.
    /// ❌ New API required on CubeMaster.
    pub async fn set_sandbox_timeout(
        &self,
        req: &SandboxTimeoutRequest,
    ) -> Result<SandboxTimeoutResponse, CubeMasterError> {
        let url = format!("{}/cube/sandbox/timeout", self.base_url);
        let resp = self
            .inner
            .post(&url)
            .json(req)
            .send()
            .await
            .map_err(CubeMasterError::Http)?;
        parse_response(resp).await
    }

    /// POST /cube/sandbox/refresh — extend TTL by a delta (seconds).
    /// ❌ New API required on CubeMaster.
    pub async fn refresh_sandbox(
        &self,
        req: &SandboxRefreshRequest,
    ) -> Result<SandboxRefreshResponse, CubeMasterError> {
        let url = format!("{}/cube/sandbox/refresh", self.base_url);
        let resp = self
            .inner
            .post(&url)
            .json(req)
            .send()
            .await
            .map_err(CubeMasterError::Http)?;
        parse_response(resp).await
    }

    /// POST /cube/sandbox/logs — fetch sandbox stdout/stderr logs.
    /// ❌ New API required on CubeMaster.
    pub async fn get_sandbox_logs(
        &self,
        req: &SandboxLogsRequest,
    ) -> Result<SandboxLogsResponse, CubeMasterError> {
        let url = format!("{}/cube/sandbox/logs", self.base_url);
        let resp = self
            .inner
            .post(&url)
            .json(req)
            .send()
            .await
            .map_err(CubeMasterError::Http)?;
        parse_response(resp).await
    }

    // ── Snapshot APIs ───────────────────────────────────────────────────────

    /// POST /cube/snapshot — create a runtime snapshot.
    pub async fn create_snapshot(
        &self,
        req: &CreateSnapshotRequest,
    ) -> Result<CreateSnapshotResponse, CubeMasterError> {
        let url = format!("{}/cube/snapshot", self.base_url);
        let resp = self
            .inner
            .post(&url)
            .json(req)
            .send()
            .await
            .map_err(CubeMasterError::Http)?;
        parse_response(resp).await
    }

    /// GET /cube/snapshot — list snapshots (paginated).
    pub async fn list_snapshots(
        &self,
        req: &ListSnapshotsRequest,
    ) -> Result<ListSnapshotsResponse, CubeMasterError> {
        let url = format!("{}/cube/snapshot", self.base_url);
        let mut builder = self
            .inner
            .get(&url)
            .query(&[("request_id", &req.request_id)])
            .query(&[("instance_type", &req.instance_type)]);
        if let Some(sid) = &req.sandbox_id {
            builder = builder.query(&[("sandbox_id", sid)]);
        }
        if let Some(name) = &req.name {
            builder = builder.query(&[("name", name)]);
        }
        if let Some(status) = &req.status {
            builder = builder.query(&[("status", status)]);
        }
        if let Some(limit) = req.limit {
            builder = builder.query(&[("limit", limit.to_string())]);
        }
        // Only forward `next_token` when it carries a real cursor.  An empty
        // string is treated by some CubeMaster builds as "start over from
        // page 0" instead of "omit the parameter", which silently breaks
        // pagination (Bug 3).  Defensively normalise it here.
        if let Some(token) = req
            .next_token
            .as_deref()
            .map(str::trim)
            .filter(|t| !t.is_empty())
        {
            builder = builder.query(&[("next_token", token)]);
        }
        let resp = builder.send().await.map_err(CubeMasterError::Http)?;
        parse_response(resp).await
    }

    /// GET /cube/snapshot/{snapshot_id} — fetch a single snapshot.
    pub async fn get_snapshot(
        &self,
        snapshot_id: &str,
        include_request: bool,
    ) -> Result<SnapshotDetailResponse, CubeMasterError> {
        validate_path_segment("snapshot_id", snapshot_id)?;
        let url = format!("{}/cube/snapshot/{}", self.base_url, snapshot_id);
        let mut builder = self.inner.get(&url);
        if include_request {
            builder = builder.query(&[("include_request", "true")]);
        }
        let resp = builder.send().await.map_err(CubeMasterError::Http)?;
        parse_response(resp).await
    }

    /// DELETE /cube/snapshot/{snapshot_id} — delete a snapshot.
    pub async fn delete_snapshot(
        &self,
        snapshot_id: &str,
        req: &DeleteSnapshotRequest,
    ) -> Result<DeleteSnapshotResponse, CubeMasterError> {
        validate_path_segment("snapshot_id", snapshot_id)?;
        let url = format!("{}/cube/snapshot/{}", self.base_url, snapshot_id);
        let resp = self
            .inner
            .delete(&url)
            .json(req)
            .send()
            .await
            .map_err(CubeMasterError::Http)?;
        parse_response(resp).await
    }

    /// POST /cube/sandbox/{sandbox_id}/rollback — rollback sandbox to a snapshot.
    pub async fn rollback_sandbox(
        &self,
        sandbox_id: &str,
        req: &RollbackRequest,
    ) -> Result<RollbackResponse, CubeMasterError> {
        validate_path_segment("sandbox_id", sandbox_id)?;
        let url = format!("{}/cube/sandbox/{}/rollback", self.base_url, sandbox_id);
        let resp = self
            .inner
            .post(&url)
            .json(req)
            .send()
            .await
            .map_err(CubeMasterError::Http)?;
        parse_response(resp).await
    }

    // NOTE: there used to be a `CubeMasterClient::get_operation()` wrapper
    // around `GET /cube/operation/{id}`.  It was removed in the synchronous
    // delete contract cleanup: snapshot create/rollback/delete now return
    // the terminal operation in their primary response, so CubeAPI no longer
    // needs to poll.  CubeMaster still serves `/cube/operation/{id}` for
    // human audit / out-of-band diagnostics — the snapshot API is
    // synchronous (CubeAPI waits for a terminal state and does not
    // expose a polling interface to callers).  Add a new wrapper
    // back here only if a programmatic CubeAPI consumer of audit data
    // emerges.

    // ── Template APIs ─────────────────────────────────────────────────────
    // Maps to CubeMaster `/cube/template` (see
    // CubeMaster/pkg/service/httpservice/cube/template.go).

    /// GET /cube/template — list templates, or fetch a single one when
    /// `template_id` is supplied. Pass `include_request=true` to also receive
    /// the original create payload.
    pub async fn list_templates(
        &self,
        template_id: Option<&str>,
        include_request: bool,
    ) -> Result<TemplateListResponse, CubeMasterError> {
        let url = format!("{}/cube/template", self.base_url);
        let mut req = self.inner.get(&url);
        if let Some(tid) = template_id {
            req = req.query(&[("template_id", tid)]);
        }
        if include_request {
            req = req.query(&[("include_request", "true")]);
        }
        let resp = req.send().await.map_err(CubeMasterError::Http)?;
        parse_response(resp).await
    }

    /// GET /cube/template?template_id=…&include_request=true — detail.
    pub async fn get_template(
        &self,
        template_id: &str,
    ) -> Result<TemplateResponse, CubeMasterError> {
        let url = format!("{}/cube/template", self.base_url);
        let resp = self
            .inner
            .get(&url)
            .query(&[("template_id", template_id), ("include_request", "true")])
            .send()
            .await
            .map_err(CubeMasterError::Http)?;
        parse_response(resp).await
    }

    /// DELETE /cube/template — delete by id.
    pub async fn delete_template(
        &self,
        req: &TemplateDeleteRequest,
    ) -> Result<RetEnvelope, CubeMasterError> {
        let url = format!("{}/cube/template", self.base_url);
        let resp = self
            .inner
            .delete(&url)
            .json(req)
            .send()
            .await
            .map_err(CubeMasterError::Http)?;
        parse_response(resp).await
    }

    /// POST /cube/template/from-image — create a template from an image.
    pub async fn create_template_from_image(
        &self,
        req: &CreateTemplateFromImageReq,
    ) -> Result<TemplateJobResponse, CubeMasterError> {
        let url = format!("{}/cube/template/from-image", self.base_url);
        let resp = self
            .inner
            .post(&url)
            .json(req)
            .send()
            .await
            .map_err(CubeMasterError::Http)?;
        parse_response(resp).await
    }

    /// GET /cube/template/from-image?job_id=… — poll a create-from-image job.
    pub async fn get_template_from_image_job(
        &self,
        job_id: &str,
    ) -> Result<TemplateJobResponse, CubeMasterError> {
        let url = format!("{}/cube/template/from-image", self.base_url);
        let resp = self
            .inner
            .get(&url)
            .query(&[("job_id", job_id)])
            .send()
            .await
            .map_err(CubeMasterError::Http)?;
        parse_response(resp).await
    }

    /// POST /cube/template/redo — rebuild an existing template.
    pub async fn redo_template(
        &self,
        req: &RedoTemplateReq,
    ) -> Result<TemplateJobResponse, CubeMasterError> {
        let url = format!("{}/cube/template/redo", self.base_url);
        let resp = self
            .inner
            .post(&url)
            .json(req)
            .send()
            .await
            .map_err(CubeMasterError::Http)?;
        parse_response(resp).await
    }

    /// GET /cube/template/build/{build_id}/status — build status.
    pub async fn get_template_build_status(
        &self,
        build_id: &str,
    ) -> Result<TemplateBuildStatusResponse, CubeMasterError> {
        validate_path_segment("build_id", build_id)?;
        let url = format!("{}/cube/template/build/{}/status", self.base_url, build_id);
        let resp = self
            .inner
            .get(&url)
            .send()
            .await
            .map_err(CubeMasterError::Http)?;
        parse_response(resp).await
    }

    /// GET /cube/template/compat — template compatibility matrix.
    pub async fn get_template_compat(&self) -> Result<TemplateCompatResponse, CubeMasterError> {
        let url = format!("{}/cube/template/compat", self.base_url);
        let resp = self
            .inner
            .get(&url)
            .send()
            .await
            .map_err(CubeMasterError::Http)?;
        parse_response(resp).await
    }

    /// POST /cube/template/compat action=adopt_baseline.
    pub async fn adopt_template_compat_baseline(
        &self,
        req: &TemplateCompatAdoptRequest,
    ) -> Result<TemplateCompatAdoptResponse, CubeMasterError> {
        let url = format!("{}/cube/template/compat", self.base_url);
        let resp = self
            .inner
            .post(&url)
            .json(req)
            .send()
            .await
            .map_err(CubeMasterError::Http)?;
        parse_response(resp).await
    }

    // ── Node / Cluster APIs ──────────────────────────────────────────────

    /// GET /internal/meta/nodes — list all nodes (capacity + health).
    pub async fn list_nodes(&self) -> Result<NodesResponse, CubeMasterError> {
        let url = format!("{}/internal/meta/nodes", self.base_url);
        let resp = self
            .inner
            .get(&url)
            .send()
            .await
            .map_err(CubeMasterError::Http)?;
        parse_response(resp).await
    }

    /// GET /internal/meta/nodes/{id} — single node detail.
    pub async fn get_node(&self, node_id: &str) -> Result<NodeResponse, CubeMasterError> {
        validate_node_id(node_id)?;
        let url = format!("{}/internal/meta/nodes/{}", self.base_url, node_id);
        let resp = self
            .inner
            .get(&url)
            .send()
            .await
            .map_err(CubeMasterError::Http)?;
        parse_response(resp).await
    }

    /// POST /internal/meta/nodes/{id}/isolation — apply or clear an
    /// administrative cordon. `operator` is the authenticated principal and is
    /// forwarded via the X-Operator header so CubeMaster records a trustworthy
    /// audit identity rather than trusting a client-supplied field.
    pub async fn set_node_isolation(
        &self,
        node_id: &str,
        operator: &str,
        req: &SetNodeIsolationRequest,
    ) -> Result<NodeResponse, CubeMasterError> {
        validate_node_id(node_id)?;
        let url = format!(
            "{}/internal/meta/nodes/{}/isolation",
            self.base_url, node_id
        );
        let resp = self
            .inner
            .post(&url)
            .header("X-Operator", operator)
            .json(req)
            .send()
            .await
            .map_err(CubeMasterError::Http)?;
        parse_response(resp).await
    }

    /// GET /internal/meta/version-matrix — cluster-wide component version matrix.
    pub async fn get_version_matrix(&self) -> Result<VersionMatrixResponse, CubeMasterError> {
        let url = format!("{}/internal/meta/version-matrix", self.base_url);
        let resp = self
            .inner
            .get(&url)
            .send()
            .await
            .map_err(CubeMasterError::Http)?;
        parse_response(resp).await
    }
}

// ─── Error ─────────────────────────────────────────────────────────────────

#[derive(Debug, thiserror::Error)]
pub enum CubeMasterError {
    #[error("HTTP transport error: {0}")]
    Http(#[from] reqwest::Error),

    #[error("CubeMaster returned error code {ret_code}: {ret_msg}")]
    Api { ret_code: i32, ret_msg: String },

    #[error("invalid path parameter {name}: {value}")]
    InvalidPathParameter { name: &'static str, value: String },

    #[error("failed to deserialise CubeMaster response: {0}")]
    Deserialize(String),
}

impl CubeMasterError {
    /// True when CubeMaster returned 404 / 130404 (not found).
    pub fn is_not_found(&self) -> bool {
        matches!(
            self,
            Self::Api {
                ret_code: 130404,
                ..
            }
        )
    }

    /// True when CubeMaster returned 130409 (conflict / wrong state).
    pub fn is_conflict(&self) -> bool {
        matches!(
            self,
            Self::Api {
                ret_code: 130409,
                ..
            }
        )
    }

    pub fn is_invalid_path_parameter(&self) -> bool {
        matches!(self, Self::InvalidPathParameter { .. })
    }

    /// True when CubeMaster doesn't have the endpoint yet (HTTP 404 on the path).
    pub fn is_endpoint_missing(&self) -> bool {
        match self {
            Self::Api { ret_code, .. } => *ret_code == 404,
            Self::Http(e) => e.status().map_or(false, |s| s == 404),
            _ => false,
        }
    }
}

/// Restrict path segments to characters that CubeMaster's resource identifiers
/// are guaranteed to use (`tpl-…`, `snap-…`, `sb-…`, `op-…`, `node-…`,
/// `job-…`, `req-…`).  We deliberately reject `_`, `.`, and `:` even though
/// they are syntactically legal in URI path segments, because:
///
/// * none of CubeMaster's generated IDs ever produce them, so any value that
///   contains one is almost certainly malformed or attacker-supplied;
/// * `:` carries scheme / port semantics in some URL parsers and was reported
///   as a potential source of routing ambiguity;
/// * `.` and `..` are reserved for relative path resolution and easily slip
///   through naive equality checks.
fn validate_path_segment(name: &'static str, value: &str) -> Result<(), CubeMasterError> {
    let is_valid = !value.is_empty()
        && value
            .bytes()
            .all(|b| b == b'-' || b.is_ascii_alphanumeric());

    if is_valid {
        Ok(())
    } else {
        Err(CubeMasterError::InvalidPathParameter {
            name,
            value: value.to_string(),
        })
    }
}

/// Validate a node id before interpolating it into a CubeMaster URL path.
///
/// Unlike [`validate_path_segment`], node ids in the open-source deployment are
/// commonly IPv4 addresses (e.g. `10.0.0.10`), so `.` and `:` must be allowed.
/// The allowed set (`A-Za-z0-9.-_:`) is URL-path-safe, and `..` / `/` / `\` /
/// whitespace are rejected, so the validated value needs no percent-encoding to
/// be safe against path traversal or routing ambiguity.
fn validate_node_id(node_id: &str) -> Result<(), CubeMasterError> {
    let invalid = |value: &str| CubeMasterError::InvalidPathParameter {
        name: "node_id",
        value: value.to_string(),
    };
    if node_id.is_empty() || node_id.len() > 255 || node_id.contains("..") {
        return Err(invalid(node_id));
    }
    let ok = node_id
        .bytes()
        .all(|b| b.is_ascii_alphanumeric() || b == b'.' || b == b'-' || b == b'_' || b == b':');
    if ok {
        Ok(())
    } else {
        Err(invalid(node_id))
    }
}

// ─── Common response envelope ──────────────────────────────────────────────

#[derive(Debug, Deserialize)]
pub struct RetCode {
    pub ret_code: i32,
    pub ret_msg: String,
}

impl RetCode {
    pub fn as_result(&self) -> Result<(), CubeMasterError> {
        if self.ret_code == 0 || self.ret_code == 200 {
            Ok(())
        } else {
            Err(CubeMasterError::Api {
                ret_code: self.ret_code,
                ret_msg: self.ret_msg.clone(),
            })
        }
    }

    pub fn into_result(self) -> Result<(), CubeMasterError> {
        self.as_result()
    }
}

// ─── Sandbox status enum ───────────────────────────────────────────────────

#[derive(Debug, Deserialize, Clone, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum SandboxStatus {
    Running,
    Paused,
    Pausing,
    Stopped,
    Error,
    #[serde(other)]
    Unknown,
}

// ─── Create sandbox ────────────────────────────────────────────────────────

#[derive(Debug, Serialize, Clone)]
pub struct CreateSandboxRequest {
    #[serde(rename = "RequestID", alias = "requestID")]
    pub request_id: String,

    pub instance_type: String,

    #[serde(skip_serializing_if = "Option::is_none")]
    pub timeout: Option<i32>,

    pub containers: Vec<ContainerSpec>,
    pub annotations: HashMap<String, String>,

    #[serde(skip_serializing_if = "Option::is_none")]
    pub labels: Option<HashMap<String, String>>,

    #[serde(rename = "distribution_scope", skip_serializing_if = "Option::is_none")]
    pub distribution_scope: Option<Vec<String>>,

    #[serde(skip_serializing_if = "Option::is_none")]
    pub volumes: Option<Vec<VolumeSpec>>,

    /// Port numbers to expose from the sandbox (e.g. [8888, 49999]).
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub exposed_ports: Vec<u16>,

    /// Network mode: "tap" | "bridge" | ""
    #[serde(skip_serializing_if = "Option::is_none")]
    pub network_type: Option<String>,

    /// Network egress policy.
    #[serde(
        rename = "cube_network_config",
        skip_serializing_if = "Option::is_none"
    )]
    pub cube_network_config: Option<CubeNetworkConfig>,

    /// Auto-pause: when true, CubeMaster publishes this sandbox to the
    /// auto-pause registry consumed by CubeProxy-sidecar; once the proxy
    /// reports it idle for `timeout` seconds the sidecar pauses it.
    /// Field name matches CubeMaster's `auto_pause` JSON tag.
    #[serde(skip_serializing_if = "std::ops::Not::not", default)]
    pub auto_pause: bool,

    /// Auto-resume: when true, an incoming request hitting a paused sandbox
    /// is transparently resumed instead of erroring. Field name matches
    /// CubeMaster's `auto_resume` JSON tag.
    #[serde(skip_serializing_if = "std::ops::Not::not", default)]
    pub auto_resume: bool,
}

/// Network egress control sent to CubeMaster.
#[derive(Debug, Serialize, Clone, Default)]
pub struct CubeNetworkConfig {
    /// Allow internet (public) access. Maps to CubeMaster allowInternetAccess.
    #[serde(
        rename = "allowInternetAccess",
        skip_serializing_if = "Option::is_none"
    )]
    pub allow_internet_access: Option<bool>,

    /// Gate inbound public-URL access. When Some(false), CubeMaster mints a
    /// per-sandbox traffic_access_token that CubeProxy then enforces via the
    /// e2b-traffic-access-token / cube-traffic-access-token request headers.
    /// Omitted on the wire when None to keep request bodies minimal for the
    /// public-by-default case. Maps to CubeMaster allowPublicTraffic.
    #[serde(rename = "allowPublicTraffic", skip_serializing_if = "Option::is_none")]
    pub allow_public_traffic: Option<bool>,

    /// Allowed outbound CIDRs whitelist.
    #[serde(rename = "allowOut", skip_serializing_if = "Vec::is_empty")]
    pub allow_out: Vec<String>,

    /// Denied outbound CIDRs blacklist.
    #[serde(rename = "denyOut", skip_serializing_if = "Vec::is_empty")]
    pub deny_out: Vec<String>,

    /// L7 egress rules, evaluated first-match-wins in list order.
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub rules: Vec<CubeEgressRule>,
}

/// L7 egress rule forwarded to CubeMaster (camelCase wire keys).
#[derive(Debug, Serialize, Clone)]
pub struct CubeEgressRule {
    pub name: String,
    pub r#match: CubeEgressRuleMatch,
    pub action: CubeEgressRuleAction,
}

#[derive(Debug, Serialize, Clone, Default)]
pub struct CubeEgressRuleMatch {
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

#[derive(Debug, Serialize, Clone)]
pub struct CubeEgressRuleAction {
    pub allow: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub audit: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub inject: Option<Vec<CubeEgressRuleInject>>,
}

#[derive(Debug, Serialize, Clone)]
pub struct CubeEgressRuleInject {
    pub header: String,
    pub secret: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub format: Option<String>,
}

#[derive(Debug, Serialize, Clone)]
pub struct ContainerSpec {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub name: Option<String>,
    pub image: ImageSpec,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub command: Option<Vec<String>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub args: Option<Vec<String>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub working_dir: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub resources: Option<ResourceSpec>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub envs: Option<Vec<EnvVar>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub volume_mounts: Option<Vec<VolumeMount>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub dns_config: Option<DnsConfig>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub r_limit: Option<RLimit>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub security_context: Option<SecurityContext>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub probe: Option<Probe>,
    /// Per-container annotations (separate from top-level sandbox annotations).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub annotations: Option<HashMap<String, String>>,
}

#[derive(Debug, Serialize, Clone)]
pub struct ImageSpec {
    pub image: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub storage_media: Option<String>,
}

#[derive(Debug, Serialize, Clone)]
pub struct ResourceSpec {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub cpu: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub mem: Option<String>,
}

/// EnvVar uses `key` (not `name`) per the CubeMaster JSON schema.
#[derive(Debug, Serialize, Clone)]
pub struct EnvVar {
    pub key: String,
    pub value: String,
}

#[derive(Debug, Serialize, Clone)]
pub struct VolumeMount {
    pub name: String,
    pub container_path: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub readonly: Option<bool>,
}

#[derive(Debug, Serialize, Clone)]
pub struct VolumeSpec {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub name: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub volume_source: Option<VolumeSource>,
}

#[derive(Debug, Serialize, Clone)]
pub struct VolumeSource {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub empty_dir: Option<EmptyDir>,
}

#[derive(Debug, Serialize, Clone)]
pub struct EmptyDir {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub size_limit: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub medium: Option<i32>,
}

/// DNS configuration injected into the container's resolv.conf.
#[derive(Debug, Serialize, Clone)]
pub struct DnsConfig {
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub servers: Vec<String>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub searches: Vec<String>,
}

/// Resource limit overrides (ulimit).
#[derive(Debug, Serialize, Clone)]
pub struct RLimit {
    /// RLIMIT_NOFILE — max open file descriptors.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub no_file: Option<u64>,
    /// RLIMIT_NPROC — max child processes.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub no_proc: Option<u64>,
}

/// Container security context.
#[derive(Debug, Serialize, Clone)]
pub struct SecurityContext {
    /// Run container as root with full privileges.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub privileged: Option<bool>,
    /// UID to run the container process as.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub run_as_user: Option<i64>,
}

/// Readiness / liveness probe configuration.
#[derive(Debug, Serialize, Clone)]
pub struct Probe {
    pub probe_handler: ProbeHandler,
    /// Probe timeout in milliseconds.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub timeout_ms: Option<u64>,
    /// How often to probe (ms).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub period_ms: Option<u64>,
    /// Min consecutive successes to be considered healthy.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub success_threshold: Option<u32>,
    /// Max failures before the sandbox is considered unhealthy.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub failure_threshold: Option<u32>,
}

#[derive(Debug, Serialize, Clone)]
pub struct ProbeHandler {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub http_get: Option<HttpGetAction>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub exec: Option<ExecAction>,
}

#[derive(Debug, Serialize, Clone)]
pub struct HttpGetAction {
    pub path: String,
    pub port: u16,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub host: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub scheme: Option<String>,
}

#[derive(Debug, Serialize, Clone)]
pub struct ExecAction {
    pub command: Vec<String>,
}

#[derive(Debug, Deserialize)]
pub struct CreateSandboxResponse {
    #[serde(rename = "RequestID", alias = "requestID")]
    pub request_id: String,
    #[serde(default)]
    pub sandbox_id: String,
    /// Per-sandbox token CubeProxy enforces against
    /// e2b-traffic-access-token / cube-traffic-access-token request
    /// headers. Populated only when the create request set
    /// allowPublicTraffic = false. Empty/None otherwise.
    #[serde(default)]
    pub traffic_access_token: Option<String>,
    pub ret: RetCode,
}

// ─── Delete sandbox ────────────────────────────────────────────────────────

#[derive(Debug, Serialize)]
pub struct DeleteSandboxRequest {
    #[serde(rename = "RequestID", alias = "requestID")]
    pub request_id: String,
    pub sandbox_id: String,
    pub instance_type: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub filter: Option<DeleteFilter>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub sync: Option<bool>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub annotations: Option<HashMap<String, String>>,
}

#[derive(Debug, Serialize)]
pub struct DeleteFilter {
    pub label_selector: HashMap<String, String>,
}

#[derive(Debug, Deserialize)]
#[allow(dead_code)]
pub struct DeleteSandboxResponse {
    #[serde(rename = "RequestID", alias = "requestID")]
    pub request_id: String,
    #[serde(default)]
    pub sandbox_id: String,
    pub ret: RetCode,
}

// ─── List sandboxes ────────────────────────────────────────────────────────

#[derive(Debug, Serialize)]
pub struct ListSandboxRequest {
    #[serde(rename = "RequestID", alias = "requestID")]
    pub request_id: String,
    pub instance_type: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub host_id: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub start_idx: Option<i32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub size: Option<i32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub filter: Option<ListFilter>,
}

#[derive(Debug, Serialize)]
pub struct ListFilter {
    pub label_selector: HashMap<String, String>,
}

#[derive(Debug, Deserialize)]
#[allow(dead_code)]
pub struct ListSandboxResponse {
    #[serde(rename = "RequestID", alias = "requestID")]
    pub request_id: String,
    #[serde(default, alias = "data")]
    pub sandboxes: Vec<SandboxInfo>,
    pub ret: RetCode,
}

/// One sandbox entry as returned by /cube/sandbox/list.
#[derive(Debug, Deserialize)]
pub struct SandboxInfo {
    pub sandbox_id: String,
    #[serde(default)]
    pub host_id: String,
    #[serde(default, deserialize_with = "deserialize_sandbox_status")]
    pub status: String,
    #[serde(default)]
    pub started_at: Option<DateTime<Utc>>,
    /// Unix nanoseconds from Cubelet container.created_at — used as fallback for started_at
    #[serde(default)]
    pub create_at: i64,
    #[serde(default)]
    pub end_at: Option<DateTime<Utc>>,
    #[serde(default, alias = "cpuCount")]
    pub cpu_count: i32,
    #[serde(default, alias = "memoryMB")]
    pub memory_mb: i32,
    #[serde(default)]
    pub template_id: String,
    #[serde(default)]
    pub annotations: HashMap<String, String>,
    #[serde(default)]
    pub labels: HashMap<String, String>,
}

// ─── Get single sandbox ────────────────────────────────────────────────────
// CubeMaster GET /cube/sandbox/info returns: { requestID, ret, data: [{ sandbox_id, status, containers, namespace }] }

#[derive(Debug, Deserialize)]
#[allow(dead_code)]
pub struct GetSandboxResponse {
    #[serde(rename = "RequestID", alias = "requestID")]
    pub request_id: String,
    #[serde(default)]
    pub data: Vec<GetSandboxDataItem>,
    pub ret: RetCode,
}

#[derive(Debug, Deserialize)]
#[allow(dead_code)]
pub struct GetSandboxDataItem {
    #[serde(default)]
    pub sandbox_id: String,
    #[serde(default)]
    pub status: i32,
    #[serde(default)]
    pub host_id: String,
    #[serde(default)]
    pub template_id: String,
    #[serde(default)]
    pub annotations: HashMap<String, String>,
    #[serde(default)]
    pub labels: HashMap<String, String>,
    #[serde(default)]
    pub containers: Vec<GetSandboxContainerItem>,
    #[serde(default)]
    pub namespace: String,
}

#[derive(Debug, Deserialize)]
#[allow(dead_code)]
pub struct GetSandboxContainerItem {
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub container_id: String,
    #[serde(default)]
    pub status: i32,
    #[serde(default)]
    pub image: String,
    #[serde(default)]
    pub create_at: i64,
    #[serde(default)]
    pub cpu: String,
    #[serde(default)]
    pub mem: String,
    #[serde(rename = "type", default)]
    pub kind: String,
    #[serde(default)]
    pub pause_at: i64,
}

/// Normalized sandbox detail used by handlers (built from GetSandboxDataItem).
#[derive(Debug, Clone)]
#[allow(dead_code)]
pub struct SandboxDetail {
    pub sandbox_id: String,
    pub host_id: String,
    pub instance_type: String,
    pub status: SandboxStatus,
    pub template_id: String,
    pub started_at: Option<DateTime<Utc>>,
    pub end_at: Option<DateTime<Utc>>,
    pub cpu_count: i32,
    pub memory_mb: i32,
    pub disk_size_mb: i32,
    pub annotations: HashMap<String, String>,
    pub labels: HashMap<String, String>,
}

fn parse_cpu_millicores(s: &str) -> i32 {
    let s = s.trim().trim_end_matches('m');
    s.parse::<i32>().unwrap_or(0) / 1000
}

fn parse_mem_mb(s: &str) -> i32 {
    let s = s
        .trim()
        .trim_end_matches("Mi")
        .trim_end_matches("MB")
        .trim_end_matches('M');
    s.parse::<i32>().unwrap_or(0)
}

pub(crate) fn datetime_from_unix_nanos(value: i64) -> Option<DateTime<Utc>> {
    if value <= 0 {
        return None;
    }

    let seconds = value.div_euclid(1_000_000_000);
    let nanos = value.rem_euclid(1_000_000_000) as u32;
    DateTime::<Utc>::from_timestamp(seconds, nanos)
}

#[derive(Deserialize)]
#[serde(untagged)]
enum SandboxStatusValue {
    Text(String),
    Number(i32),
}

fn sandbox_status_text_from_code(number: i32) -> &'static str {
    // CubeMaster CONTAINER_* status codes:
    // 0=CREATED, 1=RUNNING, 2=EXITED/STOPPED, 3=UNKNOWN, 4=PAUSING, 5=PAUSED
    match number {
        1 => "running",
        2 => "stopped",
        4 => "pausing",
        5 => "paused",
        _ => "unknown",
    }
}

fn deserialize_sandbox_status<'de, D>(deserializer: D) -> Result<String, D::Error>
where
    D: Deserializer<'de>,
{
    let value = Option::<SandboxStatusValue>::deserialize(deserializer)?;
    Ok(match value {
        Some(SandboxStatusValue::Text(text)) => normalize_sandbox_status_text(&text),
        Some(SandboxStatusValue::Number(number)) => {
            sandbox_status_text_from_code(number).to_string()
        }
        None => String::new(),
    })
}

fn normalize_sandbox_status_text(raw: &str) -> String {
    match raw.trim().to_lowercase().as_str() {
        "1" => sandbox_status_text_from_code(1).to_string(),
        "2" => sandbox_status_text_from_code(2).to_string(),
        "3" => sandbox_status_text_from_code(3).to_string(),
        "4" => sandbox_status_text_from_code(4).to_string(),
        "5" => sandbox_status_text_from_code(5).to_string(),
        "running" | "paused" | "pausing" | "stopped" | "error" => raw.trim().to_lowercase(),
        other => other.to_string(),
    }
}

pub(crate) fn extract_template_id(
    explicit_template_id: &str,
    annotations: &HashMap<String, String>,
    labels: &HashMap<String, String>,
) -> String {
    if !explicit_template_id.trim().is_empty() {
        return explicit_template_id.to_string();
    }
    annotations
        .get(TEMPLATE_ID_LABEL_KEY)
        .cloned()
        .or_else(|| labels.get(TEMPLATE_ID_LABEL_KEY).cloned())
        .unwrap_or_default()
}

impl GetSandboxResponse {
    /// Take the first item from `data` and convert to SandboxDetail. Returns None if data is empty.
    pub fn into_first_sandbox(self, instance_type: &str) -> Option<SandboxDetail> {
        let item = self.data.into_iter().next()?;
        let primary_container = item
            .containers
            .iter()
            .find(|c| c.kind == "sandbox" || c.container_id == item.sandbox_id)
            .or_else(|| item.containers.first());
        let (cpu_count, memory_mb) = primary_container
            .map(|c| (parse_cpu_millicores(&c.cpu), parse_mem_mb(&c.mem)))
            .unwrap_or((0, 0));
        let status = match item.status {
            0 => SandboxStatus::Unknown, // CONTAINER_CREATED
            1 => SandboxStatus::Running, // CONTAINER_RUNNING
            2 => SandboxStatus::Stopped, // CONTAINER_EXITED
            3 => SandboxStatus::Unknown, // CONTAINER_UNKNOWN
            4 => SandboxStatus::Pausing, // CONTAINER_PAUSING
            5 => SandboxStatus::Paused,  // CONTAINER_PAUSED
            _ => SandboxStatus::Unknown,
        };
        let template_id = extract_template_id(&item.template_id, &item.annotations, &item.labels);
        let sid = item.sandbox_id;
        Some(SandboxDetail {
            sandbox_id: sid.clone(),
            host_id: item.host_id,
            instance_type: instance_type.to_string(),
            status,
            template_id,
            started_at: primary_container.and_then(|c| datetime_from_unix_nanos(c.create_at)),
            end_at: None,
            cpu_count,
            memory_mb,
            disk_size_mb: 0,
            annotations: item.annotations,
            labels: item.labels,
        })
    }
}

impl Default for SandboxStatus {
    fn default() -> Self {
        SandboxStatus::Unknown
    }
}

// ─── Update sandbox (pause / resume) ───────────────────────────────────────
// CubeMaster POST /cube/sandbox/update, action: "pause" | "resume"

#[derive(Debug, Serialize)]
pub struct SandboxUpdateRequest {
    #[serde(rename = "requestID")]
    pub request_id: String,
    #[serde(rename = "sandbox_id")]
    pub sandbox_id: String,
    #[serde(rename = "instance_type")]
    pub instance_type: String,
    /// "pause" | "resume"
    #[serde(rename = "action")]
    pub action: String,
    /// TTL in seconds (for resume; 0 = keep original). Optional for pause.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub timeout: Option<i32>,
}

#[derive(Debug, Deserialize)]
pub struct SandboxUpdateResponse {
    pub ret: RetCode,
}

// ─── Set sandbox timeout (absolute) ───────────────────────────────────────
// ❌ New API — not yet implemented on CubeMaster

#[derive(Debug, Serialize)]
pub struct SandboxTimeoutRequest {
    #[serde(rename = "RequestID", alias = "requestID")]
    pub request_id: String,
    #[serde(rename = "sandboxID")]
    pub sandbox_id: String,
    #[serde(rename = "instanceType")]
    pub instance_type: String,
    /// New TTL in seconds from now.
    pub timeout: i32,
}

#[derive(Debug, Deserialize)]
#[allow(dead_code)]
pub struct SandboxTimeoutResponse {
    #[serde(rename = "RequestID", alias = "requestID")]
    pub request_id: String,
    #[serde(rename = "sandboxID", default)]
    pub sandbox_id: String,
    pub end_at: Option<DateTime<Utc>>,
    pub ret: RetCode,
}

// ─── Refresh sandbox TTL (relative extend) ────────────────────────────────
// ❌ New API — not yet implemented on CubeMaster

#[derive(Debug, Serialize)]
pub struct SandboxRefreshRequest {
    #[serde(rename = "RequestID", alias = "requestID")]
    pub request_id: String,
    #[serde(rename = "sandboxID")]
    pub sandbox_id: String,
    #[serde(rename = "instanceType")]
    pub instance_type: String,
    /// Seconds to add onto the current endAt.
    pub duration: i32,
}

#[derive(Debug, Deserialize)]
#[allow(dead_code)]
pub struct SandboxRefreshResponse {
    #[serde(rename = "RequestID", alias = "requestID")]
    pub request_id: String,
    #[serde(rename = "sandboxID", default)]
    pub sandbox_id: String,
    pub end_at: Option<DateTime<Utc>>,
    pub ret: RetCode,
}

// ─── Sandbox logs ──────────────────────────────────────────────────────────
// ✅ Implemented: POST /cube/sandbox/logs

#[derive(Debug, Serialize)]
pub struct SandboxLogsRequest {
    #[serde(rename = "sandboxID")]
    pub sandbox_id: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub cursor: Option<i64>,
    pub limit: i32,
}

#[derive(Debug, Deserialize)]
pub struct SandboxLogsResponse {
    pub ret: RetCode,
    #[serde(default)]
    pub logs: Vec<SandboxLogLine>,
    #[serde(rename = "nextCursor", default)]
    pub next_cursor: Option<i64>,
    #[serde(rename = "hasMore", default)]
    pub has_more: bool,
}

#[derive(Debug, Deserialize)]
pub struct SandboxLogLine {
    pub timestamp: DateTime<Utc>,
    pub message: String,
    #[serde(default)]
    pub level: String,
}

// ─── Snapshot APIs ─────────────────────────────────────────────────────

/// POST /cube/snapshot — request body.
#[derive(Debug, Serialize)]
pub struct CreateSnapshotRequest {
    pub request_id: String,
    pub sandbox_id: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub display_name: Option<String>,
    pub create_request: serde_json::Value,
}

/// Snapshot resource as returned by CubeMaster.
#[derive(Debug, Deserialize, Clone)]
#[allow(dead_code)]
pub struct SnapshotResource {
    pub snapshot_id: String,
    #[serde(default)]
    pub names: Vec<String>,
    #[serde(default)]
    pub display_name: String,
    #[serde(default)]
    pub status: String,
    #[serde(default)]
    pub origin_sandbox_id: String,
    #[serde(default)]
    pub origin_node_id: String,
    #[serde(default)]
    pub instance_type: String,
    #[serde(default)]
    pub storage_backend: String,
    #[serde(default)]
    pub created_at: Option<DateTime<Utc>>,
    #[serde(default)]
    pub updated_at: Option<DateTime<Utc>>,
}

/// Slimmed mirror of CubeMaster's nested `operation` block returned alongside
/// snapshot create / rollback / delete responses.  Master populates the full
/// `templatecenter.SnapshotOperationInfo` (operation, phase, progress, error
/// message, audit timestamps, …) but CubeAPI only ever reads `operation_id`
/// (for the `x-operation-id` audit header) and `status` (used by
/// `ensure_operation_ready` to enforce CubeMaster's synchronous contract).
/// Anything else is recoverable via `GET /cube/operation/{id}` on master, so
/// we keep this type intentionally minimal — extra fields here only invite
/// dead-code drift the next time master adds a new column.
#[derive(Debug, Deserialize, Clone, Default)]
pub struct SnapshotOperationResource {
    #[serde(default)]
    pub operation_id: String,
    #[serde(default)]
    pub status: String,
}

/// POST /cube/snapshot — response.
///
/// The snapshot create flow is synchronous: by the time CubeMaster responds
/// `ret_code == 0`, the snapshot is already `READY` (or master returns an
/// error).  Callers therefore only need the `snapshot` payload to extract
/// the new `snapshot_id` and the populated names — the inner `operation`
/// block that older async clients consulted is intentionally not surfaced
/// on this struct.  If you need it for audit purposes, hit
/// `GET /cube/operation/{operation_id}` on master directly.
#[derive(Debug, Deserialize)]
#[allow(dead_code)]
pub struct CreateSnapshotResponse {
    #[serde(rename = "RequestID", alias = "requestID", default)]
    pub request_id: String,
    pub ret: RetCode,
    pub snapshot: SnapshotResource,
}

/// GET /cube/snapshot — query params (built manually as query string).
#[derive(Debug)]
pub struct ListSnapshotsRequest {
    pub request_id: String,
    pub instance_type: String,
    pub sandbox_id: Option<String>,
    pub name: Option<String>,
    pub status: Option<String>,
    pub limit: Option<i32>,
    pub next_token: Option<String>,
}

/// GET /cube/snapshot — response.
#[derive(Debug, Deserialize)]
#[allow(dead_code)]
pub struct ListSnapshotsResponse {
    #[serde(rename = "RequestID", alias = "requestID", default)]
    pub request_id: String,
    pub ret: RetCode,
    #[serde(default, alias = "data")]
    pub items: Vec<SnapshotResource>,
    #[serde(default)]
    pub next_token: String,
}

/// GET /cube/snapshot/{snapshot_id} — response.
#[derive(Debug, Deserialize)]
#[allow(dead_code)]
pub struct SnapshotDetailResponse {
    #[serde(rename = "RequestID", alias = "requestID", default)]
    pub request_id: String,
    pub ret: RetCode,
    pub snapshot: SnapshotResource,
}

/// DELETE /cube/snapshot/{snapshot_id} — request body.
#[derive(Debug, Serialize)]
pub struct DeleteSnapshotRequest {
    pub request_id: String,
    pub instance_type: String,
}

/// DELETE /cube/snapshot/{snapshot_id} — response.
#[derive(Debug, Deserialize)]
#[allow(dead_code)]
pub struct DeleteSnapshotResponse {
    #[serde(rename = "RequestID", alias = "requestID", default)]
    pub request_id: String,
    pub ret: RetCode,
    #[serde(default)]
    pub snapshot_id: String,
    #[serde(default)]
    pub operation_id: String,
    #[serde(default)]
    pub status: String,
    #[serde(default)]
    pub operation: Option<SnapshotOperationResource>,
}

impl DeleteSnapshotResponse {
    pub fn operation_id(&self) -> Option<&str> {
        if let Some(value) = non_empty_str(self.operation_id.as_str()) {
            return Some(value);
        }
        self.operation
            .as_ref()
            .map(|operation| operation.operation_id.as_str())
            .and_then(non_empty_str)
    }

    pub fn status(&self) -> Option<&str> {
        if let Some(value) = non_empty_str(self.status.as_str()) {
            return Some(value);
        }
        self.operation
            .as_ref()
            .map(|operation| operation.status.as_str())
            .and_then(non_empty_str)
    }
}

/// POST /cube/sandbox/{sandbox_id}/rollback — request body.
#[derive(Debug, Serialize)]
pub struct RollbackRequest {
    pub request_id: String,
    pub snapshot_id: String,
    pub instance_type: String,
}

/// POST /cube/sandbox/{sandbox_id}/rollback — response.
#[derive(Debug, Deserialize)]
#[allow(dead_code)]
pub struct RollbackResponse {
    #[serde(rename = "RequestID", alias = "requestID", default)]
    pub request_id: String,
    pub ret: RetCode,
    #[serde(default)]
    pub sandbox_id: String,
    #[serde(default)]
    pub snapshot_id: String,
    #[serde(default)]
    pub operation_id: String,
    #[serde(default)]
    pub status: String,
    #[serde(default)]
    pub operation: Option<SnapshotOperationResource>,
}

impl RollbackResponse {
    pub fn operation_id(&self) -> Option<&str> {
        if let Some(value) = non_empty_str(self.operation_id.as_str()) {
            return Some(value);
        }
        self.operation
            .as_ref()
            .map(|operation| operation.operation_id.as_str())
            .and_then(non_empty_str)
    }

    pub fn status(&self) -> Option<&str> {
        if let Some(value) = non_empty_str(self.status.as_str()) {
            return Some(value);
        }
        self.operation
            .as_ref()
            .map(|operation| operation.status.as_str())
            .and_then(non_empty_str)
    }
}

// `OperationResponse` (the wrapper for `GET /cube/operation/{id}`) used to
// live here.  It was removed alongside `CubeMasterClient::get_operation` once
// the snapshot create/rollback/delete contracts became synchronous, so the
// only consumer of `/cube/operation/{id}` left in the system is human audit.
// If a programmatic consumer comes back, restore the wrapper next to the
// commented-out method on `CubeMasterClient`.

// Returns the trimmed slice of `value` if it contains any non-whitespace
// characters, otherwise `None`.
//
// We intentionally return the trimmed slice (not the original) because the
// only callers feed the result into places that reject surrounding whitespace
// downstream — most importantly the `x-operation-id` HTTP response header, where
// `HeaderValue::from_str` would accept the spaces as valid `field-value`
// octets but RFC 9110 then requires recipients to strip them, producing a
// header value that drifts between hops. Trimming here keeps the
// "non-empty" promise and the actual returned bytes consistent.
fn non_empty_str(value: &str) -> Option<&str> {
    let trimmed = value.trim();
    (!trimmed.is_empty()).then_some(trimmed)
}

// ─── Helper: parse HTTP response ───────────────────────────────────────────

async fn parse_response<T: for<'de> Deserialize<'de>>(
    resp: reqwest::Response,
) -> Result<T, CubeMasterError> {
    let status = resp.status();
    let body = resp.text().await.map_err(CubeMasterError::Http)?;

    // Try to parse the envelope first — CubeMaster may return HTTP 200 even on
    // logical failures, with ret.ret_code != 0.
    if let Ok(parsed) = serde_json::from_str::<serde_json::Value>(&body) {
        let ret_code = parsed
            .get("ret")
            .and_then(|r| r.get("ret_code"))
            .and_then(|c| c.as_i64())
            .map(|v| v as i32);

        if let Some(code) = ret_code {
            if code != 0 && code != 200 {
                let msg = parsed
                    .get("ret")
                    .and_then(|r| r.get("ret_msg"))
                    .and_then(|m| m.as_str())
                    .unwrap_or(&body)
                    .to_string();
                return Err(CubeMasterError::Api {
                    ret_code: code,
                    ret_msg: msg,
                });
            }
        } else if !status.is_success() {
            // No ret envelope, but HTTP error
            let code = status.as_u16() as i32;
            return Err(CubeMasterError::Api {
                ret_code: code,
                ret_msg: body,
            });
        }
    } else if !status.is_success() {
        return Err(CubeMasterError::Api {
            ret_code: status.as_u16() as i32,
            ret_msg: body,
        });
    }

    serde_json::from_str::<T>(&body)
        .map_err(|e| CubeMasterError::Deserialize(format!("{e}: body={body}")))
}

// ─── Templates ─────────────────────────────────────────────────────────────
// Maps CubeMaster /cube/template* responses.

/// Summary entry returned by GET /cube/template (list mode).
#[derive(Debug, Deserialize, Clone)]
pub struct TemplateSummaryItem {
    #[serde(default)]
    pub template_id: String,
    #[serde(default)]
    pub instance_type: String,
    #[serde(default)]
    pub version: String,
    #[serde(default)]
    pub status: String,
    #[serde(default)]
    pub last_error: String,
    #[serde(default)]
    pub created_at: String,
    #[serde(default)]
    pub image_info: String,
}

/// Envelope for GET /cube/template (list mode).
#[derive(Debug, Deserialize)]
#[allow(dead_code)]
pub struct TemplateListResponse {
    #[serde(rename = "RequestID", alias = "requestID", default)]
    pub request_id: String,
    #[serde(default)]
    pub data: Vec<TemplateSummaryItem>,
    pub ret: RetCode,
}

/// Envelope for GET /cube/template?template_id=... (detail) and
/// POST /cube/template (create).
#[derive(Debug, Deserialize)]
#[allow(dead_code)]
pub struct TemplateResponse {
    #[serde(rename = "RequestID", alias = "requestID", default)]
    pub request_id: String,
    pub ret: RetCode,
    #[serde(default)]
    pub template_id: String,
    #[serde(default)]
    pub instance_type: String,
    #[serde(default)]
    pub version: String,
    #[serde(default)]
    pub status: String,
    #[serde(default)]
    pub last_error: String,
    /// Opaque replica list (node placement). Left as raw JSON to avoid
    /// coupling to CubeMaster-internal types.
    #[serde(default)]
    pub replicas: Vec<serde_json::Value>,
    #[serde(default)]
    pub create_request: Option<serde_json::Value>,
}

/// Body for DELETE /cube/template.
#[derive(Debug, Serialize)]
pub struct TemplateDeleteRequest {
    #[serde(rename = "RequestID", alias = "requestID")]
    pub request_id: String,
    pub template_id: String,
    pub instance_type: String,
    #[serde(default)]
    pub sync: bool,
}

/// Bare envelope used when CubeMaster only returns `ret`.
#[derive(Debug, Deserialize)]
#[allow(dead_code)]
pub struct RetEnvelope {
    #[serde(rename = "RequestID", alias = "requestID", default)]
    pub request_id: String,
    pub ret: RetCode,
}

#[derive(Debug, Deserialize, Default, Clone)]
pub struct TemplateCompatSummary {
    #[serde(default)]
    pub stale_templates: i32,
    #[serde(default)]
    pub stale_replicas: i32,
    #[serde(default)]
    pub affected_nodes: i32,
    #[serde(default)]
    pub missing_replicas: i32,
    #[serde(default)]
    pub unknown_replicas: i32,
}

#[derive(Debug, Deserialize, Default, Clone)]
pub struct TemplateNodeCompat {
    #[serde(default)]
    pub node_id: String,
    #[serde(default)]
    pub node_ip: String,
    #[serde(default)]
    pub compat_status: String,
    #[serde(default)]
    pub bound_guest_image_version: String,
    #[serde(default)]
    pub current_guest_image_version: String,
    #[serde(default)]
    pub bound_agent_version: String,
    #[serde(default)]
    pub current_agent_version: String,
    #[serde(default)]
    pub bound_kernel_version: String,
    #[serde(default)]
    pub current_kernel_version: String,
}

#[derive(Debug, Deserialize, Default, Clone)]
pub struct TemplateCompatRow {
    #[serde(default)]
    pub template_id: String,
    #[serde(default)]
    pub instance_type: String,
    #[serde(default)]
    pub overall: String,
    #[serde(default)]
    pub nodes: Vec<TemplateNodeCompat>,
}

#[derive(Debug, Deserialize, Default, Clone)]
pub struct TemplateCompatMatrix {
    #[serde(default)]
    pub summary: TemplateCompatSummary,
    #[serde(default)]
    pub templates: Vec<TemplateCompatRow>,
}

#[derive(Debug, Deserialize)]
#[allow(dead_code)]
pub struct TemplateCompatResponse {
    #[serde(rename = "RequestID", alias = "requestID", default)]
    pub request_id: String,
    pub ret: RetCode,
    #[serde(default)]
    pub data: Option<TemplateCompatMatrix>,
}

#[derive(Debug, Serialize)]
pub struct TemplateCompatAdoptRequest {
    pub action: String,
    pub template_id: String,
}

#[derive(Debug, Deserialize)]
#[allow(dead_code)]
pub struct TemplateCompatAdoptResponse {
    #[serde(rename = "RequestID", alias = "requestID", default)]
    pub request_id: String,
    pub ret: RetCode,
    #[serde(default)]
    pub updated: i32,
}

/// Body for POST /cube/template/from-image.
#[derive(Debug, Serialize)]
pub struct CreateTemplateFromImageReq {
    #[serde(rename = "requestID")]
    pub request_id: String,
    pub instance_type: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub template_id: String,
    /// CubeMaster field name for the source image.
    pub source_image_ref: String,
    /// Writable layer size, e.g. "1G".
    #[serde(skip_serializing_if = "Option::is_none")]
    pub writable_layer_size: Option<String>,
    /// Ports exposed by the container.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub exposed_ports: Option<Vec<u16>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub network_type: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub registry_username: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub registry_password: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub distribution_scope: Option<Vec<String>>,
    /// Container-level overrides (probe, resources, envs).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub container_overrides: Option<CreateTemplateContainerOverrides>,
    /// Network / internet-access context.
    #[serde(
        rename = "cube_network_config",
        skip_serializing_if = "Option::is_none"
    )]
    pub cube_network_config: Option<CreateTemplateCubeNetworkConfig>,
}

/// Minimal container overrides for template creation.
#[derive(Debug, Serialize)]
pub struct CreateTemplateContainerOverrides {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub command: Option<Vec<String>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub args: Option<Vec<String>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub probe: Option<Probe>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub resources: Option<CreateTemplateResources>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub envs: Option<Vec<CreateTemplateEnv>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub dns_config: Option<DnsConfig>,
}

/// CPU / memory resources for template container.
#[derive(Debug, Serialize)]
pub struct CreateTemplateResources {
    /// CPU in millicores, e.g. "2000m".
    #[serde(skip_serializing_if = "Option::is_none")]
    pub cpu: Option<String>,
    /// Memory, e.g. "2000Mi".
    #[serde(skip_serializing_if = "Option::is_none")]
    pub mem: Option<String>,
}

/// Key-value env var.
#[derive(Debug, Serialize)]
pub struct CreateTemplateEnv {
    pub key: String,
    pub value: String,
}

/// Network config for template creation.
#[derive(Debug, Serialize)]
pub struct CreateTemplateCubeNetworkConfig {
    #[serde(
        rename = "allowInternetAccess",
        skip_serializing_if = "Option::is_none"
    )]
    pub allow_internet_access: Option<bool>,
    #[serde(rename = "allowOut", skip_serializing_if = "Vec::is_empty")]
    pub allow_out: Vec<String>,
    #[serde(rename = "denyOut", skip_serializing_if = "Vec::is_empty")]
    pub deny_out: Vec<String>,
}

/// Body for POST /cube/template/redo (rebuild).
#[derive(Debug, Serialize)]
pub struct RedoTemplateReq {
    #[serde(rename = "requestID", alias = "RequestID")]
    pub request_id: String,
    pub template_id: String,
    #[serde(flatten)]
    pub extra: serde_json::Map<String, serde_json::Value>,
}

/// Envelope for template-build jobs (from-image / redo / poll).
#[derive(Debug, Deserialize)]
#[allow(dead_code)]
pub struct TemplateJobResponse {
    #[serde(rename = "RequestID", alias = "requestID", default)]
    pub request_id: String,
    pub ret: RetCode,
    #[serde(default)]
    pub job: Option<TemplateJob>,
}

#[derive(Debug, Deserialize, Clone)]
#[allow(dead_code)]
pub struct TemplateJob {
    #[serde(default)]
    pub job_id: String,
    #[serde(default)]
    pub template_id: String,
    #[serde(default)]
    pub status: String,
    #[serde(default)]
    pub phase: String,
    #[serde(default)]
    pub progress: i32,
    #[serde(default)]
    pub error_message: String,
    #[serde(default)]
    pub attempt_no: i32,
    #[serde(default)]
    pub retry_of_job_id: String,
}

/// Envelope for GET /cube/template/build/{id}/status.
#[derive(Debug, Deserialize)]
#[allow(dead_code)]
pub struct TemplateBuildStatusResponse {
    pub ret: RetCode,
    #[serde(default)]
    pub build_id: String,
    #[serde(default)]
    pub template_id: String,
    #[serde(default)]
    pub attempt_no: i32,
    #[serde(default)]
    pub retry_of_job_id: String,
    #[serde(default)]
    pub status: String,
    #[serde(default)]
    pub progress: i32,
    #[serde(default)]
    pub message: String,
}

// ─── Nodes ─────────────────────────────────────────────────────────────────
// Maps CubeMaster /internal/meta/nodes responses.

#[derive(Debug, Deserialize, Clone, Default)]
#[allow(dead_code)]
pub struct NodeResources {
    #[serde(default)]
    pub milli_cpu: i64,
    #[serde(default)]
    pub memory_mb: i64,
}

#[derive(Debug, Deserialize, Clone, Default)]
#[allow(dead_code)]
pub struct NodeCondition {
    #[serde(rename = "type", default)]
    pub kind: String,
    #[serde(default)]
    pub status: String,
    #[serde(rename = "lastHeartbeatTime", default)]
    pub last_heartbeat_time: Option<DateTime<Utc>>,
    #[serde(rename = "lastTransitionTime", default)]
    pub last_transition_time: Option<DateTime<Utc>>,
    #[serde(default)]
    pub reason: String,
    #[serde(default)]
    pub message: String,
}

#[derive(Debug, Deserialize, Clone, Default)]
#[allow(dead_code)]
pub struct NodeImage {
    #[serde(default)]
    pub names: Vec<String>,
    #[serde(default)]
    pub size_bytes: i64,
    #[serde(default)]
    pub namespace: String,
    #[serde(default)]
    pub media_type: String,
}

#[derive(Debug, Deserialize, Clone, Default)]
#[allow(dead_code)]
pub struct LocalTemplate {
    #[serde(default)]
    pub template_id: String,
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub media: String,
    #[serde(default)]
    pub path: String,
    #[serde(default)]
    pub namespace: String,
}

#[derive(Debug, Deserialize, Clone, Default)]
#[allow(dead_code)]
pub struct NodeSnapshot {
    #[serde(default)]
    pub node_id: String,
    #[serde(default)]
    pub host_ip: String,
    #[serde(default)]
    pub grpc_port: i32,
    #[serde(default)]
    pub labels: HashMap<String, String>,
    #[serde(default)]
    pub capacity: NodeResources,
    #[serde(default)]
    pub allocatable: NodeResources,
    #[serde(default)]
    pub instance_type: String,
    #[serde(default)]
    pub cluster_label: String,
    #[serde(default)]
    pub quota_cpu: i64,
    #[serde(default)]
    pub quota_mem_mb: i64,
    #[serde(default)]
    pub create_concurrent_num: i64,
    #[serde(default)]
    pub max_mvm_num: i64,
    #[serde(default)]
    pub conditions: Vec<NodeCondition>,
    #[serde(default)]
    pub images: Vec<NodeImage>,
    #[serde(default)]
    pub local_templates: Vec<LocalTemplate>,
    #[serde(default)]
    pub versions: Vec<ComponentVersion>,
    #[serde(default)]
    pub heartbeat_time: Option<DateTime<Utc>>,
    #[serde(default)]
    pub healthy: bool,
    #[serde(default)]
    pub isolated: bool,
    #[serde(default)]
    pub isolated_at: i64,
    #[serde(default)]
    pub isolated_by: String,
    #[serde(default)]
    pub isolated_reason: String,
}

/// One component's version on a node, as reported by cubelet.
#[derive(Debug, Deserialize, Clone, Default)]
#[allow(dead_code)]
pub struct ComponentVersion {
    #[serde(default)]
    pub component: String,
    #[serde(default)]
    pub version: String,
    #[serde(default)]
    pub commit: String,
    #[serde(default)]
    pub build_time: String,
    #[serde(default)]
    pub source: String,
}

#[derive(Debug, Deserialize)]
#[allow(dead_code)]
pub struct NodesResponse {
    #[serde(rename = "requestID", alias = "RequestID", default)]
    pub request_id: String,
    pub ret: RetCode,
    #[serde(default)]
    pub data: Vec<NodeSnapshot>,
}

#[derive(Debug, Deserialize)]
#[allow(dead_code)]
pub struct NodeResponse {
    #[serde(rename = "requestID", alias = "RequestID", default)]
    pub request_id: String,
    pub ret: RetCode,
    #[serde(default)]
    pub data: Option<NodeSnapshot>,
}

/// Request body for the node isolation endpoint. The audit identity is carried
/// out-of-band in the X-Operator header, never here, so it cannot be forged.
#[derive(Debug, Serialize)]
pub struct SetNodeIsolationRequest {
    pub isolated: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub reason: Option<String>,
}

// ─── Version matrix ─────────────────────────────────────────────────────────

#[derive(Debug, Deserialize, Clone, Default)]
#[allow(dead_code)]
pub struct ControlPlaneVersion {
    #[serde(default)]
    pub version: String,
    #[serde(default)]
    pub commit: String,
    #[serde(default)]
    pub build_time: String,
}

#[derive(Debug, Deserialize, Clone, Default)]
#[allow(dead_code)]
pub struct ComponentVersionGroup {
    #[serde(default)]
    pub version: String,
    #[serde(default)]
    pub nodes: Vec<String>,
}

#[derive(Debug, Deserialize, Clone, Default)]
#[allow(dead_code)]
pub struct ComponentMatrixRow {
    #[serde(default)]
    pub component: String,
    #[serde(default)]
    pub declared_version: String,
    #[serde(default)]
    pub declared_versions: Vec<String>,
    #[serde(default)]
    pub consistent: bool,
    #[serde(default)]
    pub versions: Vec<ComponentVersionGroup>,
}

#[derive(Debug, Deserialize, Clone, Default)]
#[allow(dead_code)]
pub struct NodeComponentEntry {
    #[serde(default)]
    pub component: String,
    #[serde(default)]
    pub version: String,
    #[serde(default)]
    pub declared: bool,
}

#[derive(Debug, Deserialize, Clone, Default)]
#[allow(dead_code)]
pub struct NodeVersionRow {
    #[serde(default)]
    pub node_id: String,
    #[serde(default)]
    pub healthy: bool,
    #[serde(default)]
    pub components: Vec<NodeComponentEntry>,
}

#[derive(Debug, Deserialize, Clone, Default)]
#[allow(dead_code)]
pub struct VersionMatrix {
    #[serde(default)]
    pub control_plane: ControlPlaneVersion,
    #[serde(default)]
    pub components: Vec<ComponentMatrixRow>,
    #[serde(default)]
    pub nodes: Vec<NodeVersionRow>,
}

#[derive(Debug, Deserialize)]
#[allow(dead_code)]
pub struct VersionMatrixResponse {
    #[serde(rename = "requestID", alias = "RequestID", default)]
    pub request_id: String,
    pub ret: RetCode,
    #[serde(default)]
    pub data: Option<VersionMatrix>,
}

#[cfg(test)]
mod tests {
    use super::{
        non_empty_str, validate_node_id, validate_path_segment, CubeMasterError,
        GetSandboxResponse, SandboxInfo,
    };

    #[test]
    fn validate_node_id_accepts_ip_and_hostname_ids() {
        for value in [
            "10.0.0.10",
            "192.168.1.1",
            "fe80::1",
            "cube-edge-01",
            "node_a1",
            "ABC123",
        ] {
            if let Err(err) = validate_node_id(value) {
                panic!("expected {value:?} to be accepted, got {err:?}");
            }
        }
    }

    #[test]
    fn validate_node_id_rejects_traversal_and_separators() {
        for value in ["", "../etc/passwd", "a/b", "a\\b", "node a", "a..b"] {
            assert!(
                validate_node_id(value).is_err(),
                "expected {value:?} to be rejected"
            );
        }
        assert!(validate_node_id(&"a".repeat(256)).is_err());
    }

    #[test]
    fn non_empty_str_trims_surrounding_whitespace() {
        assert_eq!(non_empty_str(" op-1 "), Some("op-1"));
        assert_eq!(non_empty_str("\top-1\n"), Some("op-1"));
        assert_eq!(non_empty_str("op-1"), Some("op-1"));
    }

    #[test]
    fn non_empty_str_treats_blank_input_as_none() {
        assert_eq!(non_empty_str(""), None);
        assert_eq!(non_empty_str("   "), None);
        assert_eq!(non_empty_str("\t\n"), None);
    }

    #[test]
    fn build_id_path_segment_accepts_alphanumeric_and_hyphen() {
        for value in [
            "abc-123",
            "snap-5e40d162cda74b58a527037b",
            "tpl-abcdef0123456789",
            "sb-1",
            "op-9",
            "ABC123",
        ] {
            if let Err(err) = validate_path_segment("build_id", value) {
                panic!("expected {value:?} to be accepted, got {err:?}");
            }
        }
    }

    #[test]
    fn build_id_path_segment_rejects_path_control_characters() {
        for value in ["../../x", "abc/123", "abc?x=1", ""] {
            let err = validate_path_segment("build_id", value)
                .expect_err("build id should reject path control characters");

            assert!(matches!(
                err,
                CubeMasterError::InvalidPathParameter {
                    name: "build_id",
                    ..
                }
            ));
        }
    }

    /// CubeMaster only emits IDs with `[A-Za-z0-9-]`; the wrapper rejects every
    /// other byte so that a stray `_`, `.`, or `:` in user input fails fast at
    /// the API boundary instead of corrupting downstream URLs (Bug 4).
    #[test]
    fn build_id_path_segment_rejects_non_canonical_characters() {
        for value in [
            "abc_123",
            "abc.123",
            "abc:123",
            "abc 123",
            "abc#123",
            "abc@123",
            "abc%2F123",
            "héllo",
            "..",
            ".",
        ] {
            match validate_path_segment("snapshot_id", value) {
                Err(CubeMasterError::InvalidPathParameter { name, .. }) => {
                    assert_eq!(name, "snapshot_id");
                }
                Err(other) => panic!("unexpected error variant for {value:?}: {other:?}"),
                Ok(()) => panic!("validator unexpectedly accepted {value:?}"),
            }
        }
    }

    #[test]
    fn sandbox_info_deserializes_resource_fields_from_list_payload() {
        let payload = serde_json::json!({
            "sandbox_id": "sb-1",
            "host_id": "host-1",
            "status": 1,
            "cpu_count": 2,
            "memory_mb": 2048,
            "template_id": "tpl-1",
            "labels": { "user": "alice" }
        });

        let info: SandboxInfo =
            serde_json::from_value(payload).expect("sandbox info should deserialize");
        assert_eq!(info.cpu_count, 2);
        assert_eq!(info.memory_mb, 2048);
        assert_eq!(info.template_id, "tpl-1");
    }

    #[test]
    fn sandbox_info_accepts_camel_case_resource_aliases() {
        let payload = serde_json::json!({
            "sandbox_id": "sb-2",
            "cpuCount": 4,
            "memoryMB": 4096
        });

        let info: SandboxInfo =
            serde_json::from_value(payload).expect("sandbox info should deserialize aliases");
        assert_eq!(info.cpu_count, 4);
        assert_eq!(info.memory_mb, 4096);
    }

    #[test]
    fn get_sandbox_prefers_sandbox_container_timestamps_and_host() {
        let payload = serde_json::json!({
            "requestID": "req-1",
            "ret": { "ret_code": 0, "ret_msg": "ok" },
            "data": [{
                "sandbox_id": "sb-1",
                "host_id": "host-1",
                "status": 1,
                "template_id": "tpl-1",
                "containers": [
                    {
                        "container_id": "workload-1",
                        "type": "workload",
                        "create_at": 1713953785140309977i64,
                        "cpu": "500m",
                        "mem": "512Mi"
                    },
                    {
                        "container_id": "sb-1",
                        "type": "sandbox",
                        "create_at": 1713953785140309977i64,
                        "cpu": "2000m",
                        "mem": "2048Mi"
                    }
                ]
            }]
        });

        let response: GetSandboxResponse =
            serde_json::from_value(payload).expect("response should deserialize");
        let detail = response
            .into_first_sandbox("cubebox")
            .expect("detail should exist");

        assert_eq!(detail.host_id, "host-1");
        assert_eq!(detail.cpu_count, 2);
        assert_eq!(detail.memory_mb, 2048);
        assert_eq!(
            detail
                .started_at
                .expect("start time should exist")
                .timestamp_nanos_opt(),
            Some(1713953785140309977)
        );
    }
}
