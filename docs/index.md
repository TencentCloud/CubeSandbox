---
layout: home

hero:
  name: "Cube Sandbox"
  text: "Empowering your AI Agents."
  tagline: "Instant, Concurrent, Secure & Lightweight Sandbox Service for AI Agents"
  actions:
    - theme: brand
      text: Quick Start
      link: /guide/quickstart

features:
  - title: "⚡ Ultra-fast Startup"
    details: Resource pooling and snapshot cloning skip all cold-start overhead. Sandbox creation faster than a blink.
  - title: "🔒 Hardware Isolation"
    details: Every sandbox runs a dedicated OS kernel in its own MicroVM.
  - title: "🔌 E2B SDK Compatible"
    details: Drop-in replacement for E2B Cloud. Switch by changing one environment variable — zero client code changes.
  - title: "📦 High-density Deployment"
    details: MB-level per-sandbox overhead enables thousands of instances per server via kernel sharing and Copy-on-Write.
  - title: "🛡️ Network Security"
    details: eBPF-based inter-sandbox isolation and egress filtering at kernel level; built-in L7 security proxy enables per-domain/path/method policies with automatic credential injection — secrets never visible to sandbox code.
  - title: "📸 State Management"
    details: "Create checkpoints on running sandboxes, roll back to any saved state, or fork into parallel exploration environments at millisecond granularity."
---

## Get Started

- [Quick Start](./guide/quickstart.md) — from zero to a running sandbox in minutes
- [Self-Build Deployment](./guide/self-build-deploy.md) — build from source and deploy on a single machine
- [Multi-Node Cluster](./guide/multi-node-deploy.md) — scale to multiple nodes
- [Architecture Overview](./architecture/overview.md) — understand the system design and core components


## Examples

For SDK examples and end-to-end scenarios, see:

- [Example Projects](./guide/tutorials/examples.md) — code execution, browser automation, OpenClaw integration, RL training, and more
- [Repository examples](https://github.com/tencentcloud/CubeSandbox/tree/master/examples) — full collection on GitHub
