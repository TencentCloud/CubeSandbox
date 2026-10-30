// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! API-facing constants shared across sandbox responses.

/// This node-local marker enables the opt-in sandbox diagnostics bundle.
pub fn sandbox_diagnostics_enabled() -> bool {
    sandbox_diagnostics_enabled_at(std::path::Path::new(
        "/etc/cube-sandbox/retain-sandbox-logs",
    ))
}

fn sandbox_diagnostics_enabled_at(path: &std::path::Path) -> bool {
    match std::fs::metadata(path) {
        Ok(_) => true,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => false,
        Err(_) => true,
    }
}

#[cfg(test)]
mod diagnostics_tests {
    #[test]
    fn marker_states_fail_closed() {
        let root = std::env::temp_dir().join(format!("cube-diag-{}", uuid::Uuid::new_v4()));
        std::fs::create_dir(&root).unwrap();
        let marker = root.join("marker");
        assert!(!super::sandbox_diagnostics_enabled_at(&marker));
        std::fs::write(&marker, "").unwrap();
        assert!(super::sandbox_diagnostics_enabled_at(&marker));
        std::fs::remove_file(&marker).unwrap();
        std::fs::create_dir(&marker).unwrap();
        assert!(super::sandbox_diagnostics_enabled_at(&marker));
        std::fs::remove_dir_all(root).unwrap();
    }
}

/// Conservative fallback `envdVersion` used when a sandbox carries no collected
/// version annotation (e.g. legacy templates). Kept at the historical value so
/// e2b SDK feature gating degrades safely.
pub const ENVD_VERSION_FALLBACK: &str = "0.2.0";

/// Sandbox annotation key carrying the real envd version, collected at template
/// creation time by CubeMaster/Cubelet and propagated to sandbox instances.
pub const ENVD_VERSION_ANNOTATION: &str = "cube.master.components.envd.version";
