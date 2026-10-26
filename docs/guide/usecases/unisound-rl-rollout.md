---
title: "unisound: Stress-Testing Density Limits for RL Rollout"
author: Unisound Atlas Intelligent Computing Team
date: 2026-09-01
tags:
  - agent
  - rl
  - rollout
  - density
lang: en-US
---

# unisound: Stress-Testing Density Limits for RL Rollout

## Business Context

The Unisound Atlas Intelligent Computing Team uses Cube Sandbox to support Agent trajectory rollout across two workloads: SWE Agent data synthesis (batch-solving problems in real code repositories to produce training data) and Agent RL environment training (massive parallel episodes interacting with environments). Every trajectory needs a clean, isolated, reproducible execution environment with minute-scale lifecycles at massive parallelism — and the sandbox runs untrusted model-generated code.

## Key Challenges

- **Secure isolation as a hard requirement**: in-sandbox code behavior is unpredictable (infinite loops, memory leaks, fork bombs, anomalous network calls); namespace/Cgroup-level isolation carries escape risks.
- **Throughput over steady state**: a single node must handle tens to over a hundred creation/destruction requests per second.
- **Density limits can't be guessed**: nominal specs landing on real workloads are shaped by resource overselling, runtime peaks, and scheduling parameters.
- **Environment consistency**: the E2B SDK's default `bash -lc` launches a login shell, so environment variables don't carry over between two-phase tasks automatically.

## Solution with Cube Sandbox

- **MicroVM hardware-level isolation**: each sandbox is an independent KVM MicroVM with its own kernel, satisfying untrusted code execution scenarios.
- **Separated control/compute planes**: 1 control node (cube-api / CubeMaster / cube-lifecycle-manager / cube-proxy / coredns / webui / MySQL / Redis) + N lightweight compute nodes (only network-agent + cubelet), registered via `ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR`.
- **Active template distribution**: CubeMaster bakes OCI images into ext4 rootfs and pushes replicas to each compute node, which retains local memory snapshots; sandbox startup uses purely local FICLONE cloning with zero network IO; `/data/cubelet` requires XFS + reflink.
- **Three-step density derivation**: from sandbox specs (1 core / 4096 MiB / 10 GiB writable layer) and scheduling parameters (10 GiB memory reserved, 2× memory oversell, 3× CPU oversell capped at 80%), derive the theoretical ceiling of 117; distinguish the safety boundary (58, no oversell) from the paper ceiling (117); measured steady state lands at 80-100.
- **Direct E2B SDK integration**: training-side scheduling code needs no special adaptation; environment variables are explicitly re-set between two-phase tasks to work around login-shell state not persisting.

## Results and Benefits

- **Three density numbers**: single-node scheduling ceiling 117, no-oversell safety boundary 58, measured steady state 80-100 (160-200 on two nodes), with the bottleneck on the memory side.
- **Identification method for three bottleneck categories**: memory oversell cashed in (host OOM), disk watermark over 80% removing the node from scheduling (error code 130597 while the node still shows HEALTHY), and creation bursts stalling at network-agent tap device allocation (about 20 concurrent creations as the boundary) — the three have different pressure models and must be diagnosed separately.
- **AutoPause lever evaluation**: raising `paused_resource_release_ratio` to 0.7-0.8 could theoretically reach 3-4× paper density (350-470 per node), but resume becomes best-effort (409 on transient resource shortage); kept at the default 0.0 until training-side fault tolerance is ready — the parameter supports hot reload for later re-evaluation.

## References

- Full case study: [Unisound Engineering Practice: Stress-Testing CubeSandbox Density Limits for RL Rollout](/blog/posts/2026-09-01-unisound-rl-rollout)
- Cube Sandbox source: [TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
