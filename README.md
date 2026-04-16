<p align="center">
  <img src="docs/assets/cube-sandbox-logo.png" alt="Cube Sandbox Logo" width="140" />
</p>

<h1 align="center">CubeSandbox</h1>

<p align="center">
  <strong>Instant, Concurrent, Secure & Lightweight Sandbox for AI Agents</strong>
</p>

<p align="center">
  <a href="https://github.com/tencentcloud/CubeSandbox/stargazers"><img src="https://img.shields.io/github/stars/tencentcloud/cube-sandbox?style=social" alt="GitHub Stars" /></a>
  <a href="https://github.com/tencentcloud/CubeSandbox/issues"><img src="https://img.shields.io/github/issues/tencentcloud/cube-sandbox" alt="GitHub Issues" /></a>
  <a href="./LICENSE"><img src="https://img.shields.io/badge/License-Apache_2.0-green" alt="Apache 2.0 License" /></a>
  <a href="./CONTRIBUTING.md"><img src="https://img.shields.io/badge/PRs-welcome-brightgreen" alt="PRs Welcome" /></a>
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
  <a href="https://discord.gg/kkapzDXShb" target="_blank"><strong>Discord</strong></a>
</p>

---

Cube Sandbox is a high-performance, out-of-the-box secure sandbox service built on RustVMM and KVM. It supports both single-node deployment and can be easily scaled to a multi-node cluster. It is compatible with the E2B SDK, capable of creating a hardware-isolated sandbox environment with full service capabilities in under 60ms, while maintaining less than 5MB memory overhead.


<p align="center">
  <img src="./docs/assets/readme_speed_en_1.png" width="400" />
  <img src="./docs/assets/readme_overhead_en_1.png" width="400" />
</p>


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
- **Event-level snapshot rollback (coming soon):** High-frequency snapshot rollback at millisecond granularity, enabling rapid fork-based exploration environments from any saved state.
- **Production-ready:** Cube Sandbox has been validated at scale in Tencent Cloud production environments, proven stable and reliable.

## Benchmarks

In the context of AI Agent code execution, CubeSandbox achieves the perfect balance of security and performance:

| Metric | Docker Container | Traditional VM | CubeSandbox |
|---|---|---|---|
| **Isolation Level** | Low (Shared Kernel Namespaces) | High (Dedicated Kernel) | **Extreme (Dedicated Kernel + eBPF)** |
| **Boot Speed** | ~200ms | Minutes | **Sub-millisecond (<60ms)** |
| **Memory Overhead** | Low (Shared Kernel) | High (Full OS) | **Ultra-low (Aggressively stripped, <5MB)** |
| **Deployment Density** | High | Low | **Extreme (Thousands per node)** |
| **E2B SDK Compatible** | ❌ No | ❌ No | **✅ Drop-in** |

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
  <a href="./docs/guide/quickstart.md">
    <img src="docs/assets/fast-start.gif" alt="Cube Sandbox fast start walkthrough" width="720" />
  </a>
</p>

<p align="center">
  <em>⚡ Millisecond-level startup — watch the fast-start flow, then jump into the <a href="./docs/guide/quickstart.md" target="_blank">Quick Start guide</a>.</em>
</p>




Launch your first sandbox in 3 steps on a KVM-enabled machine:

1. **Start the Cube Sandbox Service**

```bash
curl -sL https://github.com/tencentcloud/CubeSandbox/cube-sandbox/deploy/one-click/online-install.sh | bash
```

2. **Create a Code Interpreter Sandbox Template**

```bash
cubemastercli tpl create-from-image \
  --image ccr.ccs.tencentyun.com/ags-image/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
```

3. **Run Your First Agent Code**

Set environment variables pointing to the local service: `CUBE_TEMPLATE_ID`, `E2B_API_URL`, and `E2B_API_KEY`, then simply use the official E2B SDK:

```bash
export E2B_API_URL="http://127.0.0.1:3000"
# Required: any non-empty value satisfies the SDK check
export E2B_API_KEY="dummy"
# Required: template ID obtained from Step 2 (create-from-image)
export CUBE_TEMPLATE_ID="<your-template-id>"
export SSL_CERT_FILE="$(mkcert -CAROOT)/rootCA.pem"
```

```python
import os
from e2b_code_interpreter import Sandbox # That's right, use the E2B SDK directly!

# CubeSandbox seamlessly intercepts all requests under the hood
with Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"]) as sandbox:
    # Let your LLM-generated code run safely here
    result = sandbox.run_code("print('Hello from Cube Sandbox, safely isolated!')")
    print(result)
```

Want to explore more? Check out the 📂 [`examples/`](./examples/) directory, covering scenarios like: code execution, Shell commands, file operations, browser automation, network policies, pause/resume, OpenClaw integration, and RL training.

### Deep Dive

- 📖 [Documentation Home](./docs/index.md) - Complete guide and API reference
- 🔧 [Template Concepts](./docs/guide/templates.md) - Image-to-Template concepts and workflows
- 🌟 [Example Projects](./docs/guide/tutorials/examples.md) - Hands-on examples demonstrating various Cube Sandbox use cases (Browser automation, OpenClaw integration, RL training workflows, etc.)

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
| **CubeRuntime** | The core sandbox execution layer, composed of Shim, Hypervisor, and Agent working in tandem. |

👉 For more details, please read the [Architecture Design Document](./docs/architecture/overview.md) and [CubeVS Network Model](./docs/architecture/network.md).

## Community & Contributing

We welcome contributions of all kinds—whether it’s a bug report, feature suggestion, documentation improvement, or code submission!

- 🐞 **Found a Bug?** Submit an issue on <a href="https://github.com/tencentcloud/CubeSandbox/issues" target="_blank">GitHub Issues</a>.
- 💡 **Have an Idea?** Join the conversation in <a href="https://github.com/tencentcloud/CubeSandbox/discussions" target="_blank">GitHub Discussions</a>.
- 🛠️ **Want to Code?** Check out our <a href="./CONTRIBUTING.md" target="_blank">CONTRIBUTING.md</a> to learn how to submit a Pull Request.
- 💬 **Want to Chat?** Join our <a href="https://discord.gg/kkapzDXShb" target="_blank">Discord</a>.

## License

CubeSandbox is released under the [Apache License 2.0](./LICENSE).

The birth of CubeSandbox stands on the shoulders of open-source giants. Special thanks to [Cloud Hypervisor](https://github.com/cloud-hypervisor/cloud-hypervisor), [Kata Containers](https://github.com/kata-containers/kata-containers), virtiofsd, containerd-shim-rs, ttrpc-rust, and others. We have made tailored modifications to some components to fit the CubeSandbox execution model, and the original in-file copyright notices are preserved.
