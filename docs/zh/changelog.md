# 更新日志

## 2026.05.18 发布 v0.2.2

### 🛡️ 安全修复

- **`hypervisor`: `vmm-sys-util` 升级至 0.12.1**（CVE-2023-50711, GHSA-875g-mfp6-g7f9）：`FamStructWrapper::deserialize` 未验证 header 长度与柔性数组长度是否匹配，可导致 safe Rust 代码触发越界内存访问。现已固定为 hypervisor workspace 所有 crate 共享的版本。
- **`agent` / `hypervisor`: `bytes` 和 `env_logger` 安全依赖升级**，作为同批依赖刷新的一部分。
- **回滚 `time` crate 升级（CVE-2026-25727）**：CubeSandbox 仅使用 `Rfc3339` 进行时间戳格式化输出，从不对不可信输入执行 `Rfc2822` 解析，攻击面不可达。该升级已回滚，待 MSRV 就绪后单独跟进处理。

### 🛠️ 关键修复

- **修复 CubeMaster 模板镜像任务重复创建问题**：在 `template_image_job` 表新增 `request_id` 字段并添加 `(request_id, operation)` 唯一索引，使任务提交具备幂等性，彻底消除并发或重试 API 调用引发的重复构建任务。
- **修复 `cubecli exec` 在 stdin EOF 时的 nil 指针 panic**：`StdinCloser.Read` 在 stdin 到达 EOF 时触发 nil 指针解引用，导致 exec 生命周期被静默中断。修复后改用 `errors.Is(err, io.EOF)` 进行正确的错误包装比对，shim 日志现可正常输出成对的 exec 生命周期记录。
- **修复 PVM 模板的 ext4 artifact 运行时文件物化问题**：简化 `RefreshArtifactRuntimeFiles`、`validateArtifactRuntimeFilesPresent` 和 `ensureArtifactRuntimeFiles`，使其仅处理内核文件；将 `copyKernelFileAtomically` 重命名为 `CopyFileAtomically`，支持在包外复用。

### ✨ 功能增强

- **E2B 兼容默认暴露端口**：默认沙箱暴露端口由 8080/32000 改为 **49983**，与 E2B 沙箱协议对齐。`CubeMaster` 成为默认端口的唯一权威来源，`Cubelet` 和 `network-agent` 中的硬编码默认值已移除。
- **Cubelet 存储插件支持配置 `cmdTimeout`**：在存储插件 TOML 配置中新增可选 `cmd_timeout` 字段，替代原有硬编码的 3 秒超时，允许运维人员在高并发负载下提高大文件 ext4 操作的超时限制，无需重新编译。字段缺省时行为不变。
- **Cubelet `newExt4RawByReflinkCopy` 错误诊断增强**：错误信息现在包含操作耗时、目标/基础文件大小及可用磁盘空间，格式为 `[step=N/4 cmd="…" elapsed=…ms target=size=… base=size=… free=…B]`。
- **部署脚本支持从 `.env` 同步 CubeMaster 自定义端口**：`cubemaster.yaml` 引入 `__CUBE_SANDBOX_MYSQL_PORT__` / `__CUBE_SANDBOX_REDIS_PORT__` 占位符，由 `install.sh` 从 `.env` 文件自动替换，无需手动修改 YAML。

### ⚙️ 工程改进

- **`cubecli`: 移除废弃的 `listmd` 命令**：删除无法访问的 `listmd` 子命令及其 128 行实现代码。

### 🤖 CI / DevOps

- **Claude 驱动的 AI 代码审查与 issue 分类自动化**：在 `.agents/agents/` 下引入五个 AI 审查 Agent（代码质量、性能、安全、测试覆盖率、文档准确性）。自动化工作流负责 PR 代码审查、重复 issue 检测和 issue 标签分类。新增 `scripts/gh.sh` 和 `scripts/edit-issue-labels.sh` 辅助脚本。

### 📚 文档更新

- **新增 `CONTRIBUTING_zh.md`**：`CONTRIBUTING.md` 的完整中文翻译版本。
- **社区文档 PR 要求放宽**：`CONTRIBUTING.md` 和 `CONTRIBUTING_zh.md` 均改为支持单语种提交，双语文档为可选项。
- **计算节点端口分配范围文档化**：`docs/architecture/network.md`（中英文）新增三段端口范围说明：`10000–19999`（network-agent）、`20000–29999`（CubeProxy 沙箱访问）、`30000–65535`（CubeVS SNAT）。
- **新增社区文档章节**：VitePress 新增双语故障排查、用例和集成三个章节，并配置 CI 工作流强制执行双语一致性检查。
- **域名更新**：CNAME 从 `docs.cubesandbox.ai` 切换至 `cubesandbox.com`。
- **修复 `browser-sandbox` 示例**：新增缺失的 `load_dotenv()` 调用和 `python-dotenv` 依赖，确保 `.env` 变量在脚本运行前正确加载。
- **微信群二维码更新**。

---

## 2026.05.14 发布 v0.2.1

### 🌟 重大新功能

- **官方 Python SDK (`cubesandbox` v0.1.0)**：随仓库发布的第一方 Python SDK，位于 `sdk/python/`，与 CubeAPI OpenAPI 规范完全对齐。覆盖沙箱全生命周期管理（创建/连接/暂停/销毁/列表/健康检查）、代码执行（流式 stdout/stderr）、文件系统访问、直连传输及网络策略配置。附带 12 个完整示例、并发性能基准测试，76/76 测试全部通过。

### 🚀 性能优化

- **跳过 Cubelet 每次启动时的 SHA256 校验**：将 `SyncKernelFile` 拆分为 `EnsureKernelFilePresent`（按需拷贝，快速路径）和 `RefreshKernelFile`（强制刷新并校验），消除每次启动的高开销 SHA256 比对。在模板数量较多的主机上，正常启动延迟显著降低。
- **跳过 CubeMaster 冗余的 `docker pull`**：当源镜像已存在于本地时，跳过镜像拉取，消除模板构建时不必要的镜像仓库往返请求。

### 🛡️ 安全修复

- **`shim`: protobuf 3.4.0 → 3.7.2**（RUSTSEC，恶意构造的未知字段可导致栈溢出）。同步升级 `containerd-shim-protos`、`containerd-shim` 和 `nix`。
- **`cubeapi` / `agent` / `shim` / `hypervisor`: rand 0.8.5 → 0.8.6**（GHSA-cq8v-f236-94qc，修复 `ThreadRng` 重新播种时的健全性问题）。
- **`CubeVS`: `golang.org/x/net` → v0.38.0, `golang.org/x/sys` → v0.38.0**。
- **`network-agent`: `google.golang.org/grpc` → 1.79.3**。
- **`CubeAPI/examples`: `pygments` → 2.20.0**。

### 🛠️ 关键修复

- **修复 Seccomp 静默拦截所有系统调用**：`Seccomp` 初始化现在设置 `DefaultAction = ActAllow`，空系统调用列表直接短路为空操作，而非静默阻止所有调用。
- **修复 `shim` stderr 被错误路由到 stdout**：`Exec` 流转发路径中 stderr 错误调用了 stdout 的读取方法；现在 stderr 可被正确捕获并转发。
- **修复 `CubeProxy` 多 Worker 共享相同 PRNG 种子**：OpenResty Worker 现在在 `init_worker` 中以 `(ngx.now() * 1000 + ngx.worker.id())` 为每个 Worker 独立播种，避免缓存 TTL 抖动失效和同步缓存过期风暴。
- **修复开发环境同步覆盖 `cube-shim` 软链接**：`cube-runtime` 和 `containerd-shim-cube-rs` 现在写入 `${TOOLBOX_ROOT}/cube-shim/bin`，保留工具箱软链接布局。
- **修复 HTTPS-only 镜像源导致 Dockerfile 构建失败**：在切换 APT 源至内部镜像之前先安装 `ca-certificates`，避免 TLS 证书缺失导致引导失败。

### ✨ 功能增强

- **`cubemastercli tpl watch` — 阶段性进度输出**：将原有的多行全量状态刷新替换为简洁的 `[N/7] PHASE` 进度行加终端摘要；在 CI 日志中更加友好。
- **IPAM — 全面优化与可靠性改进**（Cubelet + network-agent）：基于 `net/netip` 重写校验逻辑；通过 `encoding/binary.BigEndian` 简化 IP 与索引互转；为 `Allocate` / `Release` / `Assign` 添加边界检查和安全性限制；所有 IPAM 方法增加 `nil` 防护；明确文档化保留地址语义；新增全面的表驱动测试和并发测试。

### ⚙️ 工程改进

- **示例重组为独立顶级目录**：从 `CubeAPI/examples/` 迁移至顶层 `examples/`，新增独立的 `host-mount` 和 `network-policy` 目录（各自附带 README）；注释翻译为英文。
- **`cube-bench` 提升为 `examples/cube-bench`**：现为独立 Go 模块，带自己的 Makefile。
- **Go 工具链对齐**：`CubeVS` 和 `network-agent` 升级至 Go 1.24.8，与 Cubelet / CubeMaster 保持一致。
- **`cubecli` 国际化**：`benchrun.go` 中残余的中文使用说明翻译为英文。
- **Docker 构建上下文清理**：`Makefile` 构建器镜像现在从 `./docker` 构建，而非仓库根目录。
- **Alpine 镜像源切换**：APK 仓库从 `dl-cdn.alpinelinux.org` 切换至 `mirrors.tencent.com`。

### 🤖 CI / DevOps

- **DCO 检查工作流**：新增专用 PR 门禁，当任何非合并提交缺少有效 `Signed-off-by` 签名时阻断合入。
- **GitHub ARC (Actions Runner Controller) 支持**：自托管 ARC 运行器已接入内核/包构建工作流。
- **消除重复 PR 检查**：多个工作流的 `push` 触发器现在仅限 `master` 分支；PR 验证仅通过 `pull_request` 事件运行 — CI 成本减半。
- **`sync-to-cnb`**：改用 `CNB_GIT_PASSWORD` 密钥。

### 📚 文档更新

- **部署指南重写**：PVM 和裸金属现为首选部署路径。
- **OpenCloudOS 9 上 PVM 快速部署**：`pvm-deploy.md` 新增分步操作章节。
- **"关于我们"页面**：新增中英文版本，并配置 VitePress 导航。
- **项目 README 新增 X (Twitter) 链接**。
- **文档细节修正**：修正 Python 导入路径和架构图间距。
- **`README_zh.md` 更新微信/助手二维码**。

---

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
