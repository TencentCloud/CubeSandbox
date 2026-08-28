---
title: "Cube Sandbox v0.6.0: K8s Deployment, Volume Framework Lead Six Capabilities Toward Production"
date: 2026-07-24
author: Cube Sandbox Team
description: "Today (July 24), Cube Sandbox v0.6.0 is officially released. This version merges 92 commits from 31 contributors, bringing six core features. Among them, K8s and Volume support are the two most requested by the community and the biggest highlights of this release."
featured: false
---

# Cube Sandbox v0.6.0: K8s Deployment, Volume Framework Lead Six Capabilities Toward Production

Today (July 24), Cube Sandbox v0.6.0 is officially released.

If previous versions were about giving Cube solid foundational capabilities, v0.6.0 goes deeper into stability, fault tolerance, operations, and observability, while introducing more flexible extensibility and storage support. This version merges 92 commits from 31 contributors, bringing six core features. **Among them, K8s and Volume support are the two most requested by the community and the biggest highlights of this release.**

## K8s Deployment: Control Plane and Compute Nodes Both Supported

Starting from this version, both Cube's control plane components and compute nodes can be deployed directly to K8s clusters via Helm Chart — whether on Tencent Cloud TKE, standard K8s, or lightweight k3s.

From a technical standpoint, the value here is handing Cube's lifecycle back to K8s's control loop. In the past, bringing Cube to production often meant maintaining a separate deployment, upgrade, and scaling workflow outside the cluster — Cube was like a "bolt-on system" that needed special attention. Now, control plane components run as standard workloads, and compute nodes are incorporated into cluster management as schedulable, orchestrable resources. Rolling updates, canary releases, horizontal autoscaling — all native K8s capabilities — work on Cube as-is. Cube goes from being a "special object" to an ordinary member of the cluster.

It should be noted that K8s deployment is currently in preview, and advanced capabilities like smooth upgrades are still being refined. The direction is clear: make Cube truly K8s-native. We welcome you to try it out and feed back any rough edges you encounter.

## Volume Framework: Giving the Choice of Storage Back to Users

How sandboxes persist and share data is a concern for many teams. Starting from v0.6.0, an **E2B-compatible Volume framework** has been officially introduced.

Its design core is **customizability**. The framework itself is not bound to any specific storage implementation. Instead, it abstracts four HOOK points — Create, Destroy, Attach, and Detach — through which all sandbox-storage interactions are channeled. To plug in your own backend storage, you simply implement these hooks — no changes to the Cube core needed. Two plugin forms are available: a standalone executable (simpler to deploy, better isolation) or an RPC call (better suited for complex or long-running storage services). E2B compatibility means workloads written for the E2B Volume protocol can migrate smoothly to a self-hosted Cube cluster.

This version already supports Volume lifecycle management, sandbox-Volume binding APIs, and the accompanying SDK and CLI capabilities. In other words, the initiative for "what storage the sandbox uses" is now back in your hands.

## Template Aliases: Giving Templates a Memorable Name

Previously, launching a sandbox required specifying a long template identifier — error-prone and hard to remember. Now, you can set an alias when creating a template, and use that alias to create sandboxes directly — intuitive and less error-prone. The Python SDK already supports creating sandboxes by alias; other SDKs will follow.

## Configurable Inbound Request Host: Better Compatibility for In-Sandbox Services

Some applications rely on the request's Host header for routing, validation, or callbacks, and the default forwarded Host may not meet their needs. This version allows specifying the Host to forward to in-sandbox services at sandbox creation time, so these "Host-picky" services can also run smoothly on Cube — better compatibility, one step further.

## Compute Node Isolation: Peace of Mind for Operations

Operations inevitably require logging into nodes for maintenance, upgrades, or troubleshooting. This version **supports "isolating" a compute node at the scheduling layer** — once isolated, the scheduler stops assigning new sandboxes to that node, while existing sandboxes on the node continue running unaffected. Simply remove the isolation when maintenance is done. This makes operational actions both safe and controllable — no more hesitation over "will this affect production?"

## CubeOps as a Standalone Service: Making CubeAPI Lighter and More Focused

We've spun out the web console and operations-related logic from the CubeAPI module into a standalone CubeOps service. This decoupling lets CubeAPI return to a purer role with clearer responsibilities and easier extensibility, while the operations and console capabilities gain room to evolve independently. This is also an important step in Cube's move toward a modular, composable architecture.

## Other Updates and Fixes

Beyond the six core features, v0.6.0 includes a batch of stability and usability improvements:

- Paused-state sandboxes can now be deleted
- New PostgreSQL metadata backend
- In-sandbox cgroup upgraded from v1 to v2
- Cubelet state storage supports dynamic expansion
- Fixed interactive terminal, authentication, and protocol frame issues across multiple SDKs
- Network and security proxy aligned with E2B error code semantics and enhanced TLS compatibility
- Fixed CVE vulnerabilities and updated the PVM kernel
- Accompanying Kubernetes deployment, node isolation, Volume plugin, and other docs are now live in both Chinese and English…

## Coming Soon…

v0.6.0 is a step toward "production-ready," and Cube's direction has always been clear — cloud-native, E2B-compatible, highly available, stateful. Along these four themes, v0.7.0 and subsequent versions are in progress:

- **Making K8s deployment more "native"**: Moving from Helm deployment to CRD- and Operator-based native management, and filling in smooth upgrade capabilities.
- **Cross-machine pause and resume**: Cluster-wide cross-node pause and resume — pausing a sandbox on one host, migrating it to another, and fully restoring it, preserving memory and filesystem state, to improve cluster-wide resource utilization.
- **End-to-end high availability**: Separation of execution and operations flows, HA deployment for all control plane components, and sandbox recovery on compute node failure.
- **Continued E2B alignment**: Closing remaining API gaps, providing E2B-compatible metrics endpoints, so E2B-targeted workloads can be adopted with zero changes.
- **Performance and operations, further**: Optimizing business process IO efficiency, providing one-click performance testing tools, and continuously filling in the operational and observability capabilities needed for real production deployments.

If you're looking for a high-performance, easy-to-operate, E2B-compatible sandbox solution, give v0.6.0 a try now. Whether it works smoothly, where it gets stuck, what's still missing — these real voices tell us more about the next step than any roadmap ever could.

See you on GitHub: https://github.com/TencentCloud/CubeSandbox

v0.6.0 full Changelog: https://github.com/TencentCloud/CubeSandbox/blob/master/docs/changelog/v0.6.0.md
