---
title: "Guangdong Rising: Building a Multi-Tenant Sandbox Platform"
author: Feng Jiaqi
date: 2026-09-03
tags:
  - agent
  - multi-tenant
  - sandbox-platform
  - lifecycle
lang: en-US
---

# Guangdong Rising: Building a Multi-Tenant Sandbox Platform

## Business Context

Guangdong Rising runs AI-Agent-generated code on two product lines (SIN Builder, an AI application generation platform, and Dasheng AI, an enterprise-grade AI assistant). The team consolidated the sandbox capabilities of both product lines into a standalone service, common-sandbox-runner, as the sandbox execution layer of the Guangdong Rising SIN PaaS (AI platform), building multi-tenant sandbox governance on top of Cube Sandbox.

## Key Challenges

- **Two incompatible implementations**: each product line had its own sandbox capability with inconsistent lifecycles, leases, and reclamation policies; one leakage incident took both product lines down at the same time.
- **Stateful reuse for long sessions**: Agent sessions are long-lived and stateful — the same session must reuse the same sandbox, and a late cleanup request could kill a sandbox being used by a new-generation lease.
- **Quota vs. reclamation dilemma**: under multi-replica deployment, neither reclaiming a running sandbox nor leaking sandboxes left by crashed replicas is acceptable.
- **Cold-start latency**: sandbox cold start translates directly into user wait time; a self-built warm pool carries high maintenance and consistency costs.

## Solution with Cube Sandbox

- **Unified sandbox execution layer**: the standalone FastAPI service common-sandbox-runner is the only side that writes to Cube; product lines call it only through `POST /v1/sandbox-runs` (SSE streaming events). All cross-request state — leases, admission counts, GC candidates — lives in Redis.
- **Unified ownership accounting**: an `owner_id` derived from namespace + env + project ID + session run ID is written into sandbox metadata, so every sandbox traces back to a concrete product line, environment, and session; a dual-template design lets callers pick images by task resource footprint.
- **Leases + fencing tokens**: `POST /v1/leases` reuses sandboxes by `owner_id`; `POST /v1/lease-releases` kills a sandbox only when both `owner_id` and `lease_id` (epoch) match — late cleanup requests only seal off expired generations and never delete the current lease.
- **Admission control**: Redis Lua scripts acquire admission slots atomically (bucket-level quotas + global cap), waiting up to 30 seconds before returning a retryable CAPACITY_EXCEEDED.
- **Tri-color mark-and-sweep reclamation**: borrowing from JVM/Go — black (roots with fresh heartbeats, never reclaimed), gray (within grace period, deletion forbidden), white (no root reference for two consecutive confirmation cycles with unchanged generation, actually swept); combined with Cube's official `on_timeout=pause` + `auto_resume` lifecycle, idle leases are soft-reserved rather than killed outright.

## Results and Benefits

- **Startup latency**: hot-start from baked templates measured at 180 ms median / 210 ms P95, cold start consistently within one second; the self-built warm pool was retired entirely.
- **Architectural simplification**: about 1,350 lines of code removed net (+193 / -1,542); two product lines' sandbox logic converged into 1 shared service, guarded by 198 test cases.
- **Capacity ceiling**: the admission logic limit was relaxed from 200 to 500 sandboxes, breaking the quota deadlock caused by zombie leases — with no additional machine resources.

## References

- Full case study: [Who Has the Right to Delete a Sandbox: Building a Multi-Tenant Sandbox Platform on CubeSandbox](/blog/posts/2026-09-03-guangdong-rising)
- Cube Sandbox source: [TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
