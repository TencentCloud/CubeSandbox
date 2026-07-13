# Introduction

Cube Sandbox is a **purpose-built infrastructure for AI Agents** — a production-hardened runtime service that hosts Agent processes, stateful services, and provides secure execution environments in hardware-isolated MicroVMs. Built on RustVMM + KVM, each sandbox boots in under 60ms with less than 5MB of additional memory consumption. The core system has been validated at scale in Tencent Cloud production environments before open-sourcing.

## Key Advantages

* **⚡ Sub-60ms Boot · High Density**: Average <60ms cold start, <5MB overhead per instance — run thousands of Agents on a single node. Under 50 concurrent creations: avg 67ms, P95 90ms, P99 137ms.

* **🔒 Hardware-Level Isolation**: Each sandbox gets its own dedicated Guest OS kernel — no shared-kernel container escapes. Run untrusted LLM-generated code safely with hardware-enforced boundaries (KVM + eBPF).

* **🤖 Agent Hosting, Not Just Code Execution**: Sandboxes can host long-running Agent processes (e.g. OpenClaw assistants via AgentHub), complete service stacks (Redis, MySQL, browsers, dev environments), or one-shot code execution — all with the same lifecycle primitives.

* **🔌 Seamless E2B Migration**: Native E2B SDK compatibility — swap one URL environment variable, zero business code changes.

* **⏸️ Auto-Pause & Auto-Resume**: Idle sandboxes are automatically snapshot-suspended (zero resource cost) and transparently restored on the next request — hundred-millisecond resume, zero caller awareness.

* **📸 Snapshot · Clone · Rollback**: Hundred-millisecond checkpoints — roll back to any saved state or fork multiple exploration branches. Powered by CubeCoW Copy-on-Write with incremental dirty-page tracking.

* **🔐 Credential Vault**: Keys never enter the sandbox, model context, or logs. Secrets stay in the control plane.

* **🛡️ Egress Control**: Domain allowlists, instant block on unauthorized egress, full audit logs. CubeEgress provides L7 domain filtering, credential injection, and access auditing.

* **🖥️ Web Console**: Manage sandboxes, templates, nodes, and version matrix in the browser — open `:12088` right after install.

* **📦 Template System**: Turn OCI images into templates in one step, install presets from the Template Store, auto-distribute across nodes.

* **📦 Ready Out of the Box**: No complex dependencies. A minimal deployment script gets a full environment running in minutes.

## CubeSandbox vs Alternatives

| Metric | Docker Container | Traditional VM | CubeSandbox |
|---|---|---|---|
| **Isolation Level** | Low (Shared Kernel Namespaces) | High (Dedicated Kernel) | **Extreme (Dedicated Kernel + eBPF)** |
| **Boot Speed** | 200ms | Seconds | **Sub-60ms** |
| **Memory Overhead** | Low (Shared Kernel) | High (Full OS) | **Ultra-low (<5MB)** |
| **Deployment Density** | High | Low | **Extreme (Thousands per node)** |
| **Agent Hosting** | Manual setup | Manual setup | **✅ Native (AgentHub)** |
| **Auto-Pause / Resume** | / | / | **✅ Platform-managed (100ms resume)** |
| **Snapshot / Clone / Rollback** | / | / | **✅ Sub-second** |
| **E2B SDK Compatible** | / | / | **✅ Drop-in** |

## Next Steps

* [Quick Start](./quickstart.md) — the fastest path from zero to a running sandbox.
* [Sandbox Lifecycle](./lifecycle.md) — state model, auto-pause, auto-resume.
* [Digital Assistant (AgentHub)](./digital-assistant.md) — host and manage AI Agent instances.
* [Self-Build Deployment](./self-build-deploy.md) — single-machine deployment reference.
* [Multi-Node Cluster Deployment](./multi-node-deploy.md) — scale beyond a single machine.
* [Creating Templates from OCI Images](./tutorials/template-from-image.md) — step-by-step template guide.
* [WebUI Console](./webui.md) — visual management right after install.
* [Security Proxy & Credential Vault](./security-proxy.md) — CubeEgress domain filtering and auditing.
