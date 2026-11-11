---
title: "OMA: Architecture and Secure Execution Plane of a Cloud Managed Agents Platform"
author: Master Zhang's Blog
date: 2026-11-06
tags:
  - agent
  - managed-agents
  - security
  - architecture
lang: en-US
---

# OMA: Architecture and Secure Execution Plane of a Cloud Managed Agents Platform

## Business Context

OMA is a self-hosted cloud managed-agents platform. Referencing Anthropic's Claude Managed Agents design, it provides session management, inference/execution decoupling, credential custody, and filesystem capabilities on the enterprise's own infrastructure, letting business teams define and use Agents through APIs. The cloud sandbox uses Tencent's open-source CubeSandbox; local development uses the self-developed e2b-local (an E2B-compatible gateway in Go, running on Docker / OrbStack VMs / Apple Container).

## Key Challenges

- **The gap from model API to production**: context management, tool execution, session persistence, credential management, sandbox isolation, and observability all have to be built by application teams.
- **The shortcomings of all-in-sandbox deployment**: putting the runtime framework and execution environment together in the sandbox means the first response waits for sandbox + Agent process startup, and sandbox failures can take down the inference process and session state too.
- **Credential exfiltration risk**: if generated code can read long-lived credentials, copying or exfiltration becomes possible.
- **NAS/ossfs don't fit sandbox file needs**: quota bottlenecks (max 1,000 mount nodes per filesystem) and the "which files should this task see" view problem.
- **Reaching private-network tools**: cloud Agents calling internal MCP Servers are blocked by enterprise firewalls.

## Solution with Cube Sandbox

- **Brain-hand separation**: the Brain (an agent loop implemented with DeepSeek Harness) runs outside the sandbox, exchanging inputs/outputs/approvals/session state with the platform over the CCRv2 protocol; the sandbox carries only tool execution (via an internal Rust MCP Server). Sandboxes prepare in parallel or are created on demand — Anthropic's report credits this design with ~60% lower p50 TTFT and over 90% lower p95.
- **Sandbox adaptation layer**: CubeSandbox in the cloud (PVM supports adapted cloud servers, eBPF networking, lightweight implementation), e2b-local locally — the same interface reuses call logic; an independent adaptation layer keeps evaluating underlying options.
- **Credential custody**: long-lived credentials stay on the platform control plane (protected by Vault/KMS, ciphertext and decryption capability managed separately); sandboxes hold only session identity, and real credentials are injected by the egress gateway on upstream requests.
- **FUSE userspace filesystem**: per-Session virtual views (mount_path normalization, Skill composition, read-only/writable boundaries), with file operations uniformly authorized and logged through the platform.
- **MCP Tunnel reverse connection**: a private-network client (OpenAI's open-source tunnel-client) dials out to the cloud; the management API aligns with the Claude Managed Agents Tunnels protocol.

## Results and Benefits

- Inference process, execution environment, and session state each have their own lifecycle: sandboxes can be recreated after failure, and a restarted Brain recovers by session ID.
- Five classes of common problems — long-task recovery, credential safety, file persistence, private-network tools, and execution traceability — are handled centrally by the platform.
- Timeline + global views support usage and failure analysis by team and scenario.

## References

- Full case study: [OMA: A Cloud Managed Agents Platform — From Getting Agents Running to Delivering Reliably](/blog/posts/2026-11-06-oma-managed-agents)
- Cube Sandbox source: [TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
