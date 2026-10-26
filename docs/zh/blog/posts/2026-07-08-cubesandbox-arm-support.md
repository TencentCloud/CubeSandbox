---
title: "Cube Sandbox正式支持Arm架构！腾讯云与Arm联手解锁Agent多架构算力"
date: 2026-07-08
author: InfoQ
description: "7月3日，由腾讯云开源的 AI Agent 安全沙箱 Cube Sandbox v0.5.0 正式发布。Arm 架构原生支持作为核心特性之一，已在 Arm 工程团队的重点投入下合入开源主线。开发者现在可以在 AArch64 服务器上，实现从原生构建、部署、启动到运行典型沙箱负载的完整流程。"
featured: false
---

# Cube Sandbox正式支持Arm架构！腾讯云与Arm联手解锁Agent多架构算力

<!-- 图片 -->

7月3日，由腾讯云开源的 AI Agent 安全沙箱 **Cube Sandbox v0.5.0** 正式发布。作为专为 LLM 应用设计的硬件级隔离运行底座，v0.5.0 此次面向生产环境进行了关键升级，围绕"稳、省、广"补齐了多项能力；其中，**Arm 架构原生支持**作为核心特性之一，已在 Arm 工程团队的重点投入下合入开源主线。（[V0.5.0 详情可见：Cube v0.5.0发布：自动暂停 · Arm支持· 一键集群部署，把沙箱送进生产](https://mp.weixin.qq.com/s?__biz=MzYzNDkyMDU3MQ==&mid=2247483839&idx=1&sn=bd0f059a3e3be0654f1c1ca89e4e8107&scene=21#wechat_redirect)）

开发者现在可以在 **AArch64** 服务器上，实现从原生构建、部署、启动到运行典型沙箱负载的完整流程。

为打通从底层构建到运行时的关键路径，腾讯云 IaaS 前沿技术团队与 Arm 工程团队开展了为期数月的深度联合研发，重点攻克 **AArch64 构建、Guest Kernel、运行环境及部署链路**等关键目标。

随着算力供给加速迈向"x86 + Arm"多架构并存，行业亟需 Agent 运行底座突破传统架构边界。v0.5.0 将沙箱的核心隔离能力扩展至 Arm 生态，不仅是生产级交付能力的提升，更为横跨云、边、端多元异构场景下的 Agent 提供了可靠、灵活的运行支撑。

## 一、当算力平台走向异构

如果将"AI Agent 大规模上生产"视为对底层基建能力的一次大考，你会发现有两个变量正以肉眼可见的速度发生演变：

**首先是负载侧。** 当前的 Agent 已不再是简单的一次性脚本，而是演变为数十、数百次串行或并发的工具调用。每一次调用都意味着一次沙箱的启停、一段隔离的运行以及一次结果的回收。

这种负载模式对底层基础设施提出了极致需求：既要有**极高的算力密度**，又要有**极快的弹性响应**。

**其次是基础设施侧。** AI 时代的算力供给正快速从单一架构走向"x86 + Arm"双轨并行。凭借 Arm Neoverse 平台在云端 AI 工作负载方面的能效和性能优势，Arm 架构正在获得行业的广泛采用。随着 Amazon Graviton、Google Cloud Axion、Microsoft Azure Cobalt 等 Arm 计算平台持续扩展，以及诸多国产 Arm 处理器快速落地部署，Arm 生态正加速发展。**"AI 跑在 Arm 上"**已从尝鲜话题转变为**真实的预算投入**。

把这两个变量叠加在一起看，结论是清晰的：Agent 沙箱不能局限于 x86，它必须在多架构服务器环境中补齐部署能力。更进一步，它不应止步于在 Arm 上"跑通"，更要在这一平台上跑出**与 x86 同等量级甚至更优**的性能表现。

这正是 Cube 团队与 Arm 工程团队联合研发的起点。

## 二、从"能跑"到"原生"

从 Arm 工程团队最初介入，到代码正式合入主仓库，这场联合研发历时数月。真正推动项目从"能跑"迈向"能用"的，是最后阶段的高强度打磨——仅本轮合入主线的改动，便历经二十余次提交。

这一版的交付核心在于：Cube Sandbox 已具备在 **AArch64** 平台上**原生部署、编译、启动和运行典型沙箱工作负载**的全链路能力，项目正式进入落地阶段。具体而言，此次更新深入到了四个关键层面：

**一是原生编译与构建。**

团队清理了散落在构建脚本、Dockerfile 及组件中的 x86/amd64 默认假设，让系统能根据目标架构正确构建与输出。为降低开发门槛，团队还特别优化了构建链路，将 Guest 内核构建配置为支持跨架构交叉编译的独立目标。也就是说，开发者在 x86 机器上就能产出 Arm 内核镜像，无需依赖 Arm 架构设备也能轻松上手。

**二是原生生态的部署。**

为了适配多元算力环境，团队打通了从部署包生成到沙箱拉起的完整链路。通过优化构建工具链，开发者现在仅需一条 `docker buildx` 命令，即可同时产出兼容 x86 与 Arm 的多架构镜像，让 Cube Sandbox 能够无缝融入现有的 CI/CD 流程。

**三是平滑启动。**

针对 Arm 与 x86 在底层架构上的差异，团队重构了引导路径与 I/O 逻辑。通过引入 **UEFI 固件**替代传统的 SeaBIOS，以及对 Hypervisor 层进行深度适配，确保了沙箱在 Arm 服务器上能够像在 x86 上一样快速、稳定地启动。

**四是原生程度的高效运行。**

双方团队跳出了传统 CPU benchmark 的视角，转而聚焦 Agent 生产环境真正关心的指标：包括优化**冷启动、Snapshot 创建与回滚、高并发创建以及单机部署密度**等关键能力，确保 Cube Sandbox 在 Arm 架构下不仅能"跑起来"，更能"跑得快"、"跑得稳"。

如果说"能跑"是门槛，那么"能用"才是交付给开发者的入场券。这一步的跨越，意味着开发者已能将 Cube Sandbox 真正融入生产工作流。

## 三、如何实现跨架构

这条链路之所以极具挑战，在于 Cube Sandbox 要迁移的并非一个普通应用，而是一套面向 AI Agent 的沙箱运行体系。它既涉及外部构建和镜像产物，也涉及沙箱内部的 Guest Kernel、Guest Agent，以及更底层的虚拟化和安全隔离能力。

为了在"Agent 原生指标"上拿到极致的结果，双方团队重点解决了以下关键的技术难题：

**首先，消除对 x86 架构的依赖与硬编码假设。**

很多基建项目在长期演进中，都会把 x86 当成默认环境。Cube Sandbox 迁移的第一步，就是拆解构建脚本、Dockerfile 及组件中硬编码的 `x86_64` / `amd64` 设定。这不仅包括让 Go 组件的编译目标自动跟随宿主架构，还包括为网络模块的字节码生成逻辑补齐 **AArch64** 专属编译头文件，让项目真正具备跨架构构建与多架构输出的能力。

**其次，跨越 Hypervisor 的底层架构鸿沟。**

这是本次适配中最复杂的部分，涉及 I/O 系统、引导路径和安全机制的全面重构：

- **I/O 系统的架构跨越：** 将 Guest 对 Host 的控制通知通道从原来的 PIO（Programmed I/O）改写为了 MMIO（Memory Mapped I/O），以适配 Arm 架构特性；
- **引导路径重构：** x86 机器依靠轻量化 SeaBIOS 启动，而针对 Arm 架构，团队改造了启动逻辑，现在可以正常使用 UEFI 固件开机；
- **Seccomp 沙盒对齐：** 针对 x86 与 **AArch64** 的系统调用号差异，重写了过滤规则，确保安全隔离机制在多架构环境下依然有效。

**最后，重新验证虚拟化与安全链路。**

Cube Sandbox 的核心价值是为 Agent 提供硬件级隔离，这背后依赖 KVM、Hypervisor 等底层能力。单纯的编译通过并不够，本轮改动横跨虚拟化层、网络模块与构建工具链等多个子系统。

在双方团队的多轮交叉审查过程中，还发现并修正了一处导致基础库路径失效的隐蔽 Bug。这种对底层细节的反复校准，为系统在多架构环境下的稳定运行提供了保障。

这也是 Arm 工程团队深度介入的原因。它并未止步于代码合入，而是聚焦在 **AArch64** 环境中建立一条可启动、可通信、可验证的兼容性链路，确保 Cube 在异构平台上也能获得与原生环境一致的确定性与稳定性。

## 四、如何在 Cube 上运行你的第一台 Arm 沙箱

**环境要求：**

- **环境：** AArch64 架构的 Linux 服务器（支持虚拟化特性的物理机）
- **操作系统：** 推荐 OpenCloudOS 9（内核 6.6）
- **需在 root 用户下运行 + Docker 已运行**

> ⚠️ **注意：** Arm 上暂不支持 PVM 嵌套虚拟化，请使用原生 KVM 的物理机 / 裸金属（云 VM 需已开启 Arm Nested Virt）。

### Step 1：下载并安装

```bash
# 1. 下载 ARM64 one-click 包
wget https://github.com/TencentCloud/CubeSandbox/releases/download/v0.5.0/cube-sandbox-one-click-v0.5.0-arm64.tar.gz
# 国内用户可使用如下地址：
wget https://cnb.cool/CubeSandbox/CubeSandbox/-/releases/download/v0.5.0/cube-sandbox-one-click-v0.5.0-arm64.tar.gz

# 2. 解压并安装
tar -xzf cube-sandbox-one-click-v0.5.0-arm64.tar.gz
cd cube-sandbox-one-click-v0.5.0-arm64

# 3. 生成配置文件（大多数场景用默认值即可）
cp env.example .env
#    如果 eth0 不是主网卡，编辑 .env 显式指定 IP：
#      CUBE_SANDBOX_NODE_IP=<你的节点IP>
#    如果默认 CIDR 192.168.0.0/18 与本机网络冲突，同步改：
#      CUBE_SANDBOX_NETWORK_CIDR=<你的可用CIDR>

# 4. 一键安装
sudo ./install.sh
```

### Step 2：部署验证 & 环境变量设置

```bash
# 健康检查
sudo ./smoke.sh

# 在客户端设置 4 个环境变量（本机测试可全跑在同一台上）
export CUBE_TEMPLATE_ID=<你的模板ID>
export E2B_API_URL=http://<目标机IP>:3000
export E2B_API_KEY=e2b_000000                 # 本地任意字符串即可
export SSL_CERT_FILE=/root/.local/share/mkcert/rootCA.pem
```

### Step 3：拉起第一个沙箱

```python
# 用 e2b-code-interpreter SDK 拉起第一个 ARM 沙箱
import os

from e2b_code_interpreter import Sandbox

with Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"]) as sbx:
    print(sbx.run_code("import platform; print(platform.machine())"))
    # 预期输出：aarch64
```

更完整的配置项、部署流程和详细说明，参考官方文档 → [https://cubesandbox.com/zh/guide/self-build-deploy.html](https://cubesandbox.com/zh/guide/self-build-deploy.html)

## 五、结语

回到工程本身，Cube Sandbox v0.5.0 的关键并不只是"支持 Arm"，而是把 Agent 沙箱在 **AArch64** 环境中必须跨过的几道门槛逐一补齐，让 Arm 架构不再只是 Cube Sandbox 的一个兼容目标，而开始成为 Agent 安全执行环境中可构建、可启动、可验证的真实运行选项。

对于正在 Arm 服务器上构建 AI Agent 工作流、Agentic RL 训练平台或代码执行服务的团队来说，当前版本已经提供了一个可用起点。后续，随着冷启动、快照回滚、并发创建和部署密度等指标继续优化，Cube Sandbox 在异构算力场景中的生产价值也会进一步释放。

项目代码已在 GitHub 开源，欢迎查看代码、提交 Issue 或通过 PR 参与共建。

- **GitHub 仓库：** [https://github.com/TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox)

如果觉得 Cube 还不错，欢迎点个 Star 🌟 关注一下哦～
