// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
//! Topology graph templates for each example scenario.
//!
//! Each scenario ships a `TopologyTemplate` (nodes + edges) that describes the
//! data-flow path from the user script down to the in-sandbox runner. The
//! HTTP handler augments this with a per-run status before serialising it for
//! the UI (`@xyflow/react`).

use serde::Serialize;
use utoipa::ToSchema;

// ─── Public graph types (shared with handler models) ─────────────────────────

#[derive(Serialize, Clone, ToSchema)]
pub struct TopologyNode {
    pub id: String,
    pub label: String,
    /// `"control"` | `"data"`.
    pub plane: String,
    /// `"user"` | `"control"` | `"data"` | `"vm"` | `"store"`.
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

/// Either a fixed graph or a closure that emits nodes/edges dynamically
/// (only static templates today, but the indirection lets us add e.g. bench
/// concurrency fan-outs later without touching the registry).
#[derive(Clone)]
pub struct TopologyTemplate {
    pub nodes: Vec<TopologyNode>,
    pub edges: Vec<TopologyEdge>,
}

// ─── Builder ─────────────────────────────────────────────────────────────────

/// Build the topology template for `scenario`.
///
/// Shared base topology:
/// ```text
/// Control plane: User → CubeAPI → CubeMaster → Cubelet
/// Data plane:    CubeAPI → CubeProxy → envd → Runner
/// ```
/// MicroVM (data plane) is the sandbox isolation boundary; Cubelet creates it
/// via QMP (control-plane edge) but the workload runs inside it. envd is
/// reached by CubeProxy over a WSS tunnel — NOT via a direct microvm→envd
/// edge (that would be a containment relationship, not a network connection).
pub fn topology_for(scenario: &str) -> TopologyTemplate {
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
            description:
                "HTTP gateway: validates requests, schedules sandbox creation, proxies data.".into(),
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
            description:
                "TLS-terminating reverse proxy: forwards via WSS tunnel to in-sandbox envd.".into(),
        },
        TopologyNode {
            id: "microvm".into(),
            label: "KVM MicroVM".into(),
            plane: "data".into(),
            kind: "vm".into(),
            description:
                "QEMU/KVM MicroVM: sandbox isolation boundary running envd and the workload.".into(),
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
            description: "The interpreter process that runs the example code, forked by envd."
                .into(),
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
            edges.retain(|e| !(e.from == "cubelet" && e.to == "microvm"));
        }
        "host-mount" => {
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
                description: "Python client driving Chromium over the Chrome DevTools Protocol."
                    .into(),
            });
            nodes.push(TopologyNode {
                id: "xvfb".into(),
                label: "Xvfb :99".into(),
                plane: "data".into(),
                kind: "data".into(),
                description: "X Virtual Framebuffer providing a virtual display for Chromium."
                    .into(),
            });
            nodes.push(TopologyNode {
                id: "x11vnc".into(),
                label: "x11vnc :5900".into(),
                plane: "data".into(),
                kind: "data".into(),
                description: "VNC server mirroring the Xvfb display on port 5900.".into(),
            });
            nodes.push(TopologyNode {
                id: "novnc".into(),
                label: "noVNC :6080".into(),
                plane: "data".into(),
                kind: "data".into(),
                description:
                    "WebSocket-to-VNC gateway: browser-based desktop viewable via CubeProxy."
                        .into(),
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
            edges.push(TopologyEdge {
                from: "envd".into(),
                to: "xvfb".into(),
                label: "exec".into(),
                plane: "data".into(),
            });
            edges.push(TopologyEdge {
                from: "xvfb".into(),
                to: "chromium".into(),
                label: "DISPLAY=:99".into(),
                plane: "data".into(),
            });
            edges.push(TopologyEdge {
                from: "xvfb".into(),
                to: "x11vnc".into(),
                label: "mirrors".into(),
                plane: "data".into(),
            });
            edges.push(TopologyEdge {
                from: "x11vnc".into(),
                to: "novnc".into(),
                label: "RFB".into(),
                plane: "data".into(),
            });
        }
        "snapshot-rollback-clone" => {
            nodes.push(TopologyNode {
                id: "snapshot".into(),
                label: "Snapshot (LVM)".into(),
                plane: "control".into(),
                kind: "store".into(),
                description:
                    "CoW snapshot of the root LV. Outlives the sandbox; clones & rollback source."
                        .into(),
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
        "cubesandbox-base-nginx" => {
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

// ─── Helpers used by the service layer ───────────────────────────────────────

/// Stamp run-status into the topology so the UI can colour the user / runner
/// nodes red on failure. Status is prepended to `description` to keep the
/// schema stable without adding a new field.
pub fn topology_with_status(t: TopologyTemplate, success: bool) -> TopologyGraph {
    let mut t = t;
    let runner_status = if success { "ok" } else { "err" };
    for n in t.nodes.iter_mut() {
        if n.id == "user" || n.id == "runner" || n.id == "playwright" {
            n.description = format!("[{}] {}", runner_status, n.description);
        }
    }
    TopologyGraph {
        nodes: t.nodes,
        edges: t.edges,
    }
}
