// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! Static capability declaration of this build: the single source of truth,
//! published to the artifact's `capabilities.json` and to the running sandbox's
//! `cube-shim-capabilities.json`. Capability names are only added, never
//! redefined.

use std::collections::BTreeMap;
use std::fs;
use std::io::Write;
use std::path::{Path, PathBuf};

use lazy_static::lazy_static;
use serde::{Deserialize, Serialize};

use crate::common::SHIM_VERSION;

/// Wire schema of the capability document. Readers reject anything else.
pub const SCHEMA_VERSION: u32 = 1;

/// Component name carried in the document, matching the release manifest key.
pub const COMPONENT: &str = "cube-shim";

/// File name of the released artifact's declaration, next to `version.json`
/// and the `bin/` directory of the component tree.
pub const ARTIFACT_FILE_NAME: &str = "capabilities.json";

/// File name the running shim writes into its own bundle directory.
pub const RUNTIME_FILE_NAME: &str = "cube-shim-capabilities.json";

// ── capability names ─────────────────────────────────────────────────────────

/// `SnapshotCapture` / `SnapshotResume`: one freeze window across the memory
/// and the rootfs capture.
pub const CAP_UPDATE_SNAPSHOT_CAPTURE: &str = "update.snapshot_capture";
/// `SnapshotCapture` with `snapshot_mode=fs-only`: rootfs only, guest keeps
/// running.
pub const CAP_UPDATE_SNAPSHOT_CAPTURE_FS_ONLY: &str = "update.snapshot_capture.fs_only";
/// `CheckPauseAllowed`: the cheap open-exec preflight, without freezing.
pub const CAP_UPDATE_CHECK_PAUSE_ALLOWED: &str = "update.check_pause_allowed";
/// `RollbackSnapshot`: in-place restore to a previously taken snapshot.
pub const CAP_UPDATE_ROLLBACK_SNAPSHOT: &str = "update.rollback_snapshot";
/// `PauseToSnapshot`: pause into a Cubelet-provided destination.
pub const CAP_UPDATE_PAUSE_TO_SNAPSHOT: &str = "update.pause_to_snapshot";
/// Failures carry a stable `code: message` prefix.
pub const CAP_ERRORS_CODED: &str = "errors.coded";

/// Guest-agent probe result (runtime section): the agent can freeze a
/// filesystem. `0` means the agent answered and refused; the key being absent
/// means the answer is not known yet.
pub const CAP_GUEST_FS_FREEZE: &str = "guest.fs_freeze";

// ── coded error prefixes ─────────────────────────────────────────────────────

/// An Update action this shim does not route.
pub const ERR_UNKNOWN_ACTION: &str = "shim_error_unknown_action";
/// Pause (or rollback) refused because exec sessions are still open.
pub const ERR_PAUSE_BUSY: &str = "shim_error_pause_busy";

/// The pre-capability wording, unchanged.
pub const MSG_UNKNOWN_ACTION: &str = "unknown update ext action: ";
pub const MSG_PAUSE_BUSY: &str = "sandbox pause forbidding, terminate exec tasks first";

// ── the one table ────────────────────────────────────────────────────────────

/// Capabilities of this build that do not depend on a running sandbox.
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

/// Every routed Update action, mapped to the capability that announces it.
pub const UPDATE_ACTIONS: &[(&str, &str)] = &[
    (ACTION_ROLLBACK_SNAPSHOT, CAP_UPDATE_ROLLBACK_SNAPSHOT),
    (ACTION_PAUSE_TO_SNAPSHOT, CAP_UPDATE_PAUSE_TO_SNAPSHOT),
    (ACTION_CHECK_PAUSE_ALLOWED, CAP_UPDATE_CHECK_PAUSE_ALLOWED),
    (ACTION_SNAPSHOT_CAPTURE, CAP_UPDATE_SNAPSHOT_CAPTURE),
    (ACTION_SNAPSHOT_RESUME, CAP_UPDATE_SNAPSHOT_CAPTURE),
];

/// Capabilities that refine an action rather than naming one of their own.
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
    #[serde(default)]
    pub runtime: BTreeMap<String, u32>,
}

impl CapabilityDocument {
    /// This build's static declaration with an empty runtime section.
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
            runtime: BTreeMap::new(),
        }
    }

    /// The document as published, with the runtime results merged in.
    pub fn with_runtime(mut self, runtime: &BTreeMap<String, u32>) -> Self {
        self.runtime = runtime.clone();
        self
    }

    pub fn to_json(&self) -> Result<String, String> {
        serde_json::to_string_pretty(self)
            .map_err(|e| format!("serialize capability document: {e}"))
    }
}

/// Renders the artifact declaration (--print-capabilities), without a runtime section.
pub fn artifact_json() -> Result<String, String> {
    CapabilityDocument::current().to_json()
}

// Serialises concurrent runtime writes from the guest probe and the freeze path.
lazy_static! {
    static ref RUNTIME_WRITE_LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());
}

fn runtime_path(bundle_dir: &Path) -> PathBuf {
    bundle_dir.join(RUNTIME_FILE_NAME)
}

/// Publishes the static declaration without disturbing the bundle's existing
/// document: creates the file when missing or unreadable, never overwrites a valid one.
pub fn ensure_runtime_document(bundle_dir: &Path) -> Result<(), String> {
    let _guard = RUNTIME_WRITE_LOCK
        .lock()
        .map_err(|e| format!("runtime capability lock poisoned: {e}"))?;
    sweep_stale_temp_files(bundle_dir);
    if let Ok(raw) = fs::read_to_string(runtime_path(bundle_dir)) {
        if let Ok(existing) = serde_json::from_str::<CapabilityDocument>(&raw) {
            if existing.schema == SCHEMA_VERSION {
                return Ok(());
            }
        }
    }
    write_runtime_document_locked(bundle_dir, &BTreeMap::new())
}

/// Reads the bundle's runtime document, or the static declaration when the file
/// is missing or unreadable. Never returns an error.
fn read_or_static(bundle_dir: &Path, runtime: &mut BTreeMap<String, u32>) -> CapabilityDocument {
    match fs::read_to_string(runtime_path(bundle_dir)) {
        Ok(raw) => match serde_json::from_str::<CapabilityDocument>(&raw) {
            Ok(existing) if existing.schema == SCHEMA_VERSION => {
                runtime.clone_from(&existing.runtime);
                return existing.with_runtime(runtime);
            }
            Ok(other) => {
                // Keep the runtime answers from a newer schema.
                runtime.clone_from(&other.runtime);
            }
            Err(_) => {}
        },
        Err(_) => {}
    }
    CapabilityDocument::current()
}

/// Records one runtime capability and republishes the document atomically.
/// Level 0 means known unsupported; callers leave the key absent for unknown.
pub fn set_runtime_capability(bundle_dir: &Path, name: &str, level: u32) -> Result<(), String> {
    let _guard = RUNTIME_WRITE_LOCK
        .lock()
        .map_err(|e| format!("runtime capability lock poisoned: {e}"))?;
    let mut runtime = BTreeMap::new();
    let document = read_or_static(bundle_dir, &mut runtime);
    runtime.insert(name.to_string(), level);
    write_runtime_document_locked(bundle_dir, &document.with_runtime(&runtime).runtime)
}

/// Writes the runtime document from scratch, replacing whatever the bundle held.
/// Failures are returned so the caller can log them.
fn write_runtime_document_locked(
    bundle_dir: &Path,
    runtime: &BTreeMap<String, u32>,
) -> Result<(), String> {
    let document = CapabilityDocument::current().with_runtime(runtime);
    let body = document.to_json()?;

    // Atomic write: tmp + rename; the unique tmp name avoids concurrent clobbers.
    let target = runtime_path(bundle_dir);
    let tmp = bundle_dir.join(format!(
        "{RUNTIME_FILE_NAME}.tmp.{}.{}",
        std::process::id(),
        uuid::Uuid::new_v4().simple()
    ));
    let write_result = (|| -> Result<(), String> {
        let mut file =
            fs::File::create(&tmp).map_err(|e| format!("create {}: {e}", tmp.display()))?;
        file.write_all(body.as_bytes())
            .map_err(|e| format!("write {}: {e}", tmp.display()))?;
        Ok(())
    })();
    if let Err(e) = write_result {
        let _ = fs::remove_file(&tmp);
        return Err(e);
    }

    // Rename is retried once; fall back to an in-place write if the target does not verify.
    let mut last_err = None;
    for attempt in 0..2 {
        match fs::rename(&tmp, &target) {
            Ok(()) => {
                last_err = None;
                break;
            }
            Err(e) => {
                last_err = Some(format!(
                    "publish {} (attempt {}): {e}",
                    target.display(),
                    attempt + 1
                ));
                std::thread::sleep(std::time::Duration::from_millis(10));
            }
        }
    }
    let _ = fs::remove_file(&tmp);

    if target_holds(&target, &body) {
        return Ok(());
    }
    write_target_in_place(&target, &body).map_err(|e| match last_err {
        Some(rename_err) => format!("{rename_err}; in-place fallback failed: {e}"),
        None => format!("published file did not verify; in-place fallback failed: {e}"),
    })
}

/// Reports whether target already contains exactly body.
fn target_holds(target: &Path, body: &str) -> bool {
    fs::read_to_string(target)
        .map(|landed| landed == body)
        .unwrap_or(false)
}

/// Last-resort publish: write the target directly and verify what landed.
/// Used only when the atomic path left the wrong content behind.
fn write_target_in_place(target: &Path, body: &str) -> Result<(), String> {
    let mut file =
        fs::File::create(target).map_err(|e| format!("create {}: {e}", target.display()))?;
    file.write_all(body.as_bytes())
        .map_err(|e| format!("write {}: {e}", target.display()))?;
    if !target_holds(target, body) {
        return Err(format!(
            "{} does not hold what was written",
            target.display()
        ));
    }
    Ok(())
}

/// Removes this shim's own leftover temp files from an interrupted publish.
fn sweep_stale_temp_files(bundle_dir: &Path) {
    const STALE_AFTER: std::time::Duration = std::time::Duration::from_secs(60);
    let Ok(entries) = fs::read_dir(bundle_dir) else {
        return;
    };
    let now = std::time::SystemTime::now();
    for entry in entries.flatten() {
        let name = entry.file_name();
        let Some(name) = name.to_str() else { continue };
        let Some(rest) = name.strip_prefix(&format!("{RUNTIME_FILE_NAME}.tmp.")) else {
            continue;
        };
        // `<pid>.<uuid>`: the pid is decimal, the uuid is 32 hex characters.
        let Some((pid, uuid)) = rest.split_once('.') else {
            continue;
        };
        if pid.is_empty() || !pid.bytes().all(|b| b.is_ascii_digit()) {
            continue;
        }
        if uuid.len() != 32 || !uuid.bytes().all(|b| b.is_ascii_hexdigit()) {
            continue;
        }
        let too_young = entry
            .metadata()
            .and_then(|m| m.modified())
            .map(|mtime| now.duration_since(mtime).unwrap_or_default() < STALE_AFTER)
            .unwrap_or(true);
        if too_young {
            continue;
        }
        let _ = fs::remove_file(entry.path());
    }
}

/// The coded prefix plus the legacy human wording, in the order Cubelet reads them.
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

    /// Declared capabilities and routed actions must stay in sync.
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

    /// Every refinement must refine a routed capability.
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
        assert!(
            doc.runtime.is_empty(),
            "a build-time file claims no guest answer"
        );
        let parsed: CapabilityDocument =
            serde_json::from_str(&artifact_json().expect("render")).expect("parse");
        assert_eq!(parsed, doc);
    }

    #[test]
    fn runtime_section_is_optional_when_reading() {
        // A document written by an older build has no runtime section at all.
        let raw = r#"{"schema":1,"component":"cube-shim","version":"v0.8.0",
            "capabilities":{"update.snapshot_capture":1}}"#;
        let doc: CapabilityDocument = serde_json::from_str(raw).expect("parse");
        assert!(doc.runtime.is_empty());
        assert_eq!(doc.capabilities.get(CAP_UPDATE_SNAPSHOT_CAPTURE), Some(&1));
    }

    #[test]
    fn runtime_document_round_trips_through_the_bundle() {
        let dir = std::env::temp_dir().join(format!("caps-{}", uuid::Uuid::new_v4()));
        fs::create_dir_all(&dir).expect("mkdir");

        ensure_runtime_document(&dir).expect("write static");
        let written: CapabilityDocument =
            serde_json::from_str(&fs::read_to_string(runtime_path(&dir)).expect("read"))
                .expect("parse");
        assert_eq!(written, CapabilityDocument::current());

        set_runtime_capability(&dir, CAP_GUEST_FS_FREEZE, 1).expect("set supported");
        set_runtime_capability(&dir, CAP_GUEST_FS_FREEZE, 0).expect("set unsupported");
        let updated: CapabilityDocument =
            serde_json::from_str(&fs::read_to_string(runtime_path(&dir)).expect("read"))
                .expect("parse");
        // The latest answer wins, and the static section survives the update.
        assert_eq!(updated.runtime.get(CAP_GUEST_FS_FREEZE), Some(&0));
        assert_eq!(
            updated.capabilities.get(CAP_UPDATE_SNAPSHOT_CAPTURE),
            Some(&1)
        );

        // No temporary file is left behind.
        let leftovers: Vec<_> = fs::read_dir(&dir)
            .expect("readdir")
            .filter_map(|e| e.ok())
            .map(|e| e.file_name().to_string_lossy().to_string())
            .filter(|name| name.contains(".tmp."))
            .collect();
        assert!(leftovers.is_empty(), "leftover tmp files: {leftovers:?}");

        fs::remove_dir_all(&dir).expect("cleanup");
    }

    #[test]
    fn corrupt_runtime_document_is_replaced_rather_than_fatal() {
        let dir = std::env::temp_dir().join(format!("caps-{}", uuid::Uuid::new_v4()));
        fs::create_dir_all(&dir).expect("mkdir");
        fs::write(runtime_path(&dir), b"{ not json").expect("seed corrupt");

        set_runtime_capability(&dir, CAP_GUEST_FS_FREEZE, 1).expect("recover");
        let repaired: CapabilityDocument =
            serde_json::from_str(&fs::read_to_string(runtime_path(&dir)).expect("read"))
                .expect("parse");
        assert_eq!(repaired.schema, SCHEMA_VERSION);
        assert_eq!(repaired.runtime.get(CAP_GUEST_FS_FREEZE), Some(&1));

        fs::remove_dir_all(&dir).expect("cleanup");
    }

    #[test]
    fn coded_errors_keep_the_pre_capability_wording() {
        let unknown = coded_unknown_action(ACTION_SNAPSHOT_CAPTURE);
        assert!(unknown.starts_with(ERR_UNKNOWN_ACTION));
        // An older Cubelet matches this exact fragment.
        assert!(unknown.contains("unknown update ext action: SnapshotCapture"));

        let busy = coded_pause_busy();
        assert!(busy.starts_with(ERR_PAUSE_BUSY));
        // An older CubeMaster matches these two fragments.
        assert!(busy.contains("pause forbidding"));
        assert!(busy.contains("terminate exec tasks first"));
        assert!(is_coded_pause_busy(&busy));
        assert!(!is_coded_pause_busy(MSG_PAUSE_BUSY));
    }

    /// Static publish creates the file but never clobbers a runtime answer.
    #[test]
    fn static_publish_creates_but_never_clobbers_a_runtime_answer() {
        let dir = std::env::temp_dir().join(format!("caps-{}", uuid::Uuid::new_v4()));
        fs::create_dir_all(&dir).expect("mkdir");

        // First initialisation: creates the file with an empty runtime section.
        ensure_runtime_document(&dir).expect("create");
        let created: CapabilityDocument =
            serde_json::from_str(&fs::read_to_string(runtime_path(&dir)).expect("read"))
                .expect("parse");
        assert_eq!(created, CapabilityDocument::current());

        // The guest probe answers; a second initialisation in the same bundle must
        // not throw that answer away.
        set_runtime_capability(&dir, CAP_GUEST_FS_FREEZE, 1).expect("probe");
        ensure_runtime_document(&dir).expect("second init");
        let kept: CapabilityDocument =
            serde_json::from_str(&fs::read_to_string(runtime_path(&dir)).expect("read"))
                .expect("parse");
        assert_eq!(kept.runtime.get(CAP_GUEST_FS_FREEZE), Some(&1));

        // A corrupt file is replaced: nothing usable is being protected.
        fs::write(runtime_path(&dir), b"{ not json").expect("seed corrupt");
        ensure_runtime_document(&dir).expect("repair");
        let repaired: CapabilityDocument =
            serde_json::from_str(&fs::read_to_string(runtime_path(&dir)).expect("read"))
                .expect("parse");
        assert_eq!(repaired, CapabilityDocument::current());

        fs::remove_dir_all(&dir).expect("cleanup");
    }

    /// Stale temp files are swept; in-flight ones are not.
    #[test]
    fn stale_temp_files_are_swept_but_fresh_ones_are_not() {
        let dir = std::env::temp_dir().join(format!("caps-{}", uuid::Uuid::new_v4()));
        fs::create_dir_all(&dir).expect("mkdir");

        let stale = dir.join(format!("{RUNTIME_FILE_NAME}.tmp.123.{}", "a".repeat(32)));
        let fresh = dir.join(format!("{RUNTIME_FILE_NAME}.tmp.456.{}", "b".repeat(32)));
        let unrelated = dir.join(format!(
            "{RUNTIME_FILE_NAME}.tmp.notapid.{}",
            "c".repeat(32)
        ));
        fs::write(&stale, b"{}").expect("seed stale");
        fs::write(&fresh, b"{}").expect("seed fresh");
        fs::write(&unrelated, b"{}").expect("seed unrelated");

        // Age the one that should be swept past the staleness window.
        let old = std::time::SystemTime::now() - std::time::Duration::from_secs(600);
        fs::File::options()
            .write(true)
            .open(&stale)
            .expect("open stale")
            .set_modified(old)
            .expect("age stale");

        sweep_stale_temp_files(&dir);

        assert!(!stale.exists(), "an abandoned temp file must be removed");
        assert!(
            fresh.exists(),
            "a possibly in-flight temp file must survive"
        );
        assert!(unrelated.exists(), "a name with no real pid is not ours");

        fs::remove_dir_all(&dir).expect("cleanup");
    }

    /// Writes do not sweep temp files; initialisation does.
    #[test]
    fn a_write_leaves_stale_temp_files_for_initialisation_to_sweep() {
        let dir = std::env::temp_dir().join(format!("caps-{}", uuid::Uuid::new_v4()));
        fs::create_dir_all(&dir).expect("mkdir");

        let stale = dir.join(format!("{RUNTIME_FILE_NAME}.tmp.999.{}", "d".repeat(32)));
        fs::write(&stale, b"{}").expect("seed stale");
        let old = std::time::SystemTime::now() - std::time::Duration::from_secs(600);
        fs::File::options()
            .write(true)
            .open(&stale)
            .expect("open stale")
            .set_modified(old)
            .expect("age stale");

        set_runtime_capability(&dir, CAP_GUEST_FS_FREEZE, 1).expect("write");
        assert!(stale.exists(), "the write path must not scan the directory");

        ensure_runtime_document(&dir).expect("init");
        assert!(
            !stale.exists(),
            "initialisation is where abandoned temp files are swept"
        );

        fs::remove_dir_all(&dir).expect("cleanup");
    }
}
