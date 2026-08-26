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
    /// Settings for the S3 (S3LVOL / RCOW) backend.
    #[serde(default)]
    pub s3: S3Config,
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

/// Configuration for the S3 (S3LVOL / RCOW) backend.
#[derive(Debug, Clone, Deserialize)]
pub struct S3Config {
    /// Path to the s3lvol JSON-RPC Unix socket. Default:
    /// `/var/run/s3lvol.sock`.
    #[serde(default = "default_s3_socket_path")]
    pub socket_path: PathBuf,

    /// Directory holding the cubecow-side metadata (`index.json`).
    /// Default: `/var/lib/cubecow/s3`.
    #[serde(default = "default_s3_state_dir")]
    pub state_dir: PathBuf,

    /// Per-RPC timeout in milliseconds. Default: 10000.
    #[serde(default = "default_s3_rpc_timeout_ms")]
    pub rpc_timeout_ms: u64,

    /// How the `size_bytes → size_gib` conversion is handled at the
    /// boundary with s3lvol (which is GiB-granular): `"round_up"`
    /// (default) or `"strict"`.
    #[serde(default = "default_s3_size_policy")]
    pub size_policy: String,
}

impl Default for S3Config {
    fn default() -> Self {
        Self {
            socket_path: default_s3_socket_path(),
            state_dir: default_s3_state_dir(),
            rpc_timeout_ms: default_s3_rpc_timeout_ms(),
            size_policy: default_s3_size_policy(),
        }
    }
}

fn default_s3_socket_path() -> PathBuf {
    PathBuf::from("/var/run/s3lvol.sock")
}
fn default_s3_state_dir() -> PathBuf {
    PathBuf::from("/var/lib/cubecow/s3")
}
fn default_s3_rpc_timeout_ms() -> u64 {
    10_000
}
fn default_s3_size_policy() -> String {
    "round_up".to_string()
}

/// Available storage backends.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Deserialize, Default)]
#[serde(rename_all = "lowercase")]
pub enum BackendKind {
    /// xfs-reflink backend (FICLONE-based snapshots) — the **default**
    /// and currently the only shipping backend.
    #[default]
    Reflink,
    /// S3 backend delegating to an external S3LVOL / RCOW service.
    S3,
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
            BackendKind::S3 => self.validate_s3_requirements(),
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

    /// Field-level validation for the S3 backend.
    fn validate_s3_requirements(&self) -> CubecowResult<()> {
        let s3 = &self.backend.s3;
        if s3.socket_path.as_os_str().is_empty() {
            return Err(CubecowError::ConfigError(
                "[backend.s3] socket_path must not be empty when backend.kind = \"s3\"".to_string(),
            ));
        }
        if !s3.socket_path.is_absolute() {
            return Err(CubecowError::ConfigError(format!(
                "[backend.s3] socket_path = \"{}\" must be an absolute path",
                s3.socket_path.display()
            )));
        }
        if s3.state_dir.as_os_str().is_empty() {
            return Err(CubecowError::ConfigError(
                "[backend.s3] state_dir must not be empty when backend.kind = \"s3\"".to_string(),
            ));
        }
        if !s3.state_dir.is_absolute() {
            return Err(CubecowError::ConfigError(format!(
                "[backend.s3] state_dir = \"{}\" must be an absolute path",
                s3.state_dir.display()
            )));
        }
        match s3.size_policy.as_str() {
            "round_up" | "strict" => {}
            other => {
                return Err(CubecowError::ConfigError(format!(
                    "[backend.s3] size_policy = \"{other}\" is invalid; expected one of \
                     \"round_up\", \"strict\""
                )));
            }
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
