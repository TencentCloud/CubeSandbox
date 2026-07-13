---
title: "Cube Sandbox Officially Supports Arm: Tencent Cloud and Arm Team Up to Unlock Multi-Architecture Compute for Agents"
date: 2026-07-08
author: InfoQ
description: "On July 3, Tencent Cloud open-sourced the AI Agent secure sandbox Cube Sandbox v0.5.0. Native Arm support — one of the headline features — has been merged upstream with substantial investment from the Arm engineering team. Developers can now build, deploy, launch, and run typical sandbox workloads end-to-end on AArch64 servers."
featured: false
---

# Cube Sandbox Officially Supports Arm: Tencent Cloud and Arm Team Up to Unlock Multi-Architecture Compute for Agents


On July 3, Tencent Cloud released **Cube Sandbox v0.5.0**, the open-source AI Agent secure sandbox. Built as a hardware-isolated runtime foundation purpose-designed for LLM applications, v0.5.0 lands a set of production-critical upgrades under the banner of "stable, efficient, broad." Among them, **native Arm support** — one of the headline features — has been merged upstream with substantial investment from the Arm engineering team. (See the v0.5.0 deep dive: [Cube v0.5.0 Released: Auto-Pause, ARM Support, One-Click Cluster Deploy — Taking Sandboxes to Production](https://mp.weixin.qq.com/s?__biz=MzYzNDkyMDU3MQ==&mid=2247483839&idx=1&sn=bd0f059a3e3be0654f1c1ca89e4e8107&scene=21#wechat_redirect))

Developers can now carry out the full pipeline on **AArch64** servers — native build, deploy, launch, and run typical sandbox workloads.

To clear the critical path from low-level build through runtime, Tencent Cloud's IaaS frontier tech team and the Arm engineering team ran a months-long joint R&D effort, focused squarely on **AArch64 builds, the Guest Kernel, the runtime environment, and the deployment chain**.

As compute supply accelerates into an "x86 + Arm" multi-architecture era, the industry urgently needs an Agent runtime foundation that breaks past traditional architecture boundaries. v0.5.0 extends Cube's core isolation capabilities into the Arm ecosystem — not only as a step up in production-grade delivery, but as a reliable, flexible execution substrate for Agents operating across the cloud, edge, and endpoint in heterogeneous scenarios.

## 1. When the Compute Platform Goes Heterogeneous

If "putting AI Agents into production at scale" is a stress test of the underlying infrastructure, two variables are visibly shifting at once:

**First, the workload side.** Today's Agents are no longer one-shot scripts — they evolve into dozens or hundreds of serial or parallel tool calls. Each call implies a sandbox start, a period of isolated execution, and a result collection.

This workload pattern places extreme demands on the underlying infrastructure: it must deliver both **very high compute density** and **extremely fast elastic response**.

**Second, the infrastructure side.** AI-era compute supply is rapidly moving from a single architecture to an "x86 + Arm" dual-track. Driven by the energy efficiency and performance advantages of the Arm Neoverse platform in cloud AI workloads, Arm architecture is seeing broad industry adoption. With Arm compute platforms like Amazon Graviton, Google Cloud Axion, and Microsoft Azure Cobalt continuing to scale, alongside a wave of domestic Arm processors being rapidly deployed, the Arm ecosystem is accelerating. **"AI runs on Arm"** has shifted from a curiosity to a **real line item in the budget**.

Stack these two variables together and the conclusion is clear: Agent sandboxes can't be locked to x86. They have to ship deployment capabilities across multi-architecture server environments. Going further, it isn't enough to simply "run" on Arm — they need to deliver **performance on par with — or better than — x86** on that platform.

That is the starting point for the joint R&D between the Cube team and the Arm engineering team.

## 2. From "Runs" to "Native"

From the moment the Arm engineering team first engaged, to the code being formally merged upstream, this joint R&D effort spanned several months. What truly moved the project from "runs" to "usable" was the intense polish of the final stretch — the mainline merge alone went through more than twenty commits.

The core of this delivery is: Cube Sandbox now has the **end-to-end capability to deploy natively, compile, launch, and run typical sandbox workloads on AArch64**, and the project has formally entered the Enablement stage. Specifically, this update goes deep on four key layers:

**First, native compilation and build.**

The team cleared out the x86/amd64 default assumptions scattered across build scripts, Dockerfiles, and components, so the system correctly builds and emits output for the target architecture. To lower the bar for developers, the team also reworked the build chain: the Guest kernel build is now configured as a standalone target that supports cross-architecture cross-compilation. In other words, developers can produce an Arm kernel image from an x86 machine — no need to have Arm hardware on hand to get started.

**Second, native ecosystem deployment.**

To fit diverse compute environments, the team connected the full chain from deployment-package generation all the way to sandbox bring-up. By reworking the build toolchain, developers now need only a single `docker buildx` command to produce multi-architecture images that target both x86 and Arm — letting Cube Sandbox slot into existing CI/CD pipelines seamlessly.

**Third, smooth startup.**

To address the underlying architectural differences between Arm and x86, the team refactored the boot path and I/O logic. By introducing **UEFI firmware** to replace the legacy SeaBIOS, and through deep adaptation of the Hypervisor layer, the team made sure that sandboxes boot as quickly and as stably on Arm servers as they do on x86.

**Fourth, efficient runtime at a native level.**

The two teams stepped out of the traditional CPU-benchmark mindset and instead focused on the metrics that Agent production environments actually care about: **cold start, snapshot creation and rollback, high-concurrency creation, and per-host deployment density** — making sure Cube Sandbox doesn't just "run" on Arm, but runs *fast* and runs *steady*.

If "runs" is the floor, "usable" is the ticket handed to developers. Crossing this line means developers can now weave Cube Sandbox into real production workflows.

## 3. How Cross-Architecture Was Made Real

This path was so challenging because what Cube Sandbox had to port wasn't a normal application — it was an entire sandbox runtime system designed for AI Agents. It spans the external build and image artifacts, but also the Guest Kernel and Guest Agent inside the sandbox, and underneath that the virtualization and security-isolation layers.

To deliver the best possible results on "Agent-native metrics," the two teams focused on the following key technical problems:

**First, eliminate x86 dependencies and hardcoded assumptions.**

A lot of infrastructure projects, over years of evolution, end up treating x86 as the default environment. The first step in Cube Sandbox's port was to dismantle the hardcoded `x86_64` / `amd64` settings in build scripts, Dockerfiles, and components. That includes making Go components auto-track the host architecture as their build target, and filling in the **AArch64**-specific compile headers for the bytecode generation logic in the network module — so the project truly has cross-architecture build and multi-architecture output capability.

**Second, bridge the hypervisor's underlying-architecture gap.**

This was the most complex piece of the adaptation — touching the I/O system, the boot path, and the security mechanism, all in one go:

- **I/O system architectural transition:** The Guest-to-Host control-notification channel was rewritten from legacy PIO (Programmed I/O) to **MMIO (Memory-Mapped I/O)**, to match Arm architecture characteristics.
- **Boot path reconstruction:** x86 machines boot through lightweight SeaBIOS; for Arm, the team reworked the boot logic so UEFI firmware can be used normally.
- **Seccomp sandbox alignment:** Filter rules were rewritten to account for the syscall-number differences between x86 and **AArch64**, so the security-isolation mechanism stays effective across multi-architecture environments.

**Finally, re-validate the virtualization and security chain.**

Cube Sandbox's core value is hardware-level isolation for Agents, and that leans on KVM, the hypervisor, and other low-level capabilities. Just compiling cleanly isn't enough — this round of changes spans the virtualization layer, the network module, and the build toolchain, across multiple subsystems.

During the multiple rounds of cross-review between the two teams, a subtle bug was caught and fixed that would have broken the base-library path. This kind of repeated calibration on low-level details is what guarantees stable operation in multi-architecture environments.

This is also why the Arm engineering team went deep on the engagement. They didn't stop at the code merge — they focused on establishing a **bootable, communicative, verifiable compatibility chain on AArch64**, so that Cube delivers the same determinism and stability on heterogeneous platforms as it does on native ones.

## 4. Running Your First Arm Sandbox on Cube

**Environment requirements:**

- **Hardware:** AArch64 Linux server (a physical machine with virtualization extensions enabled)
- **OS:** OpenCloudOS 9 (kernel 6.6) recommended
- **Must run as root + Docker daemon running**

> ⚠️ **Note:** PVM nested virtualization is not yet supported on Arm. Please use a physical machine / bare-metal with native KVM (cloud VMs must have Arm nested virt enabled).

### Step 1: Download and install

```bash
# 1. Download the ARM64 one-click bundle
wget https://github.com/TencentCloud/CubeSandbox/releases/download/v0.5.0/cube-sandbox-one-click-v0.5.0-arm64.tar.gz
# Users in China can use the mirror:
wget https://cnb.cool/CubeSandbox/CubeSandbox/-/releases/download/v0.5.0/cube-sandbox-one-click-v0.5.0-arm64.tar.gz

# 2. Extract and enter the directory
tar -xzf cube-sandbox-one-click-v0.5.0-arm64.tar.gz
cd cube-sandbox-one-click-v0.5.0-arm64

# 3. Generate the config file (defaults work for most scenarios)
cp env.example .env
#    If eth0 is not your primary NIC, edit .env to set the IP explicitly:
#      CUBE_SANDBOX_NODE_IP=<your-node-ip>
#    If the default CIDR 192.168.0.0/18 conflicts with your host network, change it too:
#      CUBE_SANDBOX_NETWORK_CIDR=<your-available-cidr>

# 4. One-click install
sudo ./install.sh
```

### Step 2: Deployment verification & environment variables

```bash
# Health check
sudo ./smoke.sh

# Set 4 environment variables on the client (for local testing you can run everything on the same box)
export CUBE_TEMPLATE_ID=<your-template-id>
export E2B_API_URL=http://<target-machine-ip>:3000
export E2B_API_KEY=e2b_000000                 # any local string is fine
export SSL_CERT_FILE=/root/.local/share/mkcert/rootCA.pem
```

### Step 3: Spin up your first sandbox

```python
# Use the e2b-code-interpreter SDK to spin up your first ARM sandbox
import os

from e2b_code_interpreter import Sandbox

with Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"]) as sbx:
    print(sbx.run_code("import platform; print(platform.machine())"))
    # Expected output: aarch64
```

For the full set of configuration options, the deployment flow, and detailed instructions, see the official docs → [https://cubesandbox.com/guide/self-build-deploy.html](https://cubesandbox.com/guide/self-build-deploy.html)

## 5. Closing

Back to the engineering itself, the key thing about Cube Sandbox v0.5.0 isn't just "supports Arm" — it's that it has systematically crossed the threshold barriers that an Agent sandbox must clear in an **AArch64** environment, so that Arm is no longer merely a compatibility target for Cube Sandbox, and is starting to become a real, buildable, bootable, verifiable runtime option for Agent secure execution.

For teams building AI Agent workflows, Agentic RL training platforms, or code-execution services on Arm servers, the current version already provides a viable starting point. As cold start, snapshot rollback, concurrent creation, and deployment density continue to improve, Cube Sandbox's production value in heterogeneous compute scenarios will only grow.

The project is open source on GitHub — feel free to read the code, file an Issue, or contribute via PR.

- **GitHub repo:** [https://github.com/TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox)

If you like what you see, a Star 🌟 would mean a lot.
