---
title: "Cube v0.5.0: Auto-Pause, ARM Support, One-Click Cluster Deploy — Taking Sandboxes to Production"
date: 2026-07-03
author: Cube Sandbox Team
description: "If v0.3.0 solved 'fast' (millisecond snapshot / clone / rollback), and v0.4.0 solved 'governance' (L7 egress control + observability + cluster consistency), then v0.5.0 aims to solve 'stable, efficient, broad'. With 116 commits from 26 contributors, v0.5.0 brings four core features: AutoPause/AutoResume sandbox lifecycle automation, ARM64 full-stack native support, Tencent Cloud Terraform one-click cluster deployment, and network security enhancements."
featured: true
weight: 1
---

# Cube v0.5.0: Auto-Pause, ARM Support, One-Click Cluster Deploy — Taking Sandboxes to Production

If Cube Sandbox v0.3.0 solved "fast" (millisecond snapshot / clone / rollback), and v0.4.0 solved "governance" (L7 egress control + observability + cluster consistency), then v0.5.0 aims to solve — "stable, efficient, broad."

- **Efficient**: Agent sandboxes spend most of their time "waiting" — for user input, callbacks, the next rollout. v0.5.0 addresses the CPU and memory wasted during this "idle running."
- **Broad**: The AI-era compute foundation is shifting from x86-only to x86 / ARM dual-track. v0.5.0 enables sandboxes to run natively on ARM.
- **Stable**: v0.5.0 bridges the gap from single-machine demos to cloud cluster deployment, providing a production-grade one-click deployment architecture.

## 1. AutoPause / AutoResume: Teaching Sandboxes to "Sleep"

In the Agent world, sandboxes are idle most of the time: waiting for the next user instruction, waiting for long-task callbacks, waiting for the next Agentic RL rollout. In production environments running thousands of sandboxes daily, this means burning server budgets for nothing.

The first core feature of v0.5.0 is automatic pause (AutoPause) and resume (AutoResume) for sandboxes — implemented as a platform-side, per-sandbox capability. On the SDK side, `lifecycle` becomes a standard parameter of `Sandbox.create`, with semantics fully aligned with E2B:

- When a sandbox is idle beyond its timeout, the control plane automatically snapshots its entire runtime state (filesystem + memory) to disk and instantly shuts down the underlying sandbox. At this point, the physical resources it occupied on the host are fully released.
- When a new network request arrives, the data-plane CubeProxy automatically intercepts it, wakes up the sandbox in-place within tens of milliseconds, and seamlessly takes over subsequent traffic.

This state machine switching is entirely orchestrated by the platform. Since pause/resume is currently a single-machine behavior, to prevent single-node resource overload, we introduced a node-level config: `host.quota.paused_resource_release_ratio` (proportional resource release on pause).

- If set to 0, pause does not release scheduling quota;
- If set to 1, all scheduling quota for paused resources is released, maximizing node scheduling density.

When a large batch of sandboxes on a node is simultaneously woken up and the single node cannot handle the load, the control plane returns an error with the specific quota numbers.

Currently, users need to balance availability and high utilization through reasonable configuration. In the future, we will make resume capability break beyond single-machine boundaries, enabling sandbox recovery to freely drift within the cluster, maximizing cluster-wide utilization and making auto pause/resume truly production-ready.

## 2. ARM Native Support: Officially Entering the "Dual-Track Architecture" Era

In v0.5.0, the Arm engineering team and the Cube project team conducted deep joint R&D. From code adaptation, runtime environment, image building to basic functional verification, Cube Sandbox now has the capability to natively deploy, compile, launch, and run typical sandbox workloads on the Arm64 platform. The project status has advanced from early feasibility validation to formal Enablement.

To achieve ultimate results on "Agent-native metrics," the two teams focused on key issues including x86-bound dependencies, hypervisor differences, Multi-Arch images, Guest Kernel boot paths, and build verification flows, for example:

**1. Eliminating x86 bindings and hardcoded assumptions:** Cleaned up legacy x86 architecture assumptions and build dependencies.

**2. Bridging the underlying architecture gap across hypervisors**

- **I/O system architecture transition**: We rewrote the Guest-to-Host control notification channel from **PIO to MMIO**;
- **Boot path reconstruction**: x86 machines boot via lightweight SeaBIOS; for the ARM architecture, we reworked the boot logic to properly use UEFI firmware;
- **Seccomp sandbox alignment**: Rewrote filter rules for the syscall number differences between x86 and ARM64.

This enables Cube Sandbox to deploy, compile, and pass basic functional verification on the Arm platform. More importantly, the two teams shifted optimization targets from traditional CPU benchmarks to the metrics that Agent scenarios truly care about — launch, Snapshot, Rollback, high concurrency, and deployment density — helping users obtain more usable, more accessible, and more production-relevant Arm-native sandbox capabilities.

**For more detailed information about the Cube Sandbox Arm version, please stay tuned for the dedicated report on the Cube official account next week.**

## 3. Tencent Cloud Terraform Cluster Deployment: From "Works" to "Production-Ready"

v0.5.0 brings an officially developed Tencent Cloud Terraform cluster deployer.

All you need is a release bundle — run `create.sh` as the single entry point, and the managed IaC process takes care of everything else. It automatically plans and provisions:

- **Managed control plane**: Spins up cubemaster, cube-api, cubeproxy (replacing the legacy routing layer), and cube-webui on Tencent Cloud TKE (Container Service). To accommodate different business scales and cost expectations, the control plane replicas adopt a more sophisticated HA design:
  - **Default POC mode**: Control plane components default to 1 replica — lightweight with no extra overhead, ideal for quick validation;
  - **cubeproxy stays single-replica by default**: Due to the current lifecycle implementation, multiple replicas would affect the reliability of auto-pause/resume decisions. To ensure absolute stability of the auto-sleep/wake feature, the current version strongly recommends keeping it at 1 replica. Future versions will decouple lifecycle management from cubeproxy, enabling multi-replica HA;
  - **cubemaster with CFS image sharing**: For cubemaster multi-replica disaster recovery, simply enable Tencent Cloud File Storage CFS (shared storage) with `TENCENTCLOUD_USE_CFS=true`. Multiple control plane replicas can then share the same underlying image repository (`/data/CubeMaster/storage`) via ReadWriteMany NFS mounts, elegantly achieving seamless multi-machine failover.

- **Highly available middleware**: The backend automatically provisions cloud database MySQL and cloud cache Redis, persisting metadata and state in managed cloud services — completely eliminating the risks of single-machine container mounts.

- **Elastic compute nodes**: Within a private VPC, you can elastically configure one or more CVM PVM instances or bare-metal instances as sandbox compute nodes.

Additionally, `--mode upgrade` (online upgrade flow) is officially merged into the one-click installer in this version. Upon detecting an existing installation, it automatically performs a three-way `.env` config merge — preserving your customizations while smoothly merging in new default configs, with fail-fast pre-checks for disk space, semver compatibility, and network CIDR conflicts before upgrading.

Starting from v0.5.0, Cube upgrades and cluster scaling become a predictable, reentrant standard operation.

## 4. Network Enhancements: Inbound Sandbox Access Auth, Outbound Policy Routing

Agent sandbox networking must achieve extreme security inbound and extreme control outbound. v0.5.0 delivers two key patches for the data-plane network:

- **Inbound — Per-sandbox traffic access token**: Previously, once a sandbox network was connected, its ingress traffic was essentially "running naked." Now, restricted sandboxes created with `network.allow_public_traffic=false` are assigned a unique `traffic_access_token`. CubeProxy forcefully intercepts any inbound request without this token, returning 403.

- **Outbound — Policy routing support**: Previously, all sandbox egress traffic was forwarded through the host's primary NIC, even if the node had multiple NICs with different routes or tunnel devices. The new version supports policy routing — traffic goes through a virtual NIC `cube-router`, where node administrators can configure policy routing as needed, seamlessly integrating with existing network infrastructure. Typical scenarios include host internal/external network separation and sharing public gateways via tunnels.

## 5. Other Important Updates

In addition to the core features above, v0.5.0 includes several other noteworthy updates:

- **Pure Go native rootfs export**: Rewrote the rootfs export path in native Go (concurrent prefetch + streaming extraction + on-the-fly decompression), reducing **peak memory by 39% and build time by 16%**;
- **Fixed high-concurrency rollback deadlock**: Refactored the snapshot runtime binding model and introduced distributed locks, eliminating the **MySQL 1213 deadlock risk during concurrent rollbacks** at the database level;
- **Preserved original image ownership**: Fixed the historical bug of force-squashing uid/gid to root, **preserving original image user ownership and resolving the permission pain point for non-root images like Chromium that couldn't start**;
- **Inject envs at sandbox creation**: The E2B-compatible SDK now supports injecting envs at sandbox creation time, aligning with the E2B spec.

## Coming Soon...

Next, Cube will evolve toward deeper cloud-native and data decoupling, bringing several important new features:

- **K8s-native deployment and scheduling**: We will deeply support Kubernetes, enabling Agent sandboxes to seamlessly integrate into any standard K8s cluster like ordinary Pods, enjoying the full governance and elasticity of the cloud-native ecosystem.
- **Persistent Volume support**: Decoupling "data" from "sandbox lifecycle." Sandbox instances can be paused or destroyed at any time, but Volumes can persist across sessions and be mounted on demand, giving Agents true "long-term memory."
- **Distributed cross-machine pause/resume**: Breaking single-machine physical boundaries to achieve **"pause on node A, resume on node B"** cross-machine state migration, maximizing cluster resource utilization.
- **Full E2B protocol alignment**: We will continue to fill in the remaining protocol details and enterprise features, ensuring users can seamlessly migrate production workflows from E2B to Cube with a single switch.

And more new features to explore...

From "single-machine isolation" to "elastic clusters" to "seamless migration," we want to truly equip Cube with richer capabilities for the enterprise production environment.

If you're building Agent workflows, Agentic RL training platforms, or developer-facing code execution services, come check out the project on GitHub — file issues, send PRs, or just say hi.

**GitHub repo**: https://github.com/TencentCloud/CubeSandbox

**v0.5.0 full Changelog**: https://github.com/TencentCloud/CubeSandbox/blob/master/docs/changelog/v0.5.0.md
