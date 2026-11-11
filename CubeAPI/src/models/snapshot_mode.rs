// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! Snapshot artifact mode.
//!
//! A mode describes what a snapshot contains. It never describes a sandbox:
//! pause/resume always carry memory and have no mode.

use serde::{Deserialize, Serialize};
use std::fmt;
use utoipa::ToSchema;

/// Canonical snapshot artifact mode.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, ToSchema)]
pub enum SnapshotMode {
    #[serde(rename = "full")]
    Full,
    #[serde(rename = "fs-only")]
    FsOnly,
}

impl SnapshotMode {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Full => "full",
            Self::FsOnly => "fs-only",
        }
    }

    /// Parse a stored/internal value. Empty means full because the field was
    /// added after full snapshots already existed. Client input must use
    /// [`normalize_request`] so unknown values are rejected.
    pub fn from_stored(raw: &str) -> Self {
        match raw.trim().to_ascii_lowercase().as_str() {
            "fs-only" | "filesystem" => Self::FsOnly,
            _ => Self::Full,
        }
    }
}

impl fmt::Display for SnapshotMode {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// Stable error identifiers for the snapshot-mode contract.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum SnapshotModeError {
    Invalid(String),
    Conflict,
}

impl SnapshotModeError {
    pub fn code(&self) -> &'static str {
        match self {
            Self::Invalid(_) => "snapshot_mode_invalid",
            Self::Conflict => "snapshot_mode_conflict",
        }
    }

    pub fn message(&self) -> String {
        match self {
            Self::Invalid(raw) => {
                format!("{}: unsupported mode {:?}", self.code(), raw)
            }
            Self::Conflict => format!("{}: mode, memory and keepMemory disagree", self.code()),
        }
    }
}

/// Identifiers that mean "the request itself is wrong" (HTTP 400).
const CLIENT_ERROR_CODES: &[&str] = &[
    "snapshot_mode_invalid",
    "snapshot_mode_conflict",
    "snapshot_filesystem_only_rollback_unsupported",
];

/// Every stable identifier that can travel with a CubeMaster rejection.
/// Mirrors the identifiers in `pkgs/snapshotmode/mode.go`; server-side ones
/// (`*_freeze_failed`, `*_thaw_failed`, `*_restore_failed`) are omitted because
/// they never change the status this API returns.
const STABLE_CODES: &[&str] = &[
    "snapshot_mode_invalid",
    "snapshot_mode_conflict",
    "snapshot_filesystem_only_rollback_unsupported",
    "snapshot_filesystem_only_unsupported_node",
    "snapshot_filesystem_only_node_capability_unknown",
    "snapshot_filesystem_only_not_ready",
    "snapshot_filesystem_only_catalog_missing",
    "snapshot_filesystem_only_guest_runtime_mismatch",
    "snapshot_filesystem_only_freeze_unsupported",
    "snapshot_filesystem_only_rootfs_unsupported",
    "snapshot_filesystem_only_shared_volume_unsupported",
    "snapshot_filesystem_only_multi_container_unsupported",
    "snapshot_memory_missing",
];

/// Returns the stable identifier embedded in `message`, if any.
///
/// Identifiers reach this API inside longer sentences — Cubelet prefixes them
/// and CubeMaster stores the whole text in the job's `error_message` — so the
/// match is by containment. A prefix test would miss
/// `"failed to capture sandbox snapshot: snapshot_filesystem_only_...`".
pub fn stable_code(message: &str) -> Option<&'static str> {
    STABLE_CODES
        .iter()
        .copied()
        .find(|code| message.contains(code))
}

/// True when `code` names a rejection the client can act on.
pub fn is_client_error(code: &str) -> bool {
    CLIENT_ERROR_CODES.contains(&code)
}

fn parse_mode(raw: &str) -> Result<SnapshotMode, SnapshotModeError> {
    let value = raw.trim();
    if value.is_empty() {
        return Err(SnapshotModeError::Invalid(raw.to_string()));
    }
    match value.to_ascii_lowercase().as_str() {
        "full" => Ok(SnapshotMode::Full),
        "fs-only" | "filesystem" => Ok(SnapshotMode::FsOnly),
        _ => Err(SnapshotModeError::Invalid(raw.to_string())),
    }
}

/// Normalize the snapshot-create entry point.
///
/// `mode` is authoritative. `memory` / `keepMemory` are E2B compatibility
/// aliases and are accepted only when they agree with `mode`. Missing values
/// default to full; an explicitly empty `mode` is invalid rather than defaulted.
pub fn normalize_request(
    mode: Option<&str>,
    memory: Option<bool>,
    keep_memory: Option<bool>,
) -> Result<SnapshotMode, SnapshotModeError> {
    let from_mode = mode.map(parse_mode).transpose()?;
    let from_memory = memory.map(|v| {
        if v {
            SnapshotMode::Full
        } else {
            SnapshotMode::FsOnly
        }
    });
    let from_keep = keep_memory.map(|v| {
        if v {
            SnapshotMode::Full
        } else {
            SnapshotMode::FsOnly
        }
    });

    if let (Some(a), Some(b)) = (from_memory, from_keep) {
        if a != b {
            return Err(SnapshotModeError::Conflict);
        }
    }
    let alias = from_memory.or(from_keep);
    match (from_mode, alias) {
        (Some(a), Some(b)) if a != b => Err(SnapshotModeError::Conflict),
        (Some(a), _) => Ok(a),
        (None, Some(b)) => Ok(b),
        (None, None) => Ok(SnapshotMode::Full),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn defaults_to_full() {
        assert_eq!(
            normalize_request(None, None, None).unwrap(),
            SnapshotMode::Full
        );
        assert!(matches!(
            normalize_request(Some("  "), None, None),
            Err(SnapshotModeError::Invalid(_))
        ));
    }

    #[test]
    fn accepts_canonical_and_alias() {
        assert_eq!(
            normalize_request(Some("full"), None, None).unwrap(),
            SnapshotMode::Full
        );
        assert_eq!(
            normalize_request(Some("FS-ONLY"), None, None).unwrap(),
            SnapshotMode::FsOnly
        );
        assert_eq!(
            normalize_request(Some("filesystem"), None, None).unwrap(),
            SnapshotMode::FsOnly
        );
    }

    #[test]
    fn explicit_empty_is_invalid() {
        assert!(matches!(
            normalize_request(Some(""), None, None),
            Err(SnapshotModeError::Invalid(_))
        ));
    }

    #[test]
    fn aliases_must_agree_with_mode() {
        assert_eq!(
            normalize_request(Some("fs-only"), Some(false), None).unwrap(),
            SnapshotMode::FsOnly
        );
        assert_eq!(
            normalize_request(None, Some(false), Some(false)).unwrap(),
            SnapshotMode::FsOnly
        );
        assert!(matches!(
            normalize_request(Some("fs-only"), Some(true), None),
            Err(SnapshotModeError::Conflict)
        ));
        assert!(matches!(
            normalize_request(None, Some(false), Some(true)),
            Err(SnapshotModeError::Conflict)
        ));
    }

    #[test]
    fn recognises_identifiers_inside_wrapped_messages() {
        assert_eq!(
            stable_code("snapshot_mode_invalid: unsupported mode \"x\""),
            Some("snapshot_mode_invalid")
        );
        // Cubelet prefixes the identifier and CubeMaster stores the sentence.
        assert_eq!(
            stable_code(
                "failed to capture sandbox snapshot: snapshot_filesystem_only_freeze_unsupported: too old"
            ),
            Some("snapshot_filesystem_only_freeze_unsupported")
        );
        assert_eq!(stable_code("dial tcp 10.0.0.1: timeout"), None);
        assert_eq!(stable_code(""), None);

        assert!(is_client_error("snapshot_mode_invalid"));
        assert!(!is_client_error(
            "snapshot_filesystem_only_freeze_unsupported"
        ));
    }

    #[test]
    fn rejects_unknown() {
        assert!(matches!(
            normalize_request(Some("fs_only"), None, None),
            Err(SnapshotModeError::Invalid(_))
        ));
    }
}
