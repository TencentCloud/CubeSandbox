// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
// cubecow library crate
//
// Provides programmatic access to the cubecow xfs-reflink storage
// engine.
//
// The public entry points are:
//
// * [`Engine`] — backend-agnostic trait describing the operations every
//   storage backend supports.
// * [`ReflinkEngine`] — the xfs-reflink backend (currently the only
//   shipping backend). Implements [`Engine`].
// * [`initialize`] / [`initialize_without_logging`] — backend-selecting
//   factory functions that read [`config::AppConfig::backend`] and
//   return the appropriate `Box<dyn Engine>`. New code should prefer
//   these.

pub mod config;
mod engine;
pub mod ffi;
mod pkg;

// Re-export types that external consumers need
pub use crate::pkg::errors::{CubecowError, CubecowResult};

// Engine trait + concrete backends.
pub use crate::engine::reflink::ReflinkEngine;
pub use crate::engine::s3::S3Engine;
pub use crate::engine::Engine;

// ---------------------------------------------------------------------------
// Public types — clean API surface for lib consumers
// ---------------------------------------------------------------------------

/// Volume information returned by the library API.
///
/// This is a clean, public-facing type that exposes only the information
/// external consumers need.
///
/// The last three fields (`export_uuid`, `export_status`, `deletable`)
/// exist so [`Engine::get_volume_info`] can present a **uniform view**
/// regardless of whether the queried name is a writable volume or a
/// snapshot. For a pure volume they are always the zero value
/// (`""` / `""` / `None`); they only carry meaningful data when the
/// projected entry is actually a snapshot that has been `export_snapshot`-ed.
///
/// This is required because `get_volume_info` returns a `Volume` for
/// both `NameKind::Volume` and `NameKind::Snapshot` entries — snapshot
/// upload-status must therefore surface on the same struct.
#[derive(Debug, Clone)]
pub struct Volume {
    /// User-specified volume name.
    pub name: String,
    /// Logical size in bytes.
    pub size_bytes: u64,
    /// Backend device path used for block IO. For the xfs-reflink
    /// backend this is the regular file path inside the
    /// reflink-enabled mount.
    pub device_path: String,
    /// Number of snapshots derived from this volume.
    pub snapshot_count: i32,
    /// Creation timestamp (RFC3339).
    pub created_at: String,
    /// Cross-node export uuid produced by
    /// [`Engine::export_snapshot`]. Empty for volumes and for
    /// snapshots that have never been exported.
    pub export_uuid: String,
    /// Upload status reported by the backend when the entry has an
    /// `export_uuid`. Currently one of `"NONE"`, `"INPROGRESS"`,
    /// `"DONE"`; empty when not applicable. See the S3 backend
    /// design doc §2.4 for the underlying `rcow_get_snapshot_status`
    /// RPC contract.
    pub export_status: String,
    /// Whether the backend currently considers this exported
    /// snapshot safe to delete. `None` when not applicable (regular
    /// volume, or snapshot that has not been exported).
    pub deletable: Option<bool>,
}

/// Snapshot information returned by the library API.
#[derive(Debug, Clone)]
pub struct Snapshot {
    /// Snapshot name.
    ///
    /// This is the **canonical identifier** for a snapshot across the
    /// entire engine API surface. Every subsequent operation that needs
    /// to refer to this snapshot — later activation via
    /// [`Engine::activate_volume`], deletion via
    /// [`Engine::delete_snapshot`], snapshot-of-snapshot creation by
    /// passing this name as `source_name` to
    /// [`Engine::create_snapshot`], or enumeration via
    /// [`Engine::list_snapshots`] — addresses the snapshot by this
    /// `name`, regardless of whether it currently has a backend device
    /// node.
    pub name: String,
    /// Logical size in bytes.
    pub size_bytes: u64,
    /// Backend device path used for block I/O.
    ///
    /// **Empty string when the snapshot is not currently activated**
    /// (i.e. it was created with `activate = false` and has not yet been
    /// passed to [`Engine::activate_volume`], or it was explicitly
    /// deactivated via [`Engine::deactivate_volume`]). An empty
    /// `device_path` does **not** mean the snapshot is unusable — it can
    /// still be referenced by [`Self::name`] for all metadata-only
    /// operations, including acting as the source for further
    /// snapshots. Only direct block I/O requires the device node to be
    /// materialised.
    pub device_path: String,
    /// Name of the origin volume this snapshot was created from.
    pub origin_volume: String,
    /// Creation timestamp (RFC3339).
    pub created_at: String,
    /// Cross-node export uuid produced by
    /// [`Engine::export_snapshot`].
    ///
    /// Empty when this snapshot has never been exported. The S3
    /// backend persists this value in its on-disk index so that a
    /// subsequent [`Engine::get_volume_info`] can decide whether to
    /// query the underlying upload progress via the
    /// `rcow_get_snapshot_status` RPC.
    pub export_uuid: String,
    /// Upload status of this snapshot to the remote object store.
    ///
    /// Populated only for snapshots whose [`Self::export_uuid`] is
    /// non-empty. One of `"NONE"`, `"INPROGRESS"`, `"DONE"` for the S3
    /// backend; empty for backends that do not model cross-node
    /// export (e.g. reflink).
    pub export_status: String,
    /// Whether the backend currently considers this exported
    /// snapshot safe to delete. `None` when the snapshot has not been
    /// exported or the backend does not report this signal.
    pub deletable: Option<bool>,
}

/// Block-level info for a volume.
#[derive(Debug, Clone, Copy)]
pub struct VolumeBlockInfo {
    /// Number of logical blocks composing the volume.
    pub num_blocks: u64,
    /// Size of one block in bytes.
    pub block_size: u32,
}

// ---------------------------------------------------------------------------
// Backend-selecting factory functions
// ---------------------------------------------------------------------------

use crate::config::{AppConfig, BackendKind};

/// Construct an engine according to `config.backend.kind`.
///
/// This is the **standard entry point** for new code: it returns a
/// backend-agnostic `Box<dyn Engine>` so callers do not need to know
/// which concrete backend is in use.
pub fn initialize(config: AppConfig) -> anyhow::Result<Box<dyn Engine>> {
    match config.backend.kind {
        BackendKind::Reflink => Ok(Box::new(ReflinkEngine::initialize(config)?)),
        BackendKind::S3 => Ok(Box::new(S3Engine::initialize(config)?)),
    }
}

/// Same as [`initialize`] but skips logging setup; use when the host
/// application manages its own tracing subscriber.
pub fn initialize_without_logging(config: AppConfig) -> anyhow::Result<Box<dyn Engine>> {
    match config.backend.kind {
        BackendKind::Reflink => Ok(Box::new(ReflinkEngine::initialize_without_logging(config)?)),
        BackendKind::S3 => Ok(Box::new(S3Engine::initialize_without_logging(config)?)),
    }
}
