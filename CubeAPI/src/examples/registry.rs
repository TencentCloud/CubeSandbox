// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
//! Static example registry.
//!
//! Declares every runnable scenario and its files. This is the single source
//! of truth for what the UI lists, what source the editor shows, and what the
//! run handler dispatches to. Adding a new scenario only requires touching
//! this file.

use std::sync::OnceLock;

use super::topology::{topology_for, TopologyTemplate};

// ─── Raw spec types (internal) ────────────────────────────────────────────────

/// Static per-file metadata. One entry per runnable file.
#[derive(Clone)]
pub struct FileSpec {
    pub id: &'static str,
    pub filename: &'static str,
    pub title: &'static str,
    pub description: &'static str,
    pub language: &'static str,
}

/// Static scenario metadata.
///
/// `hidden: true` keeps the scenario on disk and queryable for future
/// re-enable, but excludes it from list/source/run responses so AI/LLM demos
/// do not leak into the UI.
pub struct ScenarioSpec {
    pub id: &'static str,
    pub category: &'static str,
    pub hidden: bool,
    pub files: &'static [FileSpec],
    /// Per-scenario run timeout in seconds. Defaults to 120 when absent.
    pub timeout_secs: Option<u64>,
    /// Topology template applied to every file inside this scenario.
    pub topology: TopologyTemplate,
    /// Associated store catalog item ID (e.g. `"sandbox-browser"`).
    /// Used by the run handler to auto-select a matching template image.
    pub store_item_id: Option<&'static str>,
}

// ─── Language map ─────────────────────────────────────────────────────────────

/// Maps `"<scenario>:<file-id>"` → source language string.
///
/// The map exists so the UI editor can choose a syntax mode without re-reading
/// the file. The `FileSpec::language` field is the canonical fallback.
pub fn file_languages() -> std::collections::HashMap<&'static str, &'static str> {
    [
        ("code-sandbox-quickstart:create", "python"),
        ("code-sandbox-quickstart:exec_code", "python"),
        ("code-sandbox-quickstart:cmd", "python"),
        ("code-sandbox-quickstart:read", "python"),
        ("code-sandbox-quickstart:pause", "python"),
        ("network-policy:network_no_internet", "python"),
        ("network-policy:network_allowlist", "python"),
        ("network-policy:network_denylist", "python"),
        ("host-mount:create_with_mount", "python"),
        ("browser-sandbox:browser", "python"),
        ("snapshot-rollback-clone:01_create_snapshot", "python"),
        ("snapshot-rollback-clone:02_list_snapshots", "python"),
        ("snapshot-rollback-clone:03_clone_from_snapshot", "python"),
        ("snapshot-rollback-clone:04_state_preserved", "python"),
        (
            "snapshot-rollback-clone:05_snapshot_outlives_sandbox",
            "python",
        ),
        ("snapshot-rollback-clone:06_clone_n", "python"),
        ("snapshot-rollback-clone:07_clone_concurrent", "python"),
        ("snapshot-rollback-clone:08_fork_three_axis", "python"),
        ("snapshot-rollback-clone:09_rollback", "python"),
        (
            "snapshot-rollback-clone:10_rollback_then_continue",
            "python",
        ),
        ("snapshot-rollback-clone:11_delete_snapshot", "python"),
        ("snapshot-rollback-clone:clone_demo", "python"),
        ("snapshot-rollback-clone:rollback_demo", "python"),
        ("cubesandbox-base-nginx:test_files", "python"),
        ("cube-bench:main", "go"),
    ]
    .into_iter()
    .collect()
}

// ─── Scenario registry ────────────────────────────────────────────────────────

/// Return the full static scenario registry.
///
/// Initialised exactly once via [`OnceLock`]; subsequent calls return the same
/// `&'static` slice, so the front-end gets a stable ordering without the
/// borrow checker complaining about temporaries.
pub fn scenario_registry() -> &'static [ScenarioSpec] {
    static REGISTRY: OnceLock<Vec<ScenarioSpec>> = OnceLock::new();
    REGISTRY.get_or_init(|| {
        vec![
            ScenarioSpec {
                id: "code-sandbox-quickstart",
                category: "basics",
                hidden: false,
                files: &[
                    FileSpec {
                        id: "create",
                        filename: "create.py",
                        title: "Create Sandbox",
                        description: "Create a sandbox from a template and read its metadata.",
                        language: "python",
                    },
                    FileSpec {
                        id: "exec_code",
                        filename: "exec_code.py",
                        title: "Execute Code",
                        description:
                            "Run Python code inside the sandbox through the Jupyter kernel.",
                        language: "python",
                    },
                    FileSpec {
                        id: "cmd",
                        filename: "cmd.py",
                        title: "Run Shell Command",
                        description:
                            "Execute a shell command inside the sandbox and capture stdout.",
                        language: "python",
                    },
                    FileSpec {
                        id: "read",
                        filename: "read.py",
                        title: "Read / Write File",
                        description: "Read and write files inside the sandbox filesystem.",
                        language: "python",
                    },
                    FileSpec {
                        id: "pause",
                        filename: "pause.py",
                        title: "Pause & Resume",
                        description: "Pause a sandbox to freeze its memory and resume it later.",
                        language: "python",
                    },
                ],
                timeout_secs: None,
                topology: topology_for("code-sandbox-quickstart"),
                store_item_id: Some("sandbox-code"),
            },
            ScenarioSpec {
                id: "network-policy",
                category: "network",
                hidden: false,
                files: &[
                    FileSpec {
                        id: "network_no_internet",
                        filename: "network_no_internet.py",
                        title: "No Internet",
                        description: "Sandbox without outbound network access.",
                        language: "python",
                    },
                    FileSpec {
                        id: "network_allowlist",
                        filename: "network_allowlist.py",
                        title: "Network Allowlist",
                        description: "Restrict egress to an explicit list of IPs.",
                        language: "python",
                    },
                    FileSpec {
                        id: "network_denylist",
                        filename: "network_denylist.py",
                        title: "Network Denylist",
                        description: "Default-allow with explicit deny entries.",
                        language: "python",
                    },
                ],
                timeout_secs: None,
                topology: topology_for("network-policy"),
                store_item_id: Some("sandbox-code"),
            },
            ScenarioSpec {
                id: "host-mount",
                category: "filesystem",
                hidden: false,
                files: &[FileSpec {
                    id: "create_with_mount",
                    filename: "create_with_mount.py",
                    title: "Create With Mount",
                    description: "Create a sandbox with a host directory mounted at /mnt.",
                    language: "python",
                }],
                timeout_secs: None,
                topology: topology_for("host-mount"),
                store_item_id: Some("sandbox-code"),
            },
            ScenarioSpec {
                id: "browser-sandbox",
                category: "browser",
                hidden: false,
                files: &[FileSpec {
                    id: "browser",
                    filename: "browser.py",
                    title: "Playwright + Chromium",
                    description: "Boot a sandbox with Chromium and run a Playwright script.",
                    language: "python",
                }],
                timeout_secs: Some(600),
                topology: topology_for("browser-sandbox"),
                store_item_id: Some("sandbox-browser"),
            },
            ScenarioSpec {
                id: "snapshot-rollback-clone",
                category: "lifecycle",
                hidden: false,
                files: &[
                    FileSpec {
                        id: "01_create_snapshot",
                        filename: "01_create_snapshot.py",
                        title: "01 Create Snapshot",
                        description: "Capture a snapshot from a running sandbox.",
                        language: "python",
                    },
                    FileSpec {
                        id: "02_list_snapshots",
                        filename: "02_list_snapshots.py",
                        title: "02 List Snapshots",
                        description: "List snapshots attached to the cluster.",
                        language: "python",
                    },
                    FileSpec {
                        id: "03_clone_from_snapshot",
                        filename: "03_clone_from_snapshot.py",
                        title: "03 Clone From Snapshot",
                        description: "Create a new sandbox from a snapshot.",
                        language: "python",
                    },
                    FileSpec {
                        id: "04_state_preserved",
                        filename: "04_state_preserved.py",
                        title: "04 State Preserved",
                        description: "Verify state survives the clone.",
                        language: "python",
                    },
                    FileSpec {
                        id: "05_snapshot_outlives_sandbox",
                        filename: "05_snapshot_outlives_sandbox.py",
                        title: "05 Snapshot Outlives",
                        description: "Snapshot outlives its source sandbox.",
                        language: "python",
                    },
                    FileSpec {
                        id: "06_clone_n",
                        filename: "06_clone_n.py",
                        title: "06 Clone N Times",
                        description: "Spin up N clones in sequence.",
                        language: "python",
                    },
                    FileSpec {
                        id: "07_clone_concurrent",
                        filename: "07_clone_concurrent.py",
                        title: "07 Clone Concurrently",
                        description: "Spin up N clones in parallel.",
                        language: "python",
                    },
                    FileSpec {
                        id: "08_fork_three_axis",
                        filename: "08_fork_three_axis.py",
                        title: "08 Fork Three-axis",
                        description: "Three orthogonal dimensions of clone/rollback.",
                        language: "python",
                    },
                    FileSpec {
                        id: "09_rollback",
                        filename: "09_rollback.py",
                        title: "09 Rollback",
                        description: "Roll the sandbox back to a previous snapshot.",
                        language: "python",
                    },
                    FileSpec {
                        id: "10_rollback_then_continue",
                        filename: "10_rollback_then_continue.py",
                        title: "10 Rollback Then Continue",
                        description: "Rollback, then resume normal execution.",
                        language: "python",
                    },
                    FileSpec {
                        id: "11_delete_snapshot",
                        filename: "11_delete_snapshot.py",
                        title: "11 Delete Snapshot",
                        description: "Clean up a snapshot from the cluster.",
                        language: "python",
                    },
                    FileSpec {
                        id: "clone_demo",
                        filename: "clone_demo.py",
                        title: "Clone Demo",
                        description: "End-to-end clone walkthrough.",
                        language: "python",
                    },
                    FileSpec {
                        id: "rollback_demo",
                        filename: "rollback_demo.py",
                        title: "Rollback Demo",
                        description: "End-to-end rollback walkthrough.",
                        language: "python",
                    },
                ],
                timeout_secs: None,
                topology: topology_for("snapshot-rollback-clone"),
                store_item_id: Some("sandbox-code"),
            },
            ScenarioSpec {
                id: "cubesandbox-base-nginx",
                category: "image",
                hidden: false,
                files: &[FileSpec {
                    id: "test_files",
                    filename: "test_files.py",
                    title: "Test Files",
                    description: "Reach the nginx-served files via the proxy.",
                    language: "python",
                }],
                timeout_secs: None,
                topology: topology_for("cubesandbox-base-nginx"),
                store_item_id: Some("sandbox-nginx"),
            },
            ScenarioSpec {
                id: "cube-bench",
                category: "perf",
                hidden: false,
                files: &[FileSpec {
                    id: "main",
                    filename: "main.go",
                    title: "Run Benchmark",
                    description: "Spawn N sandboxes in parallel and report throughput.",
                    language: "go",
                }],
                timeout_secs: None,
                topology: topology_for("cube-bench"),
                store_item_id: Some("sandbox-code"),
            },
            // ── Hidden: AI / LLM scenarios. Intentionally NOT exposed via the
            // HTTP surface. They live here so that toggling `hidden: false`
            // later (when LLM credentials are configured) is a one-line change.
            ScenarioSpec {
                id: "openclaw-integration",
                category: "agent",
                hidden: true,
                files: &[],
                timeout_secs: None,
                topology: topology_for("code-sandbox-quickstart"),
                store_item_id: None,
            },
            ScenarioSpec {
                id: "openai-agents-example",
                category: "agent",
                hidden: true,
                files: &[],
                timeout_secs: None,
                topology: topology_for("code-sandbox-quickstart"),
                store_item_id: None,
            },
            ScenarioSpec {
                id: "openai-agents-code-interpreter",
                category: "agent",
                hidden: true,
                files: &[],
                timeout_secs: None,
                topology: topology_for("code-sandbox-quickstart"),
                store_item_id: None,
            },
            ScenarioSpec {
                id: "mini-rl-training",
                category: "agent",
                hidden: true,
                files: &[],
                timeout_secs: None,
                topology: topology_for("code-sandbox-quickstart"),
                store_item_id: None,
            },
        ]
    })
}
