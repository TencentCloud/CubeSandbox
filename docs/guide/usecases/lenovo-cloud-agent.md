---
title: "Lenovo Cloud Agent: Sandbox Migration from Daytona to CubeSandbox"
author: Li Jian
date: 2026-08-20
tags:
  - agent
  - migration
  - daytona
  - e2b-compat
lang: en-US
---

# Lenovo Cloud Agent: Sandbox Migration from Daytona to CubeSandbox

## Business Context

Lenovo Research Institute AI Lab's cloud Agent product went through two phases: 1.0 treated the sandbox as a "start one per session, dispose when done" isolated execution environment; 2.0 put the daemon and Agent entirely inside the sandbox, making it a standalone node that can be backed up and rolled back. Early use of Daytona sandboxes, combined with slow startup (>10s), network restrictions (VPN required), and SaaS costs, drove the migration to CubeSandbox.

## Key Challenges

- **API incompatibility**: Daytona and E2B (Cube-compatible) APIs are incompatible, preventing simple substitution.
- **Startup performance**: Daytona sandbox startup exceeded 10 seconds, optimized to 5 seconds but still a bottleneck for batch concurrency.
- **Volume shared writes**: Early Cube versions didn't support multiple sandboxes modifying the same mounted directory.
- **Sandbox loss after server restart**: In 2.0 mode, long-running sandboxes without snapshots disappear after restart.

## Solution with CubeSandbox

Migration in three steps: decouple from Daytona → build SDK adapter layer (supporting both Daytona and E2B) → connect to Cube. The adapter layer defines a unified Sandbox Provider base class, with business code unaware of underlying differences.

Startup performance: compressed from >10 seconds to <100 milliseconds. Volume shared write issue fixed in v0.6.0. Cross-machine recovery planned for v0.7.0.

## Results and Benefits

- Startup speed from >10s to <100ms, imperceptible to users.
- SaaS fees eliminated, VPN issue resolved.
- Debugging experience significantly improved with local deployment.
- Sandbox role evolved from "tool execution container" to "independent runtime environment."

## References

- Full case study: [From Daytona to CubeSandbox: Lenovo's Cloud Agent Sandbox Migration](/blog/posts/2026-08-20-lenovo-cloud-agent)
- Cube Sandbox source: [TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
