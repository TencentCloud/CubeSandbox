# Examples

Hands-on examples demonstrating various Cube Sandbox use cases. Each example is a self-contained project with its own README, source code, and dependency definitions.

| Example | Description |
|---------|-------------|
| [Code Sandbox Quickstart](https://github.com/tencentcloud/CubeSandbox/tree/master/examples/code-sandbox-quickstart) | The most basic usage: create a sandbox, run Python code, execute shell commands, manage network policies, and more — all via the E2B SDK. |
| [Browser Sandbox (Playwright)](https://github.com/tencentcloud/CubeSandbox/tree/master/examples/browser-sandbox) | Run a headless Chromium inside a MicroVM and control it remotely with Playwright via CDP. |
| [OpenClaw Integration](https://github.com/tencentcloud/CubeSandbox/tree/master/examples/openclaw-integration) | Deploy Cube Sandbox and configure the OpenClaw skill so AI agents can execute code in isolated VM environments. |
| [SWE-bench with mini-swe-agent](https://github.com/tencentcloud/CubeSandbox/tree/master/examples/mini-rl-training) | Automate SWE-bench coding tasks in isolated sandboxes using cube-sandbox + mini-swe-agent, with multi-model support and RL training vision. |
| [OpenAI Agents SDK Integration](https://github.com/tencentcloud/CubeSandbox/tree/master/examples/openai-agents-example) | Wire OpenAI Agents SDK's `E2BSandboxClient` to Cube Sandbox. Ships a minimal Shell Agent with Pause/Resume and a full SWE-bench Django debugging agent with streaming + tracing. |
| [OpenAI Agents + Code Interpreter](https://github.com/tencentcloud/CubeSandbox/tree/master/examples/openai-agents-code-interpreter) | Data-analysis Agent running pandas / matplotlib inside a Cube Sandbox. Provides two variants: generic E2B write+exec and Jupyter-kernel Code Interpreter with cross-turn state and auto image capture. |

::: tip
All examples share the same environment variable conventions (`E2B_API_URL`, `E2B_API_KEY`, `CUBE_TEMPLATE_ID`). See the [Quick Start](../quickstart) guide to set up your Cube Sandbox deployment first.
:::
