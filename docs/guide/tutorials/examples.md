# Examples

Runnable examples demonstrating CubeSandbox features and integrations. Follow each project's README for prerequisites and setup instructions.

## Getting Started and Images

| Example | Description |
| --- | --- |
| [Code Sandbox Quickstart](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/code-sandbox-quickstart) | Create a sandbox, run Python and shell commands, work with files, and try core E2B-compatible APIs. |
| [Browser Sandbox (Playwright)](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/browser-sandbox) | Run headless Chromium in a MicroVM and control it remotely through Playwright over CDP. |
| [Custom nginx Image](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/cubesandbox-base-nginx) | Build a minimal nginx image on top of `cubesandbox-base` and test the bring-your-own-image flow end to end. |

## Agents and Framework Integrations

| Example | Description |
| --- | --- |
| [Claude Code](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/claude-code-integration) | Redirect Claude Code Bash tool calls into isolated CubeSandbox MicroVMs. |
| [LangChain](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/langchain-integration) | Use CubeSandbox as a command-execution tool with both LangChain 0.x and 1.x agents. |
| [Pi Agent](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/pi-agent-integration) | Connect Pi Agent tool execution to a CubeSandbox environment. |
| [OpenClaw](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/openclaw-integration) | Configure the OpenClaw skill so agents can execute code in isolated MicroVMs. |
| [OpenAI Agents SDK](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/openai-agents-example) | Connect `E2BSandboxClient` to CubeSandbox, including Shell Agent, pause/resume, and SWE-bench flows. |
| [OpenAI Agents + Code Interpreter](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/openai-agents-code-interpreter) | Run data-analysis agents using either generic E2B execution or a stateful Jupyter kernel. |
| [SWE-bench with mini-swe-agent](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/mini-rl-training) | Automate SWE-bench coding tasks in isolated sandboxes with multi-model support and an RL training workflow. |

## Networking and Ingress

| Example | Description |
| --- | --- |
| [Network Policy](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/network-policy) | Try air-gapped, CIDR allowlist, CIDR denylist, and runtime policy-update scenarios. |
| [Route-Aware Egress](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/route-aware-egress) | Verify sandbox egress through host routes when `cube-router` is enabled. |
| [gRPC Ingress](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/grpc-ingress) | Connect a native gRPC client through CubeProxy's plaintext ingress on port 9090. |
| [E2B Dev Sidecar](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/e2b-dev-sidecar) | Access a remote Cube cluster from an E2B SDK development environment without wildcard DNS. |

## Storage, Memory, and State

| Example | Description |
| --- | --- |
| [Host Mount](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/host-mount) | Mount host directories read-only or read-write into a sandbox at creation time. |
| [Snapshot, Rollback, and Clone](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/snapshot-rollback-clone) | Run standalone SDK examples for snapshots, rollback, cloning, state preservation, and concurrency. |
| [ivshmem](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/ivshmem) | Enable host/guest shared memory and try a ring-buffer protocol and mmap throughput benchmark. |
| [Tencent Cloud COS Volume Plugin](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/volume/cos/README.md) | Deploy and exercise the binary or RPC COS volume plugin through its complete lifecycle. |
| [S3-Compatible Volume Plugin](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/volume/s3/README.md) | Connect AWS S3, Tencent Cloud COS, Cloudflare R2, MinIO, or another S3-compatible backend. |

## Benchmarking

| Example | Description |
| --- | --- |
| [cube-bench](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/cube-bench) | Measure sandbox creation and deletion latency at configurable concurrency, with a TUI, percentile reports, and JSON export. |

::: tip
Most SDK examples use the same environment variables (`E2B_API_URL`, `E2B_API_KEY`, and `CUBE_TEMPLATE_ID`). See [Quick Start](../quickstart.md) before running them.
:::
