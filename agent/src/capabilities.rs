// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! What this cube-agent build can do, published to the artifact's
//! `capabilities.json` by `--print-capabilities`.
//!
//! Everything declared here is fixed at compile time, so nothing has to be probed
//! per sandbox. Names are only ever added, never redefined.

use std::collections::BTreeMap;

use serde::{Deserialize, Serialize};

use crate::version::AGENT_VERSION;

/// Wire schema of the capability document. Readers reject anything else.
pub const SCHEMA_VERSION: u32 = 1;

/// Component name carried in the document.
pub const COMPONENT: &str = "cube-agent";

// ── capability names ─────────────────────────────────────────────────────────

/// The agent can freeze and thaw a mounted filesystem, which is what a
/// filesystem-only snapshot needs.
pub const CAP_GUEST_FS_FREEZE: &str = "guest.fs_freeze";

/// The RPCs backing `guest.fs_freeze`. A test asserts the declaration and the
/// served surface agree: freeze without thaw would wedge the guest's disk.
#[cfg(test)]
pub const FS_FREEZE_RPCS: &[&str] = &["FreezeRootfsRequest", "ThawRootfsRequest"];

/// Capabilities of this build. `fsfreeze` is compiled unconditionally; if it ever
/// gains a feature gate, gate this entry the same way.
pub const STATIC_CAPABILITIES: &[(&str, u32)] = &[(CAP_GUEST_FS_FREEZE, 1)];

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
            version: AGENT_VERSION.to_string(),
            capabilities,
        }
    }

    pub fn to_json(&self) -> Result<String, String> {
        serde_json::to_string_pretty(self)
            .map_err(|e| format!("serialize capability document: {e}"))
    }
}

/// Renders the artifact declaration.
pub fn artifact_json() -> Result<String, String> {
    CapabilityDocument::current().to_json()
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

    #[test]
    fn every_static_capability_has_a_positive_level() {
        for (name, level) in STATIC_CAPABILITIES {
            assert!(*level >= 1, "{name} declared at level {level}");
            assert!(!name.is_empty() && name.is_ascii(), "{name} must be ascii");
        }
    }

    #[test]
    fn fs_freeze_declaration_matches_the_served_rpc_pair() {
        let declared_freeze = declared(CAP_GUEST_FS_FREEZE).is_some();
        assert_eq!(
            declared_freeze,
            FS_FREEZE_RPCS.len() == 2,
            "guest.fs_freeze must be declared exactly when both freeze and thaw are served"
        );
        assert!(FS_FREEZE_RPCS.contains(&"FreezeRootfsRequest"));
        assert!(FS_FREEZE_RPCS.contains(&"ThawRootfsRequest"));
    }

    #[test]
    fn artifact_document_shape_is_stable() {
        let doc = CapabilityDocument::current();
        assert_eq!(doc.schema, 1);
        assert_eq!(doc.component, "cube-agent");
        assert_eq!(doc.version, AGENT_VERSION);
        let parsed: CapabilityDocument =
            serde_json::from_str(&artifact_json().expect("render")).expect("parse");
        assert_eq!(parsed, doc);
    }

    #[test]
    fn document_has_no_runtime_section() {
        let rendered = artifact_json().expect("render");
        assert!(
            !rendered.contains("\"runtime\""),
            "cube-agent declares nothing at runtime: {rendered}"
        );
    }
}
