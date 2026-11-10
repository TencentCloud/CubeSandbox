// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
// xfs-reflink backed implementation of [`crate::engine::Engine`].
//
// Rationale (see `docs/design/zh/cubecow.md` for the full discussion):
//
// dm-thin gives us O(1) snapshots at the block layer at the cost of a
// kernel ioctl orchestration, a per-pool ledger, and a non-trivial
// crash-recovery surface. For workloads whose snapshot pattern is "many
// short-lived clones of large image files", a filesystem mounted on a
// reflink-capable layout (xfs `-m reflink=1`, Btrfs, OCFS2, ...)
// offers the same O(1) clone semantics at the file layer, via the
// `FICLONE` ioctl, with much simpler crash semantics: every operation
// is a single filesystem transaction.
//
// # Storage layout — fs as the single source of truth
//
// The deployer points us at a single `root_dir` that already lives on
// a `FICLONE`-capable filesystem. Underneath it, cubecow creates and
// owns the `volumes/` subtree:
//
// ```text
// <root_dir>/volumes/
//     ├── <vol-A>/
//     │   ├── <vol-A>          ← volume main file (FICLONE source)
//     │   ├── <snap-1>         ← FICLONE(<vol-A>)
//     │   └── <snap-2>         ← FICLONE(<snap-1>) — flattened in vol-A/
//     ├── <vol-B>/
//     │   └── <vol-B>
//     └── ...
// ```
//
// All metadata is reconstructable from the layout itself: volume
// listings come from `readdir(volumes/)`, snapshot listings come from
// `readdir(volumes/<vol>/)` minus the main file, sizes come from
// `stat`, creation timestamps come from `mtime`, and snap-of-snap
// relationships are flattened so that every snapshot's `origin_volume`
// is the ultimate origin volume — matching dm-thin's externally
// observable contract. **No on-disk ledger** is required.
//
// An in-memory `HashMap<String, NameKind>` tracks the global name
// namespace (volume names and snapshot names share it, identical to
// dm-thin) and is rebuilt on startup by scanning the layout. The
// scanner is responsible for cleaning up orphan artefacts left behind
// by mid-flight crashes (empty directories, zero-byte snapshot files
// whose FICLONE never landed, etc.).

use std::collections::{hash_map::Entry, HashMap};
use std::ffi::CString;
use std::fs::{File, OpenOptions};
use std::io::{ErrorKind, Write};
use std::os::unix::fs::MetadataExt;
use std::os::unix::io::AsRawFd;
use std::path::{Path, PathBuf};
use std::sync::{Arc, RwLock};
use std::time::{SystemTime, UNIX_EPOCH};

use chrono::{DateTime, Utc};
use tracing::{debug, info, warn};

use crate::config::{AppConfig, CloneMethod};
use crate::engine::Engine;
use crate::pkg::errors::{CubecowError, CubecowResult};
use crate::pkg::metrics::{
    MetricsCollector, METRIC_SNAPSHOT_COUNT, METRIC_TOTAL_BYTES, METRIC_USED_BYTES,
    METRIC_VOLUME_COUNT,
};
use crate::{Snapshot, Volume, VolumeBlockInfo};

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

/// Block size advertised through `get_volume_block_info`. This is the
/// logical sector size every Linux filesystem reports. Volumes are
/// sized in bytes so `num_blocks = size_bytes / 512`.
const REFLINK_BLOCK_SIZE: u32 = 512;

/// The `FICLONE` ioctl number on Linux.
///
/// `FICLONE` is `_IOW(0x94, 9, int)`, which expands to:
///   `(1<<30) | (4<<16) | (0x94<<8) | 9` = `0x40049409`.
///
/// Same constant is used by `bin/cubecow_snap_vs_reflink.rs`; we keep a
/// local copy here to avoid a binary-↔-library dependency cycle.
const FICLONE: libc::c_ulong = 0x40049409;

// ---------------------------------------------------------------------------
// Internal types
// ---------------------------------------------------------------------------

/// What a name in the global namespace refers to.
///
/// Both volume names and snapshot names share a single namespace (just
/// like dm-thin). For a snapshot, we additionally remember which
/// **ultimate** origin volume directory it lives under, so that
/// `delete_snapshot(name)` can locate the file without rescanning the
/// filesystem.
#[derive(Debug, Clone, PartialEq, Eq)]
enum NameKind {
    Volume,
    Snapshot { origin_volume: String },
}

// ---------------------------------------------------------------------------
// ReflinkEngine
// ---------------------------------------------------------------------------

/// xfs-reflink backed engine.
///
/// Built around a single `<root_dir>/volumes/` tree on a
/// `FICLONE`-capable filesystem. The engine is fully crash-safe by
/// virtue of the layout being self-describing (see the module-level
/// doc comment), so there is no on-disk ledger to keep in sync.
pub struct ReflinkEngine {
    /// `<root_dir>/volumes/`. The engine never touches anything outside
    /// this subtree.
    volumes_dir: PathBuf,
    /// Clone primitive for snapshots and volumes derived from them.
    clone_method: CloneMethod,
    /// Global volume + snapshot name index. Rebuilt from disk during
    /// `initialize`; mutated under a single writer lock thereafter.
    /// Read paths take a shared lock for `O(1)` lookups.
    name_index: RwLock<HashMap<String, NameKind>>,
    /// Metrics collector — same surface as the dm-thin backend so
    /// downstream consumers (`/metrics` exporters, FFI callers) see a
    /// uniform set of keys regardless of which backend is in use.
    metrics: Arc<MetricsCollector>,
}

impl ReflinkEngine {
    /// Initialize the reflink engine from an `AppConfig`.
    ///
    /// Sets up logging, ensures `<root_dir>/volumes/` exists, probes
    /// the underlying filesystem for `FICLONE` support, then scans the
    /// layout to rebuild the name index (cleaning up any orphan
    /// artefacts from a previous unclean shutdown).
    pub fn initialize(config: AppConfig) -> anyhow::Result<Self> {
        crate::pkg::logger::init_logging(&config.log)
            .map_err(|e| anyhow::anyhow!("failed to init logging: {e}"))?;
        info!("reflink engine initializing");
        Self::initialize_with_config(config)
    }

    /// Same as [`Self::initialize`] but skips logging setup.
    pub fn initialize_without_logging(config: AppConfig) -> anyhow::Result<Self> {
        info!("reflink engine initializing (logging managed externally)");
        Self::initialize_with_config(config)
    }

    fn initialize_with_config(config: AppConfig) -> anyhow::Result<Self> {
        // Defence-in-depth: programmatically-constructed `AppConfig`
        // values that bypass `from_toml_str` / `from_json_str` still go
        // through the same backend-specific contract before we mkdir or
        // probe FICLONE.
        config
            .validate()
            .map_err(|e| anyhow::anyhow!("invalid config for reflink backend: {e}"))?;

        let root_dir = config.backend.reflink.root_dir.clone();
        let clone_method = config.backend.reflink.clone_method;
        let volumes_dir = root_dir.join("volumes");

        std::fs::create_dir_all(&volumes_dir).map_err(|e| {
            anyhow::anyhow!(
                "failed to create reflink volumes dir '{}': {e}",
                volumes_dir.display()
            )
        })?;

        probe_clone_support(clone_method, &volumes_dir).map_err(|e| {
            anyhow::anyhow!(
                "filesystem under '{}' does not support {clone_method:?} clones: {e}; \
                 mount an xfs filesystem with `-m reflink=1` (or any other \
                 FICLONE-capable filesystem) at this path, or JuiceFS with \
                 clone_method = \"copy_file_range\"",
                volumes_dir.display()
            )
        })?;

        let metrics = Arc::new(MetricsCollector::new());

        // copy_file_range is the shared JuiceFS mode. A process starting on
        // one node must not perform destructive orphan cleanup while another
        // node may be publishing an object.
        let cleanup_orphans = clone_method == CloneMethod::Ficlone;
        let name_index = scan_index(&volumes_dir, cleanup_orphans).map_err(|e| {
            anyhow::anyhow!(
                "failed to scan reflink volumes dir '{}': {e}",
                volumes_dir.display()
            )
        })?;

        // Prime metrics from the freshly-built index.
        let mut volume_count: u64 = 0;
        let mut snapshot_count: u64 = 0;
        for kind in name_index.values() {
            match kind {
                NameKind::Volume => volume_count += 1,
                NameKind::Snapshot { .. } => snapshot_count += 1,
            }
        }
        metrics.set(METRIC_VOLUME_COUNT, volume_count);
        metrics.set(METRIC_SNAPSHOT_COUNT, snapshot_count);

        info!(
            volumes_dir = %volumes_dir.display(),
            ?clone_method,
            volume_count,
            snapshot_count,
            "reflink engine initialized"
        );

        Ok(Self {
            volumes_dir,
            clone_method,
            name_index: RwLock::new(name_index),
            metrics,
        })
    }

    /// Return an existing object's kind from the process-local index.
    ///
    /// JuiceFS (`copy_file_range`) additionally discovers an object created by
    /// another engine on a cache miss. Local XFS (`ficlone`) deliberately keeps
    /// its original behavior: the startup scan is the only disk scan and all
    /// later lookups use the in-memory index exclusively.
    ///
    /// Cube guarantees globally unique names and serializes one object's
    /// lifecycle. The filesystem remains the source of truth; this map is only
    /// a per-process acceleration structure.
    fn lookup_existing_name(&self, name: &str) -> CubecowResult<Option<NameKind>> {
        let cached = {
            let index = self
                .name_index
                .read()
                .expect("reflink name_index lock poisoned");
            index.get(name).cloned()
        };
        if self.clone_method == CloneMethod::Ficlone {
            return Ok(cached);
        }
        if let Some(kind) = cached.as_ref() {
            if self
                .name_kind_exists_on_disk(name, kind)
                .map_err(CubecowError::IoError)?
            {
                return Ok(cached);
            }
            let mut index = self
                .name_index
                .write()
                .expect("reflink name_index lock poisoned");
            if index.get(name) == Some(kind) {
                let removed = index.remove(name).expect("checked entry disappeared");
                self.dec_name_metric(&removed);
            }
        }
        let discovered =
            discover_name_on_disk(&self.volumes_dir, name).map_err(CubecowError::IoError)?;
        if let Some(kind) = discovered.clone() {
            let mut index = self
                .name_index
                .write()
                .expect("reflink name_index lock poisoned");
            if let Entry::Vacant(entry) = index.entry(name.to_string()) {
                match &kind {
                    NameKind::Volume => self.metrics.inc(METRIC_VOLUME_COUNT),
                    NameKind::Snapshot { .. } => self.metrics.inc(METRIC_SNAPSHOT_COUNT),
                }
                entry.insert(kind);
            }
        }
        Ok(discovered)
    }

    /// Reserve a globally unique destination name without scanning the shared
    /// namespace. In JuiceFS mode, an index hit is validated at its exact
    /// cached path so a file deleted by another engine does not block name
    /// reuse by a later, serialized lifecycle.
    fn reserve_new_name(&self, name: &str, kind: NameKind) -> CubecowResult<()> {
        loop {
            let mut index = self
                .name_index
                .write()
                .expect("reflink name_index lock poisoned");
            let Some(cached) = index.get(name).cloned() else {
                index.insert(name.to_string(), kind);
                return Ok(());
            };
            if self.clone_method == CloneMethod::Ficlone
                || self
                    .name_kind_exists_on_disk(name, &cached)
                    .map_err(CubecowError::IoError)?
            {
                return Err(CubecowError::AlreadyExists(format!(
                    "name '{name}' already exists in reflink namespace"
                )));
            }
            index.remove(name);
            self.dec_name_metric(&cached);
        }
    }

    fn name_kind_exists_on_disk(&self, name: &str, kind: &NameKind) -> std::io::Result<bool> {
        let path = match kind {
            NameKind::Volume => self.vol_main_file(name),
            NameKind::Snapshot { origin_volume } => self.snap_file(origin_volume, name),
        };
        match std::fs::metadata(path) {
            Ok(meta) => Ok(meta.is_file()),
            Err(e) if e.kind() == ErrorKind::NotFound => Ok(false),
            Err(e) => Err(e),
        }
    }

    fn dec_name_metric(&self, kind: &NameKind) {
        match kind {
            NameKind::Volume => self.metrics.dec(METRIC_VOLUME_COUNT),
            NameKind::Snapshot { .. } => self.metrics.dec(METRIC_SNAPSHOT_COUNT),
        }
    }

    // -----------------------------------------------------------------------
    // Path helpers
    // -----------------------------------------------------------------------

    /// Path to a volume's directory: `<volumes_dir>/<vol>/`.
    fn vol_dir(&self, volume_name: &str) -> PathBuf {
        self.volumes_dir.join(volume_name)
    }

    /// Path to a volume's main file: `<volumes_dir>/<vol>/<vol>`.
    fn vol_main_file(&self, volume_name: &str) -> PathBuf {
        self.volumes_dir.join(volume_name).join(volume_name)
    }

    /// Path to a snapshot file: `<volumes_dir>/<origin>/<snap>`.
    fn snap_file(&self, origin_volume: &str, snapshot_name: &str) -> PathBuf {
        self.volumes_dir.join(origin_volume).join(snapshot_name)
    }

    // -----------------------------------------------------------------------
    // Validation
    // -----------------------------------------------------------------------

    /// Reject names that would break the layout: empty, contains `/`,
    /// equals "." / "..", or starts with `.` (we reserve dotfiles for
    /// future internal markers — currently none are used, but it costs
    /// nothing to keep the namespace clean).
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
        Ok(())
    }

    // -----------------------------------------------------------------------
    // Volume metadata projection
    // -----------------------------------------------------------------------

    /// Build a public [`Volume`] view of `volume_name` by stat-ing its
    /// main file and counting siblings as snapshots.
    fn project_volume(&self, volume_name: &str) -> CubecowResult<Volume> {
        let main = self.vol_main_file(volume_name);
        let meta = std::fs::metadata(&main).map_err(|e| {
            if e.kind() == ErrorKind::NotFound {
                CubecowError::NotFound(format!("volume '{volume_name}'"))
            } else {
                CubecowError::IoError(e)
            }
        })?;

        let snapshot_count = count_snapshots_on_disk(&self.vol_dir(volume_name), volume_name)?;

        Ok(Volume {
            name: volume_name.to_string(),
            size_bytes: meta.size(),
            device_path: main.to_string_lossy().into_owned(),
            snapshot_count,
            created_at: rfc3339_from_meta(&meta),
            export_uuid: String::new(),
            export_status: String::new(),
            deletable: None,
        })
    }

    /// Build a public [`Snapshot`] view of `snapshot_name`. The origin
    /// is looked up in the in-memory index.
    fn project_snapshot(&self, snapshot_name: &str) -> CubecowResult<Snapshot> {
        let origin = {
            let idx = self
                .name_index
                .read()
                .expect("reflink name_index lock poisoned");
            match idx.get(snapshot_name) {
                Some(NameKind::Snapshot { origin_volume }) => origin_volume.clone(),
                Some(NameKind::Volume) => {
                    return Err(CubecowError::NotFound(format!(
                        "snapshot '{snapshot_name}' (a volume with that name exists)"
                    )));
                }
                None => {
                    return Err(CubecowError::NotFound(format!(
                        "snapshot '{snapshot_name}'"
                    )));
                }
            }
        };

        let path = self.snap_file(&origin, snapshot_name);
        let meta = std::fs::metadata(&path).map_err(|e| {
            if e.kind() == ErrorKind::NotFound {
                CubecowError::NotFound(format!("snapshot '{snapshot_name}'"))
            } else {
                CubecowError::IoError(e)
            }
        })?;

        Ok(Snapshot {
            name: snapshot_name.to_string(),
            size_bytes: meta.size(),
            device_path: path.to_string_lossy().into_owned(),
            origin_volume: origin,
            created_at: rfc3339_from_meta(&meta),
            export_uuid: String::new(),
            export_status: String::new(),
            deletable: None,
        })
    }

    // -----------------------------------------------------------------------
    // Filesystem stats for the underlying volumes_dir
    // -----------------------------------------------------------------------

    /// Reserve `volume_name` and clone `source_path` into its main file,
    /// rolling the reservation back on failure.
    fn clone_into_new_volume(&self, volume_name: &str, source_path: &Path) -> CubecowResult<()> {
        self.reserve_new_name(volume_name, NameKind::Volume)?;

        let mut created_dir = false;
        let result = (|| -> CubecowResult<()> {
            let dir = self.vol_dir(volume_name);
            let main = self.vol_main_file(volume_name);

            created_dir = ensure_dir_exists(&dir).map_err(CubecowError::IoError)?;

            let src_file = File::open(source_path).map_err(|e| {
                if e.kind() == ErrorKind::NotFound {
                    CubecowError::NotFound(format!(
                        "source file '{}' missing",
                        source_path.display()
                    ))
                } else {
                    CubecowError::IoError(e)
                }
            })?;

            clone_file(self.clone_method, &src_file, &main).map_err(|errno| {
                if errno == libc::EEXIST {
                    return CubecowError::AlreadyExists(format!(
                        "volume main file '{}' already exists",
                        main.display()
                    ));
                }
                let reason = describe_ficlone_errno(errno);
                CubecowError::PreconditionFailed(format!(
                    "{:?} clone failed for volume '{volume_name}' from '{}': {reason}",
                    self.clone_method,
                    source_path.display()
                ))
            })?;

            // Persist directory entries.
            let _ = fsync_dir(&dir);
            let _ = fsync_dir(&self.volumes_dir);

            self.metrics.inc(METRIC_VOLUME_COUNT);
            Ok(())
        })();

        if result.is_err() {
            // clone_file removes only artefacts it created. Never unlink the
            // main path here: EEXIST means it belongs to another engine.
            let mut idx = self
                .name_index
                .write()
                .expect("reflink name_index lock poisoned");
            idx.remove(volume_name);
            if created_dir {
                let _ = std::fs::remove_dir(self.vol_dir(volume_name));
            }
        }
        result
    }

    fn volumes_dir_stats(&self) -> (u64, u64) {
        match statvfs_total_used(&self.volumes_dir) {
            Ok(v) => v,
            Err(e) => {
                warn!(
                    error = %e,
                    path = %self.volumes_dir.display(),
                    "statvfs failed; reporting 0 for reflink volumes_dir size"
                );
                (0, 0)
            }
        }
    }
}

// ---------------------------------------------------------------------------
// Engine trait impl
// ---------------------------------------------------------------------------

impl Engine for ReflinkEngine {
    fn create_volume(&self, name: &str, size_bytes: u64) -> CubecowResult<Volume> {
        Self::validate_name(name, "volume")?;

        // Reserve the name under the writer lock to make creation
        // atomic vs. concurrent create/delete on the same name.
        self.reserve_new_name(name, NameKind::Volume)?;

        // From here on, any failure must roll the index entry back so a
        // retry can succeed.
        let mut created_dir = false;
        let mut created_main = false;
        let result = (|| -> CubecowResult<Volume> {
            let dir = self.vol_dir(name);
            let main = self.vol_main_file(name);

            created_dir = ensure_dir_exists(&dir).map_err(CubecowError::IoError)?;

            let file = OpenOptions::new()
                .write(true)
                .create_new(true)
                .open(&main)
                .map_err(|e| {
                    if e.kind() == ErrorKind::AlreadyExists {
                        CubecowError::AlreadyExists(format!(
                            "volume main file '{}' already exists",
                            main.display()
                        ))
                    } else {
                        CubecowError::IoError(e)
                    }
                })?;
            created_main = true;
            file.set_len(size_bytes).map_err(CubecowError::IoError)?;
            file.sync_all().map_err(CubecowError::IoError)?;
            // fsync the parent directory so the new entry is durable.
            fsync_dir(&dir).map_err(CubecowError::IoError)?;
            fsync_dir(&self.volumes_dir).map_err(CubecowError::IoError)?;

            let volume = self.project_volume(name)?;
            self.metrics.inc(METRIC_VOLUME_COUNT);
            info!(volume = name, size_bytes, "reflink volume created");
            Ok(volume)
        })();

        if result.is_err() {
            // Roll back only paths this call created. An EEXIST destination
            // belongs to another engine sharing the JuiceFS namespace.
            let mut idx = self
                .name_index
                .write()
                .expect("reflink name_index lock poisoned");
            idx.remove(name);
            if created_main {
                let _ = std::fs::remove_file(self.vol_main_file(name));
            }
            if created_dir {
                let _ = std::fs::remove_dir(self.vol_dir(name));
            }
        }

        result
    }

    fn delete_volume(&self, name: &str) -> CubecowResult<()> {
        let _ = self.lookup_existing_name(name)?;
        // Take the writer lock, validate that it really is a volume,
        // then unlink under the lock so a concurrent create_snapshot_from_volume
        // cannot race in between.
        let mut idx = self
            .name_index
            .write()
            .expect("reflink name_index lock poisoned");

        match idx.get(name) {
            Some(NameKind::Volume) => {}
            Some(NameKind::Snapshot { .. }) => {
                return Err(CubecowError::InvalidArg(format!(
                    "'{name}' is a snapshot; use delete_snapshot instead"
                )));
            }
            None => {
                return Err(CubecowError::NotFound(format!("volume '{name}'")));
            }
        }

        // Unlike dm-thin (where origin & snapshot are independent thin
        // LVs), reflink snapshots are stored as sibling files inside
        // the origin's directory (`<volumes_dir>/<origin>/<snap>`).  We
        // historically refused to delete a volume that still had live
        // snapshots, but that breaks legitimate cleanup flows: when
        // Cubelet commits a template (snapshot `tpl-<id>-rootfs` from
        // origin `tpl-<id>-build-rootfs`) it then destroys the build
        // sandbox, which calls `delete_volume("tpl-<id>-build-rootfs")`
        // while the freshly-created template snapshot is still alive —
        // exactly the contract dm-thin allows.
        //
        // Since snapshot files are independent copies, deleting a volume
        // unlinks only its main file. XFS keeps its original index-only
        // decision; shared JuiceFS must decide from the directory itself
        // because another engine's snapshots need not be in this index.
        let indexed_live_snapshots = idx.iter().any(|(_, kind)| match kind {
            NameKind::Snapshot { origin_volume } => origin_volume == name,
            _ => false,
        });
        let main = self.vol_main_file(name);
        let dir = self.vol_dir(name);
        match std::fs::remove_file(&main) {
            Ok(()) => {}
            Err(e) if e.kind() == ErrorKind::NotFound => {}
            Err(e) => return Err(CubecowError::IoError(e)),
        }
        let has_live_snapshots = if self.clone_method == CloneMethod::Ficlone {
            if !indexed_live_snapshots {
                match std::fs::remove_dir(&dir) {
                    Ok(()) => {}
                    Err(e) if e.kind() == ErrorKind::NotFound => {}
                    Err(e) => return Err(CubecowError::IoError(e)),
                }
            }
            indexed_live_snapshots
        } else {
            match std::fs::remove_dir(&dir) {
                Ok(()) => false,
                Err(e)
                    if e.kind() == ErrorKind::NotFound
                        || e.kind() == ErrorKind::DirectoryNotEmpty =>
                {
                    e.kind() == ErrorKind::DirectoryNotEmpty
                }
                Err(e) => return Err(CubecowError::IoError(e)),
            }
        };
        let _ = fsync_dir(&self.volumes_dir);

        idx.remove(name);
        self.metrics.dec(METRIC_VOLUME_COUNT);
        info!(
            volume = name,
            live_snapshots = has_live_snapshots,
            "reflink volume deleted"
        );
        Ok(())
    }

    fn resize_volume(&self, name: &str, new_size_bytes: u64) -> CubecowResult<(u64, u64)> {
        // Only the volume's main file is resized; existing snapshot
        // files are independent reflink copies and intentionally keep
        // their old size — same observable behaviour as taking a
        // dm-thin snapshot before an origin resize.
        let _ = self.lookup_existing_name(name)?;
        let _guard = self
            .name_index
            .read()
            .expect("reflink name_index lock poisoned");
        match _guard.get(name) {
            Some(NameKind::Volume) => {}
            Some(NameKind::Snapshot { .. }) => {
                return Err(CubecowError::InvalidArg(format!(
                    "'{name}' is a snapshot; cannot resize"
                )));
            }
            None => {
                return Err(CubecowError::NotFound(format!("volume '{name}'")));
            }
        }

        let main = self.vol_main_file(name);
        let meta = std::fs::metadata(&main).map_err(CubecowError::IoError)?;
        let old_size = meta.size();
        if new_size_bytes < old_size {
            return Err(CubecowError::InvalidArg(format!(
                "shrinking is not supported (current={old_size}, requested={new_size_bytes})"
            )));
        }
        if new_size_bytes == old_size {
            return Ok((old_size, old_size));
        }

        let file = OpenOptions::new()
            .write(true)
            .open(&main)
            .map_err(CubecowError::IoError)?;
        file.set_len(new_size_bytes)
            .map_err(CubecowError::IoError)?;
        file.sync_all().map_err(CubecowError::IoError)?;
        info!(
            volume = name,
            old_size,
            new_size = new_size_bytes,
            "reflink volume resized"
        );
        Ok((old_size, new_size_bytes))
    }

    fn get_volume_info(&self, name: &str) -> CubecowResult<Volume> {
        // Match dm-thin's contract: `get_volume_info` is the one-shot
        // "give me size + device path for this name" entry point that
        // upper layers (notably Cubelet's `CubecowVolumeManager.GetSizeBytes`
        // / `ResolveDevPath`) call regardless of whether the name refers
        // to a volume or a snapshot.  In dm-thin both kinds are the same
        // LV behind the scenes, so the call always succeeds; refusing
        // snapshots here breaks every flow that touches a template
        // rootfs (e.g. `CommitTemplateRootfs` → `GetSizeBytes`,
        // immediately on a freshly-created `tpl-<id>-rootfs` snapshot).
        //
        // We therefore mirror `activate_volume` below: snapshot names
        // are projected through the snapshot view and re-shaped as a
        // `Volume`.  The origin chain is intentionally not surfaced
        // here; callers that need it should use `list_snapshots` /
        // `get_snapshot_info` directly.
        let _ = self.lookup_existing_name(name)?;
        let idx = self
            .name_index
            .read()
            .expect("reflink name_index lock poisoned");
        match idx.get(name) {
            Some(NameKind::Volume) => {
                drop(idx);
                self.project_volume(name)
            }
            Some(NameKind::Snapshot { .. }) => {
                drop(idx);
                let snap = self.project_snapshot(name)?;
                Ok(Volume {
                    name: snap.name,
                    size_bytes: snap.size_bytes,
                    device_path: snap.device_path,
                    snapshot_count: 0,
                    created_at: snap.created_at,
                    export_uuid: snap.export_uuid,
                    export_status: snap.export_status,
                    deletable: snap.deletable,
                })
            }
            None => Err(CubecowError::NotFound(format!(
                "volume or snapshot '{name}'"
            ))),
        }
    }

    fn get_volume_block_info(&self, name: &str) -> CubecowResult<VolumeBlockInfo> {
        let vol = self.get_volume_info(name)?;
        Ok(VolumeBlockInfo {
            num_blocks: vol.size_bytes / REFLINK_BLOCK_SIZE as u64,
            block_size: REFLINK_BLOCK_SIZE,
        })
    }

    fn list_volumes(
        &self,
        page_size: usize,
        page_token: Option<&str>,
    ) -> (Vec<Volume>, Option<String>, usize) {
        // Snapshot-of-index → sorted name list, then materialise pages.
        let mut names: Vec<String> = {
            let idx = self
                .name_index
                .read()
                .expect("reflink name_index lock poisoned");
            idx.iter()
                .filter(|(_, kind)| matches!(kind, NameKind::Volume))
                .map(|(name, _)| name.clone())
                .collect()
        };
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
            match self.project_volume(name) {
                Ok(v) => out.push(v),
                Err(e) => {
                    // The on-disk view diverged from our index (e.g.
                    // someone rm'd the directory under us). Log and
                    // skip; the next reset/restart will resync.
                    warn!(volume = %name, error = %e, "reflink volume disappeared mid-list");
                }
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
        _activate: bool,
    ) -> CubecowResult<Snapshot> {
        // `activate` is irrelevant for the reflink backend: the device
        // path *is* the file path, and the file exists from the moment
        // FICLONE returns. We accept the parameter for trait-shape
        // parity with dm-thin but ignore it.
        Self::validate_name(snapshot_name, "snapshot")?;
        let _ = self.lookup_existing_name(source_name)?;

        // Resolve source: it may be either a volume (FICLONE the main
        // file) or another snapshot (FICLONE the snapshot file, but
        // place the result under the ultimate origin directory).
        let (source_path, ultimate_origin) = {
            let idx = self
                .name_index
                .read()
                .expect("reflink name_index lock poisoned");
            match idx.get(source_name) {
                Some(NameKind::Volume) => {
                    (self.vol_main_file(source_name), source_name.to_string())
                }
                Some(NameKind::Snapshot { origin_volume }) => (
                    self.snap_file(origin_volume, source_name),
                    origin_volume.clone(),
                ),
                None => {
                    return Err(CubecowError::NotFound(format!(
                        "source '{source_name}' for snapshot"
                    )));
                }
            }
        };

        // Reserve the snapshot name atomically.
        self.reserve_new_name(
            snapshot_name,
            NameKind::Snapshot {
                origin_volume: ultimate_origin.clone(),
            },
        )?;

        let result = (|| -> CubecowResult<Snapshot> {
            let dst = self.snap_file(&ultimate_origin, snapshot_name);

            let src_file = File::open(&source_path).map_err(|e| {
                if e.kind() == ErrorKind::NotFound {
                    CubecowError::NotFound(format!(
                        "source file '{}' missing for snapshot",
                        source_path.display()
                    ))
                } else {
                    CubecowError::IoError(e)
                }
            })?;

            clone_file(self.clone_method, &src_file, &dst).map_err(|errno| {
                let reason = describe_ficlone_errno(errno);
                CubecowError::PreconditionFailed(format!(
                    "{:?} clone failed for snapshot '{snapshot_name}' from '{}': {reason}",
                    self.clone_method,
                    source_path.display()
                ))
            })?;

            // Persist directory entry for the new snapshot file.
            let _ = fsync_dir(&self.vol_dir(&ultimate_origin));

            self.metrics.inc(METRIC_SNAPSHOT_COUNT);
            info!(
                snapshot = snapshot_name,
                source = source_name,
                origin_volume = %ultimate_origin,
                "reflink snapshot created"
            );
            self.project_snapshot(snapshot_name)
        })();

        if result.is_err() {
            let mut idx = self
                .name_index
                .write()
                .expect("reflink name_index lock poisoned");
            idx.remove(snapshot_name);
            // ficlone() removes the dst file on failure; nothing else
            // to clean up.
        }
        result
    }

    fn create_volume_from_snapshot(
        &self,
        source_snapshot: &str,
        volume_name: &str,
    ) -> CubecowResult<Volume> {
        // The reflink backend has no separate activation step: the
        // device path *is* the file path, and the file exists from
        // the moment FICLONE returns.
        Self::validate_name(volume_name, "volume")?;
        let _ = self.lookup_existing_name(source_snapshot)?;

        // XFS: volume and snapshot are both independent FICLONE files.
        // Clone from either. (S3 keeps a real snapshot vs volume split.)
        let source_path = {
            let idx = self
                .name_index
                .read()
                .expect("reflink name_index lock poisoned");
            match idx.get(source_snapshot) {
                Some(NameKind::Snapshot { origin_volume }) => {
                    self.snap_file(origin_volume, source_snapshot)
                }
                Some(NameKind::Volume) => self.vol_main_file(source_snapshot),
                None => {
                    return Err(CubecowError::NotFound(format!(
                        "snapshot '{source_snapshot}'"
                    )));
                }
            }
        };
        self.clone_into_new_volume(volume_name, &source_path)?;
        info!(
            volume = volume_name,
            source_snapshot, "reflink writable volume derived from snapshot"
        );
        self.project_volume(volume_name)
    }

    fn delete_snapshot(&self, snapshot_name: &str) -> CubecowResult<()> {
        let _ = self.lookup_existing_name(snapshot_name)?;
        let mut idx = self
            .name_index
            .write()
            .expect("reflink name_index lock poisoned");
        let origin = match idx.get(snapshot_name) {
            Some(NameKind::Snapshot { origin_volume }) => origin_volume.clone(),
            Some(NameKind::Volume) => {
                return Err(CubecowError::InvalidArg(format!(
                    "'{snapshot_name}' is a volume; use delete_volume instead"
                )));
            }
            None => {
                return Err(CubecowError::NotFound(format!(
                    "snapshot '{snapshot_name}'"
                )));
            }
        };

        let path = self.snap_file(&origin, snapshot_name);
        match std::fs::remove_file(&path) {
            Ok(()) => {}
            Err(e) if e.kind() == ErrorKind::NotFound => {}
            Err(e) => return Err(CubecowError::IoError(e)),
        }
        let _ = fsync_dir(&self.vol_dir(&origin));

        idx.remove(snapshot_name);
        self.metrics.dec(METRIC_SNAPSHOT_COUNT);

        let should_try_reap = if self.clone_method == CloneMethod::Ficlone {
            let origin_alive = idx.contains_key(&origin);
            let still_has_siblings = idx.iter().any(|(_, kind)| match kind {
                NameKind::Snapshot { origin_volume } => origin_volume == &origin,
                _ => false,
            });
            !origin_alive && !still_has_siblings
        } else {
            true
        };
        if should_try_reap {
            // In shared mode, remove_dir is the authoritative empty check.
            let dir = self.vol_dir(&origin);
            match std::fs::remove_dir(&dir) {
                Ok(()) => {
                    let _ = fsync_dir(&self.volumes_dir);
                }
                Err(e)
                    if e.kind() == ErrorKind::NotFound
                        || (self.clone_method != CloneMethod::Ficlone
                            && e.kind() == ErrorKind::DirectoryNotEmpty) => {}
                Err(e) => {
                    warn!(
                        origin_volume = %origin,
                        error = %e,
                        "reflink: failed to reap origin dir after snapshot delete",
                    );
                }
            }
        }

        info!(snapshot = snapshot_name, origin_volume = %origin, "reflink snapshot deleted");
        Ok(())
    }

    fn list_snapshots(
        &self,
        volume_name: &str,
        page_size: usize,
        page_token: Option<&str>,
    ) -> (Vec<Snapshot>, Option<String>) {
        // Volume must exist; missing volume → empty page (matches the
        // dm-thin backend which would also surface no snapshots).
        let mut names: Vec<String> = {
            let idx = self
                .name_index
                .read()
                .expect("reflink name_index lock poisoned");
            if !matches!(idx.get(volume_name), Some(NameKind::Volume)) {
                return (Vec::new(), None);
            }
            idx.iter()
                .filter_map(|(name, kind)| match kind {
                    NameKind::Snapshot { origin_volume } if origin_volume == volume_name => {
                        Some(name.clone())
                    }
                    _ => None,
                })
                .collect()
        };
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
            match self.project_snapshot(name) {
                Ok(s) => out.push(s),
                Err(e) => {
                    warn!(snapshot = %name, error = %e, "reflink snapshot disappeared mid-list");
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
        // The reflink backend has no notion of activation: every
        // volume / snapshot file is always reachable via its filesystem
        // path. We project the entry as a "volume-shaped" view so the
        // call site behaves identically to dm-thin's
        // [`Engine::activate_volume`] (which returns a [`Volume`] for
        // both volume *and* snapshot names).
        let _ = self.lookup_existing_name(name)?;
        let idx = self
            .name_index
            .read()
            .expect("reflink name_index lock poisoned");
        match idx.get(name) {
            Some(NameKind::Volume) => {
                drop(idx);
                self.project_volume(name)
            }
            Some(NameKind::Snapshot { .. }) => {
                drop(idx);
                let snap = self.project_snapshot(name)?;
                // Surface the snapshot using the Volume shape so
                // callers that hold a `Volume` after `activate_volume`
                // (matching dm-thin's behaviour) see the device path
                // and size. The origin volume is intentionally not
                // re-exposed here: it is already available via
                // `list_snapshots` / `Snapshot::origin_volume`.
                Ok(Volume {
                    name: snap.name,
                    size_bytes: snap.size_bytes,
                    device_path: snap.device_path,
                    snapshot_count: 0,
                    created_at: snap.created_at,
                    export_uuid: snap.export_uuid,
                    export_status: snap.export_status,
                    deletable: snap.deletable,
                })
            }
            None => Err(CubecowError::NotFound(format!(
                "volume or snapshot '{name}'"
            ))),
        }
    }

    fn deactivate_volume(&self, name: &str) -> CubecowResult<()> {
        // No-op for reflink: there is no kernel device node to tear
        // down. We still validate the name so callers receive a
        // consistent NotFound error if they pass garbage.
        let _ = self.lookup_existing_name(name)?;
        let idx = self
            .name_index
            .read()
            .expect("reflink name_index lock poisoned");
        if idx.get(name).is_some() {
            debug!(name, "reflink deactivate_volume is a no-op");
            Ok(())
        } else {
            Err(CubecowError::NotFound(format!(
                "volume or snapshot '{name}'"
            )))
        }
    }

    fn reset_node_storage(&self) -> CubecowResult<()> {
        // Wipe everything under `volumes/` and rebuild an empty index.
        // Best-effort; partial failures bubble up so the caller knows
        // they need to investigate.
        let mut idx = self
            .name_index
            .write()
            .expect("reflink name_index lock poisoned");

        // Iterate a *copy* of the current entries so we can drop them
        // from the index as we go.
        let entries: Vec<String> = std::fs::read_dir(&self.volumes_dir)
            .map_err(CubecowError::IoError)?
            .filter_map(|e| e.ok())
            .filter_map(|e| {
                e.file_type().ok().and_then(|ft| {
                    if ft.is_dir() {
                        e.file_name().into_string().ok()
                    } else {
                        None
                    }
                })
            })
            .collect();

        for name in &entries {
            let dir = self.volumes_dir.join(name);
            if let Err(e) = std::fs::remove_dir_all(&dir) {
                if e.kind() != ErrorKind::NotFound {
                    return Err(CubecowError::IoError(e));
                }
            }
        }
        let _ = fsync_dir(&self.volumes_dir);

        idx.clear();
        self.metrics.set(METRIC_VOLUME_COUNT, 0);
        self.metrics.set(METRIC_SNAPSHOT_COUNT, 0);
        info!(
            volumes_dir = %self.volumes_dir.display(),
            cleared = entries.len(),
            "reflink node storage reset"
        );
        Ok(())
    }

    /// Clone a file from another node's tree on the same shared file system
    /// into a new volume. `export_uuid` is the absolute path of the source
    /// file: the reflink backend has no export step, so a path is all
    /// another node needs to hand over.
    fn import_lvol(&self, lvol_name: &str, export_uuid: &str) -> CubecowResult<Volume> {
        Self::validate_name(lvol_name, "volume")?;
        let source_path = PathBuf::from(export_uuid.trim());
        if !source_path.is_absolute() {
            return Err(CubecowError::InvalidArg(format!(
                "import source '{export_uuid}' must be an absolute path"
            )));
        }
        let meta = std::fs::metadata(&source_path).map_err(|e| {
            if e.kind() == ErrorKind::NotFound {
                CubecowError::NotFound(format!("import source '{}'", source_path.display()))
            } else {
                CubecowError::IoError(e)
            }
        })?;
        if !meta.is_file() {
            return Err(CubecowError::InvalidArg(format!(
                "import source '{}' is not a regular file",
                source_path.display()
            )));
        }
        self.clone_into_new_volume(lvol_name, &source_path)?;
        info!(
            volume = lvol_name,
            source = %source_path.display(),
            "reflink volume imported"
        );
        self.project_volume(lvol_name)
    }

    fn metrics(&self) -> HashMap<String, u64> {
        // Refresh the size counters so callers see live filesystem
        // utilisation without a separate health-check entry point.
        let (total, used) = self.volumes_dir_stats();
        self.metrics.set(METRIC_TOTAL_BYTES, total);
        self.metrics.set(METRIC_USED_BYTES, used);
        self.metrics.snapshot()
    }
}

// ---------------------------------------------------------------------------
// Filesystem helpers
// ---------------------------------------------------------------------------

/// Ensure `path` is a directory and report whether this call created it.
/// Callers use the ownership bit to avoid deleting another engine's directory
/// while rolling back a failed shared-namespace operation.
fn ensure_dir_exists(path: &Path) -> std::io::Result<bool> {
    match std::fs::create_dir(path) {
        Ok(()) => Ok(true),
        Err(e) if e.kind() == ErrorKind::AlreadyExists => match std::fs::metadata(path) {
            Ok(meta) if meta.is_dir() => Ok(false),
            Ok(_) => Err(e),
            Err(meta_err) => Err(meta_err),
        },
        Err(e) => Err(e),
    }
}

/// Clone `src` into a freshly-created `dst_path` with `method`.
fn clone_file(method: CloneMethod, src: &File, dst_path: &Path) -> Result<(), i32> {
    match method {
        CloneMethod::Ficlone => ficlone(src, dst_path),
        CloneMethod::CopyFileRange => copy_file_range_clone(src, dst_path),
    }
}

/// Copy all of `src` into `dst_path` with `copy_file_range(2)`.
///
/// The copy lands in a dot-prefixed sibling first and is renamed into
/// place only once complete, so a crash mid-copy leaves a dotfile the
/// layout scanner ignores rather than a short volume or snapshot.
fn copy_file_range_clone(src: &File, dst_path: &Path) -> Result<(), i32> {
    let errno_of = |e: std::io::Error| e.raw_os_error().unwrap_or(libc::EIO);
    if dst_path.exists() {
        return Err(libc::EEXIST);
    }
    let file_name = dst_path
        .file_name()
        .and_then(|n| n.to_str())
        .ok_or(libc::EINVAL)?;
    let tmp_path = dst_path.with_file_name(format!(".{file_name}.clone-tmp"));
    let _ = std::fs::remove_file(&tmp_path);

    let result = (|| -> Result<(), i32> {
        let size = src.metadata().map_err(errno_of)?.len();
        let dst = OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&tmp_path)
            .map_err(errno_of)?;
        let mut off_in: libc::loff_t = 0;
        let mut off_out: libc::loff_t = 0;
        while (off_in as u64) < size {
            let chunk = std::cmp::min(size - off_in as u64, 1 << 30) as usize;
            // SAFETY: both fds stay open for the call; the offsets are
            // plain integers owned by this frame.
            let n = unsafe {
                libc::copy_file_range(
                    src.as_raw_fd(),
                    &mut off_in,
                    dst.as_raw_fd(),
                    &mut off_out,
                    chunk,
                    0,
                )
            };
            if n < 0 {
                return Err(std::io::Error::last_os_error()
                    .raw_os_error()
                    .unwrap_or(libc::EIO));
            }
            if n == 0 {
                return Err(libc::EIO);
            }
        }
        dst.sync_all().map_err(errno_of)?;
        drop(dst);
        std::fs::rename(&tmp_path, dst_path).map_err(errno_of)
    })();
    if result.is_err() {
        let _ = std::fs::remove_file(&tmp_path);
    }
    result
}

/// Issue a `FICLONE` ioctl from `src` into a freshly-created `dst_path`.
///
/// On error, the partially-created destination is removed so the caller
/// can retry without tripping over a stale entry.
fn ficlone(src: &File, dst_path: &Path) -> Result<(), i32> {
    let dst = OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(dst_path)
        .map_err(|e| e.raw_os_error().unwrap_or(libc::EIO))?;

    // SAFETY: `ioctl` takes a single integer arg (the source fd). Both
    // fds are owned for the entire call duration.
    let rc = unsafe { libc::ioctl(dst.as_raw_fd(), FICLONE, src.as_raw_fd()) };
    if rc != 0 {
        let errno = std::io::Error::last_os_error()
            .raw_os_error()
            .unwrap_or(libc::EIO);
        let _ = std::fs::remove_file(dst_path);
        return Err(errno);
    }
    Ok(())
}

/// Probe `dir` for FICLONE support by performing a tiny dummy clone.
///
/// Returns `Ok(())` only if the kernel/filesystem accepts FICLONE on
/// this directory; otherwise returns a human-readable explanation.
#[cfg(test)]
fn probe_reflink_support(dir: &Path) -> Result<(), String> {
    probe_clone_support(CloneMethod::Ficlone, dir)
}

fn probe_clone_support(method: CloneMethod, dir: &Path) -> Result<(), String> {
    let pid = std::process::id();
    let src_path = dir.join(format!(".cubecow-reflink-probe-src-{pid}"));
    let dst_path = dir.join(format!(".cubecow-reflink-probe-dst-{pid}"));
    let _ = std::fs::remove_file(&src_path);
    let _ = std::fs::remove_file(&dst_path);

    {
        let mut f = File::create(&src_path).map_err(|e| format!("create probe src: {e}"))?;
        f.write_all(&[0u8; 4096])
            .map_err(|e| format!("write probe src: {e}"))?;
        let _ = f.sync_all();
    }
    let src = File::open(&src_path).map_err(|e| format!("open probe src: {e}"))?;
    let result = clone_file(method, &src, &dst_path);
    let _ = std::fs::remove_file(&src_path);
    let _ = std::fs::remove_file(&dst_path);

    match result {
        Ok(()) => Ok(()),
        Err(errno) => Err(describe_ficlone_errno(errno)),
    }
}

fn describe_ficlone_errno(errno: i32) -> String {
    match errno {
        libc::EOPNOTSUPP => "EOPNOTSUPP — filesystem does not support reflink".to_string(),
        libc::EINVAL => {
            "EINVAL — kernel/filesystem rejected FICLONE (no reflink support)".to_string()
        }
        libc::EXDEV => "EXDEV — cross-filesystem clone".to_string(),
        libc::ENOSPC => "ENOSPC — filesystem out of space".to_string(),
        _ => format!("errno={errno}"),
    }
}

/// fsync the *directory* `path` so a freshly added/removed entry under
/// it is durable. Best-effort; failure to fsync a directory is a soft
/// error, not a data-loss event, so we surface it but don't corrupt
/// the operation result.
fn fsync_dir(path: &Path) -> std::io::Result<()> {
    let dir = File::open(path)?;
    dir.sync_all()
}

/// Convert an mtime SystemTime into an RFC3339 string so it matches
/// the format produced by the dm-thin backend.
fn rfc3339_from_meta(meta: &std::fs::Metadata) -> String {
    let st = meta
        .modified()
        .or_else(|_| meta.created())
        .unwrap_or(SystemTime::UNIX_EPOCH);
    let dt: DateTime<Utc> = st
        .duration_since(UNIX_EPOCH)
        .ok()
        .and_then(|d| DateTime::<Utc>::from_timestamp(d.as_secs() as i64, d.subsec_nanos()))
        .unwrap_or_else(|| DateTime::<Utc>::from_timestamp(0, 0).unwrap());
    dt.to_rfc3339()
}

/// Count snapshot files inside `<volumes_dir>/<vol>/`, i.e. every entry
/// that is *not* the volume's main file (`<vol>`).
fn count_snapshots_on_disk(vol_dir: &Path, volume_name: &str) -> CubecowResult<i32> {
    let mut count = 0i32;
    let read = match std::fs::read_dir(vol_dir) {
        Ok(r) => r,
        Err(e) if e.kind() == ErrorKind::NotFound => return Ok(0),
        Err(e) => return Err(CubecowError::IoError(e)),
    };
    for entry in read {
        let entry = entry.map_err(CubecowError::IoError)?;
        let name = match entry.file_name().into_string() {
            Ok(s) => s,
            Err(_) => continue,
        };
        if name == volume_name {
            continue;
        }
        if name.starts_with('.') {
            continue;
        }
        count = count.saturating_add(1);
    }
    Ok(count)
}

/// Linux `statvfs` to derive (total_bytes, used_bytes) for the
/// filesystem hosting `path`.
fn statvfs_total_used(path: &Path) -> std::io::Result<(u64, u64)> {
    let cpath = CString::new(path.as_os_str().as_encoded_bytes())
        .map_err(|_| std::io::Error::new(ErrorKind::InvalidInput, "path contains NUL"))?;
    // SAFETY: zeroed `statvfs` is a valid initial state; we pass a raw
    // pointer to the struct's storage to the libc call.
    let mut stat: libc::statvfs = unsafe { std::mem::zeroed() };
    let rc = unsafe { libc::statvfs(cpath.as_ptr(), &mut stat) };
    if rc != 0 {
        return Err(std::io::Error::last_os_error());
    }
    let frsize = stat.f_frsize as u64;
    let total = stat.f_blocks as u64 * frsize;
    let avail = stat.f_bfree as u64 * frsize;
    let used = total.saturating_sub(avail);
    Ok((total, used))
}

// ---------------------------------------------------------------------------
// Startup scan: rebuild the in-memory name index from the layout.
//
// Crash-recovery contract:
//   * `<volumes_dir>/<vol>/<vol>` exists       → register `<vol>` as a Volume.
//   * `<volumes_dir>/<vol>/` exists but the
//     `<vol>` main file inside is missing      → `<vol>` was either never
//                                                 finished being created or
//                                                 was deleted with main file
//                                                 unlinked but dir still
//                                                 around. Treat as orphan and
//                                                 remove the directory only if
//                                                 it has no other files left
//                                                 (otherwise leave it alone
//                                                 and warn).
//   * `<volumes_dir>/<vol>/<other>` files
//     where `<vol>` main file *does* exist     → register `<other>` as a
//                                                 Snapshot of `<vol>`.
//   * Zero-byte snapshot files when the
//     origin's main file is non-zero           → orphan from a crashed FICLONE;
//                                                 unlink.
fn discover_name_on_disk(volumes_dir: &Path, name: &str) -> std::io::Result<Option<NameKind>> {
    let direct_main = volumes_dir.join(name).join(name);
    match std::fs::metadata(&direct_main) {
        Ok(meta) if meta.is_file() => return Ok(Some(NameKind::Volume)),
        Ok(_) => {}
        Err(e) if e.kind() == ErrorKind::NotFound => {}
        Err(e) => return Err(e),
    }

    let entries = match std::fs::read_dir(volumes_dir) {
        Ok(entries) => entries,
        Err(e) if e.kind() == ErrorKind::NotFound => return Ok(None),
        Err(e) => return Err(e),
    };
    for entry in entries {
        let entry = entry?;
        if !entry.file_type()?.is_dir() {
            continue;
        }
        let origin = match entry.file_name().into_string() {
            Ok(origin) if !origin.starts_with('.') && origin != name => origin,
            _ => continue,
        };
        let candidate = entry.path().join(name);
        match std::fs::metadata(candidate) {
            Ok(meta) if meta.is_file() => {
                return Ok(Some(NameKind::Snapshot {
                    origin_volume: origin,
                }));
            }
            Ok(_) => {}
            Err(e) if e.kind() == ErrorKind::NotFound => {}
            Err(e) => return Err(e),
        }
    }
    Ok(None)
}

#[cfg(test)]
fn scan_and_rebuild_index(volumes_dir: &Path) -> std::io::Result<HashMap<String, NameKind>> {
    scan_index(volumes_dir, true)
}

// Shared JuiceFS startup scans are read-only because another engine may be
// between creating a directory and atomically publishing its main file.
fn scan_index(
    volumes_dir: &Path,
    cleanup_orphans: bool,
) -> std::io::Result<HashMap<String, NameKind>> {
    let mut index: HashMap<String, NameKind> = HashMap::new();

    let read = match std::fs::read_dir(volumes_dir) {
        Ok(r) => r,
        Err(e) if e.kind() == ErrorKind::NotFound => return Ok(index),
        Err(e) => return Err(e),
    };

    for entry in read {
        let entry = entry?;
        let file_type = entry.file_type()?;
        if !file_type.is_dir() {
            // Stray file at the top level — leave it untouched but
            // warn so an operator can investigate.
            warn!(
                path = %entry.path().display(),
                "unexpected non-directory entry under reflink volumes dir; ignoring"
            );
            continue;
        }
        let vol_name = match entry.file_name().into_string() {
            Ok(s) => s,
            Err(_) => continue,
        };
        if vol_name.starts_with('.') {
            continue;
        }

        let vol_dir = entry.path();
        let main_path = vol_dir.join(&vol_name);
        let main_present = match std::fs::metadata(&main_path) {
            Ok(_) => true,
            Err(e) if e.kind() == ErrorKind::NotFound => false,
            Err(e) => return Err(e),
        };

        if !main_present {
            let leftovers = read_dir_count_non_dotfiles(&vol_dir)?;
            if leftovers == 0 {
                if cleanup_orphans {
                    let _ = std::fs::remove_dir(&vol_dir);
                    warn!(
                        path = %vol_dir.display(),
                        "removed orphan empty volume directory"
                    );
                }
                continue;
            }
            warn!(
                path = %vol_dir.display(),
                "volume main file missing but snapshots remain; recovering snapshots without registering volume"
            );
        } else {
            index.insert(vol_name.clone(), NameKind::Volume);
        }

        // Enumerate snapshots within this volume directory.
        let snap_iter = std::fs::read_dir(&vol_dir)?;
        for sub in snap_iter {
            let sub = sub?;
            if !sub.file_type()?.is_file() {
                continue;
            }
            let sub_name = match sub.file_name().into_string() {
                Ok(s) => s,
                Err(_) => continue,
            };
            if sub_name == vol_name {
                continue; // main file
            }
            if sub_name.starts_with('.') {
                continue;
            }

            let sub_meta = sub.metadata()?;
            if sub_meta.size() == 0 {
                // A shared-engine startup can observe another engine between
                // creat and clone. Local XFS recovery may remove the crash
                // artefact; shared JuiceFS startup leaves it untouched.
                if cleanup_orphans {
                    let _ = std::fs::remove_file(sub.path());
                    warn!(
                        path = %sub.path().display(),
                        "removed orphan zero-byte snapshot file"
                    );
                }
                continue;
            }

            // Defensive: ensure the snapshot name doesn't already
            // exist (it shouldn't, since paths are unique, but the
            // index is global-namespace).
            if index.contains_key(&sub_name) {
                warn!(
                    path = %sub.path().display(),
                    snapshot = %sub_name,
                    "duplicate name in reflink namespace during recovery; skipping"
                );
                continue;
            }
            index.insert(
                sub_name,
                NameKind::Snapshot {
                    origin_volume: vol_name.clone(),
                },
            );
        }
    }

    Ok(index)
}

fn read_dir_count_non_dotfiles(dir: &Path) -> std::io::Result<usize> {
    let mut n = 0usize;
    for e in std::fs::read_dir(dir)? {
        let e = e?;
        if let Ok(name) = e.file_name().into_string() {
            if !name.starts_with('.') {
                n += 1;
            }
        }
    }
    Ok(n)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------
#[cfg(test)]
mod tests {
    use super::*;

    fn fs_supports_ficlone(dir: &Path) -> bool {
        std::fs::create_dir_all(dir).unwrap();
        probe_reflink_support(dir).is_ok()
    }

    fn unique_root(label: &str) -> PathBuf {
        let pid = std::process::id();
        let nanos = std::time::SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_nanos();
        std::env::temp_dir().join(format!("cubecow-reflink-test-{label}-{pid}-{nanos}"))
    }

    fn make_engine(root: &Path) -> ReflinkEngine {
        let volumes_dir = root.join("volumes");
        std::fs::create_dir_all(&volumes_dir).unwrap();
        ReflinkEngine {
            volumes_dir,
            clone_method: CloneMethod::Ficlone,
            name_index: RwLock::new(HashMap::new()),
            metrics: Arc::new(MetricsCollector::new()),
        }
    }

    #[test]
    fn copy_file_range_engine_clones_volume_and_snapshot() {
        // copy_file_range works on any local filesystem (as a data copy
        // where it is not a metadata clone), so this needs no FICLONE.
        let root = unique_root("cfr");
        let mut engine = make_engine(&root);
        engine.clone_method = CloneMethod::CopyFileRange;

        let vol = engine.create_volume("v1", 3 * 4096).unwrap();
        {
            use std::os::unix::fs::FileExt;
            let f = OpenOptions::new()
                .write(true)
                .open(&vol.device_path)
                .unwrap();
            f.write_all_at(b"cube-cfr", 4096).unwrap();
        }
        let snap = engine
            .create_snapshot_from_volume("v1", "s1", true)
            .unwrap();
        let derived = engine.create_volume_from_snapshot("s1", "v2").unwrap();
        for path in [&snap.device_path, &derived.device_path] {
            let data = std::fs::read(path).unwrap();
            assert_eq!(data.len(), 3 * 4096);
            assert_eq!(&data[4096..4104], b"cube-cfr");
        }
        let leftovers: Vec<_> = std::fs::read_dir(root.join("volumes/v1"))
            .unwrap()
            .filter_map(|e| e.ok())
            .filter(|e| e.file_name().to_string_lossy().starts_with('.'))
            .collect();
        assert!(leftovers.is_empty(), "temp files left: {leftovers:?}");
        assert!(matches!(
            engine.create_snapshot_from_volume("v1", "s1", true),
            Err(CubecowError::AlreadyExists(_))
        ));
        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn import_lvol_clones_a_foreign_file_into_a_volume() {
        let root = unique_root("import");
        let mut engine = make_engine(&root);
        engine.clone_method = CloneMethod::CopyFileRange;

        let foreign = root.join("other-node-snapshot");
        {
            use std::os::unix::fs::FileExt;
            let f = File::create(&foreign).unwrap();
            f.set_len(2 * 4096).unwrap();
            f.write_all_at(b"from-a", 4096).unwrap();
        }
        let vol = engine
            .import_lvol("sb-x-rootfs-gen0", foreign.to_str().unwrap())
            .unwrap();
        let data = std::fs::read(&vol.device_path).unwrap();
        assert_eq!(data.len(), 2 * 4096);
        assert_eq!(&data[4096..4102], b"from-a");
        assert!(engine.get_volume_info("sb-x-rootfs-gen0").is_ok());
        assert!(matches!(
            engine.import_lvol("sb-x-rootfs-gen0", foreign.to_str().unwrap()),
            Err(CubecowError::AlreadyExists(_))
        ));
        assert!(matches!(
            engine.import_lvol("sb-y", "relative/path"),
            Err(CubecowError::InvalidArg(_))
        ));
        assert!(matches!(
            engine.import_lvol("sb-z", root.join("missing").to_str().unwrap()),
            Err(CubecowError::NotFound(_))
        ));
        assert!(engine.get_volume_info("sb-z").is_err());
        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn two_engines_share_created_volumes_and_snapshots() {
        let root = unique_root("shared-discovery");
        let mut first = make_engine(&root);
        let mut second = make_engine(&root);
        first.clone_method = CloneMethod::CopyFileRange;
        second.clone_method = CloneMethod::CopyFileRange;

        first.create_volume("shared-vol", 2 * 4096).unwrap();
        assert_eq!(
            second.get_volume_info("shared-vol").unwrap().size_bytes,
            2 * 4096
        );

        first
            .create_snapshot_from_volume("shared-vol", "shared-snap", false)
            .unwrap();
        assert_eq!(
            second.get_volume_info("shared-snap").unwrap().name,
            "shared-snap"
        );
        let (snapshots, _) = second.list_snapshots("shared-vol", 0, None);
        assert_eq!(snapshots.len(), 1);
        assert_eq!(snapshots[0].name, "shared-snap");

        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn shared_source_cache_hit_is_invalidated_after_remote_delete() {
        let root = unique_root("shared-source-invalidation");
        let mut first = make_engine(&root);
        let mut second = make_engine(&root);
        first.clone_method = CloneMethod::CopyFileRange;
        second.clone_method = CloneMethod::CopyFileRange;

        second.create_volume("shared-vol", 4096).unwrap();
        assert!(first.get_volume_info("shared-vol").is_ok());
        second.delete_volume("shared-vol").unwrap();
        assert!(matches!(
            first.get_volume_info("shared-vol"),
            Err(CubecowError::NotFound(_))
        ));

        second.create_volume("shared-vol", 2 * 4096).unwrap();
        assert_eq!(
            first.get_volume_info("shared-vol").unwrap().size_bytes,
            2 * 4096
        );

        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn shared_stale_destination_does_not_block_serialized_recreate() {
        let root = unique_root("shared-destination-recreate");
        let mut first = make_engine(&root);
        let mut second = make_engine(&root);
        first.clone_method = CloneMethod::CopyFileRange;
        second.clone_method = CloneMethod::CopyFileRange;

        second.create_volume("origin", 4096).unwrap();
        second
            .create_snapshot_from_volume("origin", "pause-source", false)
            .unwrap();
        second
            .create_snapshot_from_volume("pause-source", "sb-rootfs-gen0", false)
            .unwrap();
        assert!(first.get_volume_info("sb-rootfs-gen0").is_ok());

        second.delete_snapshot("sb-rootfs-gen0").unwrap();
        let recreated = first
            .create_snapshot_from_volume("pause-source", "sb-rootfs-gen0", false)
            .unwrap();
        assert_eq!(recreated.name, "sb-rootfs-gen0");
        assert!(Path::new(&recreated.device_path).is_file());

        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn shared_volume_delete_uses_directory_state_for_unknown_snapshots() {
        let root = unique_root("shared-delete-directory-state");
        let mut first = make_engine(&root);
        let mut second = make_engine(&root);
        first.clone_method = CloneMethod::CopyFileRange;
        second.clone_method = CloneMethod::CopyFileRange;

        second.create_volume("shared-vol", 4096).unwrap();
        second
            .create_snapshot_from_volume("shared-vol", "unknown-to-first", false)
            .unwrap();
        assert!(first.get_volume_info("shared-vol").is_ok());

        first.delete_volume("shared-vol").unwrap();
        let dir = root.join("volumes").join("shared-vol");
        assert!(!dir.join("shared-vol").exists());
        assert!(dir.join("unknown-to-first").is_file());

        second.delete_snapshot("unknown-to-first").unwrap();
        assert!(!dir.exists());

        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn shared_duplicate_create_does_not_delete_existing_volume() {
        let root = unique_root("shared-duplicate-create");
        let mut first = make_engine(&root);
        let mut second = make_engine(&root);
        first.clone_method = CloneMethod::CopyFileRange;
        second.clone_method = CloneMethod::CopyFileRange;

        let original = first.create_volume("shared-vol", 4096).unwrap();
        std::fs::write(&original.device_path, b"original-data").unwrap();

        assert!(matches!(
            second.create_volume("shared-vol", 4096),
            Err(CubecowError::AlreadyExists(_))
        ));
        assert_eq!(
            std::fs::read(&original.device_path).unwrap(),
            b"original-data"
        );
        assert_eq!(
            second.get_volume_info("shared-vol").unwrap().device_path,
            original.device_path
        );

        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn shared_duplicate_clone_does_not_delete_existing_volume() {
        let root = unique_root("shared-duplicate-clone");
        let mut first = make_engine(&root);
        let mut second = make_engine(&root);
        first.clone_method = CloneMethod::CopyFileRange;
        second.clone_method = CloneMethod::CopyFileRange;

        first.create_volume("source", 4096).unwrap();
        let original = first.create_volume("destination", 4096).unwrap();
        std::fs::write(&original.device_path, b"original-data").unwrap();

        assert!(matches!(
            second.create_volume_from_snapshot("source", "destination"),
            Err(CubecowError::AlreadyExists(_))
        ));
        assert_eq!(
            std::fs::read(&original.device_path).unwrap(),
            b"original-data"
        );
        assert_eq!(
            second.get_volume_info("destination").unwrap().device_path,
            original.device_path
        );

        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn xfs_engine_keeps_startup_index_without_runtime_discovery() {
        let root = unique_root("xfs-no-runtime-discovery");
        let first = make_engine(&root);
        let second = make_engine(&root);

        first.create_volume("created-after-startup", 4096).unwrap();
        assert!(matches!(
            second.get_volume_info("created-after-startup"),
            Err(CubecowError::NotFound(_))
        ));

        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn two_engines_share_deletes_and_restart_rebuilds_namespace() {
        let root = unique_root("shared-delete-restart");
        let mut first = make_engine(&root);
        let mut second = make_engine(&root);
        first.clone_method = CloneMethod::CopyFileRange;
        second.clone_method = CloneMethod::CopyFileRange;

        first.create_volume("shared-vol", 4096).unwrap();
        first
            .create_snapshot_from_volume("shared-vol", "shared-snap", false)
            .unwrap();
        assert!(second.get_volume_info("shared-snap").is_ok());

        second.delete_snapshot("shared-snap").unwrap();
        first.delete_volume("shared-vol").unwrap();
        assert!(second.get_volume_info("shared-vol").is_err());
        second.create_volume("shared-vol", 2 * 4096).unwrap();
        assert_eq!(
            first.get_volume_info("shared-vol").unwrap().size_bytes,
            2 * 4096
        );
        first.delete_volume("shared-vol").unwrap();

        // Shared-engine startup must not perform orphan cleanup against an
        // object directory another engine may still be populating.
        let pending = root.join("volumes").join("pending-create");
        std::fs::create_dir_all(&pending).unwrap();
        let startup_config = AppConfig::from_json_str(&format!(
            r#"{{"log":{{}},"backend":{{"kind":"reflink","reflink":{{"root_dir":"{}","clone_method":"copy_file_range"}}}}}}"#,
            root.display()
        ))
        .unwrap();
        let third = ReflinkEngine::initialize_without_logging(startup_config).unwrap();
        assert!(
            pending.exists(),
            "shared startup must not reap another engine's pending directory"
        );
        drop(third);
        std::fs::remove_dir(&pending).unwrap();

        drop(first);
        drop(second);
        let config = AppConfig::from_json_str(&format!(
            r#"{{"log":{{}},"backend":{{"kind":"reflink","reflink":{{"root_dir":"{}","clone_method":"copy_file_range"}}}}}}"#,
            root.display()
        ))
        .unwrap();
        let restarted = ReflinkEngine::initialize_without_logging(config).unwrap();
        let (volumes, _, total) = restarted.list_volumes(0, None);
        assert!(volumes.is_empty());
        assert_eq!(total, 0);

        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn clone_method_parses_from_json() {
        let cfg = AppConfig::from_json_str(
            r#"{"log":{},"backend":{"kind":"reflink","reflink":{"root_dir":"/tmp/x","clone_method":"copy_file_range"}}}"#,
        )
        .unwrap();
        assert_eq!(cfg.backend.reflink.clone_method, CloneMethod::CopyFileRange);
    }

    #[test]
    fn validate_name_rejects_bad_inputs() {
        assert!(ReflinkEngine::validate_name("", "volume").is_err());
        assert!(ReflinkEngine::validate_name(".", "volume").is_err());
        assert!(ReflinkEngine::validate_name("..", "volume").is_err());
        assert!(ReflinkEngine::validate_name("a/b", "volume").is_err());
        assert!(ReflinkEngine::validate_name(".hidden", "volume").is_err());
        assert!(ReflinkEngine::validate_name("ok-name_1", "volume").is_ok());
    }

    #[test]
    fn create_and_list_volume_roundtrip() {
        let root = unique_root("crud");
        if !fs_supports_ficlone(&root.join("volumes")) {
            eprintln!("[skip] tmpdir does not support FICLONE");
            return;
        }
        let engine = make_engine(&root);

        let vol = engine.create_volume("v1", 4 * 1024 * 1024).unwrap();
        assert_eq!(vol.name, "v1");
        assert_eq!(vol.size_bytes, 4 * 1024 * 1024);
        assert!(vol.device_path.ends_with("/volumes/v1/v1"));
        assert_eq!(vol.snapshot_count, 0);

        // Duplicate must be rejected.
        let dup = engine.create_volume("v1", 4 * 1024 * 1024);
        assert!(matches!(dup, Err(CubecowError::AlreadyExists(_))));

        let (page, token, total) = engine.list_volumes(0, None);
        assert_eq!(token, None);
        assert_eq!(total, 1);
        assert_eq!(page.len(), 1);
        assert_eq!(page[0].name, "v1");

        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn snapshot_create_delete_and_listing() {
        let root = unique_root("snap");
        if !fs_supports_ficlone(&root.join("volumes")) {
            eprintln!("[skip] tmpdir does not support FICLONE");
            return;
        }
        let engine = make_engine(&root);

        engine.create_volume("vol", 1024 * 1024).unwrap();
        let s1 = engine
            .create_snapshot_from_volume("vol", "s1", false)
            .unwrap();
        assert_eq!(s1.origin_volume, "vol");
        assert_eq!(s1.size_bytes, 1024 * 1024);

        // Snap-of-snap: flattened — origin_volume stays "vol".
        let s2 = engine
            .create_snapshot_from_volume("s1", "s2", false)
            .unwrap();
        assert_eq!(s2.origin_volume, "vol");
        assert!(s2.device_path.ends_with("/volumes/vol/s2"));

        let (snaps, _) = engine.list_snapshots("vol", 0, None);
        let names: Vec<&str> = snaps.iter().map(|s| s.name.as_str()).collect();
        assert!(names.contains(&"s1"));
        assert!(names.contains(&"s2"));
        assert_eq!(snaps.len(), 2);

        // Volume deletion is allowed even while live snapshots exist
        // (matches the dm-thin contract Cubelet relies on during
        // template-build cleanup): only the volume's main file is
        // unlinked, and the dir is kept alive by the surviving
        // snapshots.
        engine.delete_volume("vol").unwrap();
        let vol_dir = root.join("volumes").join("vol");
        assert!(vol_dir.exists(), "origin dir kept alive by snapshots");
        assert!(!vol_dir.join("vol").exists(), "main file unlinked");
        // get_volume_info now reports the origin as gone.
        assert!(matches!(
            engine.get_volume_info("vol"),
            Err(CubecowError::NotFound(_))
        ));
        // Snapshot files survive on disk and remain unlinkable —
        // delete_snapshot is the canonical accessor for them.
        assert!(vol_dir.join("s1").exists());
        assert!(vol_dir.join("s2").exists());

        engine.delete_snapshot("s2").unwrap();
        // Origin dir still exists because s1 lingers.
        assert!(vol_dir.exists());
        engine.delete_snapshot("s1").unwrap();
        // Last snapshot gone → orphan dir reaped.
        assert!(
            !vol_dir.exists(),
            "orphan origin dir should be reaped after last snapshot"
        );
        // Re-deleting an already-deleted volume is a NotFound, just
        // like dm-thin reports for a stale handle.
        assert!(matches!(
            engine.delete_volume("vol"),
            Err(CubecowError::NotFound(_))
        ));

        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn create_volume_from_volume_source() {
        let root = unique_root("vol-from-vol");
        if !fs_supports_ficlone(&root.join("volumes")) {
            eprintln!("[skip] tmpdir does not support FICLONE");
            return;
        }
        let engine = make_engine(&root);

        engine.create_volume("src-vol", 1024 * 1024).unwrap();
        let cloned = engine
            .create_volume_from_snapshot("src-vol", "dst-vol")
            .unwrap();
        assert_eq!(cloned.name, "dst-vol");
        assert!(cloned.device_path.ends_with("/volumes/dst-vol/dst-vol"));
        assert!(root
            .join("volumes")
            .join("dst-vol")
            .join("dst-vol")
            .exists());

        engine.delete_volume("src-vol").unwrap();
        assert!(
            root.join("volumes")
                .join("dst-vol")
                .join("dst-vol")
                .exists(),
            "clone is independent of the source volume"
        );

        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn names_share_a_global_namespace() {
        let root = unique_root("ns");
        if !fs_supports_ficlone(&root.join("volumes")) {
            eprintln!("[skip] tmpdir does not support FICLONE");
            return;
        }
        let engine = make_engine(&root);

        engine.create_volume("a", 1024 * 1024).unwrap();
        // snapshot named "a" must conflict with the volume.
        let bad = engine.create_snapshot_from_volume("a", "a", false);
        assert!(matches!(bad, Err(CubecowError::AlreadyExists(_))));

        engine
            .create_snapshot_from_volume("a", "snap1", false)
            .unwrap();
        // volume named "snap1" must conflict with the snapshot.
        let bad = engine.create_volume("snap1", 1024 * 1024);
        assert!(matches!(bad, Err(CubecowError::AlreadyExists(_))));

        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn resize_only_grows_volume_main_file() {
        let root = unique_root("resize");
        if !fs_supports_ficlone(&root.join("volumes")) {
            eprintln!("[skip] tmpdir does not support FICLONE");
            return;
        }
        let engine = make_engine(&root);

        engine.create_volume("v", 1024 * 1024).unwrap();
        engine.create_snapshot_from_volume("v", "s", false).unwrap();

        let (old, new) = engine.resize_volume("v", 4 * 1024 * 1024).unwrap();
        assert_eq!(old, 1024 * 1024);
        assert_eq!(new, 4 * 1024 * 1024);

        // Volume reflects new size; snapshot stays at original size.
        assert_eq!(
            engine.get_volume_info("v").unwrap().size_bytes,
            4 * 1024 * 1024
        );
        let (snaps, _) = engine.list_snapshots("v", 0, None);
        assert_eq!(snaps.len(), 1);
        assert_eq!(snaps[0].size_bytes, 1024 * 1024);

        // Shrink rejected.
        let bad = engine.resize_volume("v", 0);
        assert!(matches!(bad, Err(CubecowError::InvalidArg(_))));

        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn scan_recovers_volumes_and_snapshots_after_restart() {
        let root = unique_root("recovery");
        let volumes_dir = root.join("volumes");
        if !fs_supports_ficlone(&volumes_dir) {
            eprintln!("[skip] tmpdir does not support FICLONE");
            return;
        }
        // Phase 1: create some entries, then drop the engine.
        {
            let engine = make_engine(&root);
            engine.create_volume("vol1", 1024 * 1024).unwrap();
            engine
                .create_snapshot_from_volume("vol1", "snapA", false)
                .unwrap();
            engine.create_volume("vol2", 1024 * 1024).unwrap();
        }

        // Inject a crashed-FICLONE artefact (zero-byte snapshot file).
        let orphan = volumes_dir.join("vol1").join("orphan");
        File::create(&orphan).unwrap();

        // Phase 2: rescan from disk.
        let idx = scan_and_rebuild_index(&volumes_dir).unwrap();
        assert!(matches!(idx.get("vol1"), Some(NameKind::Volume)));
        assert!(matches!(idx.get("vol2"), Some(NameKind::Volume)));
        match idx.get("snapA") {
            Some(NameKind::Snapshot { origin_volume }) => assert_eq!(origin_volume, "vol1"),
            other => panic!("expected snapA snapshot of vol1, got {other:?}"),
        }
        assert!(
            idx.get("orphan").is_none(),
            "zero-byte orphan must be skipped"
        );
        assert!(!orphan.exists(), "scan must remove the orphan file");

        let _ = std::fs::remove_dir_all(&root);
    }
}
