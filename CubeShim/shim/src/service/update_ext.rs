// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use crate::log::Log;
use crate::{common::CResult, errf, infof, sandbox::sb::SandBox};
use cube_hypervisor::config::RestoreConfig;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::path::PathBuf;

// ── annotation keys ──────────────────────────────────────────────────────────

/// Identifies the update action to perform.
///
/// Supported values: `"RollbackSnapshot"`
const ANNO_UPDATE_EXT_ACTION: &str = "cube.shimapi.update.action";

/// (RollbackSnapshot) **Required.** JSON-encoded `RollbackRestoreConfig`,
/// aligned with hypervisor `RestoreConfig`.
///
/// Required fields:
///   `source_url`  — URL of the snapshot to restore from (e.g. `file:///data/snapshots/foo`)
///
/// Optional fields (replace backend devices after restore):
///   `disks`, `net`, `fs`, `vsock`, `pmem`, `prefault`, `dirty_log`
const ANNO_ROLLBACK_RESTORE_CONFIG: &str = "cube.shimapi.update.rollback.restore_config";

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

// ── action implementations ────────────────────────────────────────────────────

/// Roll back the running VM to a previously-taken snapshot.
///
/// Steps:
/// 1. Pause the current VM and snapshot its state to a temporary path on disk.
/// 2. Resume the VM from the snapshot specified in `restore_config.source_url`,
///    optionally replacing backend devices via the other fields.
async fn do_rollback_snapshot(
    sb: &mut SandBox,
    annos: &HashMap<String, String>,
    log: &Log,
) -> CResult<()> {
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
    Ok(())
}

// ── public router ─────────────────────────────────────────────────────────────

pub async fn update_route(
    sb: &mut SandBox,
    annos: &HashMap<String, String>,
    log: &Log,
) -> CResult<()> {
    let action = match annos.get(ANNO_UPDATE_EXT_ACTION) {
        Some(a) => a.as_str(),
        None => return Ok(()), // no extended action requested
    };

    match action {
        "RollbackSnapshot" => do_rollback_snapshot(sb, annos, log).await,
        unknown => Err(format!("unknown update ext action: {}", unknown).into()),
    }
}
