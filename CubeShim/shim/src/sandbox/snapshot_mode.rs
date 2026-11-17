// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! Artifact mode of one snapshot capture.
//!
//! `mode` describes what the snapshot contains. It never changes how a
//! running sandbox behaves afterwards: pause/resume always carry memory.

use crate::common::CResult;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum SnapshotMode {
    /// Rootfs + memory + metadata.
    Full,
    /// Rootfs + metadata only; the guest keeps running while the disk is
    /// frozen.
    FsOnly,
}

/// Stable error identifiers shared with Cubelet / Master. They name the
/// feature, not the mode literal.
pub const ERR_FREEZE_UNSUPPORTED: &str = "snapshot_filesystem_only_freeze_unsupported";
pub const ERR_FREEZE_FAILED: &str = "snapshot_filesystem_only_freeze_failed";
pub const ERR_THAW_FAILED: &str = "snapshot_filesystem_only_thaw_failed";
pub const ERR_ROOTFS_UNSUPPORTED: &str = "snapshot_filesystem_only_rootfs_unsupported";

impl SnapshotMode {
    pub fn parse(raw: Option<&str>) -> CResult<Self> {
        match raw.map(str::trim).unwrap_or("") {
            "" | "full" => Ok(Self::Full),
            "fs-only" => Ok(Self::FsOnly),
            other => Err(format!("unsupported snapshot mode {other:?}").into()),
        }
    }

    pub fn has_memory(self) -> bool {
        matches!(self, Self::Full)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn missing_mode_is_full() {
        assert_eq!(SnapshotMode::parse(None).unwrap(), SnapshotMode::Full);
        assert_eq!(SnapshotMode::parse(Some("")).unwrap(), SnapshotMode::Full);
    }

    #[test]
    fn parses_both_canonical_values() {
        assert_eq!(
            SnapshotMode::parse(Some("fs-only")).unwrap(),
            SnapshotMode::FsOnly
        );
        assert_eq!(
            SnapshotMode::parse(Some("full")).unwrap(),
            SnapshotMode::Full
        );
    }

    #[test]
    fn rejects_unknown_values() {
        assert!(SnapshotMode::parse(Some("filesystem")).is_err());
        assert!(SnapshotMode::parse(Some("incremental")).is_err());
    }
}
