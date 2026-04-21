# 示例项目

以下是展示 Cube Sandbox 各种使用场景的示例项目。每个示例都是独立的项目，包含完整的 README、源码和依赖定义。

| 示例 | 说明 |
|------|------|
| [代码沙箱快速入门](https://github.com/tencentcloud/CubeSandbox/tree/master/examples/code-sandbox-quickstart) | 最基础的用法：创建沙箱、执行 Python 代码、运行 Shell 命令、管理网络策略等，全部通过 E2B SDK 完成。 |
| [浏览器沙箱（Playwright）](https://github.com/tencentcloud/CubeSandbox/tree/master/examples/browser-sandbox) | 在 MicroVM 中运行无头 Chromium，通过 CDP 协议使用 Playwright 远程控制浏览器。 |
| [OpenClaw 集成](https://github.com/tencentcloud/CubeSandbox/tree/master/examples/openclaw-integration) | 部署 Cube Sandbox 并配置 OpenClaw Skill，让 AI Agent 能够在隔离的虚拟机环境中执行代码。 |
| [SWE-bench + mini-swe-agent](https://github.com/tencentcloud/CubeSandbox/tree/master/examples/mini-rl-training) | 使用 cube-sandbox + mini-swe-agent 在隔离沙箱中自动化 SWE-bench 编码任务，支持多模型切换和 RL 训练愿景。 |
| [OpenAI Agents SDK 集成](https://github.com/tencentcloud/CubeSandbox/tree/master/examples/openai-agents-example) | 将 OpenAI Agents SDK 的 `E2BSandboxClient` 对接 Cube Sandbox。包含最小 Shell Agent（含 Pause/Resume 演示）以及完整的 SWE-bench Django 调试 Agent（流式输出 + 全链路追踪）。 |
| [OpenAI Agents + Code Interpreter](https://github.com/tencentcloud/CubeSandbox/tree/master/examples/openai-agents-code-interpreter) | 在 Cube Sandbox 中运行使用 pandas / matplotlib 的数据分析 Agent，提供通用 E2B（write+exec）与 Jupyter kernel（状态跨轮保留、图像自动捕获）两种执行形态。 |

::: tip
所有示例共享相同的环境变量约定（`E2B_API_URL`、`E2B_API_KEY`、`CUBE_TEMPLATE_ID`）。请先参考[快速开始](../quickstart)指南搭建 Cube Sandbox 环境。
:::
