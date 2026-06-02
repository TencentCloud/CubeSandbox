<p align="center">
  <img src="docs/assets/cube-sandbox-logo.png" alt="Cube Sandbox Logo" width="140" />
</p>

<h1 align="center">CubeSandbox</h1>

<p align="center">
  <strong>Instant, Concurrent, Secure & Lightweight Sandbox Service for AI Agents</strong>
</p>

<p align="center">
  <a href="https://github.com/tencentcloud/CubeSandbox/stargazers"><img src="https://img.shields.io/github/stars/tencentcloud/cubesandbox?style=social" alt="GitHub Stars" /></a>
  <a href="https://github.com/tencentcloud/CubeSandbox/issues"><img src="https://img.shields.io/github/issues/tencentcloud/cubesandbox" alt="GitHub Issues" /></a>
  <a href="./LICENSE"><img src="https://img.shields.io/badge/License-Apache_2.0-green" alt="Apache 2.0 License" /></a>
  <a href="./CONTRIBUTING.md"><img src="https://img.shields.io/badge/PRs-welcome-brightgreen" alt="PRs Welcome" /></a>
  <a href="https://pypi.org/project/cubesandbox/"><img src="https://img.shields.io/badge/PyPI-0.2.1-blue" alt="PyPI Version" /></a>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/⚡_Startup-Tens_of_ms-blue" alt="Fast startup" />
  <img src="https://img.shields.io/badge/🔒_Isolation-Hardware_Level-critical" alt="Hardware-level isolation" />
  <img src="https://img.shields.io/badge/🔌_API-E2B_Compatible-blueviolet" alt="E2B compatible" />
  <img src="https://img.shields.io/badge/📦_Deploy-High_Concurrency·High_Density-orange" alt="High concurrency & high density" />
</p>

<p align="center">
  <a href="./README_zh.md"><strong>中文文档</strong></a> ·
  <a href="./docs/guide/quickstart.md"><strong>Quick Start</strong></a> ·
  <a href="./docs/index.md"><strong>Documentation</strong></a> ·
  <a href="./docs/changelog/index.md"><strong>Changelog</strong></a> ·
  <a href="https://x.com/CubeSandbox_AI"><strong>X(Twitter)</strong></a>
</p>

---

Cube Sandbox is a high-performance, out-of-the-box secure sandbox service built on RustVMM and KVM. It supports both single-node deployment and easy scaling to multi-node clusters. It is compatible with the E2B SDK and can create a hardware-isolated, fully serviceable sandbox in under 60ms with less than 5MB of memory overhead.


<p align="center">
  <img src="./docs/assets/readme_speed_en_1.png" width="400" />
  <img src="./docs/assets/readme_overhead_en_1.png" width="400" />
</p>

## 📰 News

<table>
  <tr>
    <td align="right" valign="top" width="100">
      <a href="./docs/changelog/v0.3.0.md">
        <img src="https://img.shields.io/badge/v0.3.0-New!-6f42c1?style=flat-square" alt="v0.3.0" />
      </a>
    </td>
    <td valign="top">
      <strong>Snapshot, Clone &amp; Rollback at hundred-millisecond granularity</strong><br/>
      CubeSandbox 0.3.0 introduces the <b>CubeCoW</b> Copy-on-Write snapshot engine, enabling event-level snapshots, instant cloning, and rollback to any saved state.
      <a href="./docs/changelog/v0.3.0.md">Changelog →</a>
    </td>
  </tr>
  <tr>
    <td align="right" valign="top" width="100">
      <a href="./docs/changelog/v0.2.2.md">
        <img src="https://img.shields.io/badge/v0.2.2-2026.05.18-007bff?style=flat-square" alt="v0.2.2" />
      </a>
    </td>
    <td valign="top">
      <strong>Security hardening &amp; E2B compatibility improvements</strong><br/>
      Patched CVE-2023-50711 and other vulnerabilities, aligned default ports with the E2B protocol, and shipped critical stability fixes.
      <a href="./docs/changelog/v0.2.2.md">Changelog →</a>
    </td>
  </tr>
  <tr>
    <td align="right" valign="top" width="100">
      <a href="./docs/changelog/v0.1.0.md">
        <img src="https://img.shields.io/badge/v0.1.0-2026.04.20-28a745?style=flat-square" alt="v0.1.0" />
      </a>
    </td>
    <td valign="top">
      <strong>🎉 Initial open-source release</strong><br/>
      Cube Sandbox is now open source! Millisecond boot, hardware-level isolation, E2B-compatible sandbox for AI Agents.
      <a href="./docs/changelog/v0.1.0.md">Changelog →</a>
    </td>
  </tr>
</table>

## Demos

<table align="center">
  <tr align="center" valign="middle">
    <td width="33%" valign="middle">
      <video src="https://github.com/user-attachments/assets/f87c409e-29fc-4e86-9eac-dbeaff2aca18" controls="controls" muted="muted" style="max-width: 100%;"></video>
    </td>
    <td width="33%" valign="middle">
      <video src="https://github.com/user-attachments/assets/50e7126e-bb73-4abc-aa85-677fdf2e8c67" controls="controls" muted="muted" style="max-width: 100%;"></video>
    </td>
    <td width="33%" valign="middle">
      <video src="https://github.com/user-attachments/assets/052e0e77-e2d9-409e-90b8-d13c28b80495" controls="controls" muted="muted" style="max-width: 100%;"></video>
    </td>
  </tr>
  <tr align="center" valign="top">
    <td>
      <em>Installation & Demo</em>
    </td>
    <td>
      <em>Performance Test</em>
    </td>
    <td>
      <em>RL (SWE-Bench)</em>
    </td>
  </tr>
</table>


## Core Highlights

- **Blazing-fast cold start:** Built on resource pool pre-provisioning and snapshot cloning technology, skipping time-consuming initialization entirely. Average end-to-end cold start time for a fully serviceable sandbox is < 60ms.
- **High-density deployment on a single node:** Extreme memory reuse via CoW technology combined with a Rust-rebuilt, aggressively trimmed runtime keeps per-instance memory overhead below 5MB — run thousands of Agents on a single machine.
- **True kernel-level isolation:** No more unsafe Docker shared-kernel (Namespace) hacks. Each Agent runs with its own dedicated Guest OS kernel, eliminating container escape risks and enabling safe execution of any LLM-generated code.
- **Zero-cost migration (E2B drop-in replacement):** Natively compatible with the E2B SDK interface. Just swap one URL environment variable — no business logic changes needed — to migrate from expensive closed-source sandboxes to free Cube Sandbox with better performance.
- **Network security:** CubeVS, powered by eBPF, enforces strict inter-sandbox network isolation at the kernel level with fine-grained egress traffic filtering policies.
- **Ready to use out of the box:** One-click deployment with support for both single-node and cluster setups.
- **Event-level snapshot rollback:** High-frequency snapshot rollback at millisecond granularity. Create checkpoints on running sandboxes, roll back to any saved state, or fork into parallel exploration environments from any saved state.
- **Production-ready:** Cube Sandbox has been validated at scale in Tencent Cloud production environments, proven stable and reliable.

## Benchmarks

In the context of AI Agent code execution, CubeSandbox achieves the perfect balance of security and performance:

| Metric | Docker Container | Traditional VM | CubeSandbox |
|---|---|---|---|
| **Isolation Level** | Low (Shared Kernel Namespaces) | High (Dedicated Kernel) | **Extreme (Dedicated Kernel + eBPF)** |
| **Boot Speed** <br>*Full-OS boot duration | 200ms | Seconds | **Sub-millisecond (<60ms)** |
| **Memory Overhead** | Low (Shared Kernel) | High (Full OS) | **Ultra-low (Aggressively stripped, <5MB)** |
| **Deployment Density** | High | Low | **Extreme (Thousands per node)** |
| **E2B SDK Compatible** | / | / | **✅ Drop-in** |

*   *Cold start benchmarked on bare-metal. 60ms at single concurrency; under 50 concurrent creations, avg 67ms, P95 90ms, P99 137ms — consistently sub-150ms.*
*   *Memory overhead measured with sandbox specs ≤ 32GB. Larger configurations may see a marginal increase.*

For detailed metrics on startup latency and resource overhead, please refer to:


<table align="center">
  <tr align="center" valign="middle">
    <td width="33%" valign="middle">
      <img src="./docs/assets/1-concurrency-create.png" />
    </td>
    <td width="33%" valign="middle">
      <img src="./docs/assets/50-concurrency-create.png" />
    </td>
    <td width="33%" valign="middle">
      <img src="./docs/assets/cube-sandbox-mem-overhead.png" />
    </td>
  </tr>
  <tr align="center" valign="top">
    <td colspan="2">
      <em>Sub-150ms sandbox delivery under both single and high-concurrency workloads</em>
    </td>
    <td>
      <em>CubeSandbox base memory footprint across various instance sizes</em><br>
      <sup>(*Blue: Sandbox specifications; Orange: Base memory overhead). Note that memory consumption increases only marginally as instance sizes scale up.
</sup>
    </td>
  </tr>
</table>


</br>

## Quick Start

<p align="center">
  <img src="docs/assets/fast-start.gif" alt="Cube Sandbox fast start walkthrough" width="720" />
</p>

<p align="center">
  <em>⚡ Millisecond-level startup — watch the fast-start flow above.</em>
</p>

Cube Sandbox requires an **x86_64 Linux** environment with **KVM** support.

The guide walks you through everything in **four steps** — provisioning a server, installing Cube Sandbox, creating a sandbox template, and running your first agent code. No source build needed, up and running in minutes.

<p align="center">
  <b>Choose your deployment path:</b>
</p>

<table align="center">
  <tr align="center">
    <td align="center">
      <a href="./docs/guide/pvm-deploy.md" style="
        display: inline-block;
        background: #28a745;
        color: white;
        padding: 12px 28px;
        border-radius: 8px;
        font-size: 15px;
        font-weight: bold;
        text-decoration: none;
        white-space: nowrap;
        font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Helvetica, Arial, sans-serif;
      ">
        🖥 PVM · Cloud VM →
      </a>
      <br/>
      <sup><b>🏆 Recommended</b></sup>
    </td>
    <td align="center">
      <a href="./docs/guide/bare-metal-deploy.md" style="
        display: inline-block;
        background: #007bff;
        color: white;
        padding: 12px 28px;
        border-radius: 8px;
        font-size: 15px;
        font-weight: bold;
        text-decoration: none;
        white-space: nowrap;
        font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Helvetica, Arial, sans-serif;
      ">
        🏗 Bare Metal →
      </a>
    </td>
    <td align="center">
      <a href="./docs/guide/dev-environment.md" style="
        display: inline-block;
        background: #6c757d;
        color: white;
        padding: 12px 28px;
        border-radius: 8px;
        font-size: 15px;
        font-weight: bold;
        text-decoration: none;
        white-space: nowrap;
        font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Helvetica, Arial, sans-serif;
      ">
        💻 Dev-Env →
      </a>
      <br/>
      <sup>⚠️ <b>Not recommended — poor performance</b></sup>
    </td>
  </tr>
</table>

### Deep Dive

- 📖 [Documentation Home](./docs/index.md) - Complete guide and API reference
- 🔧 [Template Concepts](./docs/guide/templates.md) - Image-to-Template concepts and workflows
- 🌟 [Example Projects](./docs/guide/tutorials/examples.md) - Hands-on examples (code execution, browser automation, OpenClaw integration, RL training, etc.)
- 📂 [`examples/`](./examples/) - Runnable example code covering Shell commands, file operations, network policies, pause/resume, and more
- 💻 [Development Environment (QEMU VM)](./docs/guide/dev-environment.md) - No KVM? Spin up a disposable VM and run Cube Sandbox inside it
- ☁️ [PVM Deployment](./docs/guide/pvm-deploy.md) - Deploy on ordinary cloud VMs without bare-metal or nested virtualization

## Architecture

<p align="center">
  <img src="docs/assets/cube-sandbox-arch.png" alt="Cube Sandbox Architecture" />
</p>

| Component | Responsibility |
|---|---|
| **CubeAPI** | High-concurrency REST API Gateway (Rust), compatible with E2B. Swap the URL for seamless migration. |
| **CubeMaster** | Cluster orchestrator. Receives API requests and dispatches them to corresponding Cubelets. Manages resource scheduling and cluster state. |
| **CubeProxy** | Reverse proxy, compatible with the E2B protocol, routing requests to the appropriate sandbox instances. |
| **Cubelet** | Compute node local scheduling component. Manages the complete lifecycle of all sandbox instances on the node. |
| **CubeVS** | eBPF-based virtual switch, providing kernel-level network isolation and security policy enforcement. |
| **CubeHypervisor & CubeShim** | Virtualization layer — CubeHypervisor manages KVM MicroVMs, CubeShim implements the containerd Shim v2 API to integrate sandboxes into the container runtime. |

👉 For more details, please read the [Architecture Design Document](./docs/architecture/overview.md) and [CubeVS Network Model](./docs/architecture/network.md).

## Community & Contributing

We welcome contributions of all kinds—whether it’s a bug report, feature suggestion, documentation improvement, or code submission!

- 🐞 **Found a Bug or have questions?** Submit an issue on <a href="https://github.com/tencentcloud/CubeSandbox/issues" target="_blank">GitHub Issues</a>.
- 💡 **Have an Idea?** Join the conversation in <a href="https://github.com/tencentcloud/CubeSandbox/discussions" target="_blank">GitHub Discussions</a>.
- 🛠️ **Want to Code?** Check out our <a href="./CONTRIBUTING.md" target="_blank">CONTRIBUTING.md</a> to learn how to submit a Pull Request.
- 📝 **Want to contribute docs?** Submit bilingual PRs to our community doc channels: <a href="./docs/guide/troubleshooting/index.md" target="_blank">Troubleshooting</a>, <a href="./docs/guide/usecases/index.md" target="_blank">Use Cases</a>, and <a href="./docs/guide/integrations/index.md" target="_blank">Integrations</a>.
- 💬 **Want to Chat?** Join our <a href="https://discord.gg/kkapzDXShb" target="_blank">Discord</a>.

## License

CubeSandbox is released under the [Apache License 2.0](./LICENSE).

The birth of CubeSandbox stands on the shoulders of open-source giants. Special thanks to [Cloud Hypervisor](https://github.com/cloud-hypervisor/cloud-hypervisor), [Kata Containers](https://github.com/kata-containers/kata-containers), virtiofsd, containerd-shim-rs, ttrpc-rust, and others. We have made tailored modifications to some components to fit the CubeSandbox execution model, and the original in-file copyright notices are preserved.
