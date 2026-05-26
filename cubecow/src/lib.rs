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
pub use crate::engine::Engine;

// ---------------------------------------------------------------------------
// Public types — clean API surface for lib consumers
// ---------------------------------------------------------------------------

/// Volume information returned by the library API.
///
/// This is a clean, public-facing type that exposes only the information
/// external consumers need.
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
    }
}

/// Same as [`initialize`] but skips logging setup; use when the host
/// application manages its own tracing subscriber.
pub fn initialize_without_logging(config: AppConfig) -> anyhow::Result<Box<dyn Engine>> {
    match config.backend.kind {
        BackendKind::Reflink => Ok(Box::new(ReflinkEngine::initialize_without_logging(config)?)),
    }
}
