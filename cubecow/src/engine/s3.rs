// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
// S3 (S3LVOL / RCOW) backed implementation of [`crate::engine::Engine`].

use std::collections::HashMap;
use std::io::{ErrorKind, Read, Write};
use std::os::unix::net::UnixStream;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex, RwLock};
use std::time::Duration;

use chrono::Utc;
use serde::{Deserialize, Serialize};
use tracing::{debug, info, warn};

use crate::config::{AppConfig, S3Config};
use crate::engine::Engine;
use crate::pkg::errors::{CubecowError, CubecowResult};
use crate::pkg::metrics::{
    MetricsCollector, METRIC_SNAPSHOT_COUNT, METRIC_TOTAL_BYTES, METRIC_USED_BYTES,
    METRIC_VOLUME_COUNT,
};
use crate::{Snapshot, Volume, VolumeBlockInfo};

const S3_BLOCK_SIZE: u32 = 512;
const BYTES_PER_GIB: u64 = 1024 * 1024 * 1024;
const TMPCLONE_PREFIX: &str = "__cbc_tmpclone_";
const INDEX_FILE_NAME: &str = "index.json";
const INDEX_TMP_FILE_NAME: &str = "index.json.tmp";

#[derive(Debug, Clone, Serialize, Deserialize)]
struct BdevInfo {
    device_name: String,
    uuid: String,
    nqn: String,
    subsys: u64,
    nsid: u64,
    #[serde(default)]
    device_path: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
enum NameKind {
    Volume {
        size_bytes: u64,
        created_at: String,
        #[serde(default)]
        activated: Option<BdevInfo>,
    },
    Snapshot {
        origin_volume: String,
        size_bytes: u64,
        created_at: String,
        #[serde(default)]
        activated: Option<BdevInfo>,
        /// Persisted `export_uuid` returned by
        /// `rcow_export_snapshot`. Empty until the snapshot is
        /// exported. When non-empty, [`Engine::get_volume_info`]
        /// will call `rcow_get_snapshot_status` to refresh the
        /// upload progress on demand. Kept `#[serde(default)]` so
        /// pre-existing `index.json` files load cleanly.
        #[serde(default)]
        export_uuid: String,
    },
}

#[derive(Debug, Clone, Serialize, Deserialize)]
struct IndexFile {
    version: u32,
    entries: Vec<IndexEntry>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
struct IndexEntry {
    name: String,
    #[serde(flatten)]
    kind: NameKind,
}

impl Default for IndexFile {
    fn default() -> Self {
        Self {
            version: 1,
            entries: Vec::new(),
        }
    }
}

/// S3LVOL / RCOW backed engine.
pub struct S3Engine {
    cfg: S3Config,
    rpc: Arc<JsonRpcClient>,
    name_index: RwLock<HashMap<String, NameKind>>,
    metrics: Arc<MetricsCollector>,
}

impl S3Engine {
    /// Initialize the S3 engine from an `AppConfig`.
    pub fn initialize(config: AppConfig) -> anyhow::Result<Self> {
        crate::pkg::logger::init_logging(&config.log)
            .map_err(|e| anyhow::anyhow!("failed to init logging: {e}"))?;
        info!("s3 engine initializing");
        Self::initialize_with_config(config)
    }

    /// Same as [`Self::initialize`] but skips logging setup.
    pub fn initialize_without_logging(config: AppConfig) -> anyhow::Result<Self> {
        info!("s3 engine initializing (logging managed externally)");
        Self::initialize_with_config(config)
    }

    fn initialize_with_config(config: AppConfig) -> anyhow::Result<Self> {
        config
            .validate()
            .map_err(|e| anyhow::anyhow!("invalid config for s3 backend: {e}"))?;

        let s3_cfg = config.backend.s3.clone();

        std::fs::create_dir_all(&s3_cfg.state_dir).map_err(|e| {
            anyhow::anyhow!(
                "failed to create s3 state_dir '{}': {e}",
                s3_cfg.state_dir.display()
            )
        })?;

        let rpc = Arc::new(JsonRpcClient::new(
            s3_cfg.socket_path.clone(),
            Duration::from_millis(s3_cfg.rpc_timeout_ms),
        ));

        let probe_name = format!("__cbc_probe_{}", uuid::Uuid::new_v4().simple());
        match rpc.call_typed(
            "rcow_deactive_bdev",
            &serde_json::json!({ "device_name": probe_name }),
        ) {
            Ok(_) => {}
            Err(CubecowError::NotFound(_)) => {}
            Err(e) => {
                return Err(anyhow::anyhow!(
                    "failed to reach s3lvol at '{}': {e}",
                    s3_cfg.socket_path.display()
                ));
            }
        }

        let name_index = load_index_file(&s3_cfg.state_dir).map_err(|e| {
            anyhow::anyhow!(
                "failed to load s3 index at '{}': {e}",
                s3_cfg.state_dir.display()
            )
        })?;

        let leftovers: Vec<String> = name_index
            .keys()
            .filter(|n| n.starts_with(TMPCLONE_PREFIX))
            .cloned()
            .collect();
        for name in &leftovers {
            let _ = rpc.call_typed(
                "rcow_delete_lvol",
                &serde_json::json!({ "lvol_name": name }),
            );
        }
        let mut cleaned_index = name_index;
        cleaned_index.retain(|n, _| !n.starts_with(TMPCLONE_PREFIX));
        if !leftovers.is_empty() {
            warn!(
                count = leftovers.len(),
                "swept leftover __cbc_tmpclone_* entries at startup"
            );
            persist_index(&s3_cfg.state_dir, &cleaned_index)?;
        }

        let metrics = Arc::new(MetricsCollector::new());
        let mut volume_count: u64 = 0;
        let mut snapshot_count: u64 = 0;
        for kind in cleaned_index.values() {
            match kind {
                NameKind::Volume { .. } => volume_count += 1,
                NameKind::Snapshot { .. } => snapshot_count += 1,
            }
        }
        metrics.set(METRIC_VOLUME_COUNT, volume_count);
        metrics.set(METRIC_SNAPSHOT_COUNT, snapshot_count);

        info!(
            socket_path = %s3_cfg.socket_path.display(),
            state_dir = %s3_cfg.state_dir.display(),
            volume_count,
            snapshot_count,
            "s3 engine initialized"
        );

        Ok(Self {
            cfg: s3_cfg,
            rpc,
            name_index: RwLock::new(cleaned_index),
            metrics,
        })
    }

    fn validate_name(name: &str, kind: &str) -> CubecowResult<()> {
        if name.is_empty() {
            return Err(CubecowError::InvalidArg(format!("{kind} name is empty")));
        }
        if name == "." || name == ".." {
            return Err(CubecowError::InvalidArg(format!(
                "{kind} name '{name}' is reserved"
            )));
        }
        if name.contains('/') || name.contains('\0') {
            return Err(CubecowError::InvalidArg(format!(
                "{kind} name '{name}' contains an invalid character"
            )));
        }
        if name.starts_with('.') {
            return Err(CubecowError::InvalidArg(format!(
                "{kind} name '{name}' must not start with '.'"
            )));
        }
        if name.starts_with(TMPCLONE_PREFIX) {
            return Err(CubecowError::InvalidArg(format!(
                "{kind} name '{name}' uses the reserved '{TMPCLONE_PREFIX}' prefix"
            )));
        }
        Ok(())
    }

    fn size_bytes_to_gib(&self, size_bytes: u64) -> CubecowResult<u64> {
        if size_bytes == 0 {
            return Err(CubecowError::InvalidArg(
                "size_bytes must be > 0".to_string(),
            ));
        }
        let whole = size_bytes / BYTES_PER_GIB;
        let rem = size_bytes % BYTES_PER_GIB;
        match self.cfg.size_policy.as_str() {
            "round_up" => Ok(if rem == 0 { whole } else { whole + 1 }),
            "strict" => {
                if rem == 0 {
                    Ok(whole)
                } else {
                    Err(CubecowError::InvalidArg(format!(
                        "size_bytes = {size_bytes} is not a whole GiB multiple \
                         (size_policy = \"strict\")"
                    )))
                }
            }
            other => Err(CubecowError::ConfigError(format!(
                "unknown size_policy \"{other}\""
            ))),
        }
    }

    fn persist(&self, idx: &HashMap<String, NameKind>) -> CubecowResult<()> {
        persist_index(&self.cfg.state_dir, idx).map_err(CubecowError::from)
    }

    fn project_volume(name: &str, entry: &NameKind) -> Volume {
        match entry {
            NameKind::Volume {
                size_bytes,
                created_at,
                activated,
            } => Volume {
                name: name.to_string(),
                size_bytes: *size_bytes,
                device_path: activated
                    .as_ref()
                    .map(|b| b.device_path.clone())
                    .unwrap_or_default(),
                snapshot_count: 0,
                created_at: created_at.clone(),
                export_uuid: String::new(),
                export_status: String::new(),
                deletable: None,
            },
            NameKind::Snapshot {
                size_bytes,
                created_at,
                activated,
                export_uuid,
                ..
            } => Volume {
                name: name.to_string(),
                size_bytes: *size_bytes,
                device_path: activated
                    .as_ref()
                    .map(|b| b.device_path.clone())
                    .unwrap_or_default(),
                snapshot_count: 0,
                created_at: created_at.clone(),
                export_uuid: export_uuid.clone(),
                // Upload status is not materialised in the index;
                // callers who want it must go through
                // `get_volume_info`, which augments this projection
                // with a live `rcow_get_snapshot_status` RPC.
                export_status: String::new(),
                deletable: None,
            },
        }
    }

    fn project_snapshot(name: &str, entry: &NameKind) -> CubecowResult<Snapshot> {
        match entry {
            NameKind::Snapshot {
                origin_volume,
                size_bytes,
                created_at,
                activated,
                export_uuid,
            } => Ok(Snapshot {
                name: name.to_string(),
                size_bytes: *size_bytes,
                device_path: activated
                    .as_ref()
                    .map(|b| b.device_path.clone())
                    .unwrap_or_default(),
                origin_volume: origin_volume.clone(),
                created_at: created_at.clone(),
                export_uuid: export_uuid.clone(),
                export_status: String::new(),
                deletable: None,
            }),
            NameKind::Volume { .. } => Err(CubecowError::NotFound(format!(
                "snapshot '{name}' (a volume with that name exists)"
            ))),
        }
    }

    fn call_activate(&self, name: &str) -> CubecowResult<BdevInfo> {
        let resp = self.rpc.call_raw(
            "rcow_active_bdev",
            &serde_json::json!({ "device_name": name }),
        )?;
        let nested = extract_string_value(&resp)?;
        let mut info: BdevInfo = serde_json::from_str(&nested).map_err(|e| {
            CubecowError::PreconditionFailed(format!(
                "s3lvol rcow_active_bdev returned malformed nested payload: {e}"
            ))
        })?;

        let get_resp = self
            .rpc
            .call_raw("rcow_get_bdev", &serde_json::json!({ "device_name": name }))?;
        let get_nested = extract_string_value(&get_resp)?;
        let get_info: BdevInfo = serde_json::from_str(&get_nested).map_err(|e| {
            CubecowError::PreconditionFailed(format!(
                "s3lvol rcow_get_bdev returned malformed nested payload: {e}"
            ))
        })?;
        info.device_path = get_info.device_path;
        Ok(info)
    }
}

// ---------------------------------------------------------------------------
// Engine trait impl
// ---------------------------------------------------------------------------

impl Engine for S3Engine {
    fn create_volume(&self, name: &str, size_bytes: u64) -> CubecowResult<Volume> {
        Self::validate_name(name, "volume")?;
        let size_gib = self.size_bytes_to_gib(size_bytes)?;

        let mut idx = self
            .name_index
            .write()
            .expect("s3 name_index lock poisoned");
        if idx.contains_key(name) {
            return Err(CubecowError::AlreadyExists(format!(
                "name '{name}' already exists in s3 namespace"
            )));
        }

        let created_at = Utc::now().to_rfc3339();
        idx.insert(
            name.to_string(),
            NameKind::Volume {
                size_bytes,
                created_at: created_at.clone(),
                activated: None,
            },
        );

        let rpc_res = self.rpc.call_typed(
            "rcow_create_lvol",
            &serde_json::json!({
                "lvol_name": name,
                "size_gib": size_gib,
            }),
        );
        let rpc_res = match rpc_res {
            Ok(v) => Ok(v),
            Err(CubecowError::AlreadyExists(_)) => Ok(serde_json::Value::Null),
            Err(e) => Err(e),
        };
        if let Err(e) = rpc_res {
            idx.remove(name);
            return Err(e);
        }

        // Persist the volume metadata before activation so that a
        // crash between `rcow_create_lvol` and `rcow_active_bdev`
        // leaves a consistent (created-but-not-activated) entry on
        // disk. Activation is idempotent and can be retried by the
        // caller via `activate_volume` after restart.
        if let Err(e) = self.persist(&idx) {
            let _ = self.rpc.call_typed(
                "rcow_delete_lvol",
                &serde_json::json!({ "lvol_name": name }),
            );
            idx.remove(name);
            return Err(e);
        }

        // Auto-activate: the design contract is "volume ⇄ device
        // lifetime". A freshly created volume must therefore be
        // returned with a usable `device_path`. Activation failure
        // rolls back the entire create so callers do not observe an
        // orphan lvol.
        let bdev = match self.call_activate(name) {
            Ok(b) => b,
            Err(e) => {
                warn!(
                    volume = name,
                    error = %e,
                    "s3 volume created but activation failed; rolling back"
                );
                let _ = self.rpc.call_typed(
                    "rcow_deactive_bdev",
                    &serde_json::json!({ "device_name": name }),
                );
                let _ = self.rpc.call_typed(
                    "rcow_delete_lvol",
                    &serde_json::json!({ "lvol_name": name }),
                );
                idx.remove(name);
                let _ = self.persist(&idx);
                return Err(e);
            }
        };

        let entry = NameKind::Volume {
            size_bytes,
            created_at,
            activated: Some(bdev),
        };
        idx.insert(name.to_string(), entry.clone());
        self.persist(&idx)?;

        self.metrics.inc(METRIC_VOLUME_COUNT);
        info!(volume = name, size_bytes, size_gib, "s3 volume created");
        Ok(Self::project_volume(name, &entry))
    }

    fn delete_volume(&self, name: &str) -> CubecowResult<()> {
        let mut idx = self
            .name_index
            .write()
            .expect("s3 name_index lock poisoned");
        match idx.get(name) {
            Some(NameKind::Volume { .. }) => {}
            Some(NameKind::Snapshot { .. }) => {
                return Err(CubecowError::InvalidArg(format!(
                    "'{name}' is a snapshot; use delete_snapshot instead"
                )));
            }
            None => return Err(CubecowError::NotFound(format!("volume '{name}'"))),
        }

        let _ = self.rpc.call_typed(
            "rcow_deactive_bdev",
            &serde_json::json!({ "device_name": name }),
        );

        match self.rpc.call_typed(
            "rcow_delete_lvol",
            &serde_json::json!({ "lvol_name": name }),
        ) {
            Ok(_) => {}
            Err(CubecowError::NotFound(_)) => {}
            Err(e) => return Err(e),
        }

        idx.remove(name);
        self.persist(&idx)?;
        self.metrics.dec(METRIC_VOLUME_COUNT);
        info!(volume = name, "s3 volume deleted");
        Ok(())
    }

    fn resize_volume(&self, name: &str, new_size_bytes: u64) -> CubecowResult<(u64, u64)> {
        let new_size_gib = self.size_bytes_to_gib(new_size_bytes)?;
        let mut idx = self
            .name_index
            .write()
            .expect("s3 name_index lock poisoned");
        let old_size = match idx.get(name) {
            Some(NameKind::Volume { size_bytes, .. }) => *size_bytes,
            Some(NameKind::Snapshot { .. }) => {
                return Err(CubecowError::InvalidArg(format!(
                    "'{name}' is a snapshot; cannot resize"
                )));
            }
            None => return Err(CubecowError::NotFound(format!("volume '{name}'"))),
        };
        if new_size_bytes < old_size {
            return Err(CubecowError::InvalidArg(format!(
                "shrinking is not supported (current={old_size}, requested={new_size_bytes})"
            )));
        }
        if new_size_bytes == old_size {
            return Ok((old_size, old_size));
        }

        match self.rpc.call_typed(
            "rcow_resize_lvol",
            &serde_json::json!({
                "lvol_name": name,
                "size_gib": new_size_gib,
            }),
        ) {
            Ok(_) => {}
            Err(CubecowError::PreconditionFailed(msg))
                if msg.to_lowercase().contains("new size") =>
            {
                debug!(volume = name, %msg, "s3lvol reports already at target size");
            }
            Err(e) => return Err(e),
        }

        if let Some(NameKind::Volume { size_bytes, .. }) = idx.get_mut(name) {
            *size_bytes = new_size_bytes;
        }
        self.persist(&idx)?;
        info!(
            volume = name,
            old_size,
            new_size = new_size_bytes,
            "s3 volume resized"
        );
        Ok((old_size, new_size_bytes))
    }

    fn get_volume_info(&self, name: &str) -> CubecowResult<Volume> {
        // Step 1: build the base projection under the read lock and
        // capture the snapshot's `export_uuid` (if any) so we can
        // query upload status outside the lock.
        let (mut view, export_uuid) = {
            let idx = self.name_index.read().expect("s3 name_index lock poisoned");
            let entry = idx
                .get(name)
                .ok_or_else(|| CubecowError::NotFound(format!("volume or snapshot '{name}'")))?;

            let mut view = Self::project_volume(name, entry);
            if let NameKind::Volume { .. } = entry {
                view.snapshot_count = idx
                    .iter()
                    .filter(|(_, k)| {
                        matches!(k, NameKind::Snapshot { origin_volume, .. } if origin_volume == name)
                    })
                    .count() as i32;
            }
            let uuid = match entry {
                NameKind::Snapshot { export_uuid, .. } if !export_uuid.is_empty() => {
                    Some(export_uuid.clone())
                }
                _ => None,
            };
            (view, uuid)
        };

        // Step 2: if the entry is an exported snapshot, refresh
        // upload status via the RPC contract documented in the S3
        // backend design doc §2.4 (`rcow_get_snapshot_status`). Any
        // RPC failure here is best-effort: we log and return the
        // base view with empty status fields rather than failing the
        // whole `get_volume_info` call, because upload progress is
        // an *observability* signal and callers who need it can
        // retry.
        if let Some(uuid) = export_uuid {
            match self.rpc.call_raw(
                "rcow_get_snapshot_status",
                &serde_json::json!({ "export_uuid": uuid }),
            ) {
                Ok(resp) => match extract_string_value(&resp) {
                    Ok(nested) => match parse_snapshot_status(&nested) {
                        Ok((status, deletable)) => {
                            view.export_status = status;
                            view.deletable = deletable;
                        }
                        Err(e) => warn!(
                            snapshot = name,
                            export_uuid = %uuid,
                            error = %e,
                            "s3 rcow_get_snapshot_status returned unparseable payload"
                        ),
                    },
                    Err(e) => warn!(
                        snapshot = name,
                        export_uuid = %uuid,
                        error = %e,
                        "s3 rcow_get_snapshot_status missing nested string_value"
                    ),
                },
                Err(e) => warn!(
                    snapshot = name,
                    export_uuid = %uuid,
                    error = %e,
                    "s3 rcow_get_snapshot_status RPC failed"
                ),
            }
        }
        Ok(view)
    }

    fn get_volume_block_info(&self, name: &str) -> CubecowResult<VolumeBlockInfo> {
        let vol = self.get_volume_info(name)?;
        Ok(VolumeBlockInfo {
            num_blocks: vol.size_bytes / S3_BLOCK_SIZE as u64,
            block_size: S3_BLOCK_SIZE,
        })
    }

    fn list_volumes(
        &self,
        page_size: usize,
        page_token: Option<&str>,
    ) -> (Vec<Volume>, Option<String>, usize) {
        let idx = self.name_index.read().expect("s3 name_index lock poisoned");
        let mut names: Vec<String> = idx
            .iter()
            .filter(|(_, k)| matches!(k, NameKind::Volume { .. }))
            .map(|(name, _)| name.clone())
            .collect();
        names.sort();
        let total = names.len();

        let start = match page_token {
            Some(tok) => names.iter().position(|n| n == tok).unwrap_or(total),
            None => 0,
        };
        let effective_page_size = if page_size == 0 { total } else { page_size };
        let end = (start + effective_page_size).min(total);

        let mut out = Vec::with_capacity(end.saturating_sub(start));
        for name in &names[start..end] {
            if let Some(entry) = idx.get(name) {
                let mut v = Self::project_volume(name, entry);
                v.snapshot_count = idx
                    .iter()
                    .filter(|(_, k)| {
                        matches!(k, NameKind::Snapshot { origin_volume, .. } if origin_volume == name)
                    })
                    .count() as i32;
                out.push(v);
            }
        }
        let next_token = if end < total {
            Some(names[end].clone())
        } else {
            None
        };
        (out, next_token, total)
    }

    fn create_snapshot_from_volume(
        &self,
        source_name: &str,
        snapshot_name: &str,
        activate: bool,
    ) -> CubecowResult<Snapshot> {
        Self::validate_name(snapshot_name, "snapshot")?;

        let (ultimate_origin, size_bytes) = {
            let idx = self.name_index.read().expect("s3 name_index lock poisoned");
            match idx.get(source_name) {
                Some(NameKind::Volume { size_bytes, .. }) => (source_name.to_string(), *size_bytes),
                Some(NameKind::Snapshot { .. }) => {
                    return Err(CubecowError::InvalidArg(format!(
                        "'{source_name}' is a snapshot; create_snapshot_from_volume only accepts a volume as source"
                    )));
                }
                None => {
                    return Err(CubecowError::NotFound(format!(
                        "source '{source_name}' for snapshot"
                    )));
                }
            }
        };

        // Reserve the snapshot name so a concurrent create cannot race.
        {
            let mut idx = self
                .name_index
                .write()
                .expect("s3 name_index lock poisoned");
            if idx.contains_key(snapshot_name) {
                return Err(CubecowError::AlreadyExists(format!(
                    "name '{snapshot_name}' already exists in s3 namespace"
                )));
            }
            idx.insert(
                snapshot_name.to_string(),
                NameKind::Snapshot {
                    origin_volume: ultimate_origin.clone(),
                    size_bytes,
                    created_at: Utc::now().to_rfc3339(),
                    activated: None,
                    export_uuid: String::new(),
                },
            );
        }

        let rpc_res = self
            .rpc
            .call_typed(
                "rcow_create_snapshot",
                &serde_json::json!({
                    "lvol_name": source_name,
                    "snapshot_name": snapshot_name,
                }),
            )
            .map(|_| ());
        if let Err(e) = rpc_res {
            let mut idx = self
                .name_index
                .write()
                .expect("s3 name_index lock poisoned");
            idx.remove(snapshot_name);
            return Err(e);
        }

        let bdev = if activate {
            match self.call_activate(snapshot_name) {
                Ok(b) => Some(b),
                Err(e) => {
                    warn!(
                        snapshot = snapshot_name,
                        error = %e,
                        "s3 snapshot activation failed; leaving snapshot for retry"
                    );
                    let mut idx = self
                        .name_index
                        .write()
                        .expect("s3 name_index lock poisoned");
                    idx.remove(snapshot_name);
                    return Err(e);
                }
            }
        } else {
            None
        };

        let final_entry = NameKind::Snapshot {
            origin_volume: ultimate_origin.clone(),
            size_bytes,
            created_at: Utc::now().to_rfc3339(),
            activated: bdev,
            export_uuid: String::new(),
        };
        {
            let mut idx = self
                .name_index
                .write()
                .expect("s3 name_index lock poisoned");
            idx.insert(snapshot_name.to_string(), final_entry.clone());
            self.persist(&idx)?;
        }
        self.metrics.inc(METRIC_SNAPSHOT_COUNT);
        info!(
            snapshot = snapshot_name,
            source = source_name,
            origin_volume = %ultimate_origin,
            activate,
            "s3 snapshot created"
        );
        Self::project_snapshot(snapshot_name, &final_entry)
    }

    fn create_volume_from_snapshot(
        &self,
        source_snapshot: &str,
        volume_name: &str,
    ) -> CubecowResult<Volume> {
        Self::validate_name(volume_name, "volume")?;

        let mut idx = self
            .name_index
            .write()
            .expect("s3 name_index lock poisoned");

        // The source must be an existing snapshot. Cloning from a
        // writable volume is intentionally rejected — callers who
        // want a copy of a volume should snapshot it first and then
        // clone the snapshot, so the RPC intent stays honest with
        // the s3lvol `rcow_create_clone` semantics (its `snapshot_name`
        // parameter is required to name an existing snapshot).
        let size_bytes = match idx.get(source_snapshot) {
            Some(NameKind::Snapshot { size_bytes, .. }) => *size_bytes,
            Some(NameKind::Volume { .. }) => {
                return Err(CubecowError::InvalidArg(format!(
                    "'{source_snapshot}' is a volume; \
                     create_volume_from_snapshot requires a snapshot as source"
                )));
            }
            None => {
                return Err(CubecowError::NotFound(format!(
                    "snapshot '{source_snapshot}'"
                )));
            }
        };
        if idx.contains_key(volume_name) {
            return Err(CubecowError::AlreadyExists(format!(
                "name '{volume_name}' already exists in s3 namespace"
            )));
        }

        self.rpc.call_typed(
            "rcow_create_clone",
            &serde_json::json!({
                "snapshot_name": source_snapshot,
                "clone_name": volume_name,
            }),
        )?;

        // Auto-activate: mirror `create_volume`'s "volume ⇄ device
        // lifetime" contract. Activation failure rolls back the whole
        // create so callers never observe an orphan lvol. Because the
        // index entry is only inserted after activation succeeds, a
        // crash between `rcow_create_clone` and `rcow_active_bdev`
        // leaves an orphan lvol on the s3lvol side — the startup
        // sweep in `initialize_with_config` is responsible for
        // reconciling that, same as for `create_volume`.
        let bdev = match self.call_activate(volume_name) {
            Ok(b) => b,
            Err(e) => {
                warn!(
                    volume = volume_name,
                    source_snapshot,
                    error = %e,
                    "s3 clone created but activation failed; rolling back"
                );
                let _ = self.rpc.call_typed(
                    "rcow_deactive_bdev",
                    &serde_json::json!({ "device_name": volume_name }),
                );
                let _ = self.rpc.call_typed(
                    "rcow_delete_lvol",
                    &serde_json::json!({ "lvol_name": volume_name }),
                );
                return Err(e);
            }
        };

        let entry = NameKind::Volume {
            size_bytes,
            created_at: Utc::now().to_rfc3339(),
            activated: Some(bdev),
        };
        idx.insert(volume_name.to_string(), entry.clone());
        self.persist(&idx)?;

        self.metrics.inc(METRIC_VOLUME_COUNT);
        info!(
            volume = volume_name,
            source_snapshot, size_bytes, "s3 writable volume derived from snapshot"
        );
        Ok(Self::project_volume(volume_name, &entry))
    }

    fn delete_snapshot(&self, snapshot_name: &str) -> CubecowResult<()> {
        let mut idx = self
            .name_index
            .write()
            .expect("s3 name_index lock poisoned");
        match idx.get(snapshot_name) {
            Some(NameKind::Snapshot { .. }) => {}
            Some(NameKind::Volume { .. }) => {
                return Err(CubecowError::InvalidArg(format!(
                    "'{snapshot_name}' is a volume; use delete_volume instead"
                )));
            }
            None => {
                return Err(CubecowError::NotFound(format!(
                    "snapshot '{snapshot_name}'"
                )));
            }
        }

        // Lifecycle rule: a snapshot may have been directly activated
        // as a block device (`activate_volume` supports snapshots
        // natively). We therefore always attempt `rcow_deactive_bdev`
        // first — it is idempotent and cheap. Snapshots that were
        // never activated will see the RPC return successfully with
        // `already_deactivated`.
        let _ = self.rpc.call_typed(
            "rcow_deactive_bdev",
            &serde_json::json!({ "device_name": snapshot_name }),
        );
        match self.rpc.call_typed(
            "rcow_delete_lvol",
            &serde_json::json!({ "lvol_name": snapshot_name }),
        ) {
            Ok(_) => {}
            Err(CubecowError::NotFound(_)) => {}
            Err(e) => return Err(e),
        }

        idx.remove(snapshot_name);
        self.persist(&idx)?;
        self.metrics.dec(METRIC_SNAPSHOT_COUNT);
        info!(snapshot = snapshot_name, "s3 snapshot deleted");
        Ok(())
    }

    fn list_snapshots(
        &self,
        volume_name: &str,
        page_size: usize,
        page_token: Option<&str>,
    ) -> (Vec<Snapshot>, Option<String>) {
        let idx = self.name_index.read().expect("s3 name_index lock poisoned");
        if !matches!(idx.get(volume_name), Some(NameKind::Volume { .. })) {
            return (Vec::new(), None);
        }
        let mut names: Vec<String> = idx
            .iter()
            .filter_map(|(n, k)| match k {
                NameKind::Snapshot { origin_volume, .. } if origin_volume == volume_name => {
                    Some(n.clone())
                }
                _ => None,
            })
            .collect();
        names.sort();
        let total = names.len();

        let start = match page_token {
            Some(tok) => names.iter().position(|n| n == tok).unwrap_or(total),
            None => 0,
        };
        let effective_page_size = if page_size == 0 { total } else { page_size };
        let end = (start + effective_page_size).min(total);

        let mut out = Vec::with_capacity(end.saturating_sub(start));
        for name in &names[start..end] {
            if let Some(entry) = idx.get(name) {
                if let Ok(s) = Self::project_snapshot(name, entry) {
                    out.push(s);
                }
            }
        }
        let next_token = if end < total {
            Some(names[end].clone())
        } else {
            None
        };
        (out, next_token)
    }

    fn activate_volume(&self, name: &str) -> CubecowResult<Volume> {
        // Both writable volumes and read-only snapshots can be
        // directly activated as an nvmf block device on the s3lvol
        // side. `rcow_active_bdev` + `rcow_get_bdev` are the only
        // steps required — there is no need to derive a paired
        // writable clone. Callers who want a writable working copy
        // of a snapshot should explicitly `create_snapshot_from_volume`-then-
        // `activate` a clone (i.e. use snapshot-of-snapshot flatten
        // + activation) rather than piggy-back on this call.
        {
            let idx = self.name_index.read().expect("s3 name_index lock poisoned");
            if !idx.contains_key(name) {
                return Err(CubecowError::NotFound(format!(
                    "volume or snapshot '{name}'"
                )));
            }
        }

        let info = self.call_activate(name)?;

        let mut idx = self
            .name_index
            .write()
            .expect("s3 name_index lock poisoned");
        if let Some(entry) = idx.get_mut(name) {
            match entry {
                NameKind::Volume { activated, .. } => *activated = Some(info.clone()),
                NameKind::Snapshot { activated, .. } => *activated = Some(info.clone()),
            }
        }
        self.persist(&idx)?;

        let projected = Self::project_volume(name, idx.get(name).expect("just inserted"));
        debug!(name, device_path = %info.device_path, "s3 volume/snapshot activated");
        Ok(projected)
    }

    fn deactivate_volume(&self, name: &str) -> CubecowResult<()> {
        {
            let idx = self.name_index.read().expect("s3 name_index lock poisoned");
            if !idx.contains_key(name) {
                return Err(CubecowError::NotFound(format!(
                    "volume or snapshot '{name}'"
                )));
            }
        }
        match self.rpc.call_typed(
            "rcow_deactive_bdev",
            &serde_json::json!({ "device_name": name }),
        ) {
            Ok(_) => {}
            Err(CubecowError::NotFound(_)) => {}
            Err(e) => return Err(e),
        }
        let mut idx = self
            .name_index
            .write()
            .expect("s3 name_index lock poisoned");
        if let Some(entry) = idx.get_mut(name) {
            match entry {
                NameKind::Volume { activated, .. } => *activated = None,
                NameKind::Snapshot { activated, .. } => *activated = None,
            }
        }
        self.persist(&idx)?;
        debug!(name, "s3 volume deactivated");
        Ok(())
    }

    fn export_snapshot(&self, snapshot_name: &str) -> CubecowResult<String> {
        {
            let idx = self.name_index.read().expect("s3 name_index lock poisoned");
            match idx.get(snapshot_name) {
                Some(NameKind::Snapshot { .. }) => {}
                Some(NameKind::Volume { .. }) => {
                    return Err(CubecowError::InvalidArg(format!(
                        "'{snapshot_name}' is a volume; only snapshots are exportable"
                    )));
                }
                None => {
                    return Err(CubecowError::NotFound(format!(
                        "snapshot '{snapshot_name}'"
                    )));
                }
            }
        }
        let resp = self.rpc.call_raw(
            "rcow_export_snapshot",
            &serde_json::json!({ "snapshot_name": snapshot_name }),
        )?;
        let export_uuid = extract_string_value(&resp)?;

        // Persist the export_uuid on the snapshot's index entry so
        // subsequent `get_volume_info` calls can query upload
        // progress via `rcow_get_snapshot_status` without needing
        // the caller to re-supply the uuid. A snapshot that has been
        // exported more than once keeps the most recent uuid — the
        // RPC contract guarantees uuids for the same snapshot are
        // stable, but re-persisting is a cheap no-op either way.
        {
            let mut idx = self
                .name_index
                .write()
                .expect("s3 name_index lock poisoned");
            if let Some(NameKind::Snapshot {
                export_uuid: slot, ..
            }) = idx.get_mut(snapshot_name)
            {
                *slot = export_uuid.clone();
            }
            self.persist(&idx)?;
        }

        info!(snapshot = snapshot_name, %export_uuid, "s3 snapshot exported");
        Ok(export_uuid)
    }

    fn import_lvol(&self, lvol_name: &str, export_uuid: &str) -> CubecowResult<Volume> {
        Self::validate_name(lvol_name, "volume")?;
        if export_uuid.is_empty() {
            return Err(CubecowError::InvalidArg(
                "export_uuid must not be empty".to_string(),
            ));
        }

        let mut idx = self
            .name_index
            .write()
            .expect("s3 name_index lock poisoned");
        if idx.contains_key(lvol_name) {
            return Err(CubecowError::AlreadyExists(format!(
                "name '{lvol_name}' already exists in s3 namespace"
            )));
        }

        match self.rpc.call_typed(
            "rcow_import_lvol",
            &serde_json::json!({
                "lvol_name": lvol_name,
                "export_uuid": export_uuid,
                "decouple": true,
            }),
        ) {
            Ok(_) => {}
            Err(CubecowError::AlreadyExists(_)) => {}
            Err(e) => return Err(e),
        }

        // The imported volume's exact size in bytes is unknown at
        // this point; record 0 as a sentinel. Persist the (not-yet-
        // activated) entry first so a crash between `rcow_import_lvol`
        // and `rcow_active_bdev` still leaves a consistent record on
        // disk that can be re-activated after restart.
        let created_at = Utc::now().to_rfc3339();
        idx.insert(
            lvol_name.to_string(),
            NameKind::Volume {
                size_bytes: 0,
                created_at: created_at.clone(),
                activated: None,
            },
        );
        if let Err(e) = self.persist(&idx) {
            let _ = self.rpc.call_typed(
                "rcow_delete_lvol",
                &serde_json::json!({ "lvol_name": lvol_name }),
            );
            idx.remove(lvol_name);
            return Err(e);
        }

        // Auto-activate: the API contract documented in
        // `docs/design/zh/cubecow-api.md` §import_lvol requires that
        // the returned Volume be "立即可用、device_path 非空". Mirror
        // the `create_volume` rollback pattern on activation failure
        // so callers never observe an orphan imported lvol.
        let bdev = match self.call_activate(lvol_name) {
            Ok(b) => b,
            Err(e) => {
                warn!(
                    lvol = lvol_name,
                    error = %e,
                    "s3 lvol imported but activation failed; rolling back"
                );
                let _ = self.rpc.call_typed(
                    "rcow_deactive_bdev",
                    &serde_json::json!({ "device_name": lvol_name }),
                );
                let _ = self.rpc.call_typed(
                    "rcow_delete_lvol",
                    &serde_json::json!({ "lvol_name": lvol_name }),
                );
                idx.remove(lvol_name);
                let _ = self.persist(&idx);
                return Err(e);
            }
        };

        let entry = NameKind::Volume {
            size_bytes: 0,
            created_at,
            activated: Some(bdev),
        };
        idx.insert(lvol_name.to_string(), entry.clone());
        self.persist(&idx)?;

        self.metrics.inc(METRIC_VOLUME_COUNT);
        info!(lvol = lvol_name, %export_uuid, "s3 volume imported");
        Ok(Self::project_volume(lvol_name, &entry))
    }

    fn reset_node_storage(&self) -> CubecowResult<()> {
        let mut idx = self
            .name_index
            .write()
            .expect("s3 name_index lock poisoned");

        // Delete snapshots before volumes so origin-lvol delete does
        // not trip a "still referenced" precondition in s3lvol.
        let mut ordered: Vec<(String, NameKind)> = idx.drain().collect();
        ordered.sort_by_key(|(_, k)| match k {
            NameKind::Snapshot { .. } => 0,
            NameKind::Volume { .. } => 1,
        });

        let mut errs: Vec<String> = Vec::new();
        for (name, _kind) in &ordered {
            let _ = self.rpc.call_typed(
                "rcow_deactive_bdev",
                &serde_json::json!({ "device_name": name }),
            );
            match self.rpc.call_typed(
                "rcow_delete_lvol",
                &serde_json::json!({ "lvol_name": name }),
            ) {
                Ok(_) => {}
                Err(CubecowError::NotFound(_)) => {}
                Err(e) => errs.push(format!("{name}: {e}")),
            }
        }

        // Wipe the local index regardless of individual RPC failures;
        // collected errors are surfaced together at the end.
        idx.clear();
        self.persist(&idx)?;
        self.metrics.set(METRIC_VOLUME_COUNT, 0);
        self.metrics.set(METRIC_SNAPSHOT_COUNT, 0);

        if errs.is_empty() {
            info!(cleared = ordered.len(), "s3 node storage reset");
            Ok(())
        } else {
            Err(CubecowError::PreconditionFailed(format!(
                "s3 reset_node_storage: {} entries failed to delete: {}",
                errs.len(),
                errs.join("; ")
            )))
        }
    }

    fn metrics(&self) -> HashMap<String, u64> {
        self.metrics.set(METRIC_TOTAL_BYTES, 0);
        self.metrics.set(METRIC_USED_BYTES, 0);
        self.metrics.snapshot()
    }
}

// ---------------------------------------------------------------------------
// JSON-RPC client over UnixStream
// ---------------------------------------------------------------------------

struct JsonRpcClient {
    socket_path: PathBuf,
    timeout: Duration,
    next_id: AtomicU64,
    conn: Mutex<Option<UnixStream>>,
}

impl JsonRpcClient {
    fn new(socket_path: PathBuf, timeout: Duration) -> Self {
        Self {
            socket_path,
            timeout,
            next_id: AtomicU64::new(1),
            conn: Mutex::new(None),
        }
    }

    /// Wire-level call: returns the JSON-RPC `result` field on success.
    fn call_raw(
        &self,
        method: &str,
        params: &serde_json::Value,
    ) -> CubecowResult<serde_json::Value> {
        let id = self.next_id.fetch_add(1, Ordering::Relaxed);
        let req = serde_json::json!({
            "jsonrpc": "2.0",
            "method": method,
            "params": params,
            "id": id,
        });

        let mut guard = self.conn.lock().map_err(|_| {
            CubecowError::PreconditionFailed("s3lvol rpc mutex poisoned".to_string())
        })?;

        let mut attempt = 0;
        loop {
            attempt += 1;
            if guard.is_none() {
                match self.dial() {
                    Ok(s) => *guard = Some(s),
                    Err(e) => {
                        return Err(CubecowError::PreconditionFailed(format!(
                            "s3lvol rpc: connect '{}' failed: {e}",
                            self.socket_path.display()
                        )));
                    }
                }
            }
            let stream = guard.as_mut().unwrap();

            let mut framed = serde_json::to_vec(&req).map_err(|e| {
                CubecowError::PreconditionFailed(format!("s3lvol rpc: encode: {e}"))
            })?;
            framed.push(b'\n');

            match stream.write_all(&framed).and_then(|_| stream.flush()) {
                Ok(()) => {}
                Err(e)
                    if attempt < 2
                        && matches!(
                            e.kind(),
                            ErrorKind::BrokenPipe | ErrorKind::ConnectionReset
                        ) =>
                {
                    *guard = None;
                    continue;
                }
                Err(e) => {
                    return Err(CubecowError::PreconditionFailed(format!(
                        "s3lvol rpc: write {method}: {e}"
                    )));
                }
            }

            let response_bytes = match read_line(stream) {
                Ok(v) => v,
                Err(e)
                    if attempt < 2
                        && matches!(
                            e.kind(),
                            ErrorKind::BrokenPipe
                                | ErrorKind::ConnectionReset
                                | ErrorKind::UnexpectedEof
                        ) =>
                {
                    *guard = None;
                    continue;
                }
                Err(e) => {
                    return Err(CubecowError::PreconditionFailed(format!(
                        "s3lvol rpc: read {method}: {e}"
                    )));
                }
            };

            let resp: serde_json::Value = serde_json::from_slice(&response_bytes).map_err(|e| {
                CubecowError::PreconditionFailed(format!("s3lvol rpc: decode {method}: {e}"))
            })?;

            if let Some(err) = resp.get("error") {
                let msg = err
                    .get("message")
                    .and_then(|m| m.as_str())
                    .unwrap_or("unknown error");
                return Err(map_rpc_error(msg));
            }

            let result = resp
                .get("result")
                .cloned()
                .unwrap_or(serde_json::Value::Null);

            if let Some(bv) = result.get("bool_value").and_then(|v| v.as_bool()) {
                if !bv {
                    let msg = result
                        .get("string_value")
                        .and_then(|v| v.as_str())
                        .unwrap_or("s3lvol reported failure");
                    return Err(map_rpc_error(msg));
                }
            }
            return Ok(result);
        }
    }

    /// Convenience wrapper for callers that only care about success.
    fn call_typed(
        &self,
        method: &str,
        params: &serde_json::Value,
    ) -> CubecowResult<serde_json::Value> {
        self.call_raw(method, params)
    }

    fn dial(&self) -> std::io::Result<UnixStream> {
        let s = UnixStream::connect(&self.socket_path)?;
        s.set_read_timeout(Some(self.timeout))?;
        s.set_write_timeout(Some(self.timeout))?;
        Ok(s)
    }
}

/// Read a single `\n`-terminated JSON line from the stream. The line
/// terminator itself is discarded.
fn read_line(stream: &mut UnixStream) -> std::io::Result<Vec<u8>> {
    let mut out = Vec::with_capacity(256);
    let mut buf = [0u8; 1];
    loop {
        let n = stream.read(&mut buf)?;
        if n == 0 {
            return Err(std::io::Error::new(
                ErrorKind::UnexpectedEof,
                "s3lvol closed connection mid-frame",
            ));
        }
        if buf[0] == b'\n' {
            return Ok(out);
        }
        out.push(buf[0]);
    }
}

/// Extract the `string_value` field from a common-shape response.
fn extract_string_value(resp: &serde_json::Value) -> CubecowResult<String> {
    resp.get("string_value")
        .and_then(|v| v.as_str())
        .map(|s| s.to_string())
        .ok_or_else(|| {
            CubecowError::PreconditionFailed(
                "s3lvol response missing 'string_value' field".to_string(),
            )
        })
}

/// Parse the nested JSON payload returned by `rcow_get_snapshot_status`.
///
/// The wire format documented in `S3LVOL-RPC接口.md` is:
///
/// ```json
/// {
///   "export_status": "NONE" | "INPROGRESS" | "DONE",
///   "deletable":     "YES"  | "NO"
/// }
/// ```
///
/// The `export_status` key contains a **space** — that is not a typo
/// on our side. `deletable` is a stringly-typed YES/NO flag that we
/// normalise to `Option<bool>` (`None` when the field is missing or
/// carries an unrecognised value).
fn parse_snapshot_status(nested: &str) -> CubecowResult<(String, Option<bool>)> {
    let v: serde_json::Value = serde_json::from_str(nested).map_err(|e| {
        CubecowError::PreconditionFailed(format!(
            "s3lvol rcow_get_snapshot_status: malformed nested JSON: {e}"
        ))
    })?;
    let status = v
        .get("export_status")
        .and_then(|s| s.as_str())
        .unwrap_or("")
        .to_string();
    let deletable = v.get("deletable").and_then(|s| s.as_str()).and_then(|s| {
        match s.to_ascii_uppercase().as_str() {
            "YES" => Some(true),
            "NO" => Some(false),
            _ => None,
        }
    });
    Ok((status, deletable))
}

/// Map an s3lvol textual error into the closest [`CubecowError`].
fn map_rpc_error(msg: &str) -> CubecowError {
    let lower = msg.to_lowercase();
    if lower.contains("already exists") || lower.contains("already exist") {
        CubecowError::AlreadyExists(msg.to_string())
    } else if lower.contains("not found") || lower.contains("no such") {
        CubecowError::NotFound(msg.to_string())
    } else {
        CubecowError::PreconditionFailed(format!("s3lvol rpc: {msg}"))
    }
}

// ---------------------------------------------------------------------------
// Index file persistence
// ---------------------------------------------------------------------------

fn index_paths(state_dir: &Path) -> (PathBuf, PathBuf) {
    (
        state_dir.join(INDEX_FILE_NAME),
        state_dir.join(INDEX_TMP_FILE_NAME),
    )
}

/// Load the on-disk index. A missing file yields an empty index.
fn load_index_file(state_dir: &Path) -> std::io::Result<HashMap<String, NameKind>> {
    let (index_path, tmp_path) = index_paths(state_dir);
    let _ = std::fs::remove_file(&tmp_path);

    let raw = match std::fs::read(&index_path) {
        Ok(v) => v,
        Err(e) if e.kind() == ErrorKind::NotFound => return Ok(HashMap::new()),
        Err(e) => return Err(e),
    };
    let parsed: IndexFile = serde_json::from_slice(&raw).map_err(|e| {
        std::io::Error::new(
            ErrorKind::InvalidData,
            format!("failed to parse s3 index.json: {e}"),
        )
    })?;

    let mut out = HashMap::with_capacity(parsed.entries.len());
    for e in parsed.entries {
        out.insert(e.name, e.kind);
    }
    Ok(out)
}

/// Persist the in-memory index atomically via `write → fsync → rename`.
fn persist_index(state_dir: &Path, idx: &HashMap<String, NameKind>) -> std::io::Result<()> {
    let mut entries: Vec<IndexEntry> = idx
        .iter()
        .map(|(name, kind)| IndexEntry {
            name: name.clone(),
            kind: kind.clone(),
        })
        .collect();
    entries.sort_by(|a, b| a.name.cmp(&b.name));
    let file = IndexFile {
        version: 1,
        entries,
    };
    let payload = serde_json::to_vec_pretty(&file).map_err(|e| {
        std::io::Error::new(
            ErrorKind::InvalidData,
            format!("failed to serialise s3 index.json: {e}"),
        )
    })?;

    let (final_path, tmp_path) = index_paths(state_dir);
    {
        let mut f = std::fs::OpenOptions::new()
            .create(true)
            .write(true)
            .truncate(true)
            .open(&tmp_path)?;
        f.write_all(&payload)?;
        f.sync_all()?;
    }
    std::fs::rename(&tmp_path, &final_path)?;
    if let Ok(dir) = std::fs::File::open(state_dir) {
        let _ = dir.sync_all();
    }
    Ok(())
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn validate_name_rejects_reserved_prefix() {
        assert!(S3Engine::validate_name("__cbc_tmpclone_foo", "volume").is_err());
        assert!(S3Engine::validate_name("normal-name", "volume").is_ok());
    }

    #[test]
    fn size_bytes_to_gib_round_up_and_strict() {
        let cfg = S3Config {
            socket_path: PathBuf::from("/var/run/s3lvol.sock"),
            state_dir: PathBuf::from("/tmp"),
            rpc_timeout_ms: 1000,
            size_policy: "round_up".to_string(),
        };
        let engine = S3Engine {
            cfg: cfg.clone(),
            rpc: Arc::new(JsonRpcClient::new(
                cfg.socket_path.clone(),
                Duration::from_millis(1000),
            )),
            name_index: RwLock::new(HashMap::new()),
            metrics: Arc::new(MetricsCollector::new()),
        };
        assert_eq!(engine.size_bytes_to_gib(BYTES_PER_GIB).unwrap(), 1);
        assert_eq!(engine.size_bytes_to_gib(BYTES_PER_GIB + 1).unwrap(), 2);
        assert!(engine.size_bytes_to_gib(0).is_err());

        let cfg2 = S3Config {
            size_policy: "strict".to_string(),
            ..cfg
        };
        let engine2 = S3Engine {
            cfg: cfg2.clone(),
            rpc: Arc::new(JsonRpcClient::new(
                cfg2.socket_path.clone(),
                Duration::from_millis(1000),
            )),
            name_index: RwLock::new(HashMap::new()),
            metrics: Arc::new(MetricsCollector::new()),
        };
        assert_eq!(engine2.size_bytes_to_gib(2 * BYTES_PER_GIB).unwrap(), 2);
        assert!(engine2.size_bytes_to_gib(BYTES_PER_GIB + 1).is_err());
    }

    #[test]
    fn index_roundtrip() {
        let tmp = std::env::temp_dir().join(format!(
            "cubecow-s3-idx-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        std::fs::create_dir_all(&tmp).unwrap();

        let mut idx = HashMap::new();
        idx.insert(
            "vol-a".to_string(),
            NameKind::Volume {
                size_bytes: 21474836480,
                created_at: "2026-08-13T21:00:00+00:00".to_string(),
                activated: None,
            },
        );
        idx.insert(
            "snap-1".to_string(),
            NameKind::Snapshot {
                origin_volume: "vol-a".to_string(),
                size_bytes: 21474836480,
                created_at: "2026-08-13T21:05:00+00:00".to_string(),
                activated: None,
                export_uuid: String::new(),
            },
        );
        persist_index(&tmp, &idx).unwrap();

        let reloaded = load_index_file(&tmp).unwrap();
        assert_eq!(reloaded.len(), 2);
        assert!(matches!(
            reloaded.get("vol-a"),
            Some(NameKind::Volume { .. })
        ));
        assert!(matches!(
            reloaded.get("snap-1"),
            Some(NameKind::Snapshot { .. })
        ));

        let _ = std::fs::remove_dir_all(&tmp);
    }

    #[test]
    fn map_rpc_error_classifies_common_messages() {
        assert!(matches!(
            map_rpc_error("lvol already exists: vol-a"),
            CubecowError::AlreadyExists(_)
        ));
        assert!(matches!(
            map_rpc_error("lvol not found: vol-b"),
            CubecowError::NotFound(_)
        ));
        assert!(matches!(
            map_rpc_error("something else went wrong"),
            CubecowError::PreconditionFailed(_)
        ));
    }

    #[test]
    fn parse_snapshot_status_handles_all_shapes() {
        // Happy path: DONE + deletable YES.
        let (status, deletable) =
            parse_snapshot_status(r#"{"export_status":"DONE","deletable":"YES"}"#).unwrap();
        assert_eq!(status, "DONE");
        assert_eq!(deletable, Some(true));

        // INPROGRESS + NO.
        let (status, deletable) =
            parse_snapshot_status(r#"{"export_status":"INPROGRESS","deletable":"NO"}"#).unwrap();
        assert_eq!(status, "INPROGRESS");
        assert_eq!(deletable, Some(false));

        // Unknown deletable value → None; unknown status stays as-is.
        let (status, deletable) =
            parse_snapshot_status(r#"{"export_status":"WAT","deletable":"maybe"}"#).unwrap();
        assert_eq!(status, "WAT");
        assert_eq!(deletable, None);

        // Missing fields → empty status + None.
        let (status, deletable) = parse_snapshot_status(r#"{}"#).unwrap();
        assert_eq!(status, "");
        assert_eq!(deletable, None);

        // Malformed JSON → error.
        assert!(parse_snapshot_status("not-json").is_err());
    }
}
