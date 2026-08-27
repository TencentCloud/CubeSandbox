# 示例项目

以下示例展示 CubeSandbox 的主要功能与生态集成。运行前请查看各项目 README 中的前置条件和配置步骤。

## 快速入门与镜像

| 示例 | 说明 |
| --- | --- |
| [代码沙箱快速入门](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/code-sandbox-quickstart) | 创建沙箱，执行 Python 和 Shell 命令，操作文件，并体验核心 E2B 兼容 API。 |
| [浏览器沙箱（Playwright）](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/browser-sandbox) | 在 MicroVM 中运行无头 Chromium，通过 CDP 使用 Playwright 远程控制浏览器。 |
| [自定义 nginx 镜像](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/cubesandbox-base-nginx) | 基于 `cubesandbox-base` 构建最小 nginx 镜像，完整验证自定义模板镜像流程。 |

## Agent 与框架集成

| 示例 | 说明 |
| --- | --- |
| [Claude Code](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/claude-code-integration) | 将 Claude Code 的 Bash 工具调用转发到隔离的 CubeSandbox MicroVM 中执行。 |
| [LangChain](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/langchain-integration) | 在 LangChain 0.x 和 1.x Agent 中将 CubeSandbox 用作命令执行工具。 |
| [Pi Agent](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/pi-agent-integration) | 将 Pi Agent 的工具执行接入 CubeSandbox 环境。 |
| [OpenClaw](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/openclaw-integration) | 配置 OpenClaw Skill，让 Agent 在隔离的 MicroVM 中执行代码。 |
| [OpenAI Agents SDK](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/openai-agents-example) | 将 `E2BSandboxClient` 接入 CubeSandbox，包含 Shell Agent、暂停/恢复和 SWE-bench 流程。 |
| [OpenAI Agents + Code Interpreter](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/openai-agents-code-interpreter) | 使用通用 E2B 执行或有状态 Jupyter kernel 运行数据分析 Agent。 |
| [SWE-bench + mini-swe-agent](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/mini-rl-training) | 在隔离沙箱中自动处理 SWE-bench 编码任务，支持多模型与 RL 训练流程。 |

## 网络与入口

| 示例 | 说明 |
| --- | --- |
| [网络策略](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/network-policy) | 演示完全断网、CIDR 白名单、CIDR 黑名单以及运行时更新策略。 |
| [路由感知出网](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/route-aware-egress) | 启用 `cube-router` 后，验证沙箱流量按照宿主机路由出网。 |
| [gRPC Ingress](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/grpc-ingress) | 通过 CubeProxy 的 9090 明文入口连接原生 gRPC 客户端。 |
| [E2B 开发 Sidecar](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/e2b-dev-sidecar) | 在没有泛域名 DNS 的开发环境中，通过 E2B SDK 访问远程 Cube 集群。 |

## 存储、内存与状态

| 示例 | 说明 |
| --- | --- |
| [Host Mount](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/host-mount) | 创建沙箱时，以只读或读写方式挂载宿主机目录。 |
| [快照、回滚与克隆](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/snapshot-rollback-clone) | 提供快照、回滚、克隆、状态保留和并发操作的独立 SDK 示例。 |
| [ivshmem](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/ivshmem) | 启用主机与虚机共享内存，并体验环形缓冲区协议和 mmap 吞吐测试。 |
| [腾讯云 COS Volume 插件](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/volume/cos/README.zh.md) | 部署 binary 或 RPC 类型的 COS Volume 插件，并验证完整生命周期。 |
| [S3 兼容 Volume 插件](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/volume/s3/README.zh.md) | 接入 AWS S3、腾讯云 COS、Cloudflare R2、MinIO 等 S3 兼容后端。 |

## 性能测试

| 示例 | 说明 |
| --- | --- |
| [cube-bench](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/cube-bench) | 按可配置并发测量沙箱创建与删除延迟，提供 TUI、分位数报告和 JSON 导出。 |

::: tip
多数 SDK 示例使用相同的环境变量（`E2B_API_URL`、`E2B_API_KEY` 和 `CUBE_TEMPLATE_ID`）。运行前请先参考[快速开始](../quickstart.md)。
:::
