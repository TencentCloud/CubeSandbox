---
title: 产品简介
description: CubeSandbox 是专为 AI Agent 打造的开源 MicroVM 沙箱基础设施，基于 RustVMM + KVM，具备亚 60ms 冷启动、硬件级强隔离、E2B SDK 无缝兼容，以及 PVM、裸金属与 Kubernetes 多形态部署能力。
---

# 产品简介

CubeSandbox 是一款**专为 AI Agent 打造的开源沙箱基础设施** —— 既能安全执行不可信代码，也能托管长驻 Agent 进程与有状态服务。基于 RustVMM + KVM 构建，每个沙箱都是具备独立内核的硬件隔离 MicroVM，冷启动不到 60ms，单实例 VMM 额外内存消耗不足 5MB，并同时原生支持 `x86_64` 与 `ARM64` 双架构。核心系统开源前已在腾讯云生产环境规模化验证。

## 核心优势

### 极速启动与硬件级隔离

* **⚡ 亚 60ms 冷启动 · 超高密度**：基于预快照池化与 CubeCoW 写时复制，单并发冷启动平均 **41ms**；50 并发创建平均 **92.4ms**、P95 **121.9ms**，吞吐达 **512 个/秒**（裸金属 [CubeBench v0.7.3](../blog/posts/2026-10-28-cubebench-073.md) 实测）。单实例 VMM 额外内存开销 **< 5MB**（空载完整沙箱单 VM 均摊开销约 18–25 MiB），单节点可稳定承载数千实例。详见[性能基准](./performance-benchmark.md)。

* **🔒 独立内核硬件隔离**：每个沙箱拥有独立的 Guest OS 内核，彻底消除共享内核带来的容器逃逸风险。KVM + eBPF 构成硬件与内核级双重安全边界，可安全执行不可信的 LLM 生成代码。

* **⚙️ x86_64 与 ARM64 全栈原生支持**：从底层 VMM、Guest 内核、基础镜像到自动化部署链路，全栈原生支持 `x86_64` 与 `aarch64（ARM64）` 架构。

### 面向 Agent 的生命周期与状态原语

* **🤖 不只是代码执行，更能托管 Agent 与有状态服务**：同一套生命周期原语同时覆盖一次性代码执行、完整有状态服务栈（Redis、MySQL、浏览器、桌面、开发环境），以及长期运行的 Agent 进程（支持通过 [AgentHub（Preview）](./digital-assistant.md) 托管 OpenClaw 等数字助手）。

* **⏸️ 自动暂停与百毫秒级恢复**：空闲沙箱自动快照挂起以释放计算资源，下次请求到达时透明恢复 —— 百毫秒级唤醒，调用方无感知；并支持沙箱内服务异常时的[自动重启策略](./lifecycle.md)。

* **📸 快照 · 克隆 · 回滚**：基于 CubeCoW Copy-on-Write 引擎与增量脏页追踪实现百毫秒级检查点，支持回滚到任意保存状态、从同一检查点分叉多条探索分支，以及更低存储成本的仅文件系统（FS-only）快照。详见[快照、回滚与克隆](./snapshot-rollback-clone.md)。

* **🌐 跨机暂停与恢复（Preview）**：配合 [JuiceFS](./cross-node-snapshot/juicefs.md) 或 [S3](./cross-node-snapshot/cubes3lvol.md) 共享存储后端，已暂停的沙箱可在集群内任意兼容节点恢复，也可从快照在任意节点创建新沙箱 —— 节点维护或故障不再困住工作负载。详见[跨机迁移](./cross-node-snapshot/requirements.md)。

### 零信任网络、存储与生态兼容

* **🔌 无缝 E2B 迁移与多语言 SDK**：原生兼容 E2B SDK（覆盖至 E2B 2.51.0+，详见[E2B 兼容性与差异](./e2b-compatibility.md)），替换环境变量即可切换、业务代码零改动；同时提供官方 [Python](./tutorials/sdk/python.md)、[Go](./tutorials/sdk/go.md)、[Node.js](./tutorials/sdk/nodejs.md) SDK。

* **🛡️ 凭据保险箱与 L7 出口管控**：敏感密钥始终留在控制面，永不进入沙箱、模型上下文或日志。[CubeEgress](./security-proxy.md) 提供 L7 域名白名单、未授权出口即时阻断、凭据动态注入与完整审计日志，配合[网络策略](./network-policy.md)与[沙箱间互访](./sandbox-to-sandbox.md)满足企业级安全隔离。

* **💾 解耦式 Volume 存储框架**：提供兼容 E2B 标准的 [Volume 框架](./volume.md)，支持以[插件形式](./volume-plugin.md)接入 S3、COS、JuiceFS、NFS 等自定义后端存储，实现存储与沙箱生命周期解耦及多沙箱数据共享。

### 极简部署与生产级运维

* **🚀 全场景开箱即用**：提供一键部署脚本，几分钟即可拉起完整环境。支持在无 `/dev/kvm` 的普通云服务器上通过 [PVM](./pvm-deploy.md) 部署，也支持[裸金属部署](./bare-metal-deploy.md)、[多机集群部署](./multi-node-deploy.md)、[Kubernetes（Helm）部署](./kubernetes/)、[混合部署（K8s 控制面 + 物理机/虚拟机数据面）](./hybrid-deploy.md)与[腾讯云 Terraform 部署](./tencentcloud-terraform-deploy.md)。

* **📦 模板系统**：一步将 [OCI 镜像转化为模板](./tutorials/template-from-image.md)，支持模板别名、将运行中沙箱提交为模板、从模板商店安装预设及多节点自动分发。详见[模板概览](./templates.md)。

* **🖥️ Web 管理控制台与可观测性**：内置 [WebUI 控制台](./webui.md)（安装后直接访问 `:12088`），可视化管理沙箱、模板、节点与版本矩阵；提供开箱即用的 [Prometheus / Grafana 指标、日志与链路追踪](./observability.md)，以及[可扩展调度插件](./scheduler-plugin.md)与[模板级 QoS](./qos.md)。

## 5 行代码无缝接入

已有 E2B 项目无需修改业务代码，只需将请求指向你的 CubeSandbox 服务即可切换到私有化硬件隔离沙箱：

```bash
export E2B_API_URL="http://127.0.0.1:3000"
export E2B_API_KEY="e2b_000000"
```

```python
from e2b_code_interpreter import Sandbox  # 直接沿用 E2B SDK

with Sandbox.create(template="<你的模板ID>") as sandbox:
    result = sandbox.run_code("print('Hello from CubeSandbox!')")
    print(result)
```

完整部署与运行步骤见[快速开始](./quickstart.md)，API 兼容范围见[E2B 兼容性与差异](./e2b-compatibility.md)。

## 典型适用场景

* **代码解释器与 Coding Agent**：为模型生成的代码、Shell 命令与研发工具提供强隔离执行环境，已支持 [Claude Code](./integrations/claude-code.md)、[LangGraph](./integrations/langgraph.md)、[OpenAI Agents SDK](./integrations/openai-agents-sdk.md)、[Google ADK](./integrations/google-adk.md) 等主流生态（详见[框架与工具集成](./integrations/)）。
* **浏览器与桌面自动化（Computer Use）**：在独立 MicroVM 中运行无头 Chromium（Playwright）或完整 [Ubuntu 桌面](./integrations/ubuntu-desktop.md)，会话之间彻底隔离。
* **常驻有状态 Agent 与云端工作空间**：借助自动暂停/恢复、Volume 持久化与安全出口代理，长期托管开发环境、数据库与专属 Agent 实例。
* **大规模并发评测与 RL Rollout**：利用亚 60ms 启动、高密度驻留与快照克隆/回滚，高效支撑 SWE-bench 自动化评测与强化学习并行采样。

更多真实业务落地实践，请参阅[应用案例](./usecases/)与[示例项目](./tutorials/examples.md)。

## 方案对比

| 指标 | Docker 容器 | 传统虚拟机 | CubeSandbox |
| --- | --- | --- | --- |
| **隔离级别** | 低（共享内核 Namespace） | 高（独立内核） | **硬件级强隔离（独立内核 + eBPF）** |
| **启动速度** | 约 200ms | 秒至分钟级 | **亚 60ms（单并发均值 41ms）** |
| **内存开销** | 低（共享内核） | 高（完整 Guest OS） | **超低（VMM < 5MB，CoW 共享页）** |
| **单机部署密度** | 高 | 低 | **高（单节点可达数千实例）** |
| **自动暂停 / 恢复** | 需自行编排 | 手动挂起，恢复较慢 | **✅ 平台原生托管（百毫秒级恢复）** |
| **快照 / 克隆 / 回滚** | 仅文件系统层，难留内存态 | 支持（秒至分钟级） | **✅ 百毫秒级（全状态 / FS-only）** |
| **跨机暂停 / 恢复** | 需自行迁移 | 需共享存储与热迁移方案 | **✅ JuiceFS / S3 后端（Preview）** |
| **E2B SDK 兼容** | 无 | 无 | **✅ 即插即用（零代码改动）** |
| **Agent 托管** | 需自行搭建 | 需自行搭建 | **✅ 原生支持（AgentHub，Preview）** |

## 下一步

### 快速上手

* [快速开始](./quickstart.md) — 四步从零部署并运行第一个隔离沙箱。
* [E2B 兼容性与差异](./e2b-compatibility.md) — 逐项核对 E2B SDK 兼容版本与扩展能力。
* [示例项目](./tutorials/examples.md) / [框架与工具集成](./integrations/) — 代码执行、浏览器、LangGraph、Claude Code 等端到端示例。

### 选择部署方式

* [PVM 部署](./pvm-deploy.md) — 在无 `/dev/kvm` 的普通云服务器上快速部署。
* [裸金属部署](./bare-metal-deploy.md) / [本地构建部署](./self-build-deploy.md) — 原生 KVM 物理机或源码构建单机部署。
* [多机集群部署](./multi-node-deploy.md) — 横向扩展到多节点生产集群。
* [Kubernetes 部署](./kubernetes/) / [混合部署](./hybrid-deploy.md) / [腾讯云集群（Terraform）](./tencentcloud-terraform-deploy.md) — 使用 Helm、K8s 控制面 + 物理机/虚拟机数据面混合架构或 Terraform 自动化部署。

### 深入了解与运维

* [架构概览](../architecture/overview.md) — 了解控制面、数据面与核心组件交互。
* [沙箱生命周期](./lifecycle.md) / [快照、回滚与克隆](./snapshot-rollback-clone.md) / [跨机迁移](./cross-node-snapshot/requirements.md) — 掌握核心状态原语。
* [从 OCI 镜像制作模板](./tutorials/template-from-image.md) / [Volume 使用](./volume.md) — 定制镜像环境与持久化存储。
* [出网安全代理（过滤与凭据注入）](./security-proxy.md) / [WebUI 控制台](./webui.md) / [沙箱可观测性部署](./observability.md) — 安全、可视化管理与监控接入。
* [数字助手（AgentHub）](./digital-assistant.md) / [应用案例](./usecases/) — 托管 AI Agent 实例与真实生产实践。
