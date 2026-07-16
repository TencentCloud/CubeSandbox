// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
// Ceph RBD backed implementation of [`crate::engine::Engine`].
//
// Where the reflink backend stores volumes as files on one node's local
// filesystem, this backend stores them as RBD images in a Ceph pool, so
// every object is reachable from every node in the cluster. Compute
// nodes keep no persistent storage state: activation maps an image into
// the local kernel (`rbd map` -> /dev/rbdX), deactivation unmaps it,
// and the truth stays in the cluster.
//
// # Object model
//
// Every engine object — volume or snapshot — is a standalone RBD image
// in one flat pool namespace (trait contract). A snapshot is an
// *independent flattened clone* of its source: `create_snapshot` clones
// through a transient anchor snapshot and immediately `rbd flatten`s the
// result, so the new image shares no parent chain or lifetime with its
// source. There is no read-only-snapshot concept and no clone chain.
//
// The object kind is encoded in the pool image name — `vol-<name>` /
// `snap-<name>` — so a single `rbd ls` classifies the whole pool with
// no per-image reads; the prefix never leaves this backend (callers use
// plain engine names). A snapshot's origin volume is recorded on the
// image itself as image-meta `cubecow.origin`, cluster-visible with no
// separate index and no node-local cache.
//
// # Deletion
//
// Because a snapshot is an independent image (not an RBD snapshot on its
// source), deleting a volume never conflicts with snapshots derived from
// it: each is removed with a plain `rbd rm`. An image whose removal is
// still blocked (e.g. a leftover transient anchor) falls back to
// `rbd trash mv`, freeing the name immediately.
//
// # reset_node_storage
//
// Resetting *node* storage on a shared backend means detaching this
// node, not destroying cluster data: every locally mapped image of the
// pool is unmapped and nothing else is touched.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use chrono::{DateTime, NaiveDateTime, Utc};
use serde::Deserialize;
use tracing::{info, warn};

use crate::config::{AppConfig, RbdConfig};
use crate::engine::Engine;
use crate::pkg::errors::{CubecowError, CubecowResult};
use crate::pkg::executor::{CommandExecutor, CommandRunner};
use crate::pkg::metrics::{
    METRIC_SNAPSHOT_COUNT, METRIC_TOTAL_BYTES, METRIC_USED_BYTES, METRIC_VOLUME_COUNT,
};
use crate::{Snapshot, Volume, VolumeBlockInfo};

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

/// Block size advertised through `get_volume_block_info`, matching the
/// reflink backend.
const RBD_BLOCK_SIZE: u32 = 512;

/// Image-name prefix for volumes. The object kind is encoded in the
/// image name (`vol-<name>` / `snap-<name>`), so one `rbd ls` classifies
/// the whole pool without a per-image metadata read. The prefix never
/// leaves this backend: callers use plain engine names.
const VOLUME_PREFIX: &str = "vol-";

/// Image-name prefix for snapshots. See [`VOLUME_PREFIX`].
const SNAPSHOT_PREFIX: &str = "snap-";

/// image-meta key: a snapshot's origin volume (engine name).
const META_ORIGIN: &str = "cubecow.origin";

/// Prefix for the transient anchor snapshot `create_snapshot` clones
/// from. It exists only between clone and flatten and is removed within
/// the same call.
const TRANSIENT_SNAP_PREFIX: &str = "__cow_";

/// Cache TTL for pool metrics; the heartbeat polls metrics every second
/// and three cluster round-trips per tick would be wasteful.
const METRICS_CACHE_TTL: Duration = Duration::from_secs(5);

/// Cache TTL for `rbd showmapped` output used by read-only projections.
/// Mapping state only changes through this engine's own map/unmap
/// (which refresh the cache), so the TTL merely bounds staleness
/// against out-of-band manual maps.
const MAPPINGS_CACHE_TTL: Duration = Duration::from_secs(1);

// ---------------------------------------------------------------------------
// Resolution
// ---------------------------------------------------------------------------

/// A resolved engine object: a real RBD image plus what it is. The
/// `rbd info` result is carried along so projections do not re-fetch it.
struct RbdObject {
    kind: ImageKind,
    info: RbdInfo,
}

#[derive(Debug, Clone, Copy, PartialEq)]
enum ImageKind {
    Volume,
    Snapshot,
}

impl ImageKind {
    fn prefix(self) -> &'static str {
        match self {
            ImageKind::Volume => VOLUME_PREFIX,
            ImageKind::Snapshot => SNAPSHOT_PREFIX,
        }
    }

    /// Pool image name for an engine name of this kind.
    fn image_name(self, name: &str) -> String {
        format!("{}{}", self.prefix(), name)
    }
}

// ---------------------------------------------------------------------------
// rbd CLI JSON shapes (serde views of the fields we consume)
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Deserialize)]
struct RbdInfo {
    size: u64,
    #[serde(default)]
    create_timestamp: String,
}

#[derive(Debug, Clone, Deserialize)]
struct RbdMapping {
    #[serde(default)]
    pool: String,
    #[serde(default)]
    namespace: String,
    name: String,
    #[serde(default)]
    snap: String,
    device: String,
}

#[derive(Debug, Deserialize)]
struct RbdSnapLsEntry {
    name: String,
}

#[derive(Debug, Deserialize)]
struct CephDf {
    pools: Vec<CephDfPool>,
}

#[derive(Debug, Deserialize)]
struct CephDfPool {
    name: String,
    stats: CephDfPoolStats,
}

#[derive(Debug, Deserialize)]
struct CephDfPoolStats {
    /// Logical bytes stored (pre-replication). Absent on very old
    /// clusters, hence optional.
    #[serde(default)]
    stored: Option<u64>,
    /// Raw bytes consumed, including replication/EC overhead.
    bytes_used: u64,
    /// Projected writable capacity, already divided by the replication
    /// factor (logical bytes).
    max_avail: u64,
}

// ---------------------------------------------------------------------------
// RbdEngine
// ---------------------------------------------------------------------------

/// Ceph RBD backed engine. Stateless on the node: every operation is
/// resolved against the cluster through the `rbd` CLI.
pub struct RbdEngine {
    cfg: RbdConfig,
    runner: Arc<dyn CommandRunner>,
    /// Runner with a long timeout for data-copying maintenance
    /// commands (`rbd flatten`).
    slow_runner: Arc<dyn CommandRunner>,
    metrics_cache: Mutex<Option<(Instant, HashMap<String, u64>)>>,
    /// Single-flights the metrics recompute (ceph df + one image-meta
    /// call per image) so slow refreshes don't stampede the cluster.
    metrics_refresh: Mutex<()>,
    mappings_cache: Mutex<Option<(Instant, Vec<RbdMapping>)>>,
    /// Serializes map/unmap check-then-act: concurrent activations of
    /// one image would otherwise both observe "not mapped" and krbd
    /// maps it twice, leaking a device.
    map_lock: Mutex<()>,
    /// Serializes the anchor-create → clone window of create_snapshot:
    /// anchors are keyed by destination name, so racing same-name
    /// creates would remove each other's anchor mid-clone.
    snapshot_lock: Mutex<()>,
}

impl RbdEngine {
    /// Initialize the rbd engine from an `AppConfig`, setting up logging.
    pub fn initialize(config: AppConfig) -> anyhow::Result<Self> {
        crate::pkg::logger::init_logging(&config.log)
            .map_err(|e| anyhow::anyhow!("failed to init logging: {e}"))?;
        info!("rbd engine initializing");
        Self::initialize_with_config(config)
    }

    /// Same as [`Self::initialize`] but skips logging setup.
    pub fn initialize_without_logging(config: AppConfig) -> anyhow::Result<Self> {
        info!("rbd engine initializing (logging managed externally)");
        Self::initialize_with_config(config)
    }

    fn initialize_with_config(config: AppConfig) -> anyhow::Result<Self> {
        config
            .validate()
            .map_err(|e| anyhow::anyhow!("invalid config for rbd backend: {e}"))?;
        let cfg = config.backend.rbd.clone();
        let runner = Arc::new(CommandExecutor::new(Duration::from_secs(
            cfg.cmd_timeout_secs,
        )));
        let slow_runner = Arc::new(CommandExecutor::new(Duration::from_secs(
            cfg.slow_cmd_timeout_secs,
        )));
        let engine = Self::new_with_runners(cfg, runner, slow_runner);

        // Probe cluster reachability so a misconfigured pool fails engine
        // startup, not the first volume operation (mirrors the reflink
        // FICLONE probe).
        let pool = engine.pool_spec();
        engine.run_rbd(&["ls", &pool]).map_err(|e| {
            anyhow::anyhow!("rbd pool '{pool}' unusable: {}", Self::classify(e, &pool))
        })?;
        info!(
            pool = %engine.cfg.pool,
            namespace = %engine.cfg.namespace,
            "rbd engine initialized"
        );
        Ok(engine)
    }

    fn new_with_runners(
        cfg: RbdConfig,
        runner: Arc<dyn CommandRunner>,
        slow_runner: Arc<dyn CommandRunner>,
    ) -> Self {
        Self {
            cfg,
            runner,
            slow_runner,
            metrics_cache: Mutex::new(None),
            metrics_refresh: Mutex::new(()),
            mappings_cache: Mutex::new(None),
            map_lock: Mutex::new(()),
            snapshot_lock: Mutex::new(()),
        }
    }

    // -----------------------------------------------------------------------
    // CLI plumbing
    // -----------------------------------------------------------------------

    /// Global `rbd` arguments derived from config (cluster selection).
    /// Empty strings count as unset — a TOML `conf = ""` must not become
    /// a literal `--conf ""`.
    fn base_args(&self) -> Vec<String> {
        let mut args = Vec::new();
        if let Some(conf) = &self.cfg.conf {
            if !conf.as_os_str().is_empty() {
                args.push("--conf".to_string());
                args.push(conf.to_string_lossy().into_owned());
            }
        }
        if let Some(client) = &self.cfg.client {
            if !client.is_empty() {
                args.push("--id".to_string());
                args.push(client.clone());
            }
        }
        args
    }

    fn run_cmd(
        &self,
        runner: &dyn CommandRunner,
        bin: &str,
        args: &[&str],
    ) -> CubecowResult<String> {
        let mut full = self.base_args();
        full.extend(args.iter().map(|s| s.to_string()));
        let refs: Vec<&str> = full.iter().map(String::as_str).collect();
        runner.run(bin, &refs)
    }

    fn run_rbd(&self, args: &[&str]) -> CubecowResult<String> {
        self.run_cmd(&*self.runner, &self.cfg.rbd_bin, args)
    }

    /// Like [`Self::run_rbd`] but on the long-timeout runner, for
    /// commands whose runtime scales with image size (`flatten`, `rm`).
    fn run_rbd_slow(&self, args: &[&str]) -> CubecowResult<String> {
        self.run_cmd(&*self.slow_runner, &self.cfg.rbd_bin, args)
    }

    fn run_ceph(&self, args: &[&str]) -> CubecowResult<String> {
        self.run_cmd(&*self.runner, &self.cfg.ceph_bin, args)
    }

    /// Flatten an image into an independent copy using the slow runner
    /// (the data copy can take minutes).
    fn flatten_image(&self, image: &str) -> CubecowResult<()> {
        let spec = self.spec(image);
        self.run_rbd_slow(&["flatten", &spec])
            .map_err(|e| Self::classify(e, image))?;
        Ok(())
    }

    /// `pool[/namespace]/image` spec for rbd commands. `image` is a pool
    /// image name (kind prefix included), not an engine name.
    fn spec(&self, image: &str) -> String {
        if self.cfg.namespace.is_empty() {
            format!("{}/{}", self.cfg.pool, image)
        } else {
            format!("{}/{}/{}", self.cfg.pool, self.cfg.namespace, image)
        }
    }

    /// Spec for the image backing an engine name of a known kind.
    fn spec_of(&self, kind: ImageKind, name: &str) -> String {
        self.spec(&kind.image_name(name))
    }

    /// `pool[/namespace]/image@snap` spec.
    fn snap_spec(&self, image: &str, snap: &str) -> String {
        format!("{}@{}", self.spec(image), snap)
    }

    /// Pool (with namespace) spec for listing commands.
    fn pool_spec(&self) -> String {
        if self.cfg.namespace.is_empty() {
            self.cfg.pool.clone()
        } else {
            format!("{}/{}", self.cfg.pool, self.cfg.namespace)
        }
    }

    // -----------------------------------------------------------------------
    // Error classification
    // -----------------------------------------------------------------------

    /// Reinterpret a failed rbd command as a well-known error. The
    /// errno-style exit code is authoritative; stderr phrases are only a
    /// fallback for results without a usable code, matched as full
    /// messages — stderr echoes image names, so a bare "busy" would
    /// misfire on a name like "busybox". `what` names the object.
    fn classify(err: CubecowError, what: &str) -> CubecowError {
        let CubecowError::CommandFailed { cmd, reason, code } = &err else {
            return err;
        };
        match code {
            Some(2) => return CubecowError::NotFound(what.to_string()),
            Some(17) => return CubecowError::AlreadyExists(what.to_string()),
            // EBUSY(16) / ENOTEMPTY(39): the object is positively
            // blocked (mapped, watched, or has dependents) — a
            // retryable precondition.
            Some(16) | Some(39) => {
                return CubecowError::PreconditionFailed(format!("{what}: {cmd}: {reason}"))
            }
            _ => {}
        }
        if reason.contains("(2) No such file or directory") {
            return CubecowError::NotFound(what.to_string());
        }
        if reason.contains("(17) File exists") {
            return CubecowError::AlreadyExists(what.to_string());
        }
        if reason.contains("(16) Device or resource busy")
            || reason.contains("image still has watchers")
            || reason.contains("(39) Directory not empty")
            || reason.contains("image has snapshots")
        {
            return CubecowError::PreconditionFailed(format!("{what}: {cmd}: {reason}"));
        }
        err
    }

    /// Best-effort cleanup on error paths: log failures instead of
    /// silently leaking the object, without masking the primary error.
    fn cleanup_warn<T>(op: &str, object: &str, result: CubecowResult<T>) {
        if let Err(e) = result {
            warn!(object, error = %e, "rbd cleanup '{op}' failed (leftover may remain)");
        }
    }

    // -----------------------------------------------------------------------
    // Validation
    // -----------------------------------------------------------------------

    /// Reject names that would break the layout: empty, path or snap
    /// separators, a leading '.' (reserved for internal images), or a
    /// leading '-' (would parse as a CLI flag).
    fn validate_name(name: &str, kind: &str) -> CubecowResult<()> {
        if name.is_empty() {
            return Err(CubecowError::InvalidArg(format!("{kind} name is empty")));
        }
        if name.contains('/') || name.contains('@') || name.contains('\0') {
            return Err(CubecowError::InvalidArg(format!(
                "{kind} name '{name}' contains an invalid character"
            )));
        }
        if name.starts_with('.') || name.starts_with('-') {
            return Err(CubecowError::InvalidArg(format!(
                "{kind} name '{name}' must not start with '.' or '-'"
            )));
        }
        Ok(())
    }

    // -----------------------------------------------------------------------
    // Image helpers
    // -----------------------------------------------------------------------

    fn image_info(&self, spec: &str, what: &str) -> CubecowResult<RbdInfo> {
        let out = self
            .run_rbd(&["info", spec, "--format", "json"])
            .map_err(|e| Self::classify(e, what))?;
        serde_json::from_str(out.trim()).map_err(|e| {
            CubecowError::PreconditionFailed(format!("parse rbd info for '{what}': {e}"))
        })
    }

    /// Sorted `rbd ls` names of the pool, internal images filtered out.
    /// Failures warn and degrade to an empty listing (the list/metrics
    /// trait surface cannot propagate them).
    fn pool_image_names(&self, op: &str) -> Vec<String> {
        let out = match self.run_rbd(&["ls", &self.pool_spec(), "--format", "json"]) {
            Ok(out) => out,
            Err(e) => {
                warn!(error = %e, op, "rbd ls failed");
                return Vec::new();
            }
        };
        let mut names: Vec<String> = serde_json::from_str(out.trim()).unwrap_or_else(|e| {
            warn!(error = %e, op, "parse rbd ls output failed");
            Vec::new()
        });
        names.retain(|n| !n.starts_with('.'));
        names.sort();
        names
    }

    fn image_meta(&self, image: &str) -> CubecowResult<HashMap<String, String>> {
        let out = match self.run_rbd(&["image-meta", "list", &self.spec(image), "--format", "json"])
        {
            Ok(out) => out,
            Err(e) => match Self::classify(e, image) {
                CubecowError::NotFound(_) => return Ok(HashMap::new()),
                other => return Err(other),
            },
        };
        Ok(serde_json::from_str(out.trim()).unwrap_or_else(|e| {
            warn!(image, error = %e, "parse rbd image-meta failed; treating as untagged");
            HashMap::new()
        }))
    }

    fn image_meta_set(&self, image: &str, key: &str, value: &str) -> CubecowResult<()> {
        self.run_rbd(&["image-meta", "set", &self.spec(image), key, value])?;
        Ok(())
    }

    /// Existence probe for an engine name of a given kind.
    fn exists(&self, kind: ImageKind, name: &str) -> CubecowResult<bool> {
        match self.image_info(&self.spec_of(kind, name), name) {
            Ok(_) => Ok(true),
            Err(CubecowError::NotFound(_)) => Ok(false),
            Err(e) => Err(e),
        }
    }

    /// Resolve an engine name to its backing image and kind, probing the
    /// two kind prefixes (validate_name keeps the name from reaching
    /// another pool or namespace).
    fn resolve(&self, name: &str) -> CubecowResult<RbdObject> {
        Self::validate_name(name, "image")?;
        match self.image_info(&self.spec_of(ImageKind::Volume, name), name) {
            Ok(info) => {
                return Ok(RbdObject {
                    kind: ImageKind::Volume,
                    info,
                })
            }
            Err(CubecowError::NotFound(_)) => {}
            Err(e) => return Err(e),
        }
        let info = self.image_info(&self.spec_of(ImageKind::Snapshot, name), name)?;
        Ok(RbdObject {
            kind: ImageKind::Snapshot,
            info,
        })
    }

    // -----------------------------------------------------------------------
    // Mapping
    // -----------------------------------------------------------------------

    /// Run `rbd showmapped` and refresh the projection cache.
    fn load_mappings(&self) -> CubecowResult<Vec<RbdMapping>> {
        let out = self.run_rbd(&["showmapped", "--format", "json"])?;
        // A parse failure must surface as an error, not an empty list:
        // treating "unknown" as "nothing mapped" would double-map images
        // and make unmap/reset silently no-op.
        let mappings: Vec<RbdMapping> = serde_json::from_str(out.trim()).map_err(|e| {
            CubecowError::PreconditionFailed(format!("parse rbd showmapped output: {e}"))
        })?;
        *self.mappings_cache.lock().expect("mappings cache poisoned") =
            Some((Instant::now(), mappings.clone()));
        Ok(mappings)
    }

    fn invalidate_mappings_cache(&self) {
        *self.mappings_cache.lock().expect("mappings cache poisoned") = None;
    }

    fn match_mapping(&self, mappings: &[RbdMapping], image: &str, snap: &str) -> Option<String> {
        let want_snap = if snap.is_empty() { "-" } else { snap };
        for m in mappings {
            if m.pool == self.cfg.pool
                && m.namespace == self.cfg.namespace
                && m.name == image
                && (m.snap == want_snap || (snap.is_empty() && m.snap.is_empty()))
            {
                return Some(m.device.clone());
            }
        }
        None
    }

    /// Device path if `image[@snap]` is currently mapped on this node,
    /// answered from a live `rbd showmapped`. Used before map/unmap
    /// decisions, where a stale answer would double-map an image.
    fn find_mapping(&self, image: &str, snap: &str) -> CubecowResult<Option<String>> {
        let mappings = self.load_mappings()?;
        Ok(self.match_mapping(&mappings, image, snap))
    }

    /// Cached variant for read-only projections (device_path display),
    /// where bounded staleness is fine and the extra process spawn per
    /// info call is not.
    fn find_mapping_cached(&self, image: &str, snap: &str) -> CubecowResult<Option<String>> {
        if let Some((at, mappings)) = self
            .mappings_cache
            .lock()
            .expect("mappings cache poisoned")
            .as_ref()
        {
            if at.elapsed() < MAPPINGS_CACHE_TTL {
                return Ok(self.match_mapping(mappings, image, snap));
            }
        }
        self.find_mapping(image, snap)
    }

    /// Idempotently map `image`, returning the local device path.
    fn map_device(&self, image: &str, snap: &str) -> CubecowResult<String> {
        let _serialized = self.map_lock.lock().expect("map lock poisoned");
        if let Some(dev) = self.find_mapping(image, snap)? {
            return Ok(dev);
        }
        let spec = if snap.is_empty() {
            self.spec(image)
        } else {
            self.snap_spec(image, snap)
        };
        let mut args = vec!["map"];
        if let Some(opts) = &self.cfg.map_options {
            if !opts.is_empty() {
                args.push("-o");
                args.push(opts);
            }
        }
        args.push(&spec);
        let out = self.run_rbd(&args).map_err(|e| Self::classify(e, &spec))?;
        self.invalidate_mappings_cache();
        let device = out.trim().to_string();
        if device.is_empty() {
            return Err(CubecowError::PreconditionFailed(format!(
                "rbd map '{spec}' returned no device path"
            )));
        }
        Ok(device)
    }

    /// Idempotently unmap `image[@snap]` if mapped on this node.
    fn unmap_device(&self, image: &str, snap: &str) -> CubecowResult<()> {
        let _serialized = self.map_lock.lock().expect("map lock poisoned");
        if let Some(dev) = self.find_mapping(image, snap)? {
            self.run_rbd(&["unmap", &dev])
                .map_err(|e| Self::classify(e, &dev))?;
            self.invalidate_mappings_cache();
        }
        Ok(())
    }

    // -----------------------------------------------------------------------
    // Projections
    // -----------------------------------------------------------------------

    fn volume_view(name: &str, info: &RbdInfo, device_path: String) -> Volume {
        Volume {
            name: name.to_string(),
            size_bytes: info.size,
            device_path,
            // Counting snapshots would cost an extra cluster round-trip
            // on the hot info path and no caller consumes the value.
            snapshot_count: 0,
            created_at: rfc3339_from_rbd_timestamp(&info.create_timestamp),
        }
    }

    fn snapshot_view(name: &str, origin: &str, info: &RbdInfo, device_path: String) -> Snapshot {
        Snapshot {
            name: name.to_string(),
            size_bytes: info.size,
            device_path,
            origin_volume: origin.to_string(),
            created_at: rfc3339_from_rbd_timestamp(&info.create_timestamp),
        }
    }

    fn project_volume(&self, name: &str, obj: &RbdObject) -> CubecowResult<Volume> {
        let image = obj.kind.image_name(name);
        let device_path = self.find_mapping_cached(&image, "")?.unwrap_or_default();
        Ok(Self::volume_view(name, &obj.info, device_path))
    }

    fn project_snapshot(
        &self,
        name: &str,
        origin: &str,
        info: &RbdInfo,
    ) -> CubecowResult<Snapshot> {
        let image = ImageKind::Snapshot.image_name(name);
        let device_path = self.find_mapping_cached(&image, "")?.unwrap_or_default();
        Ok(Self::snapshot_view(name, origin, info, device_path))
    }

    // -----------------------------------------------------------------------
    // Deletion helper
    // -----------------------------------------------------------------------

    /// Remove an image: purge any leftover snapshots (a crashed
    /// `create_snapshot` may leave a transient anchor), then `rbd rm`,
    /// falling back to `rbd trash mv` when removal is still blocked.
    fn remove_image(&self, image: &str) -> CubecowResult<()> {
        let spec = self.spec(image);
        let out = match self.run_rbd(&["snap", "ls", &spec, "--format", "json"]) {
            Ok(out) => out,
            Err(e) => match Self::classify(e, image) {
                CubecowError::NotFound(_) => return Ok(()),
                other => return Err(other),
            },
        };
        let snaps: Vec<RbdSnapLsEntry> = serde_json::from_str(out.trim()).map_err(|e| {
            CubecowError::PreconditionFailed(format!("parse rbd snap ls for '{image}': {e}"))
        })?;
        for snap in snaps {
            let snap_spec = self.snap_spec(image, &snap.name);
            if let Err(e) = self.run_rbd(&["snap", "rm", &snap_spec]) {
                warn!(spec = %snap_spec, error = %e, "rbd snap rm failed during image removal");
            }
        }
        match self.run_rbd_slow(&["rm", &spec]) {
            Ok(_) => Ok(()),
            Err(e) => match Self::classify(e, image) {
                CubecowError::NotFound(_) => Ok(()),
                CubecowError::PreconditionFailed(_) => {
                    // Positively blocked (errno 16/39) — free the name via
                    // trash. Anything else must surface, not silently defer
                    // deletion and leak pool capacity.
                    self.run_rbd(&["trash", "mv", &spec])
                        .map_err(|e| Self::classify(e, image))?;
                    info!(image, "rbd image moved to trash (removal blocked)");
                    Ok(())
                }
                other => Err(other),
            },
        }
    }
}

// ---------------------------------------------------------------------------
// Engine trait impl
// ---------------------------------------------------------------------------

impl Engine for RbdEngine {
    fn create_volume(&self, name: &str, size_bytes: u64) -> CubecowResult<Volume> {
        Self::validate_name(name, "volume")?;
        // Engine names share one flat namespace across kinds; `rbd create`
        // only guards the vol- side (17), so probe the snap- side first.
        if self.exists(ImageKind::Snapshot, name)? {
            return Err(CubecowError::AlreadyExists(format!(
                "name '{name}' already exists in rbd namespace"
            )));
        }
        let size = format!("{size_bytes}B");
        let image = ImageKind::Volume.image_name(name);
        self.run_rbd(&[
            "create",
            "--size",
            &size,
            "--image-feature",
            &self.cfg.image_features,
            &self.spec(&image),
        ])
        .map_err(|e| Self::classify(e, name))?;

        let result = (|| -> CubecowResult<Volume> {
            // Map immediately: callers format the fresh volume right
            // away and expect a usable device path (reflink returns
            // the file path for the same reason).
            let device = self.map_device(&image, "")?;
            let info = self.image_info(&self.spec(&image), name)?;
            info!(volume = name, size_bytes, device, "rbd volume created");
            Ok(Self::volume_view(name, &info, device))
        })();

        if result.is_err() {
            Self::cleanup_warn("unmap", &image, self.unmap_device(&image, ""));
            Self::cleanup_warn("rm", &image, self.run_rbd(&["rm", &self.spec(&image)]));
        }
        result
    }

    fn delete_volume(&self, name: &str) -> CubecowResult<()> {
        if self.resolve(name)?.kind != ImageKind::Volume {
            return Err(CubecowError::InvalidArg(format!(
                "'{name}' is a snapshot; use delete_snapshot instead"
            )));
        }
        let image = ImageKind::Volume.image_name(name);
        self.unmap_device(&image, "")?;
        self.remove_image(&image)?;
        info!(volume = name, "rbd volume deleted");
        Ok(())
    }

    fn resize_volume(&self, name: &str, new_size_bytes: u64) -> CubecowResult<(u64, u64)> {
        // Any engine image is resizable (a snapshot is an independent
        // writable image, e.g. a sandbox rootfs generation grown by
        // resizeSnapshotIfTooSmall).
        let obj = self.resolve(name)?;
        let old_size = obj.info.size;
        if new_size_bytes < old_size {
            return Err(CubecowError::InvalidArg(format!(
                "shrinking is not supported (current={old_size}, requested={new_size_bytes})"
            )));
        }
        if new_size_bytes == old_size {
            return Ok((old_size, old_size));
        }
        let size = format!("{new_size_bytes}B");
        self.run_rbd(&["resize", "--size", &size, &self.spec_of(obj.kind, name)])
            .map_err(|e| Self::classify(e, name))?;
        info!(
            volume = name,
            old_size,
            new_size = new_size_bytes,
            "rbd volume resized"
        );
        Ok((old_size, new_size_bytes))
    }

    fn get_volume_info(&self, name: &str) -> CubecowResult<Volume> {
        let obj = self.resolve(name)?;
        self.project_volume(name, &obj)
    }

    fn get_volume_block_info(&self, name: &str) -> CubecowResult<VolumeBlockInfo> {
        let vol = self.get_volume_info(name)?;
        Ok(VolumeBlockInfo {
            num_blocks: vol.size_bytes / RBD_BLOCK_SIZE as u64,
            block_size: RBD_BLOCK_SIZE,
        })
    }

    fn list_volumes(
        &self,
        page_size: usize,
        page_token: Option<&str>,
    ) -> (Vec<Volume>, Option<String>, usize) {
        let volumes: Vec<String> = self
            .pool_image_names("list_volumes")
            .iter()
            .filter_map(|n| n.strip_prefix(VOLUME_PREFIX).map(str::to_string))
            .collect();
        let total = volumes.len();

        let start = match page_token {
            Some(tok) => volumes.iter().position(|n| n == tok).unwrap_or(total),
            None => 0,
        };
        let effective_page_size = if page_size == 0 { total } else { page_size };
        let end = (start + effective_page_size).min(total);

        let mut out = Vec::with_capacity(end.saturating_sub(start));
        for name in &volumes[start..end] {
            let projected = self
                .image_info(&self.spec_of(ImageKind::Volume, name), name)
                .and_then(|info| {
                    self.project_volume(
                        name,
                        &RbdObject {
                            kind: ImageKind::Volume,
                            info,
                        },
                    )
                });
            match projected {
                Ok(v) => out.push(v),
                Err(e) => {
                    warn!(volume = %name, error = %e, "rbd volume disappeared mid-list");
                }
            }
        }
        let next_token = if end < total {
            Some(volumes[end].clone())
        } else {
            None
        };
        (out, next_token, total)
    }

    fn create_snapshot(
        &self,
        source_name: &str,
        snapshot_name: &str,
        activate: bool,
    ) -> CubecowResult<Snapshot> {
        Self::validate_name(snapshot_name, "snapshot")?;
        // Ownership-check the source before creating an anchor snapshot on it.
        let source = self.resolve(source_name).map_err(|e| match e {
            CubecowError::NotFound(_) => {
                CubecowError::NotFound(format!("source '{source_name}' for snapshot"))
            }
            other => other,
        })?;
        let src_image = source.kind.image_name(source_name);
        // A snapshot-of-a-snapshot inherits the recorded origin, so every
        // snapshot points at the real volume (reflink's flattened ancestry).
        let origin = if source.kind == ImageKind::Snapshot {
            self.image_meta(&src_image)?
                .get(META_ORIGIN)
                .cloned()
                .unwrap_or_else(|| source_name.to_string())
        } else {
            source_name.to_string()
        };
        // Uniqueness across the flat namespace: the clone itself guards
        // the snap- side (17), so probe the vol- side.
        if self.exists(ImageKind::Volume, snapshot_name)? {
            return Err(CubecowError::AlreadyExists(format!(
                "name '{snapshot_name}' already exists in rbd namespace"
            )));
        }

        // Anchor snapshot on the source to clone from; removed once the
        // clone is flattened. A leftover from a crashed attempt froze the
        // source as of the crash, so it is recreated, never reused.
        let tmp_snap = format!("{TRANSIENT_SNAP_PREFIX}{snapshot_name}");
        let tmp_spec = self.snap_spec(&src_image, &tmp_snap);
        // Clone, then flatten into an independent image (no parent chain).
        let dst_image = ImageKind::Snapshot.image_name(snapshot_name);
        let dst = self.spec(&dst_image);
        {
            // Locked so a racing same-name create cannot remove this
            // anchor mid-clone. The clone is O(1) metadata; the long
            // flatten runs unlocked (a removed clone-v2 parent snapshot
            // is merely trashed until the child flattens).
            let _serialized = self.snapshot_lock.lock().expect("snapshot lock poisoned");
            if let Err(e) = self.run_rbd(&["snap", "create", &tmp_spec]) {
                match Self::classify(e, &tmp_spec) {
                    CubecowError::AlreadyExists(_) => {
                        self.run_rbd(&["snap", "rm", &tmp_spec])
                            .map_err(|e| Self::classify(e, &tmp_spec))?;
                        self.run_rbd(&["snap", "create", &tmp_spec])
                            .map_err(|e| Self::classify(e, &tmp_spec))?;
                    }
                    other => return Err(other),
                }
            }
            if let Err(e) = self.run_rbd(&[
                "--rbd_default_clone_format",
                "2",
                "clone",
                "--image-feature",
                &self.cfg.image_features,
                &tmp_spec,
                &dst,
            ]) {
                Self::cleanup_warn(
                    "snap rm",
                    &tmp_spec,
                    self.run_rbd(&["snap", "rm", &tmp_spec]),
                );
                return Err(Self::classify(e, snapshot_name));
            }
        }
        // Record the origin before the long flatten (the kind is already
        // carried by the image name). The clone copies the source's meta,
        // so the key must be overwritten.
        if let Err(e) = self.image_meta_set(&dst_image, META_ORIGIN, &origin) {
            Self::cleanup_warn("rm", &dst, self.run_rbd(&["rm", &dst]));
            Self::cleanup_warn(
                "snap rm",
                &tmp_spec,
                self.run_rbd(&["snap", "rm", &tmp_spec]),
            );
            return Err(e);
        }
        if let Err(e) = self.flatten_image(&dst_image) {
            Self::cleanup_warn("rm", &dst, self.run_rbd_slow(&["rm", &dst]));
            Self::cleanup_warn(
                "snap rm",
                &tmp_spec,
                self.run_rbd(&["snap", "rm", &tmp_spec]),
            );
            return Err(e);
        }
        // The clone is now independent; drop the anchor snapshot.
        if let Err(e) = self.run_rbd(&["snap", "rm", &tmp_spec]) {
            warn!(
                spec = %tmp_spec,
                error = %Self::classify(e, &tmp_spec),
                "rbd anchor snapshot cleanup failed (harmless leftover)"
            );
        }

        // Finish projection under a cleanup guard: any failure must not
        // leave a completed image behind (a retry would otherwise hit
        // AlreadyExists).
        let result = (|| -> CubecowResult<Snapshot> {
            let info = self.image_info(&dst, snapshot_name)?;
            // A just-created image is only mapped if we map it here.
            let device_path = if activate {
                self.map_device(&dst_image, "")?
            } else {
                String::new()
            };
            Ok(Self::snapshot_view(
                snapshot_name,
                &origin,
                &info,
                device_path,
            ))
        })();
        let snapshot = match result {
            Ok(s) => s,
            Err(e) => {
                Self::cleanup_warn("unmap", &dst_image, self.unmap_device(&dst_image, ""));
                Self::cleanup_warn("rm", &dst, self.run_rbd_slow(&["rm", &dst]));
                return Err(e);
            }
        };
        info!(
            snapshot = snapshot_name,
            source = source_name,
            activate,
            "rbd snapshot created"
        );
        Ok(snapshot)
    }

    fn delete_snapshot(&self, snapshot_name: &str) -> CubecowResult<()> {
        let obj = match self.resolve(snapshot_name) {
            Ok(obj) => obj,
            Err(CubecowError::NotFound(_)) => {
                return Err(CubecowError::NotFound(format!(
                    "snapshot '{snapshot_name}'"
                )))
            }
            Err(e) => return Err(e),
        };
        if obj.kind != ImageKind::Snapshot {
            return Err(CubecowError::InvalidArg(format!(
                "'{snapshot_name}' is a volume; use delete_volume instead"
            )));
        }
        let image = ImageKind::Snapshot.image_name(snapshot_name);
        self.unmap_device(&image, "")?;
        self.remove_image(&image)?;
        info!(snapshot = snapshot_name, "rbd snapshot deleted");
        Ok(())
    }

    fn list_snapshots(
        &self,
        volume_name: &str,
        page_size: usize,
        page_token: Option<&str>,
    ) -> (Vec<Snapshot>, Option<String>) {
        // Kind comes from the name; only snap-* images pay a meta read
        // (for the origin filter).
        let mut matching: Vec<String> = Vec::new();
        for image in &self.pool_image_names("list_snapshots") {
            let Some(name) = image.strip_prefix(SNAPSHOT_PREFIX) else {
                continue;
            };
            match self.image_meta(image) {
                Ok(meta) => {
                    if meta.get(META_ORIGIN).map(String::as_str) == Some(volume_name) {
                        matching.push(name.to_string());
                    }
                }
                Err(e) => {
                    warn!(image = %image, error = %e, "rbd image-meta failed during list_snapshots");
                }
            }
        }
        let total = matching.len();

        let start = match page_token {
            Some(tok) => matching.iter().position(|n| n == tok).unwrap_or(total),
            None => 0,
        };
        let effective_page_size = if page_size == 0 { total } else { page_size };
        let end = (start + effective_page_size).min(total);

        let mut out = Vec::with_capacity(end.saturating_sub(start));
        for name in &matching[start..end] {
            let projected = self
                .image_info(&self.spec_of(ImageKind::Snapshot, name), name)
                .and_then(|info| self.project_snapshot(name, volume_name, &info));
            match projected {
                Ok(s) => out.push(s),
                Err(e) => {
                    warn!(snapshot = %name, error = %e, "rbd snapshot disappeared mid-list");
                }
            }
        }
        let next_token = if end < total {
            Some(matching[end].clone())
        } else {
            None
        };
        (out, next_token)
    }

    fn activate_volume(&self, name: &str) -> CubecowResult<Volume> {
        let obj = self.resolve(name)?;
        let device = self.map_device(&obj.kind.image_name(name), "")?;
        Ok(Self::volume_view(name, &obj.info, device))
    }

    fn deactivate_volume(&self, name: &str) -> CubecowResult<()> {
        let obj = self.resolve(name)?;
        self.unmap_device(&obj.kind.image_name(name), "")
    }

    fn reset_node_storage(&self) -> CubecowResult<()> {
        // Resetting *node* storage on a shared backend detaches this
        // node instead of destroying cluster data: unmap every locally
        // mapped image of our pool/namespace and touch nothing else.
        let _serialized = self.map_lock.lock().expect("map lock poisoned");
        let mappings = self.load_mappings()?;
        let mut cleared = 0usize;
        for m in &mappings {
            if m.pool != self.cfg.pool || m.namespace != self.cfg.namespace {
                continue;
            }
            self.run_rbd(&["unmap", &m.device])
                .map_err(|e| Self::classify(e, &m.device))?;
            cleared += 1;
        }
        self.invalidate_mappings_cache();
        info!(pool = %self.pool_spec(), cleared, "rbd node storage reset (local mappings only)");
        Ok(())
    }

    fn metrics(&self) -> HashMap<String, u64> {
        if let Some((at, cached)) = self
            .metrics_cache
            .lock()
            .expect("metrics cache poisoned")
            .clone()
        {
            if at.elapsed() < METRICS_CACHE_TTL {
                return cached;
            }
        }
        // Latecomers on the single-flight reuse the cache the winner wrote.
        let _flight = self
            .metrics_refresh
            .lock()
            .expect("metrics refresh poisoned");
        let prev = self
            .metrics_cache
            .lock()
            .expect("metrics cache poisoned")
            .clone();
        if let Some((at, cached)) = &prev {
            if at.elapsed() < METRICS_CACHE_TTL {
                return cached.clone();
            }
        }

        let mut metrics = HashMap::new();
        let capacity: Option<(u64, u64)> = match self.run_ceph(&["df", "--format", "json"]) {
            Ok(out) => match serde_json::from_str::<CephDf>(out.trim()) {
                Ok(df) => match df.pools.iter().find(|p| p.name == self.cfg.pool) {
                    Some(pool) => {
                        // `stored` is logical bytes; `bytes_used` is raw
                        // (replication overhead included) while `max_avail`
                        // is logical — mixing them overstates the fill by
                        // the replication factor. Old clusters lack `stored`.
                        let used = pool.stats.stored.unwrap_or(pool.stats.bytes_used);
                        Some((used, used + pool.stats.max_avail))
                    }
                    None => {
                        warn!(pool = %self.cfg.pool, "pool missing from ceph df output");
                        None
                    }
                },
                Err(e) => {
                    warn!(error = %e, "parse ceph df output failed");
                    None
                }
            },
            Err(e) => {
                warn!(error = %e, "ceph df failed");
                None
            }
        };
        // Omit the capacity keys when `ceph df` fails: consumers treat
        // absence as "capacity unknown" and degrade safely; fabricating 0
        // or stale values would report a possibly-full pool as healthy.
        if let Some((used, total)) = capacity {
            metrics.insert(METRIC_USED_BYTES.to_string(), used);
            metrics.insert(METRIC_TOTAL_BYTES.to_string(), total);
        }
        // Kind is encoded in the image name, so one `rbd ls` classifies
        // the pool with no per-image reads.
        let names = self.pool_image_names("metrics");
        let volume_count = names
            .iter()
            .filter(|n| n.starts_with(VOLUME_PREFIX))
            .count() as u64;
        let snapshot_count = names
            .iter()
            .filter(|n| n.starts_with(SNAPSHOT_PREFIX))
            .count() as u64;
        metrics.insert(METRIC_VOLUME_COUNT.to_string(), volume_count);
        metrics.insert(METRIC_SNAPSHOT_COUNT.to_string(), snapshot_count);
        *self.metrics_cache.lock().expect("metrics cache poisoned") =
            Some((Instant::now(), metrics.clone()));
        metrics
    }
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

/// Convert rbd's ctime-style `create_timestamp` ("Thu Jul  3 15:04:05
/// 2026") to RFC3339, matching the other backends. Falls back to the
/// raw string when the format is unexpected. The CLI prints the
/// node-local time but no zone, so it is taken as UTC — on a non-UTC
/// host `created_at` is skewed by the zone offset (display-only).
fn rfc3339_from_rbd_timestamp(ts: &str) -> String {
    if ts.is_empty() {
        return String::new();
    }
    // ctime pads the day with a space ("Jul  3"); collapse runs of
    // whitespace and drop the weekday so the rest parses as plain
    // fields (chrono would reject an inconsistent weekday).
    let fields: Vec<&str> = ts.split_whitespace().collect();
    let without_weekday = match fields.as_slice() {
        [_, rest @ ..] if rest.len() == 4 => rest.join(" "),
        _ => return ts.to_string(),
    };
    match NaiveDateTime::parse_from_str(&without_weekday, "%b %d %H:%M:%S %Y") {
        Ok(naive) => DateTime::<Utc>::from_naive_utc_and_offset(naive, Utc).to_rfc3339(),
        Err(_) => ts.to_string(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::VecDeque;
    use std::sync::Mutex as StdMutex;

    /// Scripted command runner: a FIFO of expected (program, args)
    /// pairs with canned results. Any mismatch or leftover expectation
    /// fails the test.
    struct MockRunner {
        expectations: StdMutex<VecDeque<(String, Vec<String>, CubecowResult<String>)>>,
    }

    impl MockRunner {
        fn new() -> Arc<Self> {
            Arc::new(Self {
                expectations: StdMutex::new(VecDeque::new()),
            })
        }

        fn expect(self: &Arc<Self>, program: &str, args: &[&str], result: CubecowResult<String>) {
            self.expectations.lock().unwrap().push_back((
                program.to_string(),
                args.iter().map(|s| s.to_string()).collect(),
                result,
            ));
        }

        fn verify_drained(self: &Arc<Self>) {
            let left = self.expectations.lock().unwrap();
            assert!(
                left.is_empty(),
                "unconsumed command expectations: {:?}",
                left.iter()
                    .map(|(p, a, _)| format!("{p} {a:?}"))
                    .collect::<Vec<_>>()
            );
        }
    }

    impl CommandRunner for MockRunner {
        fn run(&self, program: &str, args: &[&str]) -> CubecowResult<String> {
            let mut q = self.expectations.lock().unwrap();
            let (want_prog, want_args, result) = q
                .pop_front()
                .unwrap_or_else(|| panic!("unexpected command: {program} {args:?}"));
            assert_eq!(program, want_prog, "program mismatch (args: {args:?})");
            let got: Vec<String> = args.iter().map(|s| s.to_string()).collect();
            assert_eq!(got, want_args, "args mismatch for {program}");
            result
        }
    }

    fn cmd_failed(reason: &str) -> CubecowError {
        CubecowError::CommandFailed {
            cmd: "rbd".to_string(),
            reason: reason.to_string(),
            code: None,
        }
    }

    fn cmd_failed_code(reason: &str, code: i32) -> CubecowError {
        CubecowError::CommandFailed {
            cmd: "rbd".to_string(),
            reason: reason.to_string(),
            code: Some(code),
        }
    }

    fn test_engine(runner: Arc<MockRunner>) -> RbdEngine {
        let cfg = RbdConfig {
            pool: "cubecow".to_string(),
            ..RbdConfig::default()
        };
        // One mock backs both the fast and slow runners, so a `flatten`
        // (slow runner) appears in the same scripted FIFO.
        RbdEngine::new_with_runners(cfg, runner.clone(), runner)
    }

    const NO_MAPPINGS: &str = "[]";

    fn info_json(size: u64) -> String {
        format!(
            "{{\"name\":\"x\",\"size\":{size},\"create_timestamp\":\"Thu Jul  3 15:04:05 2026\"}}"
        )
    }

    fn expect_info_not_found(runner: &Arc<MockRunner>, image: &str) {
        runner.expect(
            "rbd",
            &["info", &format!("cubecow/{image}"), "--format", "json"],
            Err(cmd_failed("(2) No such file or directory")),
        );
    }

    /// Script `resolve(name)`: probe `vol-<name>`, then `snap-<name>`.
    fn expect_resolve(runner: &Arc<MockRunner>, name: &str, size: u64, kind: &str) {
        if kind == "volume" {
            runner.expect(
                "rbd",
                &["info", &format!("cubecow/vol-{name}"), "--format", "json"],
                Ok(info_json(size)),
            );
        } else {
            expect_info_not_found(runner, &format!("vol-{name}"));
            runner.expect(
                "rbd",
                &["info", &format!("cubecow/snap-{name}"), "--format", "json"],
                Ok(info_json(size)),
            );
        }
    }

    #[test]
    fn create_volume_maps() {
        let runner = MockRunner::new();
        let engine = test_engine(runner.clone());

        // flat-namespace probe of the snap- side
        expect_info_not_found(&runner, "snap-v1");
        runner.expect(
            "rbd",
            &[
                "create",
                "--size",
                "1048576B",
                "--image-feature",
                "layering,exclusive-lock",
                "cubecow/vol-v1",
            ],
            Ok(String::new()),
        );
        runner.expect(
            "rbd",
            &["showmapped", "--format", "json"],
            Ok(NO_MAPPINGS.to_string()),
        );
        runner.expect(
            "rbd",
            &["map", "cubecow/vol-v1"],
            Ok("/dev/rbd0\n".to_string()),
        );
        runner.expect(
            "rbd",
            &["info", "cubecow/vol-v1", "--format", "json"],
            Ok(info_json(1048576)),
        );

        let vol = engine.create_volume("v1", 1048576).unwrap();
        assert_eq!(vol.name, "v1");
        assert_eq!(vol.size_bytes, 1048576);
        assert_eq!(vol.device_path, "/dev/rbd0");
        assert_eq!(vol.snapshot_count, 0);
        assert!(vol.created_at.starts_with("2026-07-03T15:04:05"));
        runner.verify_drained();
    }

    #[test]
    fn create_volume_duplicate_is_already_exists() {
        let runner = MockRunner::new();
        let engine = test_engine(runner.clone());

        expect_info_not_found(&runner, "snap-v1");
        runner.expect(
            "rbd",
            &[
                "create",
                "--size",
                "1048576B",
                "--image-feature",
                "layering,exclusive-lock",
                "cubecow/vol-v1",
            ],
            Err(cmd_failed("rbd: create error: (17) File exists")),
        );
        let err = engine.create_volume("v1", 1048576).unwrap_err();
        assert!(matches!(err, CubecowError::AlreadyExists(_)), "{err:?}");
        runner.verify_drained();
    }

    #[test]
    fn classify_prefers_exit_code_over_stderr_text() {
        // stderr echoes image names: "busybox" must not shadow the errno.
        let not_found = cmd_failed_code(
            "exit code 2: rbd: error opening image busybox-rootfs: (2) No such file or directory",
            2,
        );
        assert!(matches!(
            RbdEngine::classify(not_found, "busybox-rootfs"),
            CubecowError::NotFound(_)
        ));
        // A marker-free reason classifies on the exit code alone.
        let blocked = cmd_failed_code("exit code 16: ", 16);
        assert!(matches!(
            RbdEngine::classify(blocked, "v1"),
            CubecowError::PreconditionFailed(_)
        ));
        // Bare name fragments in stderr never classify without a code.
        let unknown = cmd_failed("something about busybox failed");
        assert!(matches!(
            RbdEngine::classify(unknown, "busybox"),
            CubecowError::CommandFailed { .. }
        ));
    }

    #[test]
    fn validate_name_rejects_bad_inputs() {
        assert!(RbdEngine::validate_name("", "volume").is_err());
        assert!(RbdEngine::validate_name("a/b", "volume").is_err());
        assert!(RbdEngine::validate_name("a@b", "volume").is_err());
        assert!(RbdEngine::validate_name(".hidden", "volume").is_err());
        assert!(RbdEngine::validate_name("-flag", "volume").is_err());
        assert!(RbdEngine::validate_name("ok-name_1", "volume").is_ok());
    }

    #[test]
    fn create_snapshot_clones_flattens_and_tags() {
        let runner = MockRunner::new();
        let engine = test_engine(runner.clone());

        // source resolve (volume: origin = source, no meta read)
        expect_resolve(&runner, "src", 4096, "volume");
        // flat-namespace probe of the vol- side of the destination
        expect_info_not_found(&runner, "vol-snap");
        // anchor snap -> clone -> tag origin -> flatten -> anchor rm
        runner.expect(
            "rbd",
            &["snap", "create", "cubecow/vol-src@__cow_snap"],
            Ok(String::new()),
        );
        runner.expect(
            "rbd",
            &[
                "--rbd_default_clone_format",
                "2",
                "clone",
                "--image-feature",
                "layering,exclusive-lock",
                "cubecow/vol-src@__cow_snap",
                "cubecow/snap-snap",
            ],
            Ok(String::new()),
        );
        runner.expect(
            "rbd",
            &[
                "image-meta",
                "set",
                "cubecow/snap-snap",
                "cubecow.origin",
                "src",
            ],
            Ok(String::new()),
        );
        runner.expect("rbd", &["flatten", "cubecow/snap-snap"], Ok(String::new()));
        runner.expect(
            "rbd",
            &["snap", "rm", "cubecow/vol-src@__cow_snap"],
            Ok(String::new()),
        );
        // projection (not activated: no mapping lookup for a fresh image)
        runner.expect(
            "rbd",
            &["info", "cubecow/snap-snap", "--format", "json"],
            Ok(info_json(4096)),
        );

        let snap = engine.create_snapshot("src", "snap", false).unwrap();
        assert_eq!(snap.name, "snap");
        assert_eq!(snap.origin_volume, "src");
        assert_eq!(snap.size_bytes, 4096);
        assert_eq!(snap.device_path, "");
        runner.verify_drained();
    }

    #[test]
    fn create_snapshot_recreates_stale_anchor() {
        let runner = MockRunner::new();
        let engine = test_engine(runner.clone());

        expect_resolve(&runner, "src", 4096, "volume");
        expect_info_not_found(&runner, "vol-snap");
        // A leftover anchor from a crashed attempt froze the source as of
        // the crash: it must be removed and recreated, never cloned from.
        runner.expect(
            "rbd",
            &["snap", "create", "cubecow/vol-src@__cow_snap"],
            Err(cmd_failed("(17) File exists")),
        );
        runner.expect(
            "rbd",
            &["snap", "rm", "cubecow/vol-src@__cow_snap"],
            Ok(String::new()),
        );
        runner.expect(
            "rbd",
            &["snap", "create", "cubecow/vol-src@__cow_snap"],
            Ok(String::new()),
        );
        // Fail the clone to end the call here: the fresh anchor is
        // cleaned up and the error propagates.
        runner.expect(
            "rbd",
            &[
                "--rbd_default_clone_format",
                "2",
                "clone",
                "--image-feature",
                "layering,exclusive-lock",
                "cubecow/vol-src@__cow_snap",
                "cubecow/snap-snap",
            ],
            Err(cmd_failed("(5) Input/output error")),
        );
        runner.expect(
            "rbd",
            &["snap", "rm", "cubecow/vol-src@__cow_snap"],
            Ok(String::new()),
        );

        assert!(engine.create_snapshot("src", "snap", false).is_err());
        runner.verify_drained();
    }

    #[test]
    fn create_snapshot_rejects_missing_source() {
        let runner = MockRunner::new();
        let engine = test_engine(runner.clone());
        expect_info_not_found(&runner, "vol-nope");
        expect_info_not_found(&runner, "snap-nope");
        let err = engine.create_snapshot("nope", "s", false).unwrap_err();
        assert!(matches!(err, CubecowError::NotFound(_)), "{err:?}");
        runner.verify_drained();
    }

    #[test]
    fn delete_volume_removes_image() {
        let runner = MockRunner::new();
        let engine = test_engine(runner.clone());

        expect_resolve(&runner, "v1", 4096, "volume");
        // unmap: not mapped
        runner.expect(
            "rbd",
            &["showmapped", "--format", "json"],
            Ok(NO_MAPPINGS.to_string()),
        );
        // remove_image: snap ls (none) then rm
        runner.expect(
            "rbd",
            &["snap", "ls", "cubecow/vol-v1", "--format", "json"],
            Ok("[]".to_string()),
        );
        runner.expect("rbd", &["rm", "cubecow/vol-v1"], Ok(String::new()));

        engine.delete_volume("v1").unwrap();
        runner.verify_drained();
    }

    #[test]
    fn delete_volume_rejects_snapshot() {
        let runner = MockRunner::new();
        let engine = test_engine(runner.clone());
        expect_resolve(&runner, "s1", 4096, "snapshot");
        let err = engine.delete_volume("s1").unwrap_err();
        assert!(matches!(err, CubecowError::InvalidArg(_)), "{err:?}");
        runner.verify_drained();
    }

    #[test]
    fn delete_snapshot_removes_image() {
        let runner = MockRunner::new();
        let engine = test_engine(runner.clone());

        expect_resolve(&runner, "s1", 4096, "snapshot");
        runner.expect(
            "rbd",
            &["showmapped", "--format", "json"],
            Ok(NO_MAPPINGS.to_string()),
        );
        runner.expect(
            "rbd",
            &["snap", "ls", "cubecow/snap-s1", "--format", "json"],
            Ok("[]".to_string()),
        );
        runner.expect("rbd", &["rm", "cubecow/snap-s1"], Ok(String::new()));

        engine.delete_snapshot("s1").unwrap();
        runner.verify_drained();
    }

    #[test]
    fn delete_volume_blocked_goes_to_trash() {
        let runner = MockRunner::new();
        let engine = test_engine(runner.clone());

        expect_resolve(&runner, "v1", 4096, "volume");
        runner.expect(
            "rbd",
            &["showmapped", "--format", "json"],
            Ok(NO_MAPPINGS.to_string()),
        );
        runner.expect(
            "rbd",
            &["snap", "ls", "cubecow/vol-v1", "--format", "json"],
            Ok("[]".to_string()),
        );
        // Real watcher output has neither "(16)" nor "busy": the exit
        // code alone must classify it.
        runner.expect(
            "rbd",
            &["rm", "cubecow/vol-v1"],
            Err(cmd_failed_code(
                "exit code 16: rbd: error: image still has watchers",
                16,
            )),
        );
        runner.expect("rbd", &["trash", "mv", "cubecow/vol-v1"], Ok(String::new()));

        engine.delete_volume("v1").unwrap();
        runner.verify_drained();
    }

    #[test]
    fn resize_grows_and_rejects_shrink() {
        let runner = MockRunner::new();
        let engine = test_engine(runner.clone());

        expect_resolve(&runner, "v1", 100, "volume");
        runner.expect(
            "rbd",
            &["resize", "--size", "200B", "cubecow/vol-v1"],
            Ok(String::new()),
        );
        let (old, new) = engine.resize_volume("v1", 200).unwrap();
        assert_eq!((old, new), (100, 200));

        expect_resolve(&runner, "v1", 100, "volume");
        let err = engine.resize_volume("v1", 50).unwrap_err();
        assert!(matches!(err, CubecowError::InvalidArg(_)), "{err:?}");
        runner.verify_drained();
    }

    #[test]
    fn activate_is_idempotent_when_already_mapped() {
        let runner = MockRunner::new();
        let engine = test_engine(runner.clone());

        expect_resolve(&runner, "v1", 4096, "volume");
        runner.expect(
            "rbd",
            &["showmapped", "--format", "json"],
            Ok(r#"[{"pool":"cubecow","namespace":"","name":"vol-v1","snap":"-","device":"/dev/rbd0"}]"#
                .to_string()),
        );

        let vol = engine.activate_volume("v1").unwrap();
        assert_eq!(vol.device_path, "/dev/rbd0");
        runner.verify_drained();
    }

    #[test]
    fn deactivate_unmaps_and_is_idempotent() {
        let runner = MockRunner::new();
        let engine = test_engine(runner.clone());

        expect_resolve(&runner, "v1", 4096, "volume");
        runner.expect(
            "rbd",
            &["showmapped", "--format", "json"],
            Ok(r#"[{"pool":"cubecow","namespace":"","name":"vol-v1","snap":"-","device":"/dev/rbd0"}]"#
                .to_string()),
        );
        runner.expect("rbd", &["unmap", "/dev/rbd0"], Ok(String::new()));

        engine.deactivate_volume("v1").unwrap();
        runner.verify_drained();
    }

    #[test]
    fn list_snapshots_filters_by_origin() {
        let runner = MockRunner::new();
        let engine = test_engine(runner.clone());

        runner.expect(
            "rbd",
            &["ls", "cubecow", "--format", "json"],
            Ok(r#"["snap-s1","vol-v1"]"#.to_string()),
        );
        // Only the snap-* image pays a meta read (origin filter).
        runner.expect(
            "rbd",
            &["image-meta", "list", "cubecow/snap-s1", "--format", "json"],
            Ok(r#"{"cubecow.origin":"v1"}"#.to_string()),
        );
        // project s1
        runner.expect(
            "rbd",
            &["info", "cubecow/snap-s1", "--format", "json"],
            Ok(info_json(4096)),
        );
        runner.expect(
            "rbd",
            &["showmapped", "--format", "json"],
            Ok(NO_MAPPINGS.to_string()),
        );

        let (snaps, next) = engine.list_snapshots("v1", 0, None);
        assert_eq!(snaps.len(), 1);
        assert_eq!(snaps[0].name, "s1");
        assert_eq!(snaps[0].origin_volume, "v1");
        assert!(next.is_none());
        runner.verify_drained();
    }

    #[test]
    fn metrics_reports_pool_capacity_and_counts() {
        let runner = MockRunner::new();
        let engine = test_engine(runner.clone());

        // stored is logical, bytes_used raw (3x-replicated here): the
        // metric must come from stored so used=100, not 300.
        runner.expect(
            "ceph",
            &["df", "--format", "json"],
            Ok(r#"{"pools":[{"name":"cubecow","stats":{"stored":100,"bytes_used":300,"max_avail":900}}]}"#
                .to_string()),
        );
        // Counts come from the name prefixes alone: no per-image reads.
        runner.expect(
            "rbd",
            &["ls", "cubecow", "--format", "json"],
            Ok(r#"["snap-s1","vol-v1"]"#.to_string()),
        );

        let m = engine.metrics();
        assert_eq!(m.get(METRIC_USED_BYTES), Some(&100));
        assert_eq!(m.get(METRIC_TOTAL_BYTES), Some(&1000));
        assert_eq!(m.get(METRIC_VOLUME_COUNT), Some(&1));
        assert_eq!(m.get(METRIC_SNAPSHOT_COUNT), Some(&1));
        runner.verify_drained();
    }

    #[test]
    fn metrics_omits_capacity_keys_when_ceph_df_fails() {
        let runner = MockRunner::new();
        let engine = test_engine(runner.clone());

        runner.expect(
            "ceph",
            &["df", "--format", "json"],
            Err(cmd_failed("timed out after 60s")),
        );
        runner.expect(
            "rbd",
            &["ls", "cubecow", "--format", "json"],
            Ok("[]".to_string()),
        );

        // Absent keys mean "capacity unknown"; a fabricated 0 would read
        // as a healthy empty pool.
        let m = engine.metrics();
        assert!(m.get(METRIC_USED_BYTES).is_none());
        assert!(m.get(METRIC_TOTAL_BYTES).is_none());
        assert_eq!(m.get(METRIC_VOLUME_COUNT), Some(&0));
        runner.verify_drained();
    }

    #[test]
    fn reset_unmaps_only_local_pool_mappings() {
        let runner = MockRunner::new();
        let engine = test_engine(runner.clone());

        runner.expect(
            "rbd",
            &["showmapped", "--format", "json"],
            Ok(r#"[{"pool":"cubecow","namespace":"","name":"vol-v1","snap":"-","device":"/dev/rbd0"},{"pool":"other","namespace":"","name":"x","snap":"-","device":"/dev/rbd9"}]"#
                .to_string()),
        );
        runner.expect("rbd", &["unmap", "/dev/rbd0"], Ok(String::new()));

        engine.reset_node_storage().unwrap();
        runner.verify_drained();
    }
}
