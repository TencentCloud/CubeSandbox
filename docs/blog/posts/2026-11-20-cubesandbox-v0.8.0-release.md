---
title: "CubeSandbox v0.8.0 Release: Evolved Cross-Machine Flow, Customizable Scheduling, Observable Runtime"
date: 2026-11-20
author: Cube Sandbox Team
description: "Cross-machine pause/resume depends on the S3vol backend, scheduling policies cannot be tailored per business, metrics and logs are scattered across nodes, and a crashed in-sandbox service can only be restarted by the platform above — v0.8.0 responds to each of these directions: a new JuiceFS storage backend, pluggable scheduling policies, unified collection of runtime states, crash recovery handed back to the sandbox itself, plus new sandbox-to-sandbox communication and stronger E2B compatibility."
featured: true
weight: 1
---

# CubeSandbox v0.8.0 Release: Evolved Cross-Machine Flow, Customizable Scheduling, Observable Runtime

In v0.7.0, we let sandboxes flow across machines: with the S3vol backend, a sandbox can pause on node A and resume on node B, and coexistence of multiple component versions means upgrades no longer interrupt existing assets.

But as the cluster keeps growing and moves closer to real production, deeper problems surface. For example, cross-machine pause/resume still depends on the S3vol backend, and its performance and stability have room to improve; scheduling is a built-in implementation, so packing rate and fragmentation cannot be tailored to a business, and there is no way to validate policy changes before they ship; once sandboxes run, their metrics and logs are scattered across nodes, and troubleshooting means logging into each host one by one; when a service inside a sandbox crashes, only the upper platform can do the heartbeat and restart it……

In v0.8.0, released today, CubeSandbox responds to each of these directions: **the storage backend gains JuiceFS, scheduling policies become pluggable, runtime states can be collected uniformly, and crash recovery is handed back to the sandbox infrastructure itself; sandbox-to-sandbox communication is completed, and E2B compatibility is further strengthened.**This release contains 128 PRs from 46 contributors, including 8 core features plus a large number of enhancements and bug fixes.

#### 1. JuiceFS storage backend (preview)

Previously, template distribution relied on CubeMaster pushing artifacts to each node, and cross-machine restore and snapshot-based cold start depended on the S3vol backend. v0.8.0 adds a JuiceFS-based storage backend, which keeps cross-machine pause/resume and snapshot-based cold start while bringing two improvements: first, **templates can be shared across nodes** instead of being distributed node by node; **second, cross-machine pause/resume becomes faster and more stable, and the solution is more mature.**The metadata engine supports Redis, MySQL, TiKV and other options; object storage supports MinIO, Tencent Cloud COS and more.

Cross-node template sharing means artifacts no longer need to be pushed to every node; teams that already run JuiceFS can reuse their existing infrastructure. This capability is currently in preview.

#### 2. Extensible scheduling plugin system and scheduling quality evaluation framework

As clusters grow, scheduling policies often need to be tailored to business characteristics. **v0.8.0 supports three ways to customize sandbox scheduling policies: built-in plugins, CEL expressions, and external gRPC plugins.**The first two suit relatively simple node-selection rules; the external plugin hands the whole scheduling decision to an external service, so cluster topology, tenant profiles and similar information can all participate in the decision, and cluster scheduling behavior can therefore be adjusted flexibly and dynamically.

The new scheduling quality evaluation framework addresses another problem: how to validate the effect of a policy change before it goes live. By simulating cluster topology and workloads, the framework measures packing rate, fragmentation, herding and scheduling latency of a policy at development time, improving the efficiency of policy development.

#### 3. Sandbox metrics and log collection

After sandboxes enter production, the lack of unified metrics and logs is the main obstacle to troubleshooting. v0.8.0 supports collecting sandbox resource metrics and logs, and ships default dashboard configurations that work out of the box in Prometheus and Grafana; it also connects flexibly to users' existing monitoring and logging systems, providing unified runtime monitoring and troubleshooting for production.

#### 4. Sandbox restart policy

Until now, when a service inside a sandbox failed, the sandbox instance itself kept running, and the upper platform had to implement its own failure detection and restart logic. v0.8.0 supports a restart policy for sandboxes: when the in-sandbox service fails, the sandbox is automatically restarted on the same node, handing this step back to the sandbox infrastructure itself.

#### 5. FS-only snapshots

A full snapshot stores both memory state and the file system, but many scenarios only need the file system: for example, persisting a configured environment and then cold-starting batches of new sandboxes from it, where memory state is unnecessary. v0.8.0 supports **snapshotting only the sandbox file system (FS-only), reducing snapshot storage cost**; users can cold-start new sandboxes from FS-only snapshots.

#### 6. Sandbox-to-sandbox access via port mapping

In multi-agent collaboration scenarios, sandboxes often need to talk to each other directly. v0.8.0 lets sandboxes reach each other through **host port mapping (Port-Mapping)**: this is TCP layer-4 communication, and **any TCP-based service can be exposed this way**, no longer limited to the HTTP/HTTPS/gRPC supported by CubeProxy. Sandboxes on the same host support this communication through hairpin as well.

From now on, one sandbox can reach another in two ways: use CubeProxy when layer-7 forwarding is needed, and use port mapping when any TCP service needs to be carried. Both support same-node and cross-node sandbox-to-sandbox access.

#### 7. Volume framework enhancement: passing user private data

When sandboxes interact with external storage, business logic such as authentication and bucket selection often needs user identity information. The v0.8.0 Volume framework supports passing user private data, **which can be used to implement auth, bucket selection and similar business logic**.

#### 8. Further E2B SDK compatibility

v0.8.0 adds support for the E2B /v2/sandboxes and /v2/sandboxes/{id}/connect endpoints. Users of E2B SDK 2.51.0 or above can create and connect to sandboxes directly without building an adapter layer. In addition, the Python / Node / Go SDKs gain the E2B-compatible Sandbox.fork and Sandbox.is_running capabilities.

For compatibility details, see: <https://cubesandbox.com/guide/e2b-compatibility>

#### Other features worth mentioning

- **Template-level sandbox QoS**. Sandboxes created from a specific template can be capped on network bandwidth, packet rate, disk throughput and IOPS, preventing a single sandbox from consuming all host resources and affecting other sandboxes on the same machine.

- **Node oversubscription configuration**. Supports both CLI and Web; a new node-side resident process, ops-agent, receives configuration issued by CubeOps and modifies the Cubelet configuration file.

- **Continued performance improvements**. vsock enables VIRTIO_RING_F_EVENT_IDX to cut unnecessary guest-side interrupts, raising bidirectional throughput from about 5.7 Gbps to about 9.3 Gbps; fixed high concurrent-creation latency caused by reading cgroups for resource collection on CPUs such as AMD; reduced the number of ioctls in ARM snapshot restore; guest kernel logs are redirected to virtio-console to lower cold-start overhead.

- **Deployment and operations**. K8s deployment supports private CA registries — configure trustedCACerts in Helm and images pull normally; the control plane natively supports Redis Cluster; DNS resolution moves to the control plane, so sandboxes can use K8s cluster DNS directly; compute-node components start faster.

- **Templates and storage.**Snapshot chain optimizations: same-node derived snapshots share the parent snapshot, the write path is optimized via shared memory, and snapshots are warmed up automatically when used; templates can specify which user (UID or username) sandboxes run as; artifact distribution concurrency is configurable.

- **Fixes for high-frequency and stability issues**. v0.8.0 collectively fixes more than 40 issues that occur frequently in production: panic caused by excessive sandbox logs, poisoned locks when volume activation and deletion race, occasional panic when gRPC connection pools close concurrently, shim leaks on creation failure, auto-pause failure with multiple CLM instances, and more (see the full Changelog for details).

#### Coming Soon……

After v0.8.0, Cube plans to release v1.0.0 (GA) within this year. The rough roadmap directions are:

- Feature completeness: fully E2B-compatible API with consistent multi-language SDK capabilities;
- Stability: production-grade high availability, automatic sandbox crash recovery, HA across all components;
- Observability: open integration of logs, metrics and events, with key paths observable end to end;
- Performance: continued speedups for core operations, cross-machine capability approaching local performance.

Stay tuned! Welcome to try or upgrade to the new v0.8.0, and welcome to file PRs and Issues for Cube and join the Roadmap discussions, pushing Cube Sandbox forward together.

Full Changelog: [CubeSandbox/docs/zh/changelog/v0.8.0.md at master · TencentCloud/CubeSandbox · GitHub](https://github.com/TencentCloud/CubeSandbox/blob/master/docs/zh/changelog/v0.8.0.md)

Cube Sandbox open-source repository: [GitHub - TencentCloud/CubeSandbox: Instant, Concurrent, Secure & Lightweight Sandbox for AI Agents.](https://github.com/TencentCloud/CubeSandbox)
