---
title: "Galxe Alva: Landing CubeSandbox on GKE — Selection and Troubleshooting"
author: RuiPeng Zhang
date: 2026-10-15
tags:
  - agent
  - gke
  - kubernetes
  - security
lang: en-US
---

# Galxe Alva: Landing CubeSandbox on GKE — Selection and Troubleshooting

## Business Context

Alva is a financial analysis service where LLM-driven Agents run Python/Shell, install dependencies, and execute partially untrusted analysis code. Since the business runs on overseas cloud, the full evaluation of CubeSandbox happened on GKE — a dedicated CubeSandbox Node Pool (n2-standard-8, Ubuntu Containerd, nested virtualization, 200GB dedicated disk) added inside the existing sandbox cluster, coexisting with the kata-firecracker platform without migration.

## Key Challenges

- **Fixed overhead for short-lifecycle workloads**: the existing Kata path occupies resources at full-VM granularity; with frequent workspace create/destroy, cold start dominates.
- **Overseas-cloud adaptation cost**: private registry authentication (Workload Identity/ADC vs. Docker keychain) and GKE's network data plane (kube-dns / NodeLocal DNSCache / Dataplane V2) are not out-of-the-box.
- **False-alive template snapshots**: after guest init exits abnormally, the build flow continues and the snapshot shows READY — failure postponed to restore time.
- **Guest DNS failure**: three path layers — kube-dns ClusterIP, NodeLocal DNSCache, in-Pod self-hosted relay — all failed.

## Solution with Cube Sandbox

- **Coexistence-style PoC**: control plane on main nodes, compute plane restricted to the new pool, dedicated label + taint against mis-scheduling; all Terraform changes via PR + Atlantis; images synced to internal Artifact Registry; API/Proxy exposed via source-CIDR-restricted Internal LoadBalancers — no public entrypoints, no long-lived JSON keys.
- **ADC credential fix** ([PR #1544](https://github.com/TencentCloud/CubeSandbox/pull/1544), merged upstream): use ADC only when the registry is \*.pkg.dev with no explicit credentials; explicit credentials take priority; ADC tokens via refreshable token source; failures logged distinctly as "insufficient permissions / ADC unavailable / anonymous pull."
- **Pre-snapshot liveness check** ([PR #1541](https://github.com/TencentCloud/CubeSandbox/pull/1541)): instead of ExitCh/Task.Wait bound to the create RPC context (which produces reverse false positives), CubeShim registers an init-task watcher and Cubelet calls Task.Status with an independent short-timeout context — two Running checks with a stability window before snapshotting.
- **Guest DNS fix** ([PR #1741](https://github.com/TencentCloud/CubeSandbox/pull/1741)): when the effective guest resolver is a private/link-local/loopback IPv4, Cubelet auto-allows a /32 for that resolver even under IP-only policies; reuses CubeVS's existing UDP NAT / DNS learning data plane without changing CubeVS.

## Results and Benefits

- Measured short-task path (pre-warmed template, 5 serial + 2 concurrent): Sandbox.create p50 0.228s, Shell p50 0.103s, Python p50 0.281s, full lifecycle p50 0.779s — all 7 executions succeeded.
- All three fixes contributed back upstream; the ADC and DNS fixes are merged.
- Seven distilled pieces of advice for overseas-cloud adoption: dedicated Node Pool, explicit image-architecture control, Workload Identity from day one, pre-deployment KVM/storage/CIDR verification, never treating Helm Ready as network compatibility, budgeting Cilium-style data plane adaptation separately, and ranking performance benchmarks after networking and security policies.

## References

- Full case study: [Taking the Sandbox Overseas: Selection, Troubleshooting, and Performance Notes from Landing CubeSandbox on GKE](/blog/posts/2026-10-15-alva-gke)
- Cube Sandbox source: [TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
