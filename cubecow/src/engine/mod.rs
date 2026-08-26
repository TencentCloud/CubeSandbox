// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
// Backend-agnostic engine abstraction.
//
// This module hosts:
//
// * [`Engine`] — the trait that every snapshot backend must implement.
// * [`reflink::ReflinkEngine`] — the xfs-reflink backend; the only
//   backend currently shipping with cubecow.
//
// New backends live as siblings of `reflink` and implement the same
// trait. Selection between backends happens at construction time via
// [`crate::initialize`] based on [`crate::config::BackendKind`].

pub mod reflink;
pub mod s3;

use std::collections::HashMap;

use crate::pkg::errors::{CubecowError, CubecowResult};
use crate::{Snapshot, Volume, VolumeBlockInfo};

/// Backend-agnostic interface for the cubecow storage engine.
///
/// All public consumers should program against `dyn Engine`; the
/// concrete backend is selected at construction time via
/// [`crate::initialize`] based on `AppConfig.backend.kind`.
///
/// The interface is intentionally pool-free: the reflink backend (the
/// only one that ships) treats its `[backend.reflink].root_dir` as a
/// single flat namespace and has no notion of LVM PV/VG/thin-pool
/// partitioning. Any future backend that needs sub-namespaces is
/// expected to model them with explicit prefixes in the volume name
/// rather than re-introducing a pool concept here.
pub trait Engine: Send + Sync {
    // -----------------------------------------------------------------------
    // Volume operations
    // -----------------------------------------------------------------------

    /// Create a new volume with the given name and size in bytes.
    fn create_volume(&self, name: &str, size_bytes: u64) -> CubecowResult<Volume>;

    /// Delete a volume by name.
    fn delete_volume(&self, name: &str) -> CubecowResult<()>;

    /// Resize a volume (expand only). Returns (old_size, new_size).
    fn resize_volume(&self, name: &str, new_size_bytes: u64) -> CubecowResult<(u64, u64)>;

    /// Get volume information by name.
    fn get_volume_info(&self, name: &str) -> CubecowResult<Volume>;

    /// Get block-level info (num_blocks, block_size) for a volume.
    fn get_volume_block_info(&self, name: &str) -> CubecowResult<VolumeBlockInfo>;

    /// List volumes with pagination.
    /// Returns (volumes, next_page_token, total_count).
    fn list_volumes(
        &self,
        page_size: usize,
        page_token: Option<&str>,
    ) -> (Vec<Volume>, Option<String>, usize);

    // -----------------------------------------------------------------------
    // Snapshot operations
    // -----------------------------------------------------------------------

    /// Create a snapshot from a volume or another snapshot. The `activate`
    /// flag controls whether a backing block-device handle is materialised
    /// alongside the snapshot metadata.
    fn create_snapshot_from_volume(
        &self,
        source_name: &str,
        snapshot_name: &str,
        activate: bool,
    ) -> CubecowResult<Snapshot>;

    /// Delete a snapshot by name.
    fn delete_snapshot(&self, snapshot_name: &str) -> CubecowResult<()>;

    /// Derive a writable, data-independent volume from an existing snapshot.
    /// The returned volume is auto-activated, matching the
    /// "volume ⇄ device lifetime" contract of [`Engine::create_volume`].
    fn create_volume_from_snapshot(
        &self,
        source_snapshot: &str,
        volume_name: &str,
    ) -> CubecowResult<Volume>;

    /// List snapshots of a volume with pagination.
    /// Returns (snapshots, next_page_token).
    fn list_snapshots(
        &self,
        volume_name: &str,
        page_size: usize,
        page_token: Option<&str>,
    ) -> (Vec<Snapshot>, Option<String>);

    // -----------------------------------------------------------------------
    // Activation
    // -----------------------------------------------------------------------

    /// Activate a volume or snapshot by name. Idempotent.
    fn activate_volume(&self, name: &str) -> CubecowResult<Volume>;

    /// Deactivate a volume or snapshot by name. Idempotent.
    fn deactivate_volume(&self, name: &str) -> CubecowResult<()>;

    // -----------------------------------------------------------------------
    // Cross-node export / import (optional; backend-specific)
    // -----------------------------------------------------------------------

    /// Export a snapshot for cross-node recovery. Returns an opaque `export_uuid`.
    fn export_snapshot(&self, _snapshot_name: &str) -> CubecowResult<String> {
        Err(CubecowError::PreconditionFailed(
            "export_snapshot is not implemented by this backend".to_string(),
        ))
    }

    /// Import a writable volume from an `export_uuid` produced by a remote node.
    fn import_lvol(&self, _lvol_name: &str, _export_uuid: &str) -> CubecowResult<Volume> {
        Err(CubecowError::PreconditionFailed(
            "import_lvol is not implemented by this backend".to_string(),
        ))
    }

    // -----------------------------------------------------------------------
    // Node operations
    // -----------------------------------------------------------------------

    /// Destructively reset all storage managed by this engine on the local
    /// node. Used by host re-initialisation flows.
    fn reset_node_storage(&self) -> CubecowResult<()>;

    // -----------------------------------------------------------------------
    // Observability
    // -----------------------------------------------------------------------

    /// Get all metrics as key-value pairs.
    fn metrics(&self) -> HashMap<String, u64>;
}
