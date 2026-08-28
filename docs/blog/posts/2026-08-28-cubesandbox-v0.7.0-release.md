---
title: "CubeSandbox v0.7.0: Cross-Machine Sandbox Migration and Smooth Cluster Evolution"
date: 2026-08-28
author: Cube Sandbox Team
description: "In v0.6.0, we brought CubeSandbox into Kubernetes and delivered the E2B-compatible Volume framework. v0.7.0 addresses the two things production users care about most: letting sandboxes flow across machines, and letting upgrades no longer break existing assets."
featured: true
weight: 1
---

# CubeSandbox v0.7.0: Cross-Machine Sandbox Migration and Smooth Cluster Evolution

In v0.6.0, we brought CubeSandbox into Kubernetes and delivered the E2B-compatible Volume framework. The control plane runs as standard workloads, compute nodes are managed as schedulable resources within the cluster, and storage choice is back in users' hands.

But after teams actually get clusters running, they hit one biggest problem: sandboxes can only stay on the machine they started on. Paused sandboxes can't be resumed on a different machine, and snapshots only work on the original node; worse, when components are upgraded, previously built templates and snapshots become incompatible and must be rebuilt. These two issues are what production users care about most.

So in v0.7.0, we formally put these needs on the development agenda: let sandboxes flow across machines, and let upgrades no longer disrupt existing assets.

## 1. Cross-Machine Pause/Resume

Previously, sandbox `pause/resume` and snapshot-based creation were bound to a single host — where you paused, you could only resume. v0.7.0 **uses S3 backend storage to persist sandbox memory and filesystem state to shared object storage, enabling pause on node A and resume on node B, as well as launching new sandboxes from the same snapshot on any synced node.**

For example: a compute node needs to go offline for maintenance, with a batch of paused sandboxes still on it. Previously, you could only wait for them to resume locally, or discard them; now you can migrate those sandboxes to other nodes for resumption — node draining no longer requires sacrificing sandboxes. For Agent training scenarios requiring massive parallelism and frequent pause/resume, the scheduler can place sandboxes on the least loaded machines based on real-time load.

This capability is currently in preview, with built-in MinIO as the default S3 backend. Users can also specify their own object storage.

## 2. Multi-Version Component Coexistence

Cross-machine flow introduces a connected issue: templates and snapshots built on different nodes at different times may depend on different runtime component versions; once components are upgraded, old templates and snapshots become incompatible and must be rebuilt. **v0.7.0 lets compute nodes retain historical component versions, so templates and snapshots no longer become invalid due to component upgrades, and upgrades don't interrupt existing instances' pause/resume.**

Before this, a production cluster wanting to upgrade for a bug fix might need to invalidate all existing templates and snapshots, requiring business downtime for rebuilding. For clusters requiring long-term iteration and continuous upgrades, this v0.7.0 feature represents an important shift from "rebuild everything on each upgrade" to "decouple upgrades from existing assets."

## 3. Network Subsystem Refactor: Accelerating Sandbox Network Creation

Network creation has always been a critical path in the sandbox cold-start chain, especially during batch launches. **v0.7.0 merges the previously standalone NetworkAgent into Cubelet, reducing RPC calls during sandbox creation; optimizes the eBPF network policy dispatch path, significantly reducing creation latency for sandboxes with network rules; and improves Tap device allocation stability under high-concurrency creation.**

When launching a hundred sandboxes simultaneously, each sandbox saves two RPC calls and one RCU wait — these overheads scale proportionally with batch size, reducing cold-start time. After TAP lifecycle refactoring, network anomalies during node restarts and sandbox resume also occur less frequently.

## 4. Control Plane and Operations Architecture Separation

**v0.7.0 migrates node management and other operational capabilities from CubeMaster to CubeOps.** CubeOps now carries complete node management, defaults to dual-replica deployment, and replaces cubemastercli's node subcommand with cubeopscli. Web UI provides node isolation/unisolation and node operation record viewing.

When operations discovers a disk anomaly on a node, they can isolate it in the Web UI — existing sandboxes continue running, while new sandboxes are scheduled elsewhere. This optimization makes component responsibility boundaries clearer: scheduling belongs to CubeMaster, operations belongs to CubeOps.

## Other Notable Features

Beyond the four core features, v0.7.0 implements a batch of feature enhancements and bug fixes.

- **Three-language SDK parity.** Volume CRUD and volumeMounts were previously Python-only; v0.7.0 adds Go and Node SDK support, achieving three-language parity. Template alias coverage was previously incomplete — this version fills in both build-time alias and existing template alias management phases, covering Go / Node / Python. Additionally, Python SDK supports `distribution_scope` for specifying which nodes or regions sandboxes are placed on; Node SDK supports runtime NEVER_TIMEOUT; Go SDK adds user-identity-isolated file views with `Files.ForUser`.

- **Storage block devices and template source extensions.** New SPDK-based CubeS3lvol COW: a remote copy-on-write block device backed by S3 object storage, with local write allocation, async S3 persistence, exported to host via `NVMe-oF/TCP` Loopback, supporting snapshot/clone and cross-node import/export, with local WAL + journal for crash consistency. v0.7.0 includes built-in MinIO as the default S3 volume backend, ready out of the box; private HTTP image registries can also serve as template sources directly, without needing HTTPS for internal registries.

- **Dynamic network policies and configurable forwarding.** Sandboxes support runtime network policy updates without rebuilds; CubeEgress L7 forwarding rules support custom ports, CubeProxy supports custom management ports with new plaintext gRPC ingress, CubeVS introduces same-subnet MAC address learning to avoid black holes in non-hairpin communication. passfd switches to bare-pipe vsock direct connection, improving business process IO efficiency. Kernel and guest image artifacts are now published independently, pulling from fixed kernel-release-\* / guest-image-\* at release time, without recompiling each time.

- **High-frequency issue and stability bug fixes.** v0.7.0 centrally addresses issues frequently seen in production: pause/resume state inconsistency, zombie processes and deletion anomalies (PR [#978](https://github.com/TencentCloud/CubeSandbox/pull/978) / [#985](https://github.com/TencentCloud/CubeSandbox/pull/985) / [#1137](https://github.com/TencentCloud/CubeSandbox/pull/1137) / [#1274](https://github.com/TencentCloud/CubeSandbox/pull/1274)), snapshot performance issues (PR [#1300](https://github.com/TencentCloud/CubeSandbox/pull/1300) / [#1504](https://github.com/TencentCloud/CubeSandbox/pull/1504)), template cache data races during concurrent sandbox creation (PR [#1366](https://github.com/TencentCloud/CubeSandbox/pull/1366)), and TAP device recovery anomalies during node restarts (PR [#930](https://github.com/TencentCloud/CubeSandbox/pull/930) / [#987](https://github.com/TencentCloud/CubeSandbox/pull/987) / [#1207](https://github.com/TencentCloud/CubeSandbox/pull/1207)).

## Coming Soon...

After cross-machine capabilities land, Cube Sandbox will continue digging deeper along the "cloud-native + high availability" direction:

- **Sandbox anomaly recovery**: Automatically detect and recover from VM crashes, shim process hangs, network partitions, with configurable recovery strategies (restart / rollback to snapshot / report error to caller);

- **Scheduling and operations enhancements**: Resource-aware scheduling, affinity/anti-affinity rules, priority classes, and online resource balancing with sandbox-migrating node draining.

In nearer iterations (v0.7.1 or 0.8.0), two items are in progress: 1) **Full-path high availability for the control chain**: template management separated from CubeMaster; CLM (cube-lifecycle-manager) supports multi-replica deployment; 2) **Sandbox anomaly recovery — sandboxes can be recovered across nodes on node failure.**

We welcome PRs and Issues to Cube, and participation in Roadmap design discussions to drive Cube Sandbox's evolution together.

Full Changelog: https://github.com/TencentCloud/CubeSandbox/blob/master/docs/changelog/v0.7.0.md

Cube Sandbox open-source repository: https://github.com/TencentCloud/CubeSandbox
