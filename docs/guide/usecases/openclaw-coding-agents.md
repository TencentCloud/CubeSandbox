---
title: "Running OpenClaw Coding Agents on Cube Sandbox"
author: yjjnls
date: 2026-11-10
tags:
  - agent
  - openclaw
  - network-policy
  - snapshot
lang: en-US
---

# Running OpenClaw Coding Agents on Cube Sandbox

## Business Context

A platform engineering team operates AI coding agents (OpenClaw) for internal developers. The agents review diffs, edit files across a workspace, run builds and tests, and call model APIs — executing shell commands and generated code that no reviewer can fully vet in advance. The platform needed to give every agent session a hard isolation boundary, strict egress control, session continuity across maintenance, and a cold start fast enough that an interactive developer never notices the sandbox.

The [OpenClaw integration example](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/openclaw-integration) already covers wiring the `cube-sandbox` skill to a single sandbox through the E2B-compatible SDK. This case study is about the next step: running that setup as a shared, multi-tenant service with real security and lifecycle requirements.

## Key Challenges

- **Isolation boundary.** Agents execute unreviewed commands and model-generated code. A shared-kernel container was not accepted as the boundary; each session needed its own kernel.
- **Egress control.** An agent only ever needs a handful of destinations: the model API, package registries, and an internal git mirror. The requirement was deny-by-default — not audit-only — with allowlists reviewed like code.
- **Session continuity.** Coding sessions run for hours or days and must survive node maintenance without losing the gateway state or the workspace.
- **Interactive latency.** Developers type into a live agent; a cold start that lands in seconds rather than milliseconds degrades the workflow into polling.
- **Inbound access.** The OpenClaw gateway inside each sandbox exposes an HTTP UI/API that the developer's browser and CLI must reach — without opening any other inbound path into the sandbox network.

## Solution with Cube Sandbox

Each agent session maps to one Cube sandbox created from a baked template:

```
Developer browser / CLI
        │  (mapped gateway port)
        ▼
OpenClaw gateway  ◄─── OpenClaw agents (skills, CLI tools)
        │  E2B-compatible SDK
        ▼
CubeAPI ──► CubeMaster ──► Cubelet ──► MicroVM (one per session, own kernel)
                                          ├─ workspace (persistent volume / host mount)
                                          └─ egress: default-deny + allowlist (eBPF)
```

**Template, not provisioning.** OpenClaw, its skills, and the toolchains agents actually invoke (git, node, build tools) are baked into a Cube template. Every session starts from a known-good image, and nothing is installed while the developer watches. Template-based creation keeps cold start in the sub-second range, consistent with the project's own [performance benchmark](/blog/posts/2026-06-01-cubesandbox-perf-benchmark).

**Egress policy as code.** Sessions are created with internet access off and an explicit allowlist. Domain targets are handled by Cube's DNS query/response learning, so a hostname entry keeps working when the provider's IPs change:

```python
sandbox = Sandbox.create(
    template="openclaw-agent",
    allow_internet_access=False,
    network={
        "allow_out": [
            "api.llm-provider.example",   # model API (domain, learned via DNS tracking)
            "registry.npmjs.org",         # package registry
            "pypi.org",
            "10.60.0.0/16",               # internal git mirror (CIDR)
        ],
    },
)
```

Cube installs a `0.0.0.0/0` deny and enforces allow-over-deny in the eBPF data plane; sandbox-private and host-internal CIDRs are denied regardless of the configured policy, which closed the usual "agent reaches cluster-internal services" review finding on its own. Policy changes flow through the same reviewed configuration as the rest of the platform rather than ad-hoc manual edits — see [Network Policy](/guide/network-policy).

**Inbound via port mapping only.** The gateway port is mapped at sandbox creation and surfaced through the platform's URL. There is no other inbound path into the sandbox network.

**Session continuity.** Idle sessions are paused to release CPU/memory while keeping state; paused sessions are restored with a plain connect when the developer returns:

```python
sandbox.pause()                       # checkpoint state, free CPU/memory
# ... later
sandbox = Sandbox.connect(sandbox.sandbox_id, timeout=300)
```

For maintenance windows, the workspace lives on persistent storage (host mount or the S3 volume plugin — see [Persistent Storage](/guide/persistent-storage)), and long-running work is checkpointed with snapshots so a session can be brought back after a node drain — see [Snapshot/Rollback/Clone](/guide/snapshot-rollback-clone).

## Results and Benefits

- The per-session MicroVM boundary passed the security review that a shared-kernel design had failed: agent code and host infrastructure are separated by a kernel boundary, not just namespaces.
- Deny-by-default egress held in day-to-day operation. The egress failures that did appear were agents hitting unlisted mirrors, resolved by adding entries to the reviewed allowlist — never by loosening the default.
- Sessions survive maintenance. Pause, persistent workspace, and snapshot-based restore kept multi-day agent tasks alive across node drains.
- Interactive latency stayed low because creation is template-based; the security posture did not cost the workflow its responsiveness.

Trade-offs worth knowing before copying this design:

- A MicroVM per session costs more memory than containers. Right-sizing template resources and pausing idle sessions keeps density acceptable.
- DNS allowlist entries rely on learned A-records that age out. A long-lived agent that idles past the aging window simply re-resolves on its next request, but it is worth alerting on deny spikes against known-good domains rather than assuming every deny is an attack.
- Snapshot storage grows with session count; expire cold snapshots on a schedule.

## References

- OpenClaw integration example (skill wiring): [examples/openclaw-integration](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/openclaw-integration)
- Network policy (allow/deny, DNS learning, entry limits): [Network Policy](/guide/network-policy)
- Snapshots, rollback and clone: [Snapshot/Rollback/Clone](/guide/snapshot-rollback-clone)
- Pause/resume lifecycle semantics: [Lifecycle](/guide/lifecycle)
- E2B compatibility: [E2B Compatibility](/guide/e2b-compatibility)
