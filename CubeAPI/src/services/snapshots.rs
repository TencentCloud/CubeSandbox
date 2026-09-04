// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use uuid::Uuid;

use crate::{
    cubemaster::{
        CreateSnapshotRequest, CubeMasterClient, DeleteSnapshotRequest, ListSnapshotsRequest,
        RollbackRequest as MasterRollbackRequest, SnapshotResource,
    },
    error::{AppError, AppResult},
    models::{
        DeleteSnapshotResponse as ApiDeleteSnapshotResponse, RollbackResponse, SnapshotInfo,
        SnapshotListItem,
    },
};

#[derive(Clone)]
pub struct SnapshotService {
    cubemaster: CubeMasterClient,
    instance_type: String,
}

impl SnapshotService {
    pub fn new(cubemaster: CubeMasterClient, instance_type: String) -> Self {
        Self {
            cubemaster,
            instance_type,
        }
    }

    // ── POST /sandboxes/{sandboxID}/snapshots ──────────────────────────────

    pub async fn create(
        &self,
        sandbox_id: &str,
        name: Option<String>,
        backend: Option<String>,
    ) -> AppResult<SnapshotInfo> {
        let request_id = new_request_id();
        let req = CreateSnapshotRequest {
            request_id: request_id.clone(),
            sandbox_id: sandbox_id.to_string(),
            display_name: name,
            backend,
        };

        match self.cubemaster.create_snapshot(&req).await {
            Ok(resp) => {
                resp.ret.as_result().map_err(|e| {
                    if e.is_not_found() {
                        sandbox_not_found(sandbox_id)
                    } else if e.is_conflict() {
                        snapshot_create_conflict(sandbox_id)
                    } else {
                        internal_error(e)
                    }
                })?;
                let snapshot_id = resp.snapshot.snapshot_id.clone();
                if snapshot_id.trim().is_empty() {
                    return Err(AppError::Internal(anyhow::anyhow!(
                        "snapshot create response missing snapshot_id"
                    )));
                }
                ensure_snapshot_ready(&resp.snapshot)?;
                Ok(snapshot_resource_to_info(resp.snapshot))
            }
            Err(e) if e.is_not_found() => Err(sandbox_not_found(sandbox_id)),
            Err(e) => Err(internal_error(e)),
        }
    }

    // ── GET /snapshots ─────────────────────────────────────────────────────

    pub async fn list(
        &self,
        sandbox_id: Option<&str>,
        limit: Option<i32>,
        next_token: Option<&str>,
    ) -> AppResult<(Vec<SnapshotListItem>, String)> {
        let req = ListSnapshotsRequest {
            request_id: new_request_id(),
            instance_type: self.instance_type.clone(),
            sandbox_id: sandbox_id.map(str::to_string),
            name: None,
            status: None,
            limit,
            // Normalise an empty cursor (e.g. the client sent `?nextToken=`)
            // back to `None` so we don't relay a meaningless pagination token
            // to CubeMaster (Bug 3).  Whitespace-only tokens get the same
            // treatment.
            next_token: normalize_next_token(next_token),
        };

        match self.cubemaster.list_snapshots(&req).await {
            Ok(resp) => {
                resp.ret.as_result().map_err(internal_error)?;
                let items = resp
                    .items
                    .into_iter()
                    .map(snapshot_resource_to_list_item)
                    .collect();
                Ok((items, resp.next_token))
            }
            Err(e) => Err(internal_error(e)),
        }
    }

    // ── DELETE /templates/{templateID}  (when templateID is a snapshot id) ─
    //
    // Master returns `ret_code == 0` + `status == READY` once the snapshot is
    // logically retired. Physical cleanup is synchronous only with no
    // runtime refs; otherwise the row is tombstoned and bytes are reclaimed
    // later. `ensure_operation_ready` rejects Pending/Running; CREATING /
    // in-progress delete remain conflicts. The 240 s timeout covers the
    // no-ref synchronous path.

    pub async fn delete(&self, snapshot_id: &str) -> AppResult<ApiDeleteSnapshotResponse> {
        let req = DeleteSnapshotRequest {
            request_id: new_request_id(),
            instance_type: self.instance_type.clone(),
        };

        match self.cubemaster.delete_snapshot(snapshot_id, &req).await {
            Ok(resp) => {
                let operation_id = required_operation_id(resp.operation_id(), "snapshot delete")?;
                resp.ret.as_result().map_err(|e| {
                    if e.is_not_found() {
                        snapshot_not_found(snapshot_id)
                    } else if e.is_conflict() {
                        snapshot_delete_conflict(snapshot_id)
                    } else {
                        internal_error(e)
                    }
                })?;
                let status = ensure_operation_ready(resp.status(), "snapshot delete", snapshot_id)?;

                Ok(ApiDeleteSnapshotResponse {
                    template_id: snapshot_id.to_string(),
                    operation_id,
                    status,
                })
            }
            Err(e) if e.is_not_found() => Err(snapshot_not_found(snapshot_id)),
            Err(e) => Err(internal_error(e)),
        }
    }

    pub async fn has_snapshot(&self, snapshot_id: &str) -> AppResult<bool> {
        // Identity, not visibility: a tombstoned snap- id is still a snapshot
        // and must keep using DeleteSnapshot (idempotent), not DeleteTemplate.
        if is_snapshot_identity(snapshot_id) {
            return Ok(true);
        }
        match self.cubemaster.get_snapshot(snapshot_id, false).await {
            Ok(resp) => {
                resp.ret.as_result().map_err(internal_error)?;
                Ok(true)
            }
            Err(e) if e.is_not_found() => Ok(false),
            Err(e) => Err(internal_error(e)),
        }
    }

    // ── POST /sandboxes/{sandboxID}/rollback ───────────────────────────────

    pub async fn rollback(
        &self,
        sandbox_id: &str,
        snapshot_id: &str,
        backend: Option<String>,
    ) -> AppResult<RollbackResponse> {
        let req_id = new_request_id();
        let req = MasterRollbackRequest {
            request_id: req_id.clone(),
            snapshot_id: snapshot_id.to_string(),
            instance_type: self.instance_type.clone(),
            backend,
        };

        match self.cubemaster.rollback_sandbox(sandbox_id, &req).await {
            Ok(resp) => {
                let operation_id = required_operation_id(resp.operation_id(), "snapshot rollback")?;
                resp.ret.as_result().map_err(|e| {
                    if e.is_not_found() {
                        sandbox_or_snapshot_not_found(sandbox_id, snapshot_id)
                    } else if e.is_conflict() {
                        rollback_conflict(sandbox_id, snapshot_id)
                    } else {
                        internal_error(e)
                    }
                })?;
                let status =
                    ensure_operation_ready(resp.status(), "snapshot rollback", snapshot_id)?;

                Ok(RollbackResponse {
                    sandbox_id: owned_or_fallback(resp.sandbox_id, sandbox_id),
                    snapshot_id: owned_or_fallback(resp.snapshot_id, snapshot_id),
                    operation_id,
                    status,
                })
            }
            Err(e) if e.is_not_found() => {
                Err(sandbox_or_snapshot_not_found(sandbox_id, snapshot_id))
            }
            Err(e) => Err(internal_error(e)),
        }
    }
}

// ── helpers ────────────────────────────────────────────────────────────────

fn new_request_id() -> String {
    Uuid::new_v4().to_string()
}

/// Normalise a pagination cursor coming from the request URL.  CubeMaster
/// only accepts a real cursor or no parameter at all; an empty / whitespace
/// `next_token=` query restarts pagination silently in some builds, so we
/// strip it here before reaching the client (Bug 3).
fn normalize_next_token(token: Option<&str>) -> Option<String> {
    token
        .map(str::trim)
        .filter(|t| !t.is_empty())
        .map(str::to_string)
}

fn internal_error(e: impl std::fmt::Display) -> AppError {
    AppError::Internal(anyhow::anyhow!("{}", e))
}

fn required_operation_id(operation_id: Option<&str>, operation: &str) -> AppResult<String> {
    operation_id.map(str::to_owned).ok_or_else(|| {
        AppError::Internal(anyhow::anyhow!(
            "{} response missing operation_id",
            operation
        ))
    })
}

fn sandbox_not_found(sandbox_id: &str) -> AppError {
    AppError::NotFound(format!("sandbox {} not found", sandbox_id))
}

fn snapshot_not_found(snapshot_id: &str) -> AppError {
    AppError::NotFound(format!("snapshot {} not found", snapshot_id))
}

fn sandbox_or_snapshot_not_found(sandbox_id: &str, snapshot_id: &str) -> AppError {
    AppError::NotFound(format!(
        "sandbox {} or snapshot {} not found",
        sandbox_id, snapshot_id
    ))
}

fn snapshot_create_conflict(sandbox_id: &str) -> AppError {
    AppError::Conflict(format!(
        "sandbox {} has a snapshot operation in progress",
        sandbox_id
    ))
}

fn snapshot_delete_conflict(snapshot_id: &str) -> AppError {
    AppError::Conflict(format!(
        "snapshot {} cannot be deleted (active operation in progress)",
        snapshot_id
    ))
}

fn rollback_conflict(sandbox_id: &str, snapshot_id: &str) -> AppError {
    AppError::Conflict(format!(
        "rollback conflict: sandbox={} snapshot={}",
        sandbox_id, snapshot_id
    ))
}

fn snapshot_resource_to_info(r: SnapshotResource) -> SnapshotInfo {
    let names = snapshot_names(&r);
    let backend = snapshot_backend(&r);
    let remote_status = optional_backend(&r.remote_status);
    SnapshotInfo {
        snapshot_id: r.snapshot_id,
        names,
        backend,
        remote_status,
    }
}

fn ensure_snapshot_ready(snapshot: &SnapshotResource) -> AppResult<()> {
    let status = normalized_status(Some(snapshot.status.as_str()));
    if status == "READY" {
        return Ok(());
    }
    Err(AppError::Internal(anyhow::anyhow!(
        "snapshot {} returned unexpected status {}",
        snapshot.snapshot_id,
        snapshot.status.trim()
    )))
}

fn ensure_operation_ready(
    status: Option<&str>,
    operation: &str,
    resource_id: &str,
) -> AppResult<String> {
    let status = normalized_status(status);
    if status == "READY" {
        return Ok(status);
    }
    Err(AppError::Internal(anyhow::anyhow!(
        "{} for {} returned unexpected status {}",
        operation,
        resource_id,
        status
    )))
}

fn normalized_status(status: Option<&str>) -> String {
    let status = status.unwrap_or_default().trim().to_ascii_uppercase();
    if status.is_empty() {
        "<empty>".to_string()
    } else {
        status
    }
}

fn snapshot_resource_to_list_item(r: SnapshotResource) -> SnapshotListItem {
    let names = snapshot_names(&r);
    let backend = snapshot_backend(&r);
    let remote_status = optional_backend(&r.remote_status);
    SnapshotListItem {
        snapshot_id: r.snapshot_id,
        names,
        status: r.status,
        origin_sandbox_id: if r.origin_sandbox_id.is_empty() {
            None
        } else {
            Some(r.origin_sandbox_id)
        },
        created_at: r.created_at,
        updated_at: r.updated_at,
        backend,
        remote_status,
    }
}

fn snapshot_backend(r: &SnapshotResource) -> Option<String> {
    if let Some(b) = optional_backend(&r.backend) {
        return Some(b);
    }
    optional_backend(&r.storage_backend)
}

fn optional_backend(value: &str) -> Option<String> {
    let trimmed = value.trim();
    if trimmed.is_empty() {
        None
    } else {
        Some(trimmed.to_string())
    }
}

fn snapshot_names(resource: &SnapshotResource) -> Vec<String> {
    if !resource.names.is_empty() {
        return resource.names.clone();
    }
    if !resource.display_name.trim().is_empty() {
        return vec![resource.display_name.clone()];
    }
    if resource.snapshot_id.trim().is_empty() {
        return Vec::new();
    }
    vec![resource.snapshot_id.clone()]
}

fn is_snapshot_identity(id: &str) -> bool {
    id.trim().starts_with("snap-")
}

fn owned_or_fallback(value: String, fallback: &str) -> String {
    if value.trim().is_empty() {
        fallback.to_string()
    } else {
        value
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sample_snapshot(status: &str) -> SnapshotResource {
        SnapshotResource {
            snapshot_id: "snap-1".into(),
            names: vec!["snap-name".into()],
            display_name: "snap-name".into(),
            status: status.into(),
            origin_sandbox_id: "sb-1".into(),
            origin_node_id: "node-a".into(),
            instance_type: "cubebox".into(),
            storage_backend: String::new(),
            backend: String::new(),
            remote_status: String::new(),
            created_at: None,
            updated_at: None,
        }
    }

    #[test]
    fn snapshot_identity_uses_snap_prefix() {
        assert!(is_snapshot_identity("snap-c85fc2cb8bc04831bd75f551"));
        assert!(is_snapshot_identity("  snap-abc"));
        assert!(!is_snapshot_identity("tpl-abc"));
        assert!(!is_snapshot_identity(""));
        assert!(!is_snapshot_identity("snapshot-1"));
    }

    #[test]
    fn normalize_next_token_drops_empty_and_whitespace_cursors() {
        assert_eq!(normalize_next_token(None), None);
        assert_eq!(normalize_next_token(Some("")), None);
        assert_eq!(normalize_next_token(Some("   ")), None);
        assert_eq!(normalize_next_token(Some("\t\n")), None);
        assert_eq!(
            normalize_next_token(Some("  cursor-42  ")),
            Some("cursor-42".to_string())
        );
        assert_eq!(
            normalize_next_token(Some("cursor-42")),
            Some("cursor-42".to_string())
        );
    }

    #[test]
    fn snapshot_ready_guard_rejects_non_ready_status() {
        let err = ensure_snapshot_ready(&sample_snapshot("CREATING"))
            .expect_err("non-ready snapshot should fail");
        match err {
            AppError::Internal(inner) => {
                assert!(inner.to_string().contains("unexpected status"));
            }
            other => panic!("unexpected error: {other:?}"),
        }
    }

    #[test]
    fn operation_ready_guard_rejects_non_ready_status() {
        let err = ensure_operation_ready(Some("RUNNING"), "snapshot rollback", "snap-1")
            .expect_err("non-ready operation should fail");
        match err {
            AppError::Internal(inner) => {
                assert!(inner.to_string().contains("unexpected status"));
            }
            other => panic!("unexpected error: {other:?}"),
        }
    }

    #[test]
    fn snapshot_info_uses_e2b_shape_without_operation_id() {
        let payload = serde_json::to_value(snapshot_resource_to_info(sample_snapshot("READY")))
            .expect("serialize snapshot info");

        assert_eq!(
            payload.get("snapshotID").and_then(|value| value.as_str()),
            Some("snap-1")
        );
        assert!(payload.get("operationID").is_none());
    }

    #[test]
    fn snapshot_info_falls_back_to_snapshot_id_for_names() {
        let mut snapshot = sample_snapshot("READY");
        snapshot.names.clear();
        snapshot.display_name.clear();

        let payload = serde_json::to_value(snapshot_resource_to_info(snapshot))
            .expect("serialize snapshot info");

        assert_eq!(
            payload.get("snapshotID").and_then(|value| value.as_str()),
            Some("snap-1")
        );
        assert_eq!(
            payload
                .get("names")
                .and_then(|value| value.as_array())
                .and_then(|values| values.first())
                .and_then(|value| value.as_str()),
            Some("snap-1")
        );
    }

    #[test]
    fn snapshot_list_item_uses_snapshot_id_field_shape() {
        let payload =
            serde_json::to_value(snapshot_resource_to_list_item(sample_snapshot("READY")))
                .expect("serialize snapshot list item");

        assert_eq!(
            payload.get("snapshotID").and_then(|value| value.as_str()),
            Some("snap-1")
        );
        assert_eq!(
            payload
                .get("names")
                .and_then(|value| value.as_array())
                .and_then(|values| values.first())
                .and_then(|value| value.as_str()),
            Some("snap-name")
        );
    }
}
