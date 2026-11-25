// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! Static capability declaration of this build, published to the artifact's
//! `capabilities.json` by `--print-capabilities`.
//!
//! Names are only ever added, never redefined: an incompatible change gets a new
//! name instead of a higher level on the old one.

use std::collections::BTreeMap;

use serde::{Deserialize, Serialize};

use crate::common::SHIM_VERSION;

/// Wire schema of the capability document. Readers reject anything else.
pub const SCHEMA_VERSION: u32 = 1;

/// Component name carried in the document.
pub const COMPONENT: &str = "cube-shim";

/// File name of the released artifact's declaration.
pub const ARTIFACT_FILE_NAME: &str = "capabilities.json";

// ── capability names ─────────────────────────────────────────────────────────

/// `SnapshotCapture` / `SnapshotResume`: one freeze window across memory and rootfs.
pub const CAP_UPDATE_SNAPSHOT_CAPTURE: &str = "update.snapshot_capture";
/// `SnapshotCapture` with `snapshot_mode=fs-only`: rootfs only, guest still running.
pub const CAP_UPDATE_SNAPSHOT_CAPTURE_FS_ONLY: &str = "update.snapshot_capture.fs_only";
/// `CheckPauseAllowed`: open-exec preflight, without freezing.
pub const CAP_UPDATE_CHECK_PAUSE_ALLOWED: &str = "update.check_pause_allowed";
/// `RollbackSnapshot`: in-place restore to a previously taken snapshot.
pub const CAP_UPDATE_ROLLBACK_SNAPSHOT: &str = "update.rollback_snapshot";
/// `PauseToSnapshot`: pause into a Cubelet-provided destination.
pub const CAP_UPDATE_PAUSE_TO_SNAPSHOT: &str = "update.pause_to_snapshot";
/// Failures carry a stable `code: message` prefix.
pub const CAP_ERRORS_CODED: &str = "errors.coded";

// ── coded error prefixes ─────────────────────────────────────────────────────

/// An Update action this shim does not route.
pub const ERR_UNKNOWN_ACTION: &str = "shim_error_unknown_action";
/// Pause (or rollback) refused because exec sessions are still open.
pub const ERR_PAUSE_BUSY: &str = "shim_error_pause_busy";

/// The pre-capability wording, unchanged.
pub const MSG_UNKNOWN_ACTION: &str = "unknown update ext action: ";
pub const MSG_PAUSE_BUSY: &str = "sandbox pause forbidding, terminate exec tasks first";

// ── the one table ────────────────────────────────────────────────────────────

/// Capabilities of this build. Everything here is fixed at compile time, so the
/// whole document can be published as an artifact.
pub const STATIC_CAPABILITIES: &[(&str, u32)] = &[
    (CAP_UPDATE_SNAPSHOT_CAPTURE, 1),
    (CAP_UPDATE_SNAPSHOT_CAPTURE_FS_ONLY, 1),
    (CAP_UPDATE_CHECK_PAUSE_ALLOWED, 1),
    (CAP_UPDATE_ROLLBACK_SNAPSHOT, 1),
    (CAP_UPDATE_PAUSE_TO_SNAPSHOT, 1),
    (CAP_ERRORS_CODED, 1),
];

// ── Update action names ──────────────────────────────────────────────────────

pub const ACTION_ROLLBACK_SNAPSHOT: &str = "RollbackSnapshot";
pub const ACTION_PAUSE_TO_SNAPSHOT: &str = "PauseToSnapshot";
pub const ACTION_CHECK_PAUSE_ALLOWED: &str = "CheckPauseAllowed";
pub const ACTION_SNAPSHOT_CAPTURE: &str = "SnapshotCapture";
pub const ACTION_SNAPSHOT_RESUME: &str = "SnapshotResume";

/// Routed action -> the capability that announces it. `SnapshotResume` shares
/// `snapshot_capture`: resume only ever follows a capture this shim just held.
pub const UPDATE_ACTIONS: &[(&str, &str)] = &[
    (ACTION_ROLLBACK_SNAPSHOT, CAP_UPDATE_ROLLBACK_SNAPSHOT),
    (ACTION_PAUSE_TO_SNAPSHOT, CAP_UPDATE_PAUSE_TO_SNAPSHOT),
    (ACTION_CHECK_PAUSE_ALLOWED, CAP_UPDATE_CHECK_PAUSE_ALLOWED),
    (ACTION_SNAPSHOT_CAPTURE, CAP_UPDATE_SNAPSHOT_CAPTURE),
    (ACTION_SNAPSHOT_RESUME, CAP_UPDATE_SNAPSHOT_CAPTURE),
];

/// Capabilities that refine an action rather than naming one: a shim can
/// implement `SnapshotCapture` without implementing `fs-only`.
pub const UPDATE_ACTION_REFINEMENTS: &[(&str, &str)] = &[(
    CAP_UPDATE_SNAPSHOT_CAPTURE_FS_ONLY,
    CAP_UPDATE_SNAPSHOT_CAPTURE,
)];

// ── document ─────────────────────────────────────────────────────────────────

/// The serialised form of the capability document.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct CapabilityDocument {
    pub schema: u32,
    pub component: String,
    pub version: String,
    #[serde(default)]
    pub capabilities: BTreeMap<String, u32>,
}

impl CapabilityDocument {
    /// This build's declaration.
    pub fn current() -> Self {
        let mut capabilities = BTreeMap::new();
        for (name, level) in STATIC_CAPABILITIES {
            capabilities.insert((*name).to_string(), *level);
        }
        Self {
            schema: SCHEMA_VERSION,
            component: COMPONENT.to_string(),
            version: SHIM_VERSION.to_string(),
            capabilities,
        }
    }

    pub fn to_json(&self) -> Result<String, String> {
        serde_json::to_string_pretty(self)
            .map_err(|e| format!("serialize capability document: {e}"))
    }
}

/// Renders the artifact declaration. There is no per-sandbox variant: anything
/// that varies per sandbox belongs to the component that owns it.
pub fn artifact_json() -> Result<String, String> {
    CapabilityDocument::current().to_json()
}

/// The coded prefix plus the legacy wording: `shim_error_unknown_action: unknown
/// update ext action: Foo`. The wording is kept verbatim for pre-capability readers.
pub fn coded_unknown_action(action: &str) -> String {
    format!("{ERR_UNKNOWN_ACTION}: {MSG_UNKNOWN_ACTION}{action}")
}

/// The coded prefix plus the pre-capability pause message, unchanged.
pub fn coded_pause_busy() -> String {
    format!("{ERR_PAUSE_BUSY}: {MSG_PAUSE_BUSY}")
}

/// True when `message` carries the coded pause-busy prefix.
pub fn is_coded_pause_busy(message: &str) -> bool {
    message.contains(ERR_PAUSE_BUSY)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn declared(name: &str) -> Option<u32> {
        STATIC_CAPABILITIES
            .iter()
            .find(|(cap, _)| *cap == name)
            .map(|(_, level)| *level)
    }

    /// The declaration and the router are the same list from two sides.
    #[test]
    fn every_declared_update_capability_is_routed() {
        for (name, level) in STATIC_CAPABILITIES {
            if !name.starts_with("update.") {
                continue;
            }
            assert!(*level >= 1, "{name} must be declared at level 1 or above");
            let routed = UPDATE_ACTIONS.iter().any(|(_, cap)| cap == name);
            let refines = UPDATE_ACTION_REFINEMENTS
                .iter()
                .any(|(refinement, _)| refinement == name);
            assert!(
                routed || refines,
                "capability {name} is declared but no Update action or refinement maps to it"
            );
        }
    }

    #[test]
    fn every_routed_action_announces_a_declared_capability() {
        for (action, capability) in UPDATE_ACTIONS {
            assert!(
                declared(capability).is_some(),
                "action {action} maps to undeclared capability {capability}"
            );
        }
    }

    #[test]
    fn every_refinement_refines_a_routed_capability() {
        for (refinement, base) in UPDATE_ACTION_REFINEMENTS {
            assert!(
                declared(refinement).is_some(),
                "refinement {refinement} is not declared"
            );
            assert!(
                UPDATE_ACTIONS.iter().any(|(_, cap)| cap == base),
                "refinement {refinement} refines {base}, which no routed action announces"
            );
        }
    }

    #[test]
    fn every_static_capability_has_a_positive_level() {
        for (name, level) in STATIC_CAPABILITIES {
            assert!(*level >= 1, "{name} declared at level {level}");
            assert!(!name.is_empty() && name.is_ascii(), "{name} must be ascii");
        }
    }

    #[test]
    fn artifact_document_shape_is_stable() {
        let doc = CapabilityDocument::current();
        assert_eq!(doc.schema, 1);
        assert_eq!(doc.component, "cube-shim");
        assert_eq!(doc.version, SHIM_VERSION);
        let parsed: CapabilityDocument =
            serde_json::from_str(&artifact_json().expect("render")).expect("parse");
        assert_eq!(parsed, doc);
    }

    /// Nothing here is discovered at runtime, so the document must not imply a
    /// section that would be.
    #[test]
    fn document_has_no_runtime_section() {
        let rendered = artifact_json().expect("render");
        assert!(
            !rendered.contains("\"runtime\""),
            "cube-shim declares nothing at runtime: {rendered}"
        );
    }

    #[test]
    fn reading_an_older_document_tolerates_absent_optional_sections() {
        let raw = r#"{"schema":1,"component":"cube-shim","version":"v0.8.0",
            "capabilities":{"update.snapshot_capture":1}}"#;
        let doc: CapabilityDocument = serde_json::from_str(raw).expect("parse");
        assert_eq!(doc.capabilities.get(CAP_UPDATE_SNAPSHOT_CAPTURE), Some(&1));
    }

    #[test]
    fn coded_errors_keep_the_pre_capability_wording() {
        let unknown = coded_unknown_action(ACTION_SNAPSHOT_CAPTURE);
        assert!(unknown.starts_with(ERR_UNKNOWN_ACTION));
        assert!(unknown.contains("unknown update ext action: SnapshotCapture"));

        let busy = coded_pause_busy();
        assert!(busy.starts_with(ERR_PAUSE_BUSY));
        assert!(busy.contains("pause forbidding"));
        assert!(busy.contains("terminate exec tasks first"));
        assert!(is_coded_pause_busy(&busy));
        assert!(!is_coded_pause_busy(MSG_PAUSE_BUSY));
    }
}
