---
title: Introduction
description: CubeSandbox is an open-source, hardware-isolated MicroVM sandbox infrastructure built for AI Agents with RustVMM + KVM — offering sub-60ms cold starts, drop-in E2B SDK compatibility, and PVM, bare-metal, and Kubernetes deployments.
---

# Introduction

CubeSandbox is an **open-source sandbox infrastructure purpose-built for AI Agents** — a production-hardened runtime that executes untrusted code, hosts long-running Agent processes, and runs stateful services. Built on RustVMM + KVM, each sandbox runs inside a hardware-isolated MicroVM with its own dedicated kernel, booting in under 60ms with less than 5MB of additional VMM memory overhead, and natively supporting both `x86_64` and `ARM64`. The core system was validated at scale in Tencent Cloud production environments before open-sourcing.

## Key Advantages

### Instant Boot & Hardware Isolation

* **⚡ Sub-60ms Cold Start · High Density**: Powered by pre-warmed snapshot pooling and CubeCoW copy-on-write, single-concurrency cold starts average **41ms**, while 50 concurrent creations average **92.4ms** (P95 **121.9ms**) at **512 sandboxes/s** throughput on bare metal ([CubeBench v0.7.3](../blog/posts/2026-10-28-cubebench-073.md)). Per-instance VMM overhead is **< 5MB** (~18–25 MiB amortized host memory per idle VM), enabling thousands of instances per node. See [Performance Benchmark](./performance-benchmark.md).

* **🔒 Dedicated-Kernel Hardware Isolation**: Every sandbox gets its own Guest OS kernel — eliminating shared-kernel container escape surfaces. KVM + eBPF enforce hardware and kernel-level boundaries so you can run untrusted LLM-generated code safely.

* **⚙️ Native x86_64 & ARM64 Pipeline**: Full-stack native support across `x86_64` and `aarch64 (ARM64)` — from the hypervisor and guest kernel to base images and automated deployment.

### Agent-Native Lifecycle & State Primitives

* **🤖 Agent Hosting, Not Just Code Execution**: The same lifecycle primitives cover one-shot code execution, full stateful service stacks (Redis, MySQL, browsers, desktops, dev environments), and long-running Agent processes (including hosting OpenClaw assistants via [AgentHub (Preview)](./digital-assistant.md)).

* **⏸️ Auto-Pause & Hundred-Millisecond Resume**: Idle sandboxes are automatically snapshot-suspended to release compute resources and transparently restored on the next request — hundred-millisecond wake-up with zero caller awareness — plus configurable [auto-restart policies](./lifecycle.md) when in-sandbox services crash.

* **📸 Snapshot · Clone · Rollback**: Hundred-millisecond checkpoints powered by CubeCoW and incremental dirty-page tracking let you roll back to any saved state, fork parallel exploration branches, or take lower-cost filesystem-only (FS-only) snapshots. See [Snapshot, Rollback & Clone](./snapshot-rollback-clone.md).

* **🌐 Cross-Node Pause & Resume (Preview)**: With the [JuiceFS](./cross-node-snapshot/juicefs.md) or [S3](./cross-node-snapshot/cubes3lvol.md) shared snapshot backend, paused sandboxes can be resumed — and new sandboxes created from snapshots — on any compatible node in the cluster, so node maintenance or failure no longer strands workloads. See [Cross-Node Migration](./cross-node-snapshot/requirements.md).

### Zero-Trust Networking, Storage & E2B Compatibility

* **🔌 Seamless E2B Migration & Official SDKs**: Native E2B SDK compatibility (covering up to E2B 2.51.0+; see [E2B Compatibility](./e2b-compatibility.md)) — swap an environment variable with zero business code changes. Official [Python](./tutorials/sdk/python.md), [Go](./tutorials/sdk/go.md), and [Node.js](./tutorials/sdk/nodejs.md) SDKs are also included.

* **🛡️ Credential Vault & L7 Egress Control**: Secrets stay in the control plane and never enter sandboxes, model context, or logs. [CubeEgress](./security-proxy.md) enforces L7 domain allowlists, blocks unauthorized egress immediately, injects credentials dynamically, and records full audit logs, complementing [Network Policy](./network-policy.md) and [Sandbox-to-Sandbox Access](./sandbox-to-sandbox.md).

* **💾 Decoupled Volume Storage Framework**: An E2B-compatible [Volume framework](./volume.md) with [pluggable storage drivers](./volume-plugin.md) (S3, COS, JuiceFS, NFS, and custom backends) decouples storage lifecycles from sandboxes and supports multi-sandbox data sharing.

### Simple Deployment & Production Operations

* **🚀 Ready Out of the Box**: A minimal one-click script brings up a complete environment in minutes. Deploy on standard cloud VMs without `/dev/kvm` via [PVM](./pvm-deploy.md), or run on [bare metal](./bare-metal-deploy.md), [multi-node clusters](./multi-node-deploy.md), [Kubernetes (Helm)](./kubernetes/), [hybrid deployments (K8s control plane + bare-metal/VM data plane)](./hybrid-deploy.md), and [Tencent Cloud (Terraform)](./tencentcloud-terraform-deploy.md).

* **📦 Template System**: Convert [OCI images into templates](./tutorials/template-from-image.md) in one step, use template aliases, commit running sandboxes as templates, install presets from the Template Store, and auto-distribute across nodes. See [Templates Overview](./templates.md).

* **🖥️ Web Console & Production Observability**: Built-in [WebUI Dashboard](./webui.md) (open `:12088` right after install) to manage sandboxes, templates, nodes, and version matrices visually — alongside turnkey [Prometheus / Grafana metrics, logs, and tracing](./observability.md), [extensible scheduler plugins](./scheduler-plugin.md), and [per-template QoS](./qos.md).

## Drop-In E2B Compatibility in 5 Lines

Existing E2B workloads require zero application code changes — point the SDK to your CubeSandbox endpoint to switch to self-hosted, hardware-isolated MicroVMs:

```bash
export E2B_API_URL="http://127.0.0.1:3000"
export E2B_API_KEY="e2b_000000"
```

```python
from e2b_code_interpreter import Sandbox  # Use the E2B SDK directly

with Sandbox.create(template="<your-template-id>") as sandbox:
    result = sandbox.run_code("print('Hello from CubeSandbox!')")
    print(result)
```

See [Quick Start](./quickstart.md) for the full end-to-end walkthrough and [E2B Compatibility](./e2b-compatibility.md) for API coverage details.

## Typical Use Cases

* **Code Interpreters & Coding Agents**: Run untrusted generated code and shell workflows safely with integrations for [Claude Code](./integrations/claude-code.md), [LangGraph](./integrations/langgraph.md), [OpenAI Agents SDK](./integrations/openai-agents-sdk.md), [Google ADK](./integrations/google-adk.md), and more (see [Framework & Tool Integrations](./integrations/)).
* **Browser & Desktop Automation (Computer Use)**: Run headless Chromium (Playwright) or a full [Ubuntu Desktop](./integrations/ubuntu-desktop.md) inside isolated MicroVMs with strict per-session boundaries.
* **Persistent Stateful Agents & Cloud Workspaces**: Combine auto-pause/resume, decoupled Volumes, and L7 egress proxy to host long-lived dev environments, databases, and dedicated Agent instances.
* **High-Concurrency Evaluation & RL Rollout**: Leverage sub-60ms boot, high node density, and fast snapshot clone/rollback for SWE-bench evaluation and reinforcement learning rollouts.

Explore real-world production deployments in [Case Studies](./usecases/) and runnable demos in [Example Projects](./tutorials/examples.md).

## CubeSandbox vs Alternatives

| Metric | Docker Container | Traditional VM | CubeSandbox |
| --- | --- | --- | --- |
| **Isolation Level** | Low (Shared Kernel Namespaces) | High (Dedicated Kernel) | **Hardware-Isolated (Dedicated Kernel + eBPF)** |
| **Boot Speed** | ~200ms | Seconds to minutes | **Sub-60ms (41ms avg at c=1)** |
| **Memory Overhead** | Low (Shared Kernel) | High (Full Guest OS) | **Ultra-low (< 5MB VMM, CoW page sharing)** |
| **Single-Node Density** | High | Low | **High (Thousands per node)** |
| **Auto-Pause / Resume** | Custom orchestration | Manual suspend, slow wake | **✅ Platform-managed (100ms-level resume)** |
| **Snapshot / Clone / Rollback** | Filesystem-only, no RAM state | Supported (seconds to minutes) | **✅ Hundred-millisecond (Full-state / FS-only)** |
| **Cross-Node Pause / Resume** | Custom migration | Needs shared storage & live migration | **✅ JuiceFS / S3 backend (Preview)** |
| **E2B SDK Compatible** | No | No | **✅ Drop-in (zero code changes)** |
| **Agent Hosting** | Manual setup | Manual setup | **✅ Native (AgentHub, Preview)** |

## Next Steps

### Get Started

* [Quick Start](./quickstart.md) — go from zero to a running isolated sandbox in four steps.
* [E2B Compatibility](./e2b-compatibility.md) — version-by-version E2B SDK compatibility matrix and CubeSandbox extensions.
* [Example Projects](./tutorials/examples.md) / [Framework & Tool Integrations](./integrations/) — end-to-end demos for code execution, browsers, LangGraph, Claude Code, and more.

### Choose a Deployment Option

* [PVM Deployment](./pvm-deploy.md) — deploy on standard cloud servers without `/dev/kvm`.
* [Bare-Metal Deployment](./bare-metal-deploy.md) / [Self-Build Deployment](./self-build-deploy.md) — deploy on native KVM hosts or build from source.
* [Multi-Node Cluster Deployment](./multi-node-deploy.md) — scale out to a multi-node production cluster.
* [Kubernetes Deployment](./kubernetes/) / [Hybrid Deployment](./hybrid-deploy.md) / [Tencent Cloud Cluster (Terraform)](./tencentcloud-terraform-deploy.md) — automated Helm installation, hybrid K8s control plane + bare-metal/VM data plane, or Terraform provisioning.

### Dive Deeper & Operate

* [Architecture Overview](../architecture/overview.md) — control plane, data plane, and core component interactions.
* [Sandbox Lifecycle](./lifecycle.md) / [Snapshot, Rollback & Clone](./snapshot-rollback-clone.md) / [Cross-Node Migration](./cross-node-snapshot/requirements.md) — master core state primitives.
* [Creating Templates from OCI Images](./tutorials/template-from-image.md) / [Using Volumes](./volume.md) — customize runtime images and persistent storage.
* [Egress Proxy (Filtering & Secrets)](./security-proxy.md) / [WebUI Dashboard](./webui.md) / [Sandbox Observability Deployment](./observability.md) — security, visual operations, and monitoring.
* [Digital Assistant (AgentHub)](./digital-assistant.md) / [Case Studies](./usecases/) — hosted AI Agent instances and production stories.
