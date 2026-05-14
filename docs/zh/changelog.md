# 更新日志

## 2026.05.07 发布 v0.2.0

### 🌟 重大新功能

- **新增 Web 管理控制台（Dashboard）**：全新可视化管理界面，支持集群概览、节点与沙箱状态查看、模板管理、API 密钥管理等核心功能；同步新增 CubeAPI Web 端点为 Dashboard 提供数据支撑。

- **新增 PVM 部署模式**：借助 PVM（Pagetable-based Virtual Machine），**普通云服务器无需裸金属，也无需嵌套虚拟化，即可完整运行 CubeSandbox**。腾讯云已在生产环境大规模部署并验证，相关改进已开源至 [OpenCloudOS 内核](https://gitee.com/OpenCloudOS/OpenCloudOS-Kernel.git)。

### ✨ 功能增强

- **CubeMaster 模板创建支持自定义 DNS**：`cubemastercli template` 命令新增 `--dns` 参数，支持在创建模板镜像时指定 DNS 服务器地址。

### 🛠️ 关键修复

- **修复**磁盘 QoS（blk_qos）配置完全失效问题：Cubelet 读取 QoS annotation 时使用了错误的 key，导致沙箱磁盘 IOPS/带宽限速静默不生效；修复后配置按预期生效。

- **修复** Host Mount 请求被静默丢弃问题：CubeAPI 写入 `host-mount` annotation 时 key 与 CubeMaster 读取时的 `hostdir-mount` 不一致，导致所有宿主机目录挂载请求被忽略；修复后两侧 key 对齐，功能恢复正常。

- **修复** Cubelet 挂载命名空间无法接收宿主机 mount 事件：Cubelet 以私有模式创建挂载命名空间，导致宿主机后续挂载无法传播至 Cubelet；修复后改为 slave 模式，宿主机挂载事件单向传播，沙箱 host-mount 功能完整可用。

- **修复** DeadGC 误判 pause 中的沙箱导致其永久冻结：`scanDeadContainer` 在沙箱处于 pausing/paused 状态时向 shim 发起 state 查询，因 shim 持有互斥锁而超时，Cubelet 将状态标记为 UNKNOWN，沙箱无法恢复；修复后 DeadGC 主动跳过此类沙箱。

### 🌐 网络改进

- **禁用 virtio-net TAP 网络卸载（TSO/UFO/CSUM）**：此前 hypervisor 向 guest 通告了多项网络硬件卸载能力，guest 发出的 CHECKSUM_PARTIAL 包在宿主机 NIC 不支持对应卸载时会导致网络异常，甚至影响同宿主机其他流量；修复后 hypervisor 不再通告这些能力，guest 自行处理 checksum 与分段。

### ⚙️ 工程改进

- **Cubelet CLI 日志标准化**：将 `cubecli` 各子命令中遗留的 `myPrint` 自定义输出统一迁移为标准结构化日志。
- **废弃代码清理**：移除 CubeMaster affinityutil 测试中不再使用的 `AppId` 字段。

### 📚 文档更新

- **新增 PVM 部署指南**（中英双语）：包含 PVM host kernel 下载安装、GRUB 配置、模块加载及验证的完整流程。
- **优化快速入门**：说明普通云服务器可通过 PVM 部署，无需裸金属。
- **更新 code-sandbox-quickstart 示例 README**（中英双语）。

---

## 2026.04.27 发布 v0.1.2

### 🛠️ 关键修复

- **修复**了当沙箱模板不存在时，CubeMaster 返回 5xx 而非 4xx 的问题。
- **修复**了 v0.1.1 一键部署过程中 SSL RootCA 证书缺失的问题。
- **修复**了 v0.1.1 部署过程中 CubeProxy 镜像构建失败的问题。

## 2026.04.24 发布 v0.1.1

### 🛠️ 关键修复

- **修复**了模板重建时未使用最新 vmlinux 的问题，提升了沙箱环境稳定性。
- **更新**了一键安装脚本，支持非 eth0 网卡，解决了 CubeProxy CA 证书稳定性问题。
- **禁用**了初始化阶段主网卡上的 GRO，以提升网络稳定性。
- **修复**了模板清理时目标不存在场景下的错误处理，确保返回正确错误。

### ✨ 新功能

- **新增** `cubebox destroy` 命令，支持通过 CLI 删除沙箱。
- **新增** OpenAI Agents SDK（含 Code Interpreter）集成示例。

### 📚 文档更新

- **重写**了 HTTPS 与域名配置文档，补充了泛域名 DNS 记录说明。

### ⚙️ 工程改进

- **实现**了多组件并行 CI 构建流水线，优化构建效率。
- **新增**了将 GitHub Release 资产自动同步到 `cnb.cool/CubeSandbox/CubeSandbox` 的能力。

## 2026.04.20 发布 v0.1.0

### Cube Sandbox 首次开源发布

**面向 AI Agent 的极速、高并发、安全且轻量化沙箱。**

### 核心亮点

Cube Sandbox 是一款基于 RustVMM 与 KVM 构建的高性能、开箱即用的安全沙箱服务。它既支持单机部署，也可以轻松扩展到多机集群。Cube Sandbox 对外兼容 E2B SDK，能够在 60ms 内创建具备完整服务能力的硬件隔离沙箱环境，同时将单实例内存开销控制在 5MB 以下。

- **极致冷启动：** 基于资源池化预置与快照克隆技术，跳过耗时的初始化流程。可服务沙箱端到端冷启动平均耗时 < 60ms。
- **单机高密部署：** 借助 CoW 技术实现极致内存复用，并通过 Rust 重构与深度裁剪运行时，将单实例内存开销降至 5MB 以下，单机可承载数千 Agent。
- **真正的内核级隔离：** 每个 Agent 运行在独立 Guest OS 内核中，避免容器逃逸风险，可安全执行任意大模型生成代码。
- **零成本迁移（E2B 无缝替换）：** 原生兼容 E2B SDK 接口，仅需替换一个 URL 环境变量，无需业务代码改动。
- **网络安全：** 基于 eBPF 的 CubeVS 在内核态实现严格的沙箱间网络隔离，并支持细粒度出站流量过滤策略。

### 生产可用

**在开源之前，Cube Sandbox 已在腾讯云生产环境经历大规模验证，稳定可靠。** 在它正式开源前，就已经支撑真实 AI Agent 业务负载并服务真实用户。

在真实生产部署中，单台物理机可在数分钟内拉起数以万计的沙箱实例。

我们今天开源的不是一个原型，而是一套已经通过真实规模考验的生产级基础设施。

### 致每一位贡献者：过去、现在与未来

在这套代码公开之前，它就已经在完成自己的使命：毫秒级启动沙箱、在内核级隔离 Agent 负载，并在腾讯云真实生产流量下稳定运行。这一切并非偶然。

今天，我们打开这扇门。你们共同塑造的高性能 Agent 基础设施，将走向整个社区，属于每一位相信「安全、即时、轻量的代码执行应当开源且可自托管」的开发者。

致过去的贡献者：你们打下了地基。  
致未来的贡献者：你们将把地基发展成生态。

开源因你们而闪耀！
