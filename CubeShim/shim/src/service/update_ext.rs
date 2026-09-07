// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use crate::log::Log;
use crate::{common::CResult, errf, infof, sandbox::sb::SandBox};
use cube_hypervisor::config::RestoreConfig;
use cube_hypervisor::SnapshotType;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::path::{Path, PathBuf};

// ── annotation keys ──────────────────────────────────────────────────────────

/// Identifies the update action to perform.
///
/// Supported values: `"RollbackSnapshot"`, `"PauseToSnapshot"`
const ANNO_UPDATE_EXT_ACTION: &str = "cube.shimapi.update.action";

/// (RollbackSnapshot) **Required.** JSON-encoded `RollbackRestoreConfig`,
/// aligned with hypervisor `RestoreConfig`.
///
/// Required fields:
///   `source_url`  — URL of the snapshot to restore from (e.g. `file:///data/snapshots/foo`)
///
/// Optional fields (replace backend devices after restore):
///   `disks`, `net`, `fs`, `vsock`, `pmem`, `prefault`, `dirty_log`, `memory_vol_url`
const ANNO_ROLLBACK_RESTORE_CONFIG: &str = "cube.shimapi.update.rollback.restore_config";

/// (PauseToSnapshot) **Required.** JSON-encoded `PauseSnapshotConfig`.
/// Cubelet prepares the CubeCow destination / memory volume and passes them
/// here so pause shares the CommitSandbox catalog layout (no hardcoded
/// `/data/cubelet/root/pausevm/<id>` bypass).
const ANNO_PAUSE_SNAPSHOT_CONFIG: &str = "cube.shimapi.update.pause.snapshot_config";

// ── restore config aligned with hypervisor RestoreConfig ─────────────────────

/// Wire format for RollbackSnapshot, directly mirrors `RestoreConfig` in
/// hypervisor/vmm/src/config.rs so callers work with a single familiar struct.
#[derive(Debug, Serialize, Deserialize)]
struct RollbackRestoreConfig {
    /// URL of the snapshot to restore from (e.g. `file:///data/snapshots/foo`).
    pub source_url: String,

    /// Replace block devices after restore.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub disks: Option<Vec<cube_hypervisor::vm_config::DiskConfig>>,

    /// Replace network interfaces after restore.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub net: Option<Vec<cube_hypervisor::vm_config::NetConfig>>,

    /// Replace virtio-fs mounts after restore.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub fs: Option<Vec<cube_hypervisor::vm_config::FsConfig>>,

    /// Replace vsock device after restore.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub vsock: Option<cube_hypervisor::vm_config::VsockConfig>,

    /// Replace pmem devices after restore.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub pmem: Option<Vec<cube_hypervisor::vm_config::PmemConfig>>,

    /// Prefault memory pages on restore (default: false).
    #[serde(default)]
    pub prefault: bool,

    /// Enable dirty log after restore (default: false).
    #[serde(default)]
    pub dirty_log: bool,

    /// Optional URL for reading memory range data from a separate volume.
    /// Mirrors RestoreConfig.memory_vol_url: when set, memory data is read
    /// from this path instead of source_url/<memory-ranges>.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub memory_vol_url: Option<String>,
}

impl From<RollbackRestoreConfig> for RestoreConfig {
    fn from(r: RollbackRestoreConfig) -> Self {
        RestoreConfig {
            source_url: PathBuf::from(r.source_url),
            disks: r.disks,
            net: r.net,
            fs: r.fs,
            vsock: r.vsock,
            pmem: r.pmem,
            prefault: r.prefault,
            dirty_log: r.dirty_log,
            memory_vol_url: r.memory_vol_url,
            ivshmem: None,
        }
    }
}

/// Wire format for PauseToSnapshot. Cubelet owns path selection (catalog /
/// CubeCow); the shim only writes the MicroVM snapshot and then exits.
#[derive(Debug, Serialize, Deserialize)]
struct PauseSnapshotConfig {
    /// Host directory (or `file://` URL) for config.json / state.json.
    pub destination_url: String,

    /// Optional CubeCow / device URL for memory ranges. When set, matches the
    /// CommitSandbox layout (`--memory-vol`).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub memory_vol_url: Option<String>,

    /// Same values as cube-runtime `--snapshot-type`: `full`, `incremental`,
    /// `soft-dirty`. Missing / empty / unknown defaults to Full so older
    /// Cubelets keep the historical full dump.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub snapshot_type: Option<String>,
}

fn parse_pause_snapshot_type(raw: Option<&str>) -> SnapshotType {
    let s = raw.map(str::trim).unwrap_or("");
    if s.is_empty() {
        return SnapshotType::Full;
    }
    s.parse().unwrap_or(SnapshotType::Full)
}

/// Outcome of an extended update action.
#[derive(Debug, Default, Clone, Copy)]
pub struct UpdateOutcome {
    /// When true, the shim process should exit after the Update RPC returns
    /// successfully. PauseToSnapshot leaves this false: Cubelet reaps the shim
    /// via task Delete (keep_tombstone) after a clean Update OK, avoiding a
    /// race where self-exit closes ttrpc before the client sees success.
    pub exit_shim: bool,
}

// ── action implementations ────────────────────────────────────────────────────

/// Roll back the running VM to a previously-taken snapshot.
///
/// Steps:
/// 1. Mark the sandbox paused (a logical state transition, not a VMM-level pause) and
///    disconnect the guest agent, then delete the current VM with `VmDelete`.
/// 2. Restore the target snapshot specified in `restore_config.source_url`, optionally
///    replacing backend devices via the other fields. Restore failure is terminal; the
///    previous VM cannot be restored because no temporary checkpoint is taken.
async fn do_rollback_snapshot(
    sb: &mut SandBox,
    annos: &HashMap<String, String>,
    log: &Log,
) -> CResult<UpdateOutcome> {
    // --- parse restore_config (required) ---
    let raw = annos
        .get(ANNO_ROLLBACK_RESTORE_CONFIG)
        .ok_or_else(|| format!("missing annotation: {}", ANNO_ROLLBACK_RESTORE_CONFIG))?;

    let rollback_cfg: RollbackRestoreConfig = serde_json::from_str(raw)
        .map_err(|e| format!("invalid {}: {}", ANNO_ROLLBACK_RESTORE_CONFIG, e))?;

    infof!(
        log,
        "rollback snapshot: target source_url={}",
        rollback_cfg.source_url
    );

    let restore_config: RestoreConfig = rollback_cfg.into();

    // --- delegate to sb ---
    sb.rollback_vm(restore_config).await.map_err(|e| {
        errf!(log, "rollback snapshot failed: {}", e);
        e
    })?;

    infof!(log, "rollback snapshot: finished");
    Ok(UpdateOutcome { exit_shim: false })
}

/// Pause the MicroVM into a Cubelet-provided snapshot destination, then ask
/// the task service to exit the shim process.
///
/// Unlike legacy `task.Pause` (hardcoded `/data/cubelet/root/pausevm/<id>`,
/// shim stays alive for in-place Resume), this path:
/// 1. Writes config/state under `destination_url` and optional memory into
///    `memory_vol_url` (CubeCow).
/// 2. Deletes the MicroVM (`pause2snapshot`).
/// 3. Signals shim exit so Resume recreates the sandbox from catalog with the
///    same sandboxID (Cubelet / CubeMaster).
async fn do_pause_to_snapshot(
    sb: &mut SandBox,
    annos: &HashMap<String, String>,
    log: &Log,
) -> CResult<UpdateOutcome> {
    let raw = annos
        .get(ANNO_PAUSE_SNAPSHOT_CONFIG)
        .ok_or_else(|| format!("missing annotation: {}", ANNO_PAUSE_SNAPSHOT_CONFIG))?;

    let pause_cfg: PauseSnapshotConfig = serde_json::from_str(raw)
        .map_err(|e| format!("invalid {}: {}", ANNO_PAUSE_SNAPSHOT_CONFIG, e))?;

    if pause_cfg.destination_url.trim().is_empty() {
        return Err(format!(
            "{}: destination_url is required",
            ANNO_PAUSE_SNAPSHOT_CONFIG
        )
        .into());
    }

    let destination_path = strip_file_url(&pause_cfg.destination_url);
    if destination_path.is_empty() {
        return Err(format!(
            "{}: destination_url is empty after normalization",
            ANNO_PAUSE_SNAPSHOT_CONFIG
        )
        .into());
    }
    // Refuse relative paths — Cubelet must pass an absolute host path.
    if !Path::new(&destination_path).is_absolute() {
        return Err(format!(
            "{}: destination_url must be an absolute path (got {})",
            ANNO_PAUSE_SNAPSHOT_CONFIG, destination_path
        )
        .into());
    }

    let snapshot_type = parse_pause_snapshot_type(pause_cfg.snapshot_type.as_deref());
    infof!(
        log,
        "pause to snapshot: destination={} memory_vol_url={:?} snapshot_type={}",
        destination_path,
        pause_cfg.memory_vol_url,
        snapshot_type
    );

    sb.pause_vm_to_snapshot(&destination_path, pause_cfg.memory_vol_url, snapshot_type)
        .await
        .map_err(|e| {
            errf!(log, "pause to snapshot failed: {}", e);
            e
        })?;

    infof!(
        log,
        "pause to snapshot: finished; wait for Cubelet Delete to reap shim"
    );
    Ok(UpdateOutcome { exit_shim: false })
}

fn strip_file_url(url: &str) -> String {
    let trimmed = url.trim();
    if let Some(rest) = trimmed.strip_prefix("file://") {
        rest.to_string()
    } else {
        trimmed.to_string()
    }
}

// ── public router ─────────────────────────────────────────────────────────────

pub async fn update_route(
    sb: &mut SandBox,
    annos: &HashMap<String, String>,
    log: &Log,
) -> CResult<UpdateOutcome> {
    let action = match annos.get(ANNO_UPDATE_EXT_ACTION) {
        Some(a) => a.as_str(),
        None => return Ok(UpdateOutcome::default()), // no extended action requested
    };

    match action {
        "RollbackSnapshot" => do_rollback_snapshot(sb, annos, log).await,
        "PauseToSnapshot" => do_pause_to_snapshot(sb, annos, log).await,
        unknown => Err(format!("unknown update ext action: {}", unknown).into()),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn strip_file_url_accepts_plain_and_file_scheme() {
        assert_eq!(strip_file_url("file:///data/snap"), "/data/snap");
        assert_eq!(strip_file_url("/data/snap"), "/data/snap");
        assert_eq!(strip_file_url("  /data/snap  "), "/data/snap");
    }

    #[test]
    fn pause_snapshot_config_roundtrip() {
        let raw = r#"{"destination_url":"/data/snap/pause-1","memory_vol_url":"file:///dev/cubecow/mem1"}"#;
        let cfg: PauseSnapshotConfig = serde_json::from_str(raw).unwrap();
        assert_eq!(cfg.destination_url, "/data/snap/pause-1");
        assert_eq!(
            cfg.memory_vol_url.as_deref(),
            Some("file:///dev/cubecow/mem1")
        );
        assert_eq!(cfg.snapshot_type, None);
        assert_eq!(
            parse_pause_snapshot_type(cfg.snapshot_type.as_deref()),
            SnapshotType::Full
        );
    }

    #[test]
    fn pause_snapshot_config_parses_soft_dirty() {
        let raw = r#"{"destination_url":"/data/snap/pause-1","memory_vol_url":"file:///dev/cubecow/mem1","snapshot_type":"soft-dirty"}"#;
        let cfg: PauseSnapshotConfig = serde_json::from_str(raw).unwrap();
        assert_eq!(cfg.snapshot_type.as_deref(), Some("soft-dirty"));
        assert_eq!(
            parse_pause_snapshot_type(cfg.snapshot_type.as_deref()),
            SnapshotType::SoftDirty
        );
        assert_eq!(
            parse_pause_snapshot_type(Some("incremental")),
            SnapshotType::Incremental
        );
        assert_eq!(parse_pause_snapshot_type(Some("weird")), SnapshotType::Full);
        assert_eq!(parse_pause_snapshot_type(Some("")), SnapshotType::Full);
        assert_eq!(parse_pause_snapshot_type(None), SnapshotType::Full);
    }
}
