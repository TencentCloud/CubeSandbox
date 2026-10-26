---
title: "Hongze Info: Sandboxing Financial Research Agents"
author: Wang Zhengkai
date: 2026-08-26
tags:
  - agent
  - financial
  - host-mount
  - cubeegress
lang: en-US
---

# Hongze Info: Sandboxing Financial Research Agents

## Business Context

Hongze Info's 4as (All (Domain) Agents as a Service) is a financial research Agent product that organizes domain knowledge, research methodologies, data tools, and persistent state into deployable research Agents. A single task lasts tens of seconds to several minutes, requiring a complete Agent loop in an isolated environment.

## Key Challenges

- **Complete Agent loop isolation**: Research Agent runs involve model interactions, code execution, file operations, and external tool calls — requiring complete process and file boundaries.
- **Versioned domain resource distribution**: Large domain resources (DAS) need to be distributed by version and authorization to each sandbox, without repeated transfers per run.
- **Credential isolation**: Long-term API keys for models and tools cannot enter the guest environment — they must be injected at the egress side.
- **Localized deployment**: Financial institutions require running in designated cloud environments or customer-side infrastructure — managed sandboxes can't cover this.

## Solution with CubeSandbox

Three core use cases:

| Scenario | CubeSandbox Capability | Description |
|---|---|---|
| Complete Pi loop | Runs inside Sandbox guest | Model-tool main loop, bash/file operations, MCP calls all in isolated environment |
| DAS resource distribution | Host Mount (read-only) | Distributes immutable file trees by revision, multiple sandboxes share same host cache |
| Credential management | CubeEgress (egress injection) | Long-term keys stay on egress side, auth info auto-injected when requests leave sandbox |

## Results and Benefits

- Same Agent runtime supports both customer-side deployment and Cloud service.
- Domain resources visible on demand — unmounted resources invisible to guest.
- Credentials centrally rotated and revoked by platform — Sandbox template doesn't need rebuilding when keys change.
- Deployed in multiple commercial projects.

## References

- Full case study: [Sandboxing Financial Research Agents: Hongze Info's Practice with CubeSandbox](/blog/posts/2026-08-28-horizon-insights)
- Cube Sandbox source: [TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
