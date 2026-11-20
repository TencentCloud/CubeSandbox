---
title: "AnyDev: Gateway, Network, Storage, and Blue-Green Upgrades for a Multi-Tenant Sandbox Platform"
author: Liao Xiyou, Chang Zhenhua
date: 2026-11-20
tags:
  - agent
  - multi-tenant
  - gateway
  - blue-green-deployment
lang: en-US
---

# AnyDev: Gateway, Network, Storage, and Blue-Green Upgrades for a Multi-Tenant Sandbox Platform

## Business Background

AnyDev is a development platform serving multiple internal Tencent business lines, using CubeSandbox to provide isolated execution environments for agent applications. Once sandboxes joined a multi-tenant platform, the team had to handle tenant isolation, approval of management operations, enterprise network and storage integration, and protection of running tasks during foundation upgrades. Their principle: do not modify the sandbox foundation — build enterprise-side capabilities around it, split across a front gateway, a self-service portal, and data consumers.

## Pain Points

- **Tenant boundaries cannot be guaranteed by the foundation**: forwarding a list API without tenant scoping is a privilege escalation, and template operations affect every tenant sharing the foundation.
- **Private subnet conflicts**: Cube denies 10/8, 172.16/12, 192.168/16 and 169.254/16 by default, while enterprise intranets, VPCs and container networks almost all fall inside those ranges.
- **Upgrade risk lands on existing sandboxes**: during the 0.6 → 0.7 upgrade, control-plane component splits and the template/snapshot compatibility baseline broke; rolling in place would kill business teams' long-running tasks.

## Solution on CubeSandbox

- **Four things the front gateway does**: identity translation (one E2B key per tenant, so the foundation's key-based rate limiting naturally lands at tenant level), API isolation (method+path whitelist, blocking interfaces without tenant semantics, new APIs blocked by default), quota pre-checks (template whitelists require parsing the request body), and accounting with ownership checks (tenant/project written into sandbox metadata).
- **Management actions funneled through the self-service portal**: template creation, quota changes, egress whitelists and "never timeout" requests are all approved there, with tenant scoping and audit trails on the platform side.
- **Bidirectional traffic convergence**: inbound goes uniformly through the enterprise gateway (domain mode `<port>-<sandboxID>.<domain>` or path mode), outbound through the enterprise egress gateway; credential injection rewrites auth headers so secrets never land in the sandbox; the deployment guide requires the VPC/subnet to avoid enterprise ranges, with intranet access allowed item by item.
- **Storage**: template governance funneled through the portal with aliases as stable references; Volumes built on a self-developed plugin over enterprise shared storage, beating object-storage plugins on performance and the permission system.
- **Blue-green upgrades**: existing workloads stay on blue, new ones go green, with the gateway controlling creation routing per tenant and the ledger recording each sandbox's cluster; rollback is a seconds-level routing change, and compatibility issues only affect new sandboxes cut over to green.

## Results

- The foundation keeps its simple "only knows the Key" model while tenant-level visibility is carried by the outer layer; switching clusters or shapes is invisible to business sides.
- The 0.6 → 0.7 upgrade completed blue-green with zero interruption to existing sandboxes and seconds-level rollback cost.
- Converged egress IPs made IP-based allowlisting feasible, and egress logs are attributable to tenants.
- Five reusable conclusions covering management-plane construction, "block + funnel", subnet planning, Volume plugin selection, and blue-green upgrades.

## References

- Full case article: [Four Platform Capabilities Around a Multi-Tenant Sandbox: Gateway, Network, Storage, and Blue-Green Upgrades](/blog/posts/2026-11-20-anydev)
- Cube Sandbox source: [TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
