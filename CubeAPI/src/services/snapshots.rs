// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use uuid::Uuid;

use crate::{
    cubemaster::{
        CapabilitiesResponse, CreateSnapshotRequest, CubeMasterClient, CubeMasterError,
        DeleteSnapshotRequest, ListSnapshotsRequest, RollbackRequest as MasterRollbackRequest,
        SnapshotResource,
    },
    error::{AppError, AppResult},
    models::{
        DeleteSnapshotResponse as ApiDeleteSnapshotResponse, RollbackResponse, SnapshotInfo,
        SnapshotListItem, SnapshotMode,
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
        mode: SnapshotMode,
    ) -> AppResult<SnapshotInfo> {
        validate_snapshot_name(name.as_deref())?;
        if mode == SnapshotMode::FsOnly {
            ensure_filesystem_only_supported(&self.cubemaster).await?;
        }
        let request_id = new_request_id();
        let req = CreateSnapshotRequest {
            request_id: request_id.clone(),
            sandbox_id: sandbox_id.to_string(),
            display_name: name,
            backend,
            mode: Some(mode.as_str().to_string()),
        };

        match self.cubemaster.create_snapshot(&req).await {
            Ok(resp) => {
                resp.ret
                    .as_result()
                    .map_err(|e| map_snapshot_create_error(e, sandbox_id))?;
                let snapshot_id = resp.snapshot.snapshot_id.clone();
                if snapshot_id.trim().is_empty() {
                    return Err(AppError::Internal(anyhow::anyhow!(
                        "snapshot create response missing snapshot_id"
                    )));
                }
                ensure_snapshot_ready(&resp.snapshot)?;
                // The recorded mode must match the request; a stale record is
                // left in place to avoid racing the capture job.
                let recorded = SnapshotMode::from_stored(&resp.snapshot.mode);
                if recorded != mode {
                    return Err(AppError::Conflict(format!(
                        "snapshot_mode_conflict: requested {} but the control plane recorded {}",
                        mode, recorded
                    )));
                }
                Ok(snapshot_resource_to_info(resp.snapshot))
            }
            Err(e) => Err(map_snapshot_create_error(e, sandbox_id)),
        }
    }

    // ── GET /snapshots ─────────────────────────────────────────────────────

    pub async fn list(
        &self,
        sandbox_id: Option<&str>,
        limit: Option<i32>,
        next_token: Option<&str>,
        name: Option<&str>,
    ) -> AppResult<(Vec<SnapshotListItem>, String)> {
        let req = ListSnapshotsRequest {
            request_id: new_request_id(),
            instance_type: self.instance_type.clone(),
            sandbox_id: sandbox_id.map(str::to_string),
            name: normalize_next_token(name),
            status: None,
            // Apply the documented "default 100, max 100" contract here so an
            // oversized or non-positive `limit` never reaches CubeMaster.
            limit: Some(normalize_limit(limit)),
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

    /// IDs of the live snapshots whose name exactly matches `name`.
    pub async fn ids_by_name(&self, name: &str) -> AppResult<Vec<String>> {
        let req = ListSnapshotsRequest {
            request_id: new_request_id(),
            instance_type: self.instance_type.clone(),
            sandbox_id: None,
            name: Some(name.to_string()),
            status: None,
            limit: None,
            next_token: None,
        };
        let resp = self
            .cubemaster
            .list_snapshots(&req)
            .await
            .map_err(internal_error)?;
        resp.ret.as_result().map_err(internal_error)?;
        Ok(resp.items.into_iter().map(|s| s.snapshot_id).collect())
    }

    /// Resolve an id or name strictly within snapshots: a miss is a 404 and
    /// never falls through to templates.
    pub async fn resolve_only(&self, id_or_name: &str) -> AppResult<String> {
        let id_or_name = id_or_name.trim();
        if id_or_name.is_empty() {
            return Err(AppError::BadRequest(
                "snapshot id or name is required".to_string(),
            ));
        }
        if is_snapshot_identity(id_or_name) {
            return Ok(id_or_name.to_string());
        }
        let mut ids = self.ids_by_name(id_or_name).await?;
        match ids.len() {
            0 => Err(snapshot_not_found(id_or_name)),
            1 => Ok(ids.remove(0)),
            _ => Err(ambiguous_name_conflict(id_or_name)),
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
                resp.ret
                    .as_result()
                    .map_err(|e| map_rollback_error(e, sandbox_id, snapshot_id))?;
                let status =
                    ensure_operation_ready(resp.status(), "snapshot rollback", snapshot_id)?;

                Ok(RollbackResponse {
                    sandbox_id: owned_or_fallback(resp.sandbox_id, sandbox_id),
                    snapshot_id: owned_or_fallback(resp.snapshot_id, snapshot_id),
                    operation_id,
                    status,
                })
            }
            Err(e) => Err(map_rollback_error(e, sandbox_id, snapshot_id)),
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

/// Page size `GET /snapshots` falls back to when the caller omits `limit`.
///
/// This matches the "default 100" that `ListSnapshotsQuery` and `openapi.yml`
/// advertise. Note that it is a *change* for callers that omit `limit`: they
/// used to receive CubeMaster's own default of 20 items
/// (`normalizeListSnapshotsOptions` in
/// `CubeMaster/pkg/templatecenter/snapshot_view.go`), and now get 100.
/// `openapi.yml` is a manually exported artifact, so keeping the two in step
/// is a convention rather than something enforced here.
const SNAPSHOT_PAGE_LIMIT_DEFAULT: i32 = 100;

/// Largest page size `GET /snapshots` will ask CubeMaster for.
///
/// CubeMaster already clamps anything above 100 back down to 100, so this is
/// defence in depth rather than a bound that was previously missing.
/// Keep this aligned with `normalizeListSnapshotsOptions` in
/// `CubeMaster/pkg/templatecenter/snapshot_view.go`.
const SNAPSHOT_PAGE_LIMIT_MAX: i32 = 100;

/// Normalise the `limit` query parameter against the documented contract
/// ("default 100, max 100") before it reaches CubeMaster.
///
/// CubeMaster already enforces a contract of its own on this endpoint —
/// `normalizeListSnapshotsOptions` turns `Limit <= 0` into 20 and clamps
/// anything above 100 down to 100 — so this does not fix a missing bound. It
/// makes CubeAPI agree with what `ListSnapshotsQuery` / `openapi.yml`
/// advertise, at the cost of moving the default page size for callers that
/// omit `limit` from 20 to 100.
///
/// Non-positive values fall back to the default rather than being forwarded,
/// mirroring how `normalize_next_token` strips an empty cursor instead of
/// relaying it.
fn normalize_limit(limit: Option<i32>) -> i32 {
    match limit {
        Some(value) if value > 0 => value.min(SNAPSHOT_PAGE_LIMIT_MAX),
        _ => SNAPSHOT_PAGE_LIMIT_DEFAULT,
    }
}

fn map_snapshot_create_error(e: crate::cubemaster::CubeMasterError, sandbox_id: &str) -> AppError {
    if e.is_not_found() {
        return sandbox_not_found(sandbox_id);
    }
    if let Some(err) = stable_rejection(&e) {
        return err;
    }
    if e.is_conflict() {
        return snapshot_create_conflict(sandbox_id);
    }
    map_master_rejection(e)
}

fn map_rollback_error(
    e: crate::cubemaster::CubeMasterError,
    sandbox_id: &str,
    snapshot_id: &str,
) -> AppError {
    if e.is_not_found() {
        return sandbox_or_snapshot_not_found(sandbox_id, snapshot_id);
    }
    if let Some(err) = stable_rejection(&e) {
        return err;
    }
    if e.is_conflict() {
        return rollback_conflict(sandbox_id, snapshot_id);
    }
    map_master_rejection(e)
}

/// Maps a rejection whose message carries one of our stable identifiers; such
/// a message is authoritative and only identifiers we own become a 400.
fn stable_rejection(e: &crate::cubemaster::CubeMasterError) -> Option<AppError> {
    use crate::cubemaster::CubeMasterError;
    use crate::models::snapshot_mode::{is_client_error, stable_code};

    let CubeMasterError::Api { ret_code, ret_msg } = e else {
        return None;
    };
    let code = stable_code(ret_msg)?;
    match *ret_code {
        130400 if is_client_error(code) => Some(AppError::BadRequest(ret_msg.clone())),
        130409 => Some(AppError::Conflict(ret_msg.clone())),
        _ => None,
    }
}

/// Maps the Master codes whose meaning does not depend on the message text.
fn map_master_rejection(e: crate::cubemaster::CubeMasterError) -> AppError {
    use crate::cubemaster::CubeMasterError;
    match e {
        CubeMasterError::Api {
            ret_code: 130503,
            ret_msg,
        } => AppError::ServiceUnavailable {
            message: ret_msg,
            retry_after: 1,
        },
        other => internal_error(other),
    }
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

pub(crate) fn ambiguous_name_conflict(name: &str) -> AppError {
    AppError::Conflict(format!(
        "snapshot name {name:?} is ambiguous; delete by snap- id instead"
    ))
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
        mode: SnapshotMode::from_stored(&r.mode),
    }
}

/// Refuse a filesystem-only request the control plane cannot honour.
///
/// A Master that predates the modes contract drops the unknown request field,
/// so without this the caller would get `201 Created` and a full snapshot. A
/// transport failure is reported as 503 rather than 409: the request may well
/// be valid once the control plane answers again.
pub(crate) async fn ensure_filesystem_only_supported(
    cubemaster: &CubeMasterClient,
) -> AppResult<()> {
    filesystem_only_supported(cubemaster.get_capabilities().await)
}

/// The decision half of [`ensure_filesystem_only_supported`], split out so every
/// answer the control plane can give is testable without a live Master.
fn filesystem_only_supported(
    capabilities: Result<CapabilitiesResponse, CubeMasterError>,
) -> AppResult<()> {
    let capabilities = capabilities.map_err(|err| match err {
        // No capabilities route means no filesystem-only support; not retryable.
        CubeMasterError::Api { ret_code: 404, .. } => AppError::Conflict(
            "snapshot_filesystem_only_unsupported_node: the control plane does not \
             support filesystem-only snapshots"
                .to_string(),
        ),
        err => AppError::ServiceUnavailable {
            message: format!("cannot verify filesystem-only snapshot support: {err}"),
            retry_after: 2,
        },
    })?;
    let advertised = capabilities
        .snapshot_modes
        .iter()
        .any(|mode| mode == SnapshotMode::FsOnly.as_str());
    if !advertised {
        return Err(AppError::Conflict(
            "snapshot_filesystem_only_unsupported_node: the control plane does not \
             advertise the fs-only snapshot mode"
                .to_string(),
        ));
    }
    Ok(())
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
        mode: SnapshotMode::from_stored(&r.mode),
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

/// Reject names starting with `tpl-`/`snap-`: they would be read as IDs on delete.
fn validate_snapshot_name(name: Option<&str>) -> AppResult<()> {
    let name = name.map(str::trim).unwrap_or_default();
    if name.starts_with("tpl-") || name.starts_with("snap-") {
        return Err(AppError::BadRequest(format!(
            "snapshot name {name:?} must not start with 'tpl-' or 'snap-'"
        )));
    }
    Ok(())
}

pub(crate) fn is_snapshot_identity(id: &str) -> bool {
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
            mode: String::new(),
            remote_status: String::new(),
            created_at: None,
            updated_at: None,
        }
    }

    fn master_api(ret_code: i32, ret_msg: &str) -> crate::cubemaster::CubeMasterError {
        crate::cubemaster::CubeMasterError::Api {
            ret_code,
            ret_msg: ret_msg.into(),
        }
    }

    #[test]
    fn create_maps_a_client_identifier_to_bad_request() {
        let err = map_snapshot_create_error(
            master_api(
                130400,
                "rejected: snapshot_filesystem_only_rollback_unsupported: snap-1",
            ),
            "sb-1",
        );
        assert!(matches!(err, AppError::BadRequest(_)));
    }

    #[test]
    fn create_keeps_an_unclassified_params_error_a_server_error() {
        let err = map_snapshot_create_error(master_api(130400, "gorm: database is locked"), "sb-1");
        assert!(matches!(err, AppError::Internal(_)));
    }

    #[test]
    fn create_reports_an_identified_conflict_with_masters_message() {
        let err = map_snapshot_create_error(
            master_api(
                130409,
                "snapshot_filesystem_only_shared_volume_unsupported: data",
            ),
            "sb-1",
        );
        match err {
            AppError::Conflict(message) => {
                assert!(message.contains("shared_volume"), "{message}");
            }
            other => panic!("expected Conflict, got {other:?}"),
        }
    }

    #[test]
    fn create_keeps_the_canned_conflict_message_for_operations() {
        let err = map_snapshot_create_error(
            master_api(130409, "sandbox has an active operation"),
            "sb-1",
        );
        match err {
            AppError::Conflict(message) => assert!(message.contains("in progress"), "{message}"),
            other => panic!("expected Conflict, got {other:?}"),
        }
    }

    #[test]
    fn rollback_maps_a_filesystem_only_target_to_bad_request() {
        let err = map_rollback_error(
            master_api(
                130400,
                "snapshot_filesystem_only_rollback_unsupported: snap-1",
            ),
            "sb-1",
            "snap-1",
        );
        assert!(matches!(err, AppError::BadRequest(_)));
    }

    #[test]
    fn master_unavailable_maps_to_retryable_503() {
        let err = map_master_rejection(master_api(
            130503,
            "snapshot_filesystem_only_node_capability_unknown",
        ));
        assert!(matches!(
            err,
            AppError::ServiceUnavailable { retry_after: 1, .. }
        ));
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
    fn snapshot_name_rejects_reserved_id_prefixes() {
        for name in ["snap-x", "tpl-x", "  snap-x", "snap-", "tpl-"] {
            assert!(
                matches!(
                    validate_snapshot_name(Some(name)),
                    Err(AppError::BadRequest(_))
                ),
                "{name:?} should be rejected"
            );
        }
        for name in [
            None,
            Some(""),
            Some("  "),
            Some("prod"),
            Some("Snap-x"),
            Some("e2e-snap-1"),
        ] {
            assert!(validate_snapshot_name(name).is_ok(), "{name:?} should pass");
        }
    }

    #[tokio::test]
    async fn create_rejects_reserved_prefix_without_calling_cubemaster() {
        // The fake has no routes, so any request would fail with Internal, not BadRequest.
        let svc = service_for(spawn_fake_cubemaster(axum::Router::new()).await);
        assert!(matches!(
            svc.create("sb-1", Some("snap-x".into()), None, SnapshotMode::Full)
                .await,
            Err(AppError::BadRequest(_))
        ));
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

    #[test]
    fn normalize_limit_applies_default_and_caps_the_maximum() {
        // `ListSnapshotsQuery` documents "default 100, max 100". CubeMaster
        // already enforces its own contract (20 default, 100 cap), so this
        // pins what CubeAPI sends rather than fixing a missing bound.
        assert_eq!(normalize_limit(None), 100);
        assert_eq!(normalize_limit(Some(100)), 100);
        assert_eq!(normalize_limit(Some(1)), 1);
        assert_eq!(normalize_limit(Some(101)), 100);
        assert_eq!(normalize_limit(Some(i32::MAX)), 100);
        assert_eq!(normalize_limit(Some(0)), 100);
        assert_eq!(normalize_limit(Some(-5)), 100);
    }

    async fn spawn_fake_cubemaster(handler: axum::Router) -> String {
        crate::services::test_support::serve(handler).await
    }

    fn service_for(base: String) -> SnapshotService {
        crate::services::test_support::snapshot_service(base)
    }

    fn list_response(items: serde_json::Value) -> serde_json::Value {
        serde_json::json!({
            "RequestID": "req-1",
            "ret": { "ret_code": 0, "ret_msg": "success" },
            "data": items,
            "next_token": ""
        })
    }

    fn snapshot_item(id: &str) -> serde_json::Value {
        serde_json::json!({
            "snapshot_id": id,
            "display_name": "prod",
            "status": "READY",
        })
    }

    #[tokio::test]
    async fn ids_by_name_returns_single_match() {
        use axum::{extract::Query, routing::get, Json, Router};
        use std::collections::HashMap;

        async fn list_handler(
            Query(params): Query<HashMap<String, String>>,
        ) -> Json<serde_json::Value> {
            assert_eq!(params.get("name").map(String::as_str), Some("prod"));
            Json(list_response(serde_json::json!([snapshot_item("snap-x")])))
        }

        let base =
            spawn_fake_cubemaster(Router::new().route("/cube/snapshot", get(list_handler))).await;
        let svc = service_for(base);
        assert_eq!(
            svc.ids_by_name("prod").await.unwrap(),
            vec!["snap-x".to_string()]
        );
    }

    #[tokio::test]
    async fn ids_by_name_no_match_is_empty() {
        use axum::{routing::get, Json, Router};

        async fn list_handler() -> Json<serde_json::Value> {
            Json(list_response(serde_json::json!([])))
        }

        let base =
            spawn_fake_cubemaster(Router::new().route("/cube/snapshot", get(list_handler))).await;
        let svc = service_for(base);
        assert!(svc.ids_by_name("missing").await.unwrap().is_empty());
    }

    #[tokio::test]
    async fn ids_by_name_returns_all_matches() {
        use axum::{routing::get, Json, Router};

        async fn list_handler() -> Json<serde_json::Value> {
            Json(list_response(serde_json::json!([
                snapshot_item("snap-a"),
                snapshot_item("snap-b"),
            ])))
        }

        let base =
            spawn_fake_cubemaster(Router::new().route("/cube/snapshot", get(list_handler))).await;
        let svc = service_for(base);
        assert_eq!(
            svc.ids_by_name("prod").await.unwrap(),
            vec!["snap-a".to_string(), "snap-b".to_string()]
        );
    }

    #[tokio::test]
    async fn resolve_only_snap_id_and_empty_make_no_request() {
        let svc = service_for(spawn_fake_cubemaster(axum::Router::new()).await);
        assert_eq!(svc.resolve_only("  snap-abc ").await.unwrap(), "snap-abc");
        assert!(matches!(
            svc.resolve_only("  ").await,
            Err(AppError::BadRequest(_))
        ));
    }

    #[tokio::test]
    async fn resolve_only_name_miss_is_not_found_even_for_tpl_prefix() {
        use axum::{routing::get, Json, Router};

        async fn list_handler() -> Json<serde_json::Value> {
            Json(list_response(serde_json::json!([])))
        }

        let base =
            spawn_fake_cubemaster(Router::new().route("/cube/snapshot", get(list_handler))).await;
        let svc = service_for(base);
        for name in ["prod", "tpl-abc"] {
            assert!(matches!(
                svc.resolve_only(name).await,
                Err(AppError::NotFound(_))
            ));
        }
    }

    #[tokio::test]
    async fn resolve_only_name_returns_single_id_and_rejects_duplicates() {
        use axum::{extract::Query, routing::get, Json, Router};
        use std::collections::HashMap;

        async fn list_handler(
            Query(params): Query<HashMap<String, String>>,
        ) -> Json<serde_json::Value> {
            let items = match params.get("name").map(String::as_str) {
                Some("one") => serde_json::json!([snapshot_item("snap-x")]),
                _ => serde_json::json!([snapshot_item("snap-a"), snapshot_item("snap-b")]),
            };
            Json(list_response(items))
        }

        let base =
            spawn_fake_cubemaster(Router::new().route("/cube/snapshot", get(list_handler))).await;
        let svc = service_for(base);
        assert_eq!(svc.resolve_only("one").await.unwrap(), "snap-x");
        match svc.resolve_only("many").await {
            Err(AppError::Conflict(msg)) => assert!(msg.contains("ambiguous"), "{msg}"),
            other => panic!("expected Conflict, got {other:?}"),
        }
    }
}

#[cfg(test)]
mod filesystem_only_gate_tests {
    use super::*;

    fn capabilities(modes: &[&str]) -> CapabilitiesResponse {
        CapabilitiesResponse {
            snapshot_modes: modes.iter().map(|mode| mode.to_string()).collect(),
        }
    }

    /// Fake Master that answers capabilities as enabled and records
    /// `recorded_mode` on the created snapshot.
    async fn create_against_master_reporting(recorded_mode: &str) -> AppResult<SnapshotInfo> {
        use axum::{
            routing::{get, post},
            Json, Router,
        };

        let mode = recorded_mode.to_string();
        let app = Router::new()
            .route(
                "/cube/capabilities",
                get(|| async {
                    Json(serde_json::json!({
                        "RequestID": "req-1",
                        "ret": { "ret_code": 0, "ret_msg": "success" },
                        "snapshot_modes": ["full", "fs-only"]
                    }))
                }),
            )
            .route(
                "/cube/snapshot",
                post(move || {
                    let mode = mode.clone();
                    async move {
                        Json(serde_json::json!({
                            "RequestID": "req-1",
                            "ret": { "ret_code": 0, "ret_msg": "success" },
                            "snapshot": {
                                "snapshot_id": "snap-1",
                                "display_name": "snap-name",
                                "status": "READY",
                                "mode": mode
                            }
                        }))
                    }
                }),
            );

        let service = crate::services::test_support::snapshot_service(
            crate::services::test_support::serve(app).await,
        );
        service
            .create("sb-1", None, None, SnapshotMode::FsOnly)
            .await
    }

    #[tokio::test]
    async fn a_control_plane_that_ignores_the_mode_is_a_conflict() {
        let err = create_against_master_reporting("full")
            .await
            .expect_err("a full artifact for an fs-only request must be refused");
        match err {
            AppError::Conflict(message) => {
                assert!(message.contains("snapshot_mode_conflict"), "{message}")
            }
            other => panic!("expected Conflict, got {other:?}"),
        }
    }

    #[tokio::test]
    async fn a_matching_mode_is_accepted() {
        let info = create_against_master_reporting("fs-only")
            .await
            .expect("a matching artifact must be accepted");
        assert_eq!(info.mode, SnapshotMode::FsOnly);
    }

    #[test]
    fn a_control_plane_that_does_not_advertise_the_mode_is_terminal() {
        let err = filesystem_only_supported(Ok(capabilities(&["full"]))).unwrap_err();
        match err {
            AppError::Conflict(message) => assert!(
                message.contains("snapshot_filesystem_only_unsupported_node"),
                "{message}"
            ),
            other => panic!("expected Conflict, got {other:?}"),
        }
    }

    #[test]
    fn an_advertising_control_plane_passes() {
        assert!(filesystem_only_supported(Ok(capabilities(&["full", "fs-only"]))).is_ok());
    }

    #[test]
    fn a_master_without_the_route_is_terminal() {
        let err = filesystem_only_supported(Err(CubeMasterError::Api {
            ret_code: 404,
            ret_msg: "404 page not found".into(),
        }))
        .unwrap_err();
        match err {
            AppError::Conflict(message) => assert!(
                message.contains("snapshot_filesystem_only_unsupported_node"),
                "{message}"
            ),
            other => panic!("expected Conflict, got {other:?}"),
        }
    }

    #[test]
    fn a_transport_failure_is_retryable() {
        let err =
            filesystem_only_supported(Err(CubeMasterError::Deserialize("connection reset".into())))
                .unwrap_err();
        assert!(
            matches!(err, AppError::ServiceUnavailable { .. }),
            "{err:?}"
        );
    }
}
