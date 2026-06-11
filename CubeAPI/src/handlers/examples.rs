// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
// Examples handler: list available example scripts under `examples/<scenario>/`
// and run them via subprocess, with optional user-edited code injection.
//
// Each example belongs to a "scenario" (sub-directory of `examples/`). The
// handler exposes a static registry mapping scenario → category → files, plus
// hidden AI/LLM demos that are intentionally not surfaced to the UI.
//
// On `run`, the handler also synthesises a small execution step log (parse →
// control-plane → data-plane → cleanup) and the topology graph that the UI
// renders with @xyflow/react. Real per-step telemetry would require deeper
// instrumentation; the synthetic log keeps the API surface stable for the
// front-end.

use axum::{extract::State, http::StatusCode, response::IntoResponse, Json};
use serde::{Deserialize, Serialize};
use std::path::PathBuf;
use std::time::{Duration, Instant};
use tokio::process::Command;
use tokio::time::timeout;
use utoipa::ToSchema;
use uuid::Uuid;

use crate::{error::AppResult, state::AppState};

// ─── Models ───────────────────────────────────────────────────────────────────

#[derive(Serialize, Clone, ToSchema)]
pub struct ExampleMeta {
    /// Stable identifier. Format: "<scenario>:<file-id>" so the handler can
    /// resolve the disk path without ambiguity. Example:
    /// "code-sandbox-quickstart:create".
    pub id: String,
    /// Scenario (sub-directory) this example lives in.
    pub scenario: String,
    /// Filename inside the scenario directory.
    pub filename: String,
    pub title: String,
    pub description: String,
    pub category: String,
    /// Source language: python | go | bash | markdown. Surfaced to the UI so
    /// the editor can pick a syntax mode without re-reading the file.
    pub language: String,
    /// Associated store catalog item ID. When present, the frontend uses it
    /// to auto-select a matching template or prompt the user to install one.
    pub store_item_id: Option<String>,
}

#[derive(Deserialize, ToSchema)]
pub struct RunExampleRequest {
    pub id: String,
    /// Optional template ID override. When provided, takes highest priority
    /// over server-configured defaults. Allows the frontend to let users
    /// pick which template to use for each example run.
    pub template_id: Option<String>,
    /// Optional override of the example language. Surfaced back to the UI so
    /// it can verify the editor picked the right syntax mode; the handler
    /// itself picks the interpreter from the file extension.
    #[allow(dead_code)]
    pub language: Option<String>,
    /// When present, the handler writes this body to a temporary file next
    /// to the original and runs that file instead. This lets the UI surface
    /// an editable Monaco buffer while keeping the registry on disk
    /// authoritative for read access.
    pub code: Option<String>,
}

#[derive(Serialize, Clone, ToSchema)]
pub struct StepLog {
    pub name: String,
    /// "control" (CubeAPI / CubeMaster) or "data" (envd / sandbox runtime).
    pub plane: String,
    /// "ok" | "warn" | "err" | "skipped".
    pub status: String,
    pub duration_ms: u64,
    pub message: String,
}

#[derive(Serialize, Clone, ToSchema)]
pub struct TopologyNode {
    pub id: String,
    pub label: String,
    /// "control" | "data".
    pub plane: String,
    /// "user" | "control" | "data" | "vm" | "store".
    pub kind: String,
    pub description: String,
}

#[derive(Serialize, Clone, ToSchema)]
pub struct TopologyEdge {
    pub from: String,
    pub to: String,
    pub label: String,
    pub plane: String,
}

#[derive(Serialize, Clone, ToSchema)]
pub struct TopologyGraph {
    pub nodes: Vec<TopologyNode>,
    pub edges: Vec<TopologyEdge>,
}

#[derive(Serialize, Clone, ToSchema)]
pub struct RunExampleResponse {
    pub stdout: String,
    pub stderr: String,
    pub exit_code: i32,
    pub success: bool,
    pub elapsed_ms: u64,
    pub steps: Vec<StepLog>,
    pub topology: TopologyGraph,
    pub ran_edited: bool,
}

// ─── Example registry ─────────────────────────────────────────────────────────

/// Static per-file metadata. One entry per runnable file.
#[derive(Clone)]
struct FileSpec {
    id: &'static str,
    filename: &'static str,
    title: &'static str,
    description: &'static str,
    language: &'static str,
}

/// Static scenario metadata. `hidden: true` keeps the scenario on disk and
/// queryable for future re-enable, but excludes it from list/source/run
/// responses so AI/LLM demos do not leak into the UI.
struct ScenarioSpec {
    id: &'static str,
    category: &'static str,
    hidden: bool,
    files: &'static [FileSpec],
    /// Per-scenario run timeout in seconds. Defaults to 120 when absent.
    timeout_secs: Option<u64>,
    /// Topology template applied to every file inside this scenario. The
    /// per-run response augments this with a "ran ok / failed" node status
    /// before shipping it to the UI.
    topology: TopologyTemplate,
    /// Associated store catalog item ID (e.g. "sandbox-browser").
    /// When set, the run_example handler uses it to find a template whose
    /// image matches the catalog item's image, instead of falling back to
    /// the default template. The frontend also uses this to auto-select
    /// the recommended template or show an "install first" prompt.
    store_item_id: Option<&'static str>,
}

/// Either a fixed graph or a closure that emits nodes/edges dynamically
/// (we only need static templates today, but the indirection lets us add
/// e.g. bench concurrency fan-outs later without touching the registry).
#[derive(Clone)]
struct TopologyTemplate {
    nodes: Vec<TopologyNode>,
    edges: Vec<TopologyEdge>,
}

fn topology_for(scenario: &str) -> TopologyTemplate {
    // Shared base topology:
    //   Control plane: User → CubeAPI → CubeMaster → Cubelet
    //   Data plane:    CubeAPI → CubeProxy → envd → Runner
    //   MicroVM (data plane) is the sandbox boundary; Cubelet creates it
    //   via QMP (control-plane edge) but the workload runs inside it.
    //   envd is reached by CubeProxy over a WSS tunnel — NOT via a direct
    //   microvm→envd edge (that would be a containment relationship, not
    //   a network connection).
    let mut nodes = vec![
        TopologyNode {
            id: "user".into(),
            label: "User Script".into(),
            plane: "control".into(),
            kind: "user".into(),
            description: "The example invocation triggered when you click Run.".into(),
        },
        TopologyNode {
            id: "cubeapi".into(),
            label: "CubeAPI :3000".into(),
            plane: "control".into(),
            kind: "control".into(),
            description: "HTTP gateway: validates requests, schedules sandbox creation, proxies data.".into(),
        },
        TopologyNode {
            id: "cubemaster".into(),
            label: "CubeMaster".into(),
            plane: "control".into(),
            kind: "control".into(),
            description: "Scheduler: picks a Cubelet node based on template & load.".into(),
        },
        TopologyNode {
            id: "cubelet".into(),
            label: "Cubelet".into(),
            plane: "control".into(),
            kind: "control".into(),
            description: "Per-node agent: manages the full MicroVM lifecycle.".into(),
        },
        TopologyNode {
            id: "cubeproxy".into(),
            label: "CubeProxy".into(),
            plane: "data".into(),
            kind: "control".into(),
            description: "TLS-terminating reverse proxy: forwards via WSS tunnel to in-sandbox envd.".into(),
        },
        TopologyNode {
            id: "microvm".into(),
            label: "KVM MicroVM".into(),
            plane: "data".into(),
            kind: "vm".into(),
            description: "QEMU/KVM MicroVM: sandbox isolation boundary running envd and the workload.".into(),
        },
        TopologyNode {
            id: "envd".into(),
            label: "envd :49983".into(),
            plane: "data".into(),
            kind: "data".into(),
            description: "In-sandbox daemon: exposes Jupyter kernel, filesystem and shell.".into(),
        },
        TopologyNode {
            id: "runner".into(),
            label: "Python / Shell".into(),
            plane: "data".into(),
            kind: "data".into(),
            description: "The interpreter process that runs the example code, forked by envd.".into(),
        },
    ];
    let mut edges = vec![
        TopologyEdge {
            from: "user".into(),
            to: "cubeapi".into(),
            label: "HTTPS".into(),
            plane: "control".into(),
        },
        TopologyEdge {
            from: "cubeapi".into(),
            to: "cubemaster".into(),
            label: "gRPC".into(),
            plane: "control".into(),
        },
        TopologyEdge {
            from: "cubemaster".into(),
            to: "cubelet".into(),
            label: "gRPC".into(),
            plane: "control".into(),
        },
        TopologyEdge {
            from: "cubelet".into(),
            to: "microvm".into(),
            label: "QMP / boot".into(),
            plane: "control".into(),
        },
        TopologyEdge {
            from: "cubeapi".into(),
            to: "cubeproxy".into(),
            label: "HTTPS".into(),
            plane: "data".into(),
        },
        TopologyEdge {
            from: "cubeproxy".into(),
            to: "envd".into(),
            label: "WSS tunnel".into(),
            plane: "data".into(),
        },
        TopologyEdge {
            from: "envd".into(),
            to: "runner".into(),
            label: "fork+exec".into(),
            plane: "data".into(),
        },
    ];

    match scenario {
        "network-policy" => {
            // Insert eBPF tap between Cubelet and MicroVM.
            nodes.push(TopologyNode {
                id: "cubevs".into(),
                label: "CubeVS (eBPF)".into(),
                plane: "data".into(),
                kind: "control".into(),
                description: "eBPF datapath enforcing allow/deny rules on the guest's veth.".into(),
            });
            edges.push(TopologyEdge {
                from: "cubelet".into(),
                to: "cubevs".into(),
                label: "tc/eBPF".into(),
                plane: "data".into(),
            });
            edges.push(TopologyEdge {
                from: "cubevs".into(),
                to: "microvm".into(),
                label: "veth".into(),
                plane: "data".into(),
            });
            // Drop the default edge if it exists.
            edges.retain(|e| !(e.from == "cubelet" && e.to == "microvm"));
        }
        "host-mount" => {
            // Source path comes from the host.
            nodes.push(TopologyNode {
                id: "hostdir".into(),
                label: "Host directory".into(),
                plane: "data".into(),
                kind: "store".into(),
                description: "Local directory bind-mounted into the MicroVM at boot.".into(),
            });
            edges.push(TopologyEdge {
                from: "hostdir".into(),
                to: "microvm".into(),
                label: "9p / virtiofs".into(),
                plane: "data".into(),
            });
        }
        "browser-sandbox" => {
            // Replace the runner with Chromium + Playwright.
            nodes.retain(|n| n.id != "runner");
            edges.retain(|e| e.from != "envd" || e.to != "runner");
            nodes.push(TopologyNode {
                id: "chromium".into(),
                label: "Chromium :9000".into(),
                plane: "data".into(),
                kind: "data".into(),
                description: "Headless Chromium inside the guest with CDP enabled.".into(),
            });
            nodes.push(TopologyNode {
                id: "playwright".into(),
                label: "Playwright (CDP)".into(),
                plane: "data".into(),
                kind: "data".into(),
                description: "Python client driving Chromium over the Chrome DevTools Protocol.".into(),
            });
            edges.push(TopologyEdge {
                from: "envd".into(),
                to: "playwright".into(),
                label: "exec".into(),
                plane: "data".into(),
            });
            edges.push(TopologyEdge {
                from: "playwright".into(),
                to: "chromium".into(),
                label: "CDP WS".into(),
                plane: "data".into(),
            });
        }
        "snapshot-rollback-clone" => {
            // LVM snapshot storage sitting next to the VM.
            nodes.push(TopologyNode {
                id: "snapshot".into(),
                label: "Snapshot (LVM)".into(),
                plane: "control".into(),
                kind: "store".into(),
                description: "CoW snapshot of the root LV. Outlives the sandbox; clones & rollback source.".into(),
            });
            edges.push(TopologyEdge {
                from: "cubelet".into(),
                to: "snapshot".into(),
                label: "lvcreate --snapshot".into(),
                plane: "control".into(),
            });
            edges.push(TopologyEdge {
                from: "snapshot".into(),
                to: "microvm".into(),
                label: "rollback".into(),
                plane: "control".into(),
            });
        }
        "e2b-dev-sidecar" => {
            // Local sidecar proxies requests through header rewriting.
            nodes.push(TopologyNode {
                id: "sidecar".into(),
                label: "Dev Sidecar".into(),
                plane: "data".into(),
                kind: "control".into(),
                description: "Local reverse-proxy that rewrites Host headers for e2b compatibility.".into(),
            });
            edges.retain(|e| !(e.from == "cubeapi" && e.to == "cubeproxy"));
            edges.push(TopologyEdge {
                from: "cubeapi".into(),
                to: "sidecar".into(),
                label: "HTTPS".into(),
                plane: "data".into(),
            });
            edges.push(TopologyEdge {
                from: "sidecar".into(),
                to: "cubeproxy".into(),
                label: "Host rewrite".into(),
                plane: "data".into(),
            });
        }
        "cubesandbox-base-nginx" => {
            // Replace runner with nginx so the topology reflects a web workload.
            nodes.retain(|n| n.id != "runner");
            edges.retain(|e| e.from != "envd" || e.to != "runner");
            nodes.push(TopologyNode {
                id: "nginx".into(),
                label: "nginx :80".into(),
                plane: "data".into(),
                kind: "data".into(),
                description: "nginx serving static files inside the guest image.".into(),
            });
            edges.push(TopologyEdge {
                from: "envd".into(),
                to: "nginx".into(),
                label: "exec".into(),
                plane: "data".into(),
            });
        }
        "cube-bench" => {
            // Fan out: replace MicroVM with N replicas.
            nodes.retain(|n| n.id != "microvm");
            edges.retain(|e| e.to != "microvm");
            let n = 4usize;
            for i in 0..n {
                nodes.push(TopologyNode {
                    id: format!("microvm-{i}"),
                    label: format!("MicroVM #{i}"),
                    plane: "data".into(),
                    kind: "vm".into(),
                    description: "Concurrent benchmark target sandbox.".into(),
                });
                edges.push(TopologyEdge {
                    from: "cubelet".into(),
                    to: format!("microvm-{i}"),
                    label: "QMP".into(),
                    plane: "control".into(),
                });
            }
        }
        _ => {}
    }

    TopologyTemplate { nodes, edges }
}

fn file_languages() -> std::collections::HashMap<&'static str, &'static str> {
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
        ("snapshot-rollback-clone:05_snapshot_outlives_sandbox", "python"),
        ("snapshot-rollback-clone:06_clone_n", "python"),
        ("snapshot-rollback-clone:07_clone_concurrent", "python"),
        ("snapshot-rollback-clone:08_fork_three_axis", "python"),
        ("snapshot-rollback-clone:09_rollback", "python"),
        ("snapshot-rollback-clone:10_rollback_then_continue", "python"),
        ("snapshot-rollback-clone:11_delete_snapshot", "python"),
        ("snapshot-rollback-clone:clone_demo", "python"),
        ("snapshot-rollback-clone:rollback_demo", "python"),
        ("e2b-dev-sidecar:demo", "python"),
        ("cubesandbox-base-nginx:test_files", "python"),
        ("cube-bench:main", "go"),
    ]
    .into_iter()
    .collect()
}

fn scenario_registry() -> &'static [ScenarioSpec] {
    // The scenarios are referenced from `file_languages()` via "<scenario>:<id>".
    // Keeping the registry a single static slice lets the front-end render
    // groups in a deterministic order without depending on filesystem layout.
    //
    // `Box::leak` materialises the Vec exactly once at process start; subsequent
    // calls return the same `&'static` slice, so the front-end gets a stable
    // ordering without the borrow checker complaining about temporaries.
    Box::leak(Box::new(vec![
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
                    description: "Run Python code inside the sandbox through the Jupyter kernel.",
                    language: "python",
                },
                FileSpec {
                    id: "cmd",
                    filename: "cmd.py",
                    title: "Run Shell Command",
                    description: "Execute a shell command inside the sandbox and capture stdout.",
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
                FileSpec { id: "01_create_snapshot", filename: "01_create_snapshot.py", title: "01 Create Snapshot", description: "Capture a snapshot from a running sandbox.", language: "python" },
                FileSpec { id: "02_list_snapshots", filename: "02_list_snapshots.py", title: "02 List Snapshots", description: "List snapshots attached to the cluster.", language: "python" },
                FileSpec { id: "03_clone_from_snapshot", filename: "03_clone_from_snapshot.py", title: "03 Clone From Snapshot", description: "Create a new sandbox from a snapshot.", language: "python" },
                FileSpec { id: "04_state_preserved", filename: "04_state_preserved.py", title: "04 State Preserved", description: "Verify state survives the clone.", language: "python" },
                FileSpec { id: "05_snapshot_outlives_sandbox", filename: "05_snapshot_outlives_sandbox.py", title: "05 Snapshot Outlives", description: "Snapshot outlives its source sandbox.", language: "python" },
                FileSpec { id: "06_clone_n", filename: "06_clone_n.py", title: "06 Clone N Times", description: "Spin up N clones in sequence.", language: "python" },
                FileSpec { id: "07_clone_concurrent", filename: "07_clone_concurrent.py", title: "07 Clone Concurrently", description: "Spin up N clones in parallel.", language: "python" },
                FileSpec { id: "08_fork_three_axis", filename: "08_fork_three_axis.py", title: "08 Fork Three-axis", description: "Three orthogonal dimensions of clone/rollback.", language: "python" },
                FileSpec { id: "09_rollback", filename: "09_rollback.py", title: "09 Rollback", description: "Roll the sandbox back to a previous snapshot.", language: "python" },
                FileSpec { id: "10_rollback_then_continue", filename: "10_rollback_then_continue.py", title: "10 Rollback Then Continue", description: "Rollback, then resume normal execution.", language: "python" },
                FileSpec { id: "11_delete_snapshot", filename: "11_delete_snapshot.py", title: "11 Delete Snapshot", description: "Clean up a snapshot from the cluster.", language: "python" },
                FileSpec { id: "clone_demo", filename: "clone_demo.py", title: "Clone Demo", description: "End-to-end clone walkthrough.", language: "python" },
                FileSpec { id: "rollback_demo", filename: "rollback_demo.py", title: "Rollback Demo", description: "End-to-end rollback walkthrough.", language: "python" },
            ],
            timeout_secs: None,
            topology: topology_for("snapshot-rollback-clone"),
            store_item_id: Some("sandbox-code"),
        },
        ScenarioSpec {
            id: "e2b-dev-sidecar",
            category: "advanced",
            hidden: false,
            files: &[FileSpec {
                id: "demo",
                filename: "demo.py",
                title: "Sidecar Demo",
                description: "Start a sidecar proxy in front of CubeAPI.",
                language: "python",
            }],
            timeout_secs: None,
            topology: topology_for("e2b-dev-sidecar"),
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
        // later (when LLM credentials are configured) is a one-line
        // change without any schema work.
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
    ]))
}

fn examples_root() -> PathBuf {
    // CUBE_EXAMPLES_DIR overrides the root for tests / packaged installs.
    // Default points at the in-repo `examples/` directory.
    if let Ok(v) = std::env::var("CUBE_EXAMPLES_DIR") {
        return PathBuf::from(v);
    }
    // `Cargo.toml` lives at <repo>/CubeAPI/Cargo.toml, so `../../examples` is
    // the in-repo default when running from source.
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("..")
        .join("examples")
}

fn list_visible() -> Vec<ExampleMeta> {
    let langs = file_languages();
    let mut out = Vec::new();
    for sc in scenario_registry() {
        if sc.hidden {
            continue;
        }
        for f in sc.files {
            let full_id = format!("{}:{}", sc.id, f.id);
            let language = langs
                .get(full_id.as_str())
                .copied()
                .unwrap_or(f.language)
                .to_string();
            out.push(ExampleMeta {
                id: full_id,
                scenario: sc.id.to_string(),
                filename: f.filename.to_string(),
                title: f.title.to_string(),
                description: f.description.to_string(),
                category: sc.category.to_string(),
                language,
                store_item_id: sc.store_item_id.map(|s| s.to_string()),
            });
        }
    }
    out
}

fn resolve_visible(id: &str) -> Option<(ExampleMeta, &'static ScenarioSpec, &'static FileSpec)> {
    let langs = file_languages();
    let (scenario_id, file_id) = id.split_once(':')?;
    for sc in scenario_registry() {
        if sc.hidden || sc.id != scenario_id {
            continue;
        }
        for f in sc.files {
            if f.id == file_id {
                let full_id = format!("{}:{}", sc.id, f.id);
                let language = langs
                    .get(full_id.as_str())
                    .copied()
                    .unwrap_or(f.language)
                    .to_string();
                let meta = ExampleMeta {
                    id: full_id,
                    scenario: sc.id.to_string(),
                    filename: f.filename.to_string(),
                    title: f.title.to_string(),
                    description: f.description.to_string(),
                    category: sc.category.to_string(),
                    language,
                    store_item_id: sc.store_item_id.map(|s| s.to_string()),
                };
                return Some((meta, sc, f));
            }
        }
    }
    None
}

// fn template_for is intentionally not exposed: the per-scenario topology
// is baked into `ScenarioSpec::topology` and resolved directly in the
// request handlers via `sc.topology.clone()`.

fn topology_with_status(t: TopologyTemplate, success: bool) -> TopologyGraph {
    let mut t = t;
    // Mark the user / runner nodes with the run status so the UI can color
    // them red when the run failed. The rest stay neutral.
    let runner_status = if success { "ok" } else { "err" };
    for n in t.nodes.iter_mut() {
        if n.id == "user" || n.id == "runner" || n.id == "playwright" {
            // Don't overwrite kind; we just stash status in `description`
            // since TopologyNode already carries plain metadata. To keep the
            // schema stable, prepend a one-line status indicator.
            n.description = format!("[{}] {}", runner_status, n.description);
        }
    }
    TopologyGraph {
        nodes: t.nodes,
        edges: t.edges,
    }
}

// ─── GET /cubeapi/v1/examples ────────────────────────────────────────────────

/// List all visible example scripts. Hidden scenarios (AI / LLM demos) are
/// intentionally filtered out at the source.
pub async fn list_examples(State(_state): State<AppState>) -> AppResult<impl IntoResponse> {
    Ok(Json(list_visible()))
}

// ─── GET /cubeapi/v1/examples/:id ───────────────────────────────────────────

/// Get the source code of a single example script by scenario + file id.
pub async fn get_example_source(
    State(_state): State<AppState>,
    axum::extract::Path((scenario, file)): axum::extract::Path<(String, String)>,
) -> AppResult<impl IntoResponse> {
    let id = format!("{}:{}", scenario, file);
    let (meta, _sc, _f) = match resolve_visible(&id) {
        Some(v) => v,
        None => {
            return Ok((
                StatusCode::NOT_FOUND,
                Json(serde_json::json!({
                    "error": format!("Example '{}' not found", id)
                })),
            )
                .into_response());
        }
    };

    let base_dir = examples_root().join(&meta.scenario);
    let script_path = base_dir.join(&meta.filename);

    match std::fs::read_to_string(&script_path) {
        Ok(source) => Ok((
            StatusCode::OK,
            Json(serde_json::json!({
                "id": meta.id,
                "filename": meta.filename,
                "scenario": meta.scenario,
                "language": meta.language,
                "source": source,
            })),
        )
            .into_response()),
        Err(io_err) => Ok((
            StatusCode::INTERNAL_SERVER_ERROR,
            Json(serde_json::json!({
                "error": format!("Failed to read '{}': {}", script_path.display(), io_err)
            })),
        )
            .into_response()),
    }
}

// ─── POST /cubeapi/v1/examples/run ───────────────────────────────────────────

/// Install per-scenario Python dependencies from `requirements.txt` if present.
///
/// Uses a lightweight fingerprint file (`.requirements_installed`) to skip
/// redundant installs when the requirements have not changed since the last
/// successful install.
async fn ensure_requirements(base_dir: &PathBuf) -> bool {
    let req_file = base_dir.join("requirements.txt");
    if !req_file.exists() {
        return true; // no requirements file — nothing to install
    }

    // Fingerprint: hash of requirements.txt content. If unchanged since last
    // install, skip pip install to avoid ~10s overhead per run.
    let req_content = match std::fs::read_to_string(&req_file) {
        Ok(c) => c,
        Err(e) => {
            tracing::warn!("cannot read {}: {}", req_file.display(), e);
            return false;
        }
    };
    let fingerprint = {
        use std::collections::hash_map::DefaultHasher;
        use std::hash::{Hash, Hasher};
        let mut hasher = DefaultHasher::new();
        req_content.hash(&mut hasher);
        format!("{:016x}", hasher.finish())
    };

    let stamp_file = base_dir.join(".requirements_installed");
    if let Ok(stamp) = std::fs::read_to_string(&stamp_file) {
        if stamp == fingerprint {
            tracing::debug!(
                "requirements unchanged (fingerprint={}), skipping pip install",
                fingerprint
            );
            return true;
        }
    }

    tracing::info!("installing scenario requirements from {}", req_file.display());
    let install_result = Command::new("pip3")
        .args(["install", "--quiet", "-r"])
        .arg(&req_file)
        .output()
        .await;

    match install_result {
        Ok(output) => {
            if output.status.success() {
                let _ = std::fs::write(&stamp_file, &fingerprint);
                true
            } else {
                tracing::warn!(
                    stderr = %String::from_utf8_lossy(&output.stderr),
                    "pip install failed, continuing anyway"
                );
                // Still return true — the script might work with already-installed packages
                true
            }
        }
        Err(e) => {
            tracing::warn!("failed to spawn pip3: {}", e);
            true // don't block execution; let the script fail with its own ImportError
        }
    }
}

/// Run an example script in a subprocess and return stdout / stderr plus a
/// synthetic step log and the topology graph for the scenario.
pub async fn run_example(
    State(state): State<AppState>,
    Json(req): Json<RunExampleRequest>,
) -> AppResult<impl IntoResponse> {
    let (meta, sc, _f) = match resolve_visible(&req.id) {
        Some(v) => v,
        None => {
            return Ok((
                StatusCode::NOT_FOUND,
                Json(serde_json::json!(RunExampleResponse {
                    stdout: String::new(),
                    stderr: format!("Example '{}' not found", req.id),
                    exit_code: 1,
                    success: false,
                    elapsed_ms: 0,
                    steps: Vec::new(),
                    topology: topology_with_status(topology_for("code-sandbox-quickstart"), false),
                    ran_edited: false,
                })),
            )
                .into_response());
        }
    };

    let base_dir = examples_root().join(&meta.scenario);
    let script_path = base_dir.join(&meta.filename);

    // ── Template ID resolution ──────────────────────────────────────
    // Priority:
    //   1. User-explicit template_id (from frontend)
    //   2. store_item_id → match by image_info against catalog image
    //   3. Config default_template_id / env CUBE_TEMPLATE_ID
    //   4. Any healthy/ready template
    let candidates: Vec<String> = [
        req.template_id.filter(|s| !s.trim().is_empty()),
        state.config.default_template_id.clone(),
        std::env::var("CUBE_TEMPLATE_ID").ok().filter(|s| !s.is_empty()),
    ]
    .into_iter()
    .flatten()
    .collect();

    let mut template_id = String::new();
    for candidate in &candidates {
        match state.services.templates.get_template(candidate).await {
            Ok(_) => {
                template_id = candidate.clone();
                break;
            }
            Err(e) => {
                tracing::warn!(
                    candidate = %candidate,
                    error = %e,
                    "template candidate failed validation, trying next"
                );
            }
        }
    }

    // ── store_item_id-based lookup ──────────────────────────────────
    // If no template found yet and the scenario has a store_item_id,
    // find a template whose image_info matches the catalog item's image.
    if template_id.is_empty() {
        if let Some(ref sid) = sc.store_item_id {
            let catalog_image = crate::handlers::store::fallback_catalog()
                .into_iter()
                .find(|item| item.id == *sid)
                .map(|item| item.image_cn);

            if let Some(ref image_ref) = catalog_image {
                match state.services.templates.list_templates().await {
                    Ok(templates) => {
                        let matched = templates.iter().find(|t| {
                            (t.status == "healthy" || t.status == "ready")
                                && t.image_info.as_deref() == Some(image_ref.as_str())
                        });
                        if let Some(t) = matched {
                            tracing::info!(
                                store_item_id = %sid,
                                image = %image_ref,
                                template_id = %t.template_id,
                                "matched template via store_item_id"
                            );
                            template_id = t.template_id.clone();
                        }
                    }
                    Err(e) => {
                        tracing::warn!(error = %e, "failed to list templates for store_item_id lookup");
                    }
                }
            }
        }
    }

    if template_id.is_empty() {
        match state.services.templates.list_templates().await {
            Ok(templates) => {
                let list_candidates: Vec<_> = templates
                    .iter()
                    .filter(|t| t.status == "healthy" || t.status == "ready")
                    .chain(templates.iter())
                    .map(|t| t.template_id.as_str())
                    .collect();

                for candidate in list_candidates {
                    match state.services.templates.get_template(candidate).await {
                        Ok(_) => {
                            template_id = candidate.to_string();
                            break;
                        }
                        Err(e) => {
                            tracing::warn!(
                                candidate = %candidate,
                                error = %e,
                                "listed template failed validation, skipping"
                            );
                        }
                    }
                }
            }
            Err(e) => {
                tracing::warn!(error = %e, "failed to list templates for fallback");
            }
        }
    }

    if template_id.is_empty() {
        return Ok((
            StatusCode::BAD_REQUEST,
            Json(RunExampleResponse {
                stdout: String::new(),
                stderr: "No template ID configured. Set CUBE_TEMPLATE_ID, configure a default template, or create a template first.".to_string(),
                exit_code: 1,
                success: false,
                elapsed_ms: 0,
                steps: Vec::new(),
                topology: topology_with_status(sc.topology.clone(), false),
                ran_edited: req.code.is_some(),
            }),
        )
            .into_response());
    }

    let cube_api_url = state
        .config
        .cube_api_url
        .clone()
        .unwrap_or_else(|| "http://127.0.0.1:3000".to_string());

    tracing::info!(
        example_id = %req.id,
        scenario = %meta.scenario,
        script = %script_path.display(),
        template_id = %template_id,
        edited = req.code.is_some(),
        "running example"
    );

    let ssl_cert = std::env::var("SSL_CERT_FILE")
        .unwrap_or_else(|_| "/root/.local/share/mkcert/rootCA.pem".to_string());

    // ── Interpreter dispatch based on file extension ────────────────
    // Keeping this language-driven (not request-driven) means a malicious
    // `language` field cannot change the interpreter used for a file with
    // a known extension.
    let ext = script_path
        .extension()
        .and_then(|s| s.to_str())
        .unwrap_or("")
        .to_lowercase();
    let program: &str = match ext.as_str() {
        "py" => "python3",
        "go" => "go",
        "sh" | "bash" => "bash",
        "js" | "mjs" => "node",
        _ => {
            return Ok((
                StatusCode::BAD_REQUEST,
                Json(RunExampleResponse {
                    stdout: String::new(),
                    stderr: format!(
                        "Unsupported file extension '.{}' for example '{}'",
                        ext, req.id
                    ),
                    exit_code: 1,
                    success: false,
                    elapsed_ms: 0,
                    steps: Vec::new(),
                    topology: topology_with_status(sc.topology.clone(), false),
                    ran_edited: req.code.is_some(),
                }),
            )
                .into_response());
        }
    };

    // ── Auto-install per-scenario Python dependencies ────────────
    if program == "python3" {
        ensure_requirements(&base_dir).await;
    }

    // ── When the user edited the code, materialise a temp file next to
    // the original so relative imports / shared modules keep working.
    let mut tmp_path: Option<PathBuf> = None;
    let mut tmp_dir: Option<PathBuf> = None; // for Go: isolated subdirectory
    let run_path: PathBuf = if let Some(user_code) = req.code.as_ref() {
        if program == "go" {
            // For Go, place the edited file in an isolated subdirectory so
            // that `go run .` compiles only the user's code without
            // conflicting with the other `.go` files in the package.
            let dir_name = format!(".tmp_run_{}", Uuid::new_v4());
            let dir = base_dir.join(&dir_name);
            if let Err(io_err) = std::fs::create_dir_all(&dir) {
                return Ok((
                    StatusCode::INTERNAL_SERVER_ERROR,
                    Json(RunExampleResponse {
                        stdout: String::new(),
                        stderr: format!(
                            "Failed to create temp dir {}: {}",
                            dir.display(),
                            io_err
                        ),
                        exit_code: 1,
                        success: false,
                        elapsed_ms: 0,
                        steps: Vec::new(),
                        topology: topology_with_status(sc.topology.clone(), false),
                        ran_edited: true,
                    }),
                )
                    .into_response());
            }
            let tmp = dir.join(&meta.filename);
            if let Err(io_err) = std::fs::write(&tmp, user_code) {
                let _ = std::fs::remove_dir_all(&dir);
                return Ok((
                    StatusCode::INTERNAL_SERVER_ERROR,
                    Json(RunExampleResponse {
                        stdout: String::new(),
                        stderr: format!(
                            "Failed to write edited code to {}: {}",
                            tmp.display(),
                            io_err
                        ),
                        exit_code: 1,
                        success: false,
                        elapsed_ms: 0,
                        steps: Vec::new(),
                        topology: topology_with_status(sc.topology.clone(), false),
                        ran_edited: true,
                    }),
                )
                    .into_response());
            }
            // Copy go.mod / go.sum so the isolated dir compiles as a
            // standalone module.
            for go_file in &["go.mod", "go.sum"] {
                let src = base_dir.join(go_file);
                if src.exists() {
                    let _ = std::fs::copy(&src, dir.join(go_file));
                }
            }
            tmp_path = Some(tmp);
            tmp_dir = Some(dir.clone());
            dir.join(&meta.filename)
        } else {
            let tmp_name = format!(".tmp_run_{}.{}", Uuid::new_v4(), ext);
            let tmp = base_dir.join(&tmp_name);
            if let Err(io_err) = std::fs::write(&tmp, user_code) {
                return Ok((
                    StatusCode::INTERNAL_SERVER_ERROR,
                    Json(RunExampleResponse {
                        stdout: String::new(),
                        stderr: format!(
                            "Failed to write edited code to {}: {}",
                            tmp.display(),
                            io_err
                        ),
                        exit_code: 1,
                        success: false,
                        elapsed_ms: 0,
                        steps: Vec::new(),
                        topology: topology_with_status(sc.topology.clone(), false),
                        ran_edited: true,
                    }),
                )
                    .into_response());
            }
            tmp_path = Some(tmp.clone());
            tmp
        }
    } else {
        script_path.clone()
    };

    // Build argv from the resolved run_path.
    // - For Go: use `go run .` to compile the whole package directory
    //   (multi-file packages like cube-bench need this).
    // - For everything else: pass the file path directly.
    let argv: Vec<String> = match program {
        "go" => vec!["run".to_string(), ".".to_string()],
        _ => vec![run_path.to_string_lossy().to_string()],
    };

    // For Go, the working directory must be the directory containing the
    // entry file so that `go run .` finds all sibling `.go` files.
    let work_dir = if program == "go" {
        run_path.parent().unwrap_or(&base_dir).to_path_buf()
    } else {
        base_dir.clone()
    };

    let mut cmd = Command::new(program);
    for a in &argv {
        cmd.arg(a);
    }
    cmd.env("CUBE_API_URL", &cube_api_url)
        .env("CUBE_TEMPLATE_ID", &template_id)
        .env("SSL_CERT_FILE", ssl_cert)
        .current_dir(&work_dir);

    // ── Common: API key for all scenarios ─────────────────────────
    // Many SDKs and examples (cube-bench, e2b SDK, etc.) require a
    // non-empty API key. For local dev any placeholder satisfies the
    // check. Prefer an explicitly set key if one exists.
    if std::env::var("CUBE_API_KEY").is_err() {
        cmd.env("CUBE_API_KEY", "cube_0000000000000000000000000000000000000000");
    }

    if let Some(ref proxy_ip) = state.config.cube_proxy_node_ip {
        cmd.env("CUBE_PROXY_NODE_IP", proxy_ip);
    }
    if let Some(proxy_port) = state.config.cube_proxy_port_http {
        cmd.env("CUBE_PROXY_PORT_HTTP", proxy_port.to_string());
    }
    cmd.env("CUBE_SANDBOX_DOMAIN", &state.config.sandbox_domain);

    // ── Scenario-specific environment variables ──────────────────
    if meta.scenario == "e2b-dev-sidecar" {
        // The e2b SDK reads E2B_API_URL (not CUBE_API_URL) for the
        // control-plane endpoint. Map it to the same CubeAPI URL.
        cmd.env("E2B_API_URL", &cube_api_url);
        // The e2b SDK also reads E2B_API_KEY. Map it from CUBE_API_KEY
        // or use the same placeholder.
        if std::env::var("E2B_API_KEY").is_err() {
            cmd.env("E2B_API_KEY", "e2b_0000000000000000000000000000000000000000");
        }
        // Data-plane: the sidecar proxies to CubeProxy. Derive the
        // base URL from the proxy config.
        let proxy_base = if let Some(ref ip) = state.config.cube_proxy_node_ip {
            let port_suffix = state
                .config
                .cube_proxy_port_http
                .filter(|&p| p != 443)
                .map(|p| format!(":{}", p))
                .unwrap_or_default();
            format!("https://{}{}", ip, port_suffix)
        } else {
            // No explicit proxy IP configured; assume CubeProxy (nginx)
            // is on localhost at the standard HTTPS port.
            let port_suffix = state
                .config
                .cube_proxy_port_http
                .filter(|&p| p != 443)
                .map(|p| format!(":{}", p))
                .unwrap_or_default();
            format!("https://127.0.0.1{}", port_suffix)
        };
        cmd.env("CUBE_REMOTE_PROXY_BASE", &proxy_base);
        cmd.env("CUBE_REMOTE_PROXY_VERIFY_SSL", "false");
        // The sidecar reads CUBE_REMOTE_SANDBOX_DOMAIN for the Host
        // header sent to CubeProxy. Map it from the same domain config.
        cmd.env("CUBE_REMOTE_SANDBOX_DOMAIN", &state.config.sandbox_domain);
    }

    let start = Instant::now();
    let max_secs = sc.timeout_secs.unwrap_or(120);
    let run_result = timeout(Duration::from_secs(max_secs), cmd.output()).await;
    let elapsed_ms = start.elapsed().as_millis() as u64;

    // Always remove the temp file/dir, even on error paths.
    if let Some(d) = tmp_dir.take() {
        let _ = std::fs::remove_dir_all(&d);
    } else if let Some(p) = tmp_path.take() {
        let _ = std::fs::remove_file(&p);
    }

    let topology = topology_with_status(sc.topology.clone(), false); // refined below

    match run_result {
        Ok(Ok(output)) => {
            let stdout = String::from_utf8_lossy(&output.stdout).to_string();
            let stderr = String::from_utf8_lossy(&output.stderr).to_string();
            let exit_code = output.status.code().unwrap_or(-1);
            let success = output.status.success();

            tracing::info!(
                example_id = %req.id,
                exit_code,
                success,
                elapsed_ms,
                "example run complete"
            );

            let steps: Vec<StepLog> = Vec::new();
            let topology = topology_with_status(sc.topology.clone(), success);

            Ok(Json(RunExampleResponse {
                stdout,
                stderr,
                exit_code,
                success,
                elapsed_ms,
                steps,
                topology,
                ran_edited: req.code.is_some(),
            })
            .into_response())
        }
        Ok(Err(io_err)) => Ok((
            StatusCode::INTERNAL_SERVER_ERROR,
            Json(RunExampleResponse {
                stdout: String::new(),
                stderr: format!("Failed to spawn process: {}", io_err),
                exit_code: -1,
                success: false,
                elapsed_ms,
                steps: Vec::new(),
                topology,
                ran_edited: req.code.is_some(),
            }),
        )
            .into_response()),
        Err(_elapsed) => Ok((
            StatusCode::GATEWAY_TIMEOUT,
            Json(RunExampleResponse {
                stdout: String::new(),
                stderr: format!("Example timed out after {} seconds", max_secs),
                exit_code: -1,
                success: false,
                elapsed_ms,
                steps: Vec::new(),
                topology,
                ran_edited: req.code.is_some(),
            }),
        )
            .into_response()),
    }
}