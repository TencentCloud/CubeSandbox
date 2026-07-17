# 示例项目

以下是展示 Cube Sandbox 各种使用场景的示例项目。每个示例都是独立的项目，包含完整的 README、源码和依赖定义。

| 示例 | 说明 |
|------|------|
| [代码沙箱快速入门](https://github.com/tencentcloud/CubeSandbox/tree/master/examples/code-sandbox-quickstart) | 最基础的用法：创建沙箱、执行 Python 代码、运行 Shell 命令、管理网络策略等，全部通过 E2B SDK 完成。 |
| [PostgreSQL 有状态沙箱](https://github.com/tencentcloud/CubeSandbox/tree/master/examples/postgresql-stateful-sandbox) | 通过本地 Unix Socket 运行 PostgreSQL 16.14，并验证断网 SQL、数据库快照恢复以及两条隔离的 schema migration 分支。 |
| [浏览器沙箱（Playwright）](https://github.com/tencentcloud/CubeSandbox/tree/master/examples/browser-sandbox) | 在 MicroVM 中运行无头 Chromium，通过 CDP 协议使用 Playwright 远程控制浏览器。 |
| [OpenClaw 集成](https://github.com/tencentcloud/CubeSandbox/tree/master/examples/openclaw-integration) | 部署 Cube Sandbox 并配置 OpenClaw Skill，让 AI Agent 能够在隔离的虚拟机环境中执行代码。 |
| [SWE-bench + mini-swe-agent](https://github.com/tencentcloud/CubeSandbox/tree/master/examples/mini-rl-training) | 使用 cube-sandbox + mini-swe-agent 在隔离沙箱中自动化 SWE-bench 编码任务，支持多模型切换和 RL 训练愿景。 |
| [OpenAI Agents SDK 集成](https://github.com/tencentcloud/CubeSandbox/tree/master/examples/openai-agents-example) | 将 OpenAI Agents SDK 的 `E2BSandboxClient` 对接 Cube Sandbox。包含最小 Shell Agent（含 Pause/Resume 演示）以及完整的 SWE-bench Django 调试 Agent（流式输出 + 全链路追踪）。 |
| [OpenAI Agents + Code Interpreter](https://github.com/tencentcloud/CubeSandbox/tree/master/examples/openai-agents-code-interpreter) | 在 Cube Sandbox 中运行使用 pandas / matplotlib 的数据分析 Agent，提供通用 E2B（write+exec）与 Jupyter kernel（状态跨轮保留、图像自动捕获）两种执行形态。 |
| [cube-bench](https://github.com/tencentcloud/CubeSandbox/tree/master/examples/cube-bench) | Go 编写的 CLI 压测工具，可在可配置并发数下测量沙箱创建/删除延迟。具备实时 TUI 看板（Bubbletea/Lipgloss）、分位数报告（P50/P95/P99）和 JSON 导出功能。 |

::: tip
E2B 兼容示例通常使用 `E2B_API_URL`、`E2B_API_KEY` 和
`CUBE_TEMPLATE_ID`；基于 Cube 原生 SDK 的示例使用 `CUBE_API_URL`、
`CUBE_TEMPLATE_ID` 以及可选的 `CUBE_PROXY_NODE_IP`。请以各示例 README
列出的变量为准；在新部署上运行前，请先参考[快速开始](../quickstart.md)。
:::
