---
title: "Lexmount AI: Putting the Browser Runtime Inside the Agent Sandbox"
author: Xiong Xiuzhang
date: 2026-08-13
tags:
  - agent
  - browser
  - browser-runtime
  - production
lang: en-US
---

# Lexmount AI: Putting the Browser Runtime Inside the Agent Sandbox

## Business Context

Lexmount's self-developed Agent browser runtime (Lexmount Insight Flow) has a core use case: enabling AI agents to execute web tasks at scale and reliably. This business form inherently imposes several "must-haves" on the underlying sandbox: real internet access, batch concurrent launch, per-sandbox runtime state injection, and accurate capacity perception.

During the selection phase, the team searched the open-source community for tools that could "launch many lightweight sandboxes." After benchmarking OpenSandbox and Cube Sandbox, they found Cube had lower resource usage, so they chose the latter. Following from v0.1.0 to v0.5.1, around four boundaries, the team hit four pitfalls: no public network access, template creation failures, environment variable loss, and phantom resource oversell.

## Key Challenges

Agent browser runtime is a composite workload with four hard requirements for sandboxes:

- **Outbound network**: Every page the Agent opens, every API it calls, must go from the sandbox to the public internet.
- **Batch launch**: Multi-user parallelism and RL batch rollout scenarios require dense bursts of sandbox creation requests on a single machine.
- **Dynamic state injection**: Different users and tasks have different API keys and context labels that need per-sandbox injection.
- **Real capacity perception**: Browser runtime is a resource-heavy workload — accurate per-node capacity numbers are essential.

## Solution with Cube Sandbox

The team split sandboxization into two paths, with the business orchestration layer doing policy routing by risk level:

1. **Bash-only isolation**: For low-risk, short-execution scenarios, the Agent Loop stays in the business orchestration layer; only `Bash/exec` tool calls enter the sandbox. Sandboxes are disposable and stateless.
2. **Full Agent Runtime in sandbox**: For high-risk, resource-heavy scenarios (browser automation, long-running tasks), the Agent's resident process enters the sandbox entirely, with dedicated CPU/memory and a persistent workspace.

During integration with Cube Sandbox, the team resolved four problems:

- **Outbound network**: Found that `getGatewayMacAddr()` selected the wrong gateway MAC in multi-neighbor environments, sending SYN packets to the wrong next hop. Verified via `tcpdump` and fixed — PR [#224](https://github.com/TencentCloud/CubeSandbox/pull/224) merged.
- **Batch launch**: Found the startup script pulled up Cubelet before network-agent's `/readyz` passed, making the Images service unavailable. Fixed to wait for `/readyz` before starting — PR [#304](https://github.com/TencentCloud/CubeSandbox/pull/304) merged.
- **Dynamic state injection**: Found `CreateSandboxRequest.containers` was hardcoded to `vec![]`, dropping environment variables. Submitted PR [#634](https://github.com/TencentCloud/CubeSandbox/pull/634), confirmed this is a snapshot restore architectural constraint rather than a底层 defect. Compensated with a two-phase "control plane proxy, in-sandbox execution" design.
- **Real capacity**: Found v0.2.x mock resource metrics caused inflated capacity numbers, fixed in v0.3.0. Derived the principle that "capacity must be measured under real business load."

## Results and Benefits

- **Network connectivity**: Fixed L2 next-hop selection, giving the browser runtime stable outbound network access.
- **Batch concurrency**: Fixed startup sequencing races, enabling stable batch sandbox launches on a single machine.
- **State injection**: Two-phase design achieves per-sandbox state injection within the snapshot restore architectural constraint, with hot-start P95 sub-second.
- **Accurate capacity**: Eliminated mock metric interference, with capacity assessment based on real load and multi-source cross-validation.

## References

- Full case study: [Putting the Browser Inside the Agent Sandbox: Lexmount's Hands-On Experience with CubeSandbox](/blog/posts/2026-08-13-lexmount-browser-agent)
- Cube Sandbox source: [TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
