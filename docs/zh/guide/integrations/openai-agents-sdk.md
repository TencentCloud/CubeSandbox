---
title: OpenAI Agents SDK 集成指南
author: ZedingZhang
date: 2026-08-19
tags:
  - integration
  - openai-agents-sdk
  - agent
lang: zh-CN
---

# OpenAI Agents SDK 集成指南

[English](../../../guide/integrations/openai-agents-sdk.md)

让 [OpenAI Agents SDK](https://developers.openai.com/api/docs/guides/agents/sandboxes) 的
`SandboxAgent` 使用 CubeSandbox MicroVM 作为沙箱执行环境。CubeSandbox 暴露 E2B 兼容 API，
因此可以直接复用 SDK 内置的 `E2BSandboxClient` 作为沙箱执行平面，无需实现自定义 provider。

本文是一份简明的集成入口。仓库已经提供完整的 Shell Agent、SWE-bench、暂停/恢复和 Code
Interpreter 示例；下方链接可以直接运行并查看这些实现。

## 集成对象与版本

| 组件 | 仓库示例使用的基线 |
| --- | --- |
| OpenAI Agents SDK | 带 Sandbox Agents 支持的 Python 包 `openai-agents[e2b]` |
| Python | 3.10+ |
| CubeSandbox | E2B 兼容 CubeAPI，以及可访问的 CubeProxy 数据平面 |
| 沙箱模式 | 通用 E2B（`E2BSandboxType.E2B`）和 Code Interpreter（`E2BSandboxType.CODE_INTERPRETER`） |

OpenAI Agents SDK 的 Sandbox Agents 目前处于 beta。示例 requirements 有意安装当前 SDK
版本；生产部署应在完成验证后锁定解析出的依赖版本。

## 前置条件

- 已运行的 [CubeSandbox 部署](/zh/guide/quickstart)，并且可以访问 CubeAPI，通常为
  `http://<cube-host>:3000`。
- `cubemastercli` 已连接集群，并已获得一个沙箱模板 ID。
- 运行 Agent harness 的主机安装了 Python 3.10+。
- 运行完整 Agent demo 时，需要 TokenHub 或其他 OpenAI 兼容 LLM 端点的 API Key 和模型名。

::: warning 控制平面与数据平面
`E2B_API_URL` 用于选择 CubeAPI 控制平面端点。官方 E2B SDK 还会访问每个沙箱的数据平面域名。
一键本地部署自带 CoreDNS；生产环境应配置泛域名 DNS。若必须在没有泛域名 DNS 的本地环境中
使用官方 E2B SDK，请使用 [E2B 开发 sidecar](/zh/guide/multi-node-deploy#官方-e2b-sdk-无泛域名-dns开发-sidecar)。
:::

## 安装与配置

### 1. 选择 CubeSandbox 模板

`simple_demo.py` 可以使用任何在 `49983` 端口运行 envd 的 Linux 模板。你可以复用已有模板，
也可以创建仓库调试 demo 使用的 SWE-bench 模板：

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-image.tencentcloudcr.com/demo/django_1776_django-13447:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --cpu 4000 --memory 8192 \
  --probe 49983
```

该命令会异步构建模板。使用输出中的任务 ID 监控构建进度：

```bash
cubemastercli tpl watch --job-id <job_id>
```

等待状态变为 `READY`，然后记录输出中的 `template_id`。

### 2. 安装示例依赖

```bash
cd examples/openai-agents-example
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
cp .env.example .env
```

配置 `.env`：

| 变量 | 用途 |
| --- | --- |
| `E2B_API_URL` | CubeAPI 控制平面地址，例如 `http://<cube-host>:3000` |
| `E2B_API_KEY` | E2B SDK 必填；开启 CubeAPI 鉴权时使用鉴权回调接受的 `e2b_` 前缀 Key，未开启时使用 `e2b_000000` |
| `CUBE_TEMPLATE_ID` | CubeSandbox 模板 ID |
| `TOKENHUB_API_KEY` | 仓库 demo 默认使用的 TokenHub Key |
| `OPENAI_API_KEY` / `OPENAI_BASE_URL` | 其他 OpenAI 兼容 LLM 的凭据和端点 |
| `CUBE_SSL_CERT_FILE` | 可选，自签名 CubeSandbox 部署的 CA bundle |

模型名必须存在于配置的 LLM 端点。模板变量名由应用自行决定：现有 E2B 应用可以保留原变量名，
仓库示例为了清晰使用 `CUBE_TEMPLATE_ID`。

## 集成代码片段

保留原有 Agent 定义，只替换沙箱连接配置：

```python
import asyncio
import os

from agents import Runner
from agents.run import RunConfig
from agents.sandbox import SandboxRunConfig
from agents.extensions.sandbox import (
    E2BSandboxClient,
    E2BSandboxClientOptions,
    E2BSandboxType,
)

async def main():
    run_config = RunConfig(
        sandbox=SandboxRunConfig(
            client=E2BSandboxClient(),
            options=E2BSandboxClientOptions(
                sandbox_type=E2BSandboxType.E2B,
                template=os.environ["CUBE_TEMPLATE_ID"],
                timeout=300,
            ),
        ),
        workflow_name="Cube shell agent",
    )

    result = await Runner.run(
        agent,
        "What OS is running? Show uname and /etc/os-release.",
        run_config=run_config,
    )
    print(result.final_output)


# `agent` 是已有的 SandboxAgent。
asyncio.run(main())
```

仓库中的 [`simple_demo.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/openai-agents-example/simple_demo.py)
在这段核心配置之外补齐了完整的 `SandboxAgent`、模型配置、资源清理，以及当前 CubeSandbox envd
所需的兼容处理。

### 迁移已有的 E2B Agent

客户端类无需更换。只需把现有 E2B 配置指向 Cube，并传入 Cube 模板 ID：

```diff
- E2B_API_URL="https://api.e2b.dev"
- E2B_API_KEY="<e2b-cloud-key>"
- SANDBOX_TEMPLATE="<e2b-template>"
+ E2B_API_URL="http://<cube-host>:3000"
+ E2B_API_KEY="e2b_000000"
+ SANDBOX_TEMPLATE="<cube-template-id>"
```

上例适用于未开启 CubeAPI 鉴权的部署。如果已经开启鉴权，请把 `e2b_000000` 替换为鉴权回调
接受的 `e2b_` 前缀凭据。

`SANDBOX_TEMPLATE` 代表应用原先传给 `E2BSandboxClientOptions(template=...)` 的环境变量，
无需特意改名。

## 可运行 Demo

先在不请求 LLM 的情况下验证沙箱链路：

```bash
cd examples/openai-agents-example
python main.py --sandbox-only --timeout 60
```

验证文件系统状态能否跨暂停/恢复保留：

```bash
python simple_demo.py --pause-resume
```

然后让 Shell Agent 执行一个真实任务：

```bash
python simple_demo.py \
  --question "What OS is running? Show uname and the first 3 lines of /etc/os-release."
```

对于更完整的工作流，`main.py` 会让 Agent 检查 Django 源码并分析 SWE-bench 的
`django__django-13447` Bug。参数和预期流程见双语
[示例 README](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/openai-agents-example)。

## 进阶用法

- **长任务：**同时设置 `E2BSandboxClientOptions(timeout=...)` 的沙箱生命周期，以及合适的
  Agent 最大轮数。
- **暂停与恢复：**设置 `pause_on_exit=True`，保留会话状态，然后调用
  `E2BSandboxClient.resume(...)`。仓库 demo 完整执行了写入、暂停、恢复、读取和清理流程。
- **Code Interpreter：**使用
  [`openai-agents-code-interpreter`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/openai-agents-code-interpreter)
  中的示例。通用执行需要 `49983` 端口的 envd；Jupyter 模式还要求模板镜像在 `49999`
  端口提供 Code Interpreter 服务。
- **网络与存储控制：**通过[网络策略](/zh/guide/network-policy)、
  [安全代理](/zh/guide/security-proxy)和[持久化存储](/zh/guide/persistent-storage)配置 Cube
  专有能力。E2B 兼容层未覆盖的能力可以预先写入模板，或通过 CubeSandbox 原生 API 管理。

## 注意事项

- 仓库示例把 E2B envd 用户设为 `root`，并在对接旧版 envd 时移除 `stdin` 参数。如果你的部署
  仍需这些适配，请从可运行示例复制兼容代码块。
- 仅配置 `E2B_API_URL` 不能替代数据平面的 DNS 或 sidecar 配置；需要同时验证 CubeAPI 和
  CubeProxy 的可达性。
- `E2BSandboxType.CODE_INTERPRETER` 需要专门构建的模板；选择该枚举不会自动安装或启动
  Jupyter。
- 应把沙箱视为不受信任的执行环境。除非任务明确需要，否则把 LLM 凭据保留在 Agent harness 中，
  不要传入 MicroVM。

## 参考资料

- [OpenAI Sandbox Agents 官方文档](https://developers.openai.com/api/docs/guides/agents/sandboxes)
- [可运行的 Shell Agent 与 SWE-bench 示例](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/openai-agents-example)
- [可运行的 Code Interpreter 示例](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/openai-agents-code-interpreter)
- [OpenAI Agents SDK × CubeSandbox 详细实现说明](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/openai-agents-example/openai-agents-sandbox-cube-integration_zh.md)
- [从客户端连接 CubeSandbox 集群](/zh/guide/multi-node-deploy#从客户端连接集群)
