# Changelog

## 2026.04.27 Release v0.1.2

### 🛠️ Critical Fixes

- **Fixed** the issue where CubeMaster returned a 5xx error instead of a 4xx error when the sandbox template does not exist.
- **Fixed** the missing SSL RootCA certificate issue during one-click deployment in v0.1.1.
- **Fixed** the cube proxy image build failure during deployment in v0.1.1.

## 2026.04.24 Release v0.1.1

### 🛠️ Critical Fixes

- **Fixed** the issue where the latest vmlinux was not used during template reconstruction, improving the stability of the sandbox environment.
- **Updated** the one-click installation script to support non-eth0 network interfaces, resolving stability issues with CubeProxy CA certificates.
- **Disabled** GRO on the primary network interface during initialization to enhance network stability.
- **Fixed** incorrect error handling when the target was not found during template cleanup, ensuring proper error returns.

### ✨ New Features

- **Added** the `cubebox destroy` command, enabling sandbox deletion via the CLI.
- **Added** integration examples for the OpenAI Agents SDK (including a code interpreter).

### 📚 Documentation Updates

- **Rewrote** the HTTPS and domain configuration documentation, adding explanations for wildcard DNS records.

### ⚙️ Engineering Improvements

- **Implemented** a parallel CI build pipeline for multiple components to optimize build efficiency.
- **Added** support for automatic synchronization of GitHub Release assets to `cnb.cool/CubeSandbox/CubeSandbox`.

## 2026.04.20 Release v0.1.0

### Initial open-source release of Cube Sandbox

**Instant, Concurrent, Secure & Lightweight Sandbox for AI Agents.**

### Core Highlights

Cube Sandbox is a high-performance, out-of-the-box secure sandbox
service built on RustVMM and KVM. It supports both single-node
deployment and can be easily scaled to a multi-node cluster. It is
compatible with the E2B SDK, capable of creating a hardware-isolated
sandbox environment with full service capabilities in under 60ms,
while maintaining less than 5MB memory overhead.

- Blazing-fast cold start: built on resource pool pre-provisioning
  and snapshot cloning technology, average end-to-end cold start
  time for a fully serviceable sandbox is < 60ms.

- High-density deployment on a single node: extreme memory reuse via
  CoW technology combined with a Rust-rebuilt, aggressively trimmed
  runtime keeps per-instance memory overhead below 5MB — run
  thousands of Agents on a single machine.

- True kernel-level isolation: each Agent runs with its own dedicated
  Guest OS kernel, eliminating container escape risks and enabling
  safe execution of any LLM-generated code.

- Zero-cost migration (E2B drop-in replacement): natively compatible
  with the E2B SDK interface. Just swap one URL environment variable
  — no business logic changes needed.

- Network security: CubeVS, powered by eBPF, enforces strict
  inter-sandbox network isolation at the kernel level with
  fine-grained egress traffic filtering policies.

### Production-ready 

**Cube Sandbox has been validated at scale in Tencent Cloud production
environments, proven stable and reliable** — before this day it ever
existed as open source, it had already quietly run behind real AI
Agent workloads, serving real users, at production load.

In real production deployments, a single physical machine can spin up
tens of thousands of sandboxes within minutes.

We open-source it today not as a prototype, but as production-hardened
infrastructure that has already stood the test of real-world scale.

### A Note to Every Contributor — Past, Present, and Future

Before this code was ever public, it was already doing its job:
spinning up sandboxes in milliseconds, isolating Agent workloads
at the kernel level, and holding up under real production load
at Tencent Cloud. None of that happened by accident.

Today we open the door. The high-performance Agent infrastructure
you shaped now belongs to the world — to every developer who believes
that safe, instant, and lightweight code execution should be open
and self-hostable.

To those who contributed before this day: you built the foundation.
To those who will contribute after: you are what turns a foundation
into an ecosystem.

Open source shines because of you!
