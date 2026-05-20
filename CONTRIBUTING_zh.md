# 为 Cube Sandbox 贡献

感谢你对 Cube Sandbox 的关注与支持！本文档提供了参与贡献的相关指南和说明，帮助你快速上手。

## 贡献方式

- **报告 Bug** — 在 [GitHub Issue](https://github.com/tencentcloud/CubeSandbox/issues) 中提交问题，并附上复现步骤。
- **建议新功能** — 在 Issue 中描述你的使用场景和建议方案。
- **改善文档** — 修复错别字、优化表述或补充示例。
- **提交代码** — 修复 Bug、实现新功能或改进性能。

## 社区文档频道

除常规文档修缮外，我们在 `docs/guide/` 下维护了三个社区文档频道：

- **故障排障** — 部署与运维经验总结，存放于 [`docs/guide/troubleshooting/`](./docs/guide/troubleshooting/index.md) 和 [`docs/zh/guide/troubleshooting/`](./docs/zh/guide/troubleshooting/index.md)
- **应用案例** — 真实业务或生产环境的使用案例，存放于 [`docs/guide/usecases/`](./docs/guide/usecases/index.md) 和 [`docs/zh/guide/usecases/`](./docs/zh/guide/usecases/index.md)
- **生态集成** — 各框架或 Agent 的集成指南（每个框架一篇），存放于 [`docs/guide/integrations/`](./docs/guide/integrations/index.md) 和 [`docs/zh/guide/integrations/`](./docs/zh/guide/integrations/index.md)

### ⛺️ 社区文档 PR 要求

- **选择一种语言** — 每篇新增或更新的文章须提供 `docs/guide/<频道>/<slug>.md` 或 `docs/zh/guide/<频道>/<slug>.md` 其中的一个版本。
- **如果想要提供中英两种语言**：
  - **需使用相同文件名** — 文件名统一采用英文 kebab-case 格式，例如 `langchain.md` 或 `e2b-api-401-timeout.md`。
  - **保持 frontmatter 一致** — 两个语言版本应使用相同的 frontmatter 字段（`title`、`author`、`date`、`tags`、`lang`）。

- **从提供的模板开始** — 每个频道均包含一个 `_template.md` 模板文件，以及列有当前文章列表和使用说明的索引页，请以此为起点进行编写。

## 快速上手

### 环境要求

- 支持 KVM 的 Linux 系统（x86_64）
- Docker
- Go 1.21+
- Rust 1.75+（需包含 `x86_64-unknown-linux-musl` target）
- protoc（Protocol Buffers 编译器）

### 构建环境

Cube Sandbox 提供了基于 Docker 的构建镜像，以确保一致的构建环境：

```bash
# 构建构建镜像
make builder-image

# 进入构建容器的交互式 Shell
make builder-shell

# 构建所有 Go 组件（CubeMaster、Cubelet、network-agent）
make all

# 构建单个组件
make cubemaster
make cubelet
make agent
make shim
```

完整的构建目标列表请参见 [Makefile](./Makefile)。

### 项目结构

| 目录 | 语言 | 说明 |
|---|---|---|
| `CubeAPI/` | Rust | 兼容 E2B 的 REST API 网关 |
| `CubeMaster/` | Go | 调度器与集群管理 |
| `Cubelet/` | Go | 节点级沙箱生命周期管理 Agent |
| `CubeProxy/` | Go | 沙箱请求路由的反向代理 |
| `CubeShim/` | Rust | 连接 containerd 与 KVM MicroVM 的 Shim |
| `agent/` | Rust | 运行在每个沙箱内部的 Guest Daemon |
| `hypervisor/` | Rust | 基于 KVM 的 MicroVM 管理器（Cloud Hypervisor 分支） |
| `mvs/` / `CubeNet/` | Go | CubeVS 基于 eBPF 的网络隔离 |
| `network-agent/` | Go | 网络管理服务 |
| `deploy/` | Shell | 部署脚本与 Guest 镜像工具 |
| `examples/` | Python | SDK 示例与端到端使用场景 |
| `docs/` | Markdown | VitePress 文档站（中英双语） |

## 提交 Pull Request

1. **Fork** 仓库，并从 `main` 分支创建功能分支。
2. **修改代码** — 保持每个提交专注且原子化。
3. **测试** — 确保现有测试和 Lint 检查均能通过。
4. **添加测试** — 行为变更时请补充针对性的测试覆盖。
5. **更新文档** — 若改动影响用户可见的行为，请同步更新相关文档。
6. **发起 PR** — 描述改动的动机和内容，并关联相关 Issue。

### 提交组织规范

提交应保持逻辑清晰、相互独立：

- **每次提交只涉及一个组件** — 若改动跨越多个组件（如同时涉及 `CubeAPI` 和 `Cubelet`），请拆分为多个独立提交，每个组件单独提交。
- **保持提交原子性** — 每个提交应代表一个单一、完整的改动，能够被独立理解和审查。
- **重构与行为改动分开** — 不要将代码清理或重构与功能性改动混在同一提交中。
- **按逻辑顺序排列提交** — 当 PR 包含多个提交时，应使每个提交都基于前一个（例如：先提交基础设施改动，再提交依赖它的功能改动）。

### 提交信息规范

编写清晰的提交信息，说明改动的*原因*：

```
component: 改动的简短描述

更详细的说明，阐述改动的动机、权衡或背景。
Closes #123

Signed-off-by: Your Name <your.email@example.com>
```

摘要部分需以组件名称作为前缀（例如 `cubeapi:`、`cubelet:`、`docs:`、`shim:`）。

### 开发者来源证书（DCO）

所有提交**必须**包含 `Signed-off-by` 行，以证明你已阅读并同意[开发者来源证书（DCO）](https://developercertificate.org/)。这表明你有权在本项目许可证下提交该贡献。

在提交时使用 `-s` 参数自动添加：

```bash
git commit -s -m "component: 你的提交信息"
```

或在提交信息末尾手动追加以下内容：

```
Signed-off-by: Your Name <your.email@example.com>
```

不包含有效 `Signed-off-by` 行的提交将不会被接受。

### 代码风格

- **Go** — 遵循标准 `gofmt` 格式化规范及项目约定。
- **Rust** — 遵循 `rustfmt` 和 `clippy` 的建议。
- **文档** — 使用清晰简洁的语言，中英文文档应保持同步。

## 报告安全问题

如果你发现安全漏洞，请通过 [GitHub Security Advisories](https://github.com/tencentcloud/CubeSandbox/security/advisories) 进行负责任的披露，而不是在公开 Issue 中提交。

## 许可证

向 Cube Sandbox 贡献代码，即表示你同意你的贡献将以 [Apache License 2.0](./LICENSE) 进行许可。

## AI 生成代码政策

AI Agent **不得**添加 `Signed-off-by` 标签。只有人类才能合法地证明开发者来源证书（DCO）。人类提交者有责任：

- 审查所有 AI 生成的代码
- 确保符合许可证要求
- 添加自己的 `Signed-off-by` 标签以证明 DCO
- 对贡献内容承担全部责任
