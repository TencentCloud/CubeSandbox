// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
// Configuration management
//
// Loads and validates configuration from TOML files or in-memory JSON strings.
// Provides typed access to all configuration parameters.

use std::path::{Path, PathBuf};

use serde::Deserialize;

use crate::pkg::errors::{CubecowError, CubecowResult};

/// Top-level application configuration.
#[derive(Debug, Clone, Deserialize)]
pub struct AppConfig {
    /// Logging configuration
    pub log: LogConfig,
    /// Storage backend selection. The xfs-reflink backend is the only
    /// shipping backend at the moment, but the [`BackendConfig`] block
    /// is preserved so future backends can be added without changing
    /// existing config files.
    #[serde(default)]
    pub backend: BackendConfig,
}

/// Backend selection block.
///
/// Controls which storage backend the engine uses at runtime. Backends
/// implement the same `Engine` trait, so all FFI / SDK consumers see
/// identical behaviour at the API level.
#[derive(Debug, Clone, Default, Deserialize)]
pub struct BackendConfig {
    /// Which backend to instantiate. See [`BackendKind`].
    #[serde(default)]
    pub kind: BackendKind,
    /// Settings for the xfs-reflink backend.
    #[serde(default)]
    pub reflink: ReflinkConfig,
    /// Settings for the Ceph RBD backend.
    #[serde(default)]
    pub rbd: RbdConfig,
}

/// Configuration for the xfs-reflink backend.
///
/// The reflink backend is intentionally minimal: The deployer is
/// expected to provide a directory that already lives on a filesystem
/// supporting the `FICLONE` ioctl (typically xfs mounted with
/// `reflink=1`, but Btrfs / OCFS2 also work). cubecow will create and
/// own the `volumes/` subtree underneath this directory; everything
/// else under `root_dir` is left untouched.
#[derive(Debug, Clone, Deserialize)]
pub struct ReflinkConfig {
    /// Filesystem directory under which the reflink backend stores all
    /// volume / snapshot files. Must live on a `FICLONE`-capable
    /// filesystem; the engine probes this on startup and refuses to
    /// initialise if the probe fails.
    ///
    /// Default: `/var/lib/cubecow/reflink`.
    #[serde(default = "default_reflink_root_dir")]
    pub root_dir: PathBuf,
}

impl Default for ReflinkConfig {
    fn default() -> Self {
        Self {
            root_dir: default_reflink_root_dir(),
        }
    }
}

fn default_reflink_root_dir() -> PathBuf {
    PathBuf::from("/var/lib/cubecow/reflink")
}

/// Configuration for the Ceph RBD backend.
///
/// The backend drives the `rbd` CLI (and `ceph` for pool capacity), so
/// cluster connectivity follows the standard Ceph client conventions:
/// `/etc/ceph/ceph.conf` and the default admin keyring unless `conf` /
/// `client` say otherwise. The configured pool (optionally narrowed to
/// an RBD namespace) should be dedicated to cubecow: the backend lists
/// and classifies every image in it by convention. `reset_node_storage`
/// only unmaps this node's local devices in the pool and never deletes
/// cluster data.
///
/// A deletion that is positively blocked (image busy or still watched)
/// falls back to `rbd trash mv` so the name frees up immediately; the
/// trash itself is never purged by cubecow — reclaiming that capacity
/// (`rbd trash purge` or a purge schedule) is the operator's call.
///
/// Requires Ceph Nautilus or newer: the backend relies on clone
/// format 2, the array form of `rbd showmapped --format json`, and
/// (preferred, with a fallback) `stored` in `ceph df`.
#[derive(Debug, Clone, Deserialize)]
pub struct RbdConfig {
    /// RBD pool holding all cubecow images. Required.
    #[serde(default)]
    pub pool: String,
    /// Optional RBD namespace within the pool.
    #[serde(default)]
    pub namespace: String,
    /// Optional ceph.conf path, forwarded as `--conf`.
    #[serde(default)]
    pub conf: Option<PathBuf>,
    /// Optional Ceph client id (without the `client.` prefix),
    /// forwarded as `--id`.
    #[serde(default)]
    pub client: Option<String>,
    /// `rbd` binary. Default: resolved from PATH.
    #[serde(default = "default_rbd_bin")]
    pub rbd_bin: String,
    /// `ceph` binary, used for pool capacity metrics.
    #[serde(default = "default_ceph_bin")]
    pub ceph_bin: String,
    /// Per-command timeout in seconds for regular rbd/ceph calls.
    #[serde(default = "default_rbd_cmd_timeout_secs")]
    pub cmd_timeout_secs: u64,
    /// Timeout in seconds for long-running maintenance commands
    /// (`rbd flatten`), which copy image data and scale with size.
    #[serde(default = "default_rbd_slow_cmd_timeout_secs")]
    pub slow_cmd_timeout_secs: u64,
    /// Image features for `rbd create` / `rbd clone`. Restricted to
    /// krbd-mappable features by default.
    #[serde(default = "default_rbd_image_features")]
    pub image_features: String,
    /// Optional krbd map options, forwarded as `rbd map -o <value>`
    /// (e.g. "exclusive").
    #[serde(default)]
    pub map_options: Option<String>,
}

impl Default for RbdConfig {
    fn default() -> Self {
        Self {
            pool: String::new(),
            namespace: String::new(),
            conf: None,
            client: None,
            rbd_bin: default_rbd_bin(),
            ceph_bin: default_ceph_bin(),
            cmd_timeout_secs: default_rbd_cmd_timeout_secs(),
            slow_cmd_timeout_secs: default_rbd_slow_cmd_timeout_secs(),
            image_features: default_rbd_image_features(),
            map_options: None,
        }
    }
}

fn default_rbd_bin() -> String {
    "rbd".to_string()
}
fn default_ceph_bin() -> String {
    "ceph".to_string()
}
fn default_rbd_cmd_timeout_secs() -> u64 {
    60
}
fn default_rbd_slow_cmd_timeout_secs() -> u64 {
    3600
}
fn default_rbd_image_features() -> String {
    "layering,exclusive-lock".to_string()
}

/// Available storage backends.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Deserialize, Default)]
#[serde(rename_all = "lowercase")]
pub enum BackendKind {
    /// xfs-reflink backend (FICLONE-based snapshots) — the **default**.
    #[default]
    Reflink,
    /// Ceph RBD backend (network block devices; snapshots and clones
    /// live in the cluster, so any node can attach any volume).
    Rbd,
}

/// Logging configuration.
#[derive(Debug, Clone, Deserialize)]
pub struct LogConfig {
    /// Log level: trace, debug, info, warn, error (default: info)
    #[serde(default = "default_log_level")]
    pub level: String,
    /// Output format: "json", "compact" or "pretty" (default: compact)
    #[serde(default = "default_log_format")]
    pub format: String,
    /// Optional log file path. When set, logs are written to this file
    /// with automatic rotation. If not set, logs go to stdout.
    #[serde(default)]
    pub file: Option<PathBuf>,
    /// Log file rotation policy: "daily", "hourly", or "never" (default: daily).
    /// Only effective when `file` is set.
    #[serde(default = "default_log_rotation")]
    pub rotation: String,
}

// ---------------------------------------------------------------------------
// Default value functions
// ---------------------------------------------------------------------------

fn default_log_level() -> String {
    "info".to_string()
}
fn default_log_format() -> String {
    "compact".to_string()
}
fn default_log_rotation() -> String {
    "daily".to_string()
}

// ---------------------------------------------------------------------------
// Config loading
// ---------------------------------------------------------------------------

impl AppConfig {
    /// Load configuration from a TOML file.
    pub fn load<P: AsRef<Path>>(path: P) -> CubecowResult<Self> {
        Self::load_toml_file(path)
    }

    /// Load configuration from a TOML file path.
    pub fn load_toml_file<P: AsRef<Path>>(path: P) -> CubecowResult<Self> {
        let path = path.as_ref();
        let content = std::fs::read_to_string(path).map_err(|e| {
            CubecowError::ConfigError(format!(
                "failed to read config file '{}': {e}",
                path.display()
            ))
        })?;
        Self::from_toml_str(&content).map_err(|e| match e {
            CubecowError::ConfigError(msg) => CubecowError::ConfigError(format!(
                "failed to parse config file '{}': {msg}",
                path.display()
            )),
            other => other,
        })
    }

    /// Parse configuration from a TOML string.
    ///
    /// Runs [`Self::validate`] after deserialisation so callers never see
    /// a half-baked `AppConfig` that names a backend without supplying the
    /// fields that backend requires.
    pub fn from_toml_str(content: &str) -> CubecowResult<Self> {
        let cfg: Self = toml::from_str(content)
            .map_err(|e| CubecowError::ConfigError(format!("failed to parse TOML config: {e}")))?;
        cfg.validate()?;
        Ok(cfg)
    }

    /// Parse configuration from a JSON string.
    ///
    /// Runs [`Self::validate`] after deserialisation, mirroring
    /// [`Self::from_toml_str`].
    pub fn from_json_str(content: &str) -> CubecowResult<Self> {
        let cfg: Self = serde_json::from_str(content)
            .map_err(|e| CubecowError::ConfigError(format!("failed to parse JSON config: {e}")))?;
        cfg.validate()?;
        Ok(cfg)
    }

    /// Validate that all configuration invariants hold for the selected
    /// backend.
    ///
    /// Engine constructors call this again as a defence-in-depth check,
    /// so even programmatically-built `AppConfig` values cannot bypass
    /// the contract.
    pub fn validate(&self) -> CubecowResult<()> {
        // ---- Universally-required fields --------------------------------
        match self.log.format.as_str() {
            "json" | "compact" | "pretty" => {}
            other => {
                return Err(CubecowError::ConfigError(format!(
                    "[log] format = \"{other}\" is invalid; expected one of \
                     \"json\", \"compact\", \"pretty\""
                )));
            }
        }
        match self.log.rotation.as_str() {
            "daily" | "hourly" | "never" => {}
            other => {
                return Err(CubecowError::ConfigError(format!(
                    "[log] rotation = \"{other}\" is invalid; expected one of \
                     \"daily\", \"hourly\", \"never\""
                )));
            }
        }

        // ---- Backend-specific required fields ----------------------------
        match self.backend.kind {
            BackendKind::Reflink => self.validate_reflink_requirements(),
            BackendKind::Rbd => self.validate_rbd_requirements(),
        }
    }

    /// Field-level validation for the reflink backend.
    ///
    /// The only thing this backend strictly needs is a non-empty,
    /// absolute `root_dir`. Filesystem capability (FICLONE support) is
    /// probed at engine startup, not here — `validate()` is meant to be
    /// cheap and side-effect free.
    fn validate_reflink_requirements(&self) -> CubecowResult<()> {
        let root_dir = &self.backend.reflink.root_dir;
        if root_dir.as_os_str().is_empty() {
            return Err(CubecowError::ConfigError(
                "[backend.reflink] root_dir must not be empty when backend.kind = \"reflink\""
                    .to_string(),
            ));
        }
        if !root_dir.is_absolute() {
            return Err(CubecowError::ConfigError(format!(
                "[backend.reflink] root_dir = \"{}\" must be an absolute path",
                root_dir.display()
            )));
        }
        Ok(())
    }

    /// Field-level validation for the rbd backend.
    ///
    /// Cluster reachability is probed at engine startup, not here —
    /// `validate()` is meant to be cheap and side-effect free.
    fn validate_rbd_requirements(&self) -> CubecowResult<()> {
        let rbd = &self.backend.rbd;
        if rbd.pool.is_empty() {
            return Err(CubecowError::ConfigError(
                "[backend.rbd] pool must not be empty when backend.kind = \"rbd\"".to_string(),
            ));
        }
        for (field, value) in [("pool", &rbd.pool), ("namespace", &rbd.namespace)] {
            if value.contains('/') || value.contains('@') || value.starts_with('-') {
                return Err(CubecowError::ConfigError(format!(
                    "[backend.rbd] {field} = \"{value}\" must not contain '/' or '@' \
                     or start with '-'"
                )));
            }
        }
        if rbd.cmd_timeout_secs == 0 || rbd.slow_cmd_timeout_secs == 0 {
            return Err(CubecowError::ConfigError(
                "[backend.rbd] command timeouts must be positive".to_string(),
            ));
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn from_json_str_applies_defaults() {
        let config = AppConfig::from_json_str(r#"{"log":{}}"#).unwrap();

        assert_eq!(config.log.level, "info");
        assert_eq!(config.log.format, "compact");
        assert_eq!(config.log.rotation, "daily");
        assert!(config.log.file.is_none());
        assert_eq!(config.backend.kind, BackendKind::Reflink);
        assert_eq!(
            config.backend.reflink.root_dir,
            PathBuf::from("/var/lib/cubecow/reflink")
        );
    }

    #[test]
    fn from_json_str_parses_explicit_values() {
        let config = AppConfig::from_json_str(
            r#"{
                "log": {
                    "level": "debug",
                    "format": "json",
                    "file": "/tmp/cubecow.log",
                    "rotation": "hourly"
                },
                "backend": {
                    "kind": "reflink",
                    "reflink": { "root_dir": "/data/cubecow/reflink" }
                }
            }"#,
        )
        .unwrap();

        assert_eq!(config.log.level, "debug");
        assert_eq!(config.log.format, "json");
        assert_eq!(config.log.file, Some(PathBuf::from("/tmp/cubecow.log")));
        assert_eq!(config.log.rotation, "hourly");
        assert_eq!(config.backend.kind, BackendKind::Reflink);
        assert_eq!(
            config.backend.reflink.root_dir,
            PathBuf::from("/data/cubecow/reflink")
        );
    }

    #[test]
    fn from_json_str_rejects_invalid_json() {
        let err = AppConfig::from_json_str(r#"{"log":{"level":42}}"#).unwrap_err();
        match err {
            CubecowError::ConfigError(msg) => {
                assert!(msg.contains("failed to parse JSON config"));
            }
            other => panic!("unexpected error: {other:?}"),
        }
    }

    #[test]
    fn validate_rejects_invalid_log_format() {
        let err = AppConfig::from_json_str(r#"{"log":{"format":"yaml"}}"#).unwrap_err();
        match err {
            CubecowError::ConfigError(msg) => {
                assert!(msg.contains("[log] format"));
                assert!(msg.contains("yaml"));
            }
            other => panic!("unexpected error: {other:?}"),
        }
    }

    #[test]
    fn validate_rejects_invalid_log_rotation() {
        let err = AppConfig::from_json_str(r#"{"log":{"rotation":"weekly"}}"#).unwrap_err();
        match err {
            CubecowError::ConfigError(msg) => {
                assert!(msg.contains("[log] rotation"));
                assert!(msg.contains("weekly"));
            }
            other => panic!("unexpected error: {other:?}"),
        }
    }

    #[test]
    fn validate_rejects_reflink_with_relative_root_dir() {
        let err = AppConfig::from_json_str(
            r#"{
                "log": {},
                "backend": {"reflink": {"root_dir": "var/lib/cubecow/reflink"}}
            }"#,
        )
        .unwrap_err();
        match err {
            CubecowError::ConfigError(msg) => {
                assert!(msg.contains("[backend.reflink] root_dir"));
                assert!(msg.contains("absolute"));
            }
            other => panic!("unexpected error: {other:?}"),
        }
    }

    #[test]
    fn validate_rejects_rbd_pool_that_parses_as_flag() {
        let err = AppConfig::from_json_str(
            r#"{
                "log": {},
                "backend": {"kind": "rbd", "rbd": {"pool": "--read-only"}}
            }"#,
        )
        .unwrap_err();
        match err {
            CubecowError::ConfigError(msg) => {
                assert!(msg.contains("[backend.rbd] pool"));
                assert!(msg.contains("start with '-'"));
            }
            other => panic!("unexpected error: {other:?}"),
        }
    }

    #[test]
    fn validate_accepts_minimal_reflink_config() {
        let config = AppConfig::from_json_str(r#"{"log":{}}"#).unwrap();
        assert_eq!(config.backend.kind, BackendKind::Reflink);
        assert!(config
            .backend
            .reflink
            .root_dir
            .starts_with("/var/lib/cubecow/reflink"));
        // Re-running validate() on the parsed value must remain a no-op.
        config.validate().unwrap();
    }
}
