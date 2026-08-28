---
title: CrewAI 集成指南
author: eatLaoJun
date: 2026-08-28
tags:
  - integration
  - crewai
  - agent
lang: zh-CN
---

# CrewAI 集成指南

[English](../../../guide/integrations/crewai.md)

让 CrewAI Agent 的 Shell Tool 在 CubeSandbox MicroVM 中执行。CrewAI 内置的
`E2BExecTool` 使用官方 E2B SDK，而 CubeSandbox 提供 E2B 兼容 API。因此，Agent、Task
和 Tool 定义都可以保持不变，只需替换沙箱地址、API Key 和模板 ID。

## 集成对象与版本

| 组件 | 本文使用的基线 |
| --- | --- |
| CrewAI / `crewai-tools` | `1.15.18`，安装 `e2b` extra |
| Python | 3.10–3.13 |
| 沙箱 Tool | `crewai_tools.E2BExecTool` |
| CubeSandbox | E2B 兼容 CubeAPI，以及可访问的 CubeProxy 数据平面 |
| Cube 模板 | envd 监听 `49983` 端口的 Linux 镜像 |

本文使用 `E2BExecTool` 而不是 Code Interpreter Tool，因此普通 CubeSandbox 模板即可
运行。`E2BPythonTool` 还要求模板在 `49999` 端口运行 Code Interpreter 服务。

## 前置条件

- 已运行的 [CubeSandbox 部署](/zh/guide/quickstart)，并且 CubeAPI 可访问，通常为
  `http://<cube-host>:3000`。
- 一个状态为 `READY` 的 Cube 模板 ID；模板需要暴露并探测 envd 的 `49983` 端口。
- 运行 CrewAI 的主机安装了 Python 3.10–3.13。
- OpenAI API Key，或按照 CrewAI LLM 文档配置的其他模型提供商。

::: warning 控制平面与数据平面
`E2B_API_URL` 指向 CubeAPI 控制平面。E2B SDK 还会通过 CubeProxy 访问每个沙箱的数据平面
域名。生产环境应配置泛域名 DNS；本地开发没有泛域名 DNS 时，可使用
[E2B 开发 sidecar](/zh/guide/multi-node-deploy#官方-e2b-sdk-无泛域名-dns-开发-sidecar)。
:::

## 安装与配置

### 1. 准备 Cube 模板

可以复用任何在 `49983` 端口运行 envd 的 Linux 模板。例如：

```bash
cubemastercli tpl create-from-image \
  --image ghcr.io/tencentcloud/cubesandbox-base:2026.16 \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

等待构建状态变为 `READY`，然后复制输出中的 `template_id`：

```bash
cubemastercli tpl watch --job-id <job_id>
```

### 2. 安装依赖

```bash
python3 -m venv .venv
source .venv/bin/activate
pip install "crewai==1.15.18" "crewai-tools[e2b]==1.15.18" "python-dotenv>=1,<2"
```

相同的版本锁定使 CrewAI 框架与 Tools 保持一致；`e2b` extra 会安装沙箱 Tool 所需的
E2B SDK。

### 3. 配置环境变量

创建 `.env` 文件：

```dotenv
E2B_API_URL=http://<cube-host>:3000
E2B_API_KEY=e2b_000000
CUBE_TEMPLATE_ID=<ready-template-id>

OPENAI_API_KEY=<your-openai-key>
OPENAI_MODEL_NAME=gpt-4o-mini
```

| 变量 | 使用方 | 用途 |
| --- | --- | --- |
| `E2B_API_URL` | E2B SDK | CubeAPI 控制平面地址 |
| `E2B_API_KEY` | E2B SDK | CubeAPI Bearer 凭据；仅在未开启鉴权时使用 `e2b_000000` |
| `CUBE_TEMPLATE_ID` | Demo | 传给 `E2BExecTool(template=...)` |
| `OPENAI_API_KEY` | CrewAI | 保留在本地 Agent 进程中的 LLM 凭据 |
| `OPENAI_MODEL_NAME` | CrewAI | LLM 模型；未设置时默认为 `gpt-4o-mini` |
| `OPENAI_API_BASE` | CrewAI | 可选，OpenAI 兼容模型提供商的 Base URL |

本配置不需要设置 Tool 的 `domain` 参数。底层 E2B SDK 会直接读取 `E2B_API_URL`，并把
沙箱创建请求发送给 CubeAPI。

## 集成代码片段

已有的 E2B CrewAI 应用只需修改连接配置：

```diff
- E2B_API_URL=https://api.e2b.dev
- E2B_API_KEY=<e2b-cloud-key>
- CUBE_TEMPLATE_ID=<e2b-template>
+ E2B_API_URL=http://<cube-host>:3000
+ E2B_API_KEY=e2b_000000
+ CUBE_TEMPLATE_ID=<cube-template-id>
```

Tool 定义保持不变：

```python
import os

from crewai_tools import E2BExecTool

sandbox_tool = E2BExecTool(
    template=os.environ["CUBE_TEMPLATE_ID"],
    persistent=False,
    sandbox_timeout=120,
)
```

在 `crewai-tools` 1.15.18 中，`template`、`persistent` 和 `sandbox_timeout` 都是
`E2BExecTool` 的构造字段。创建或连接沙箱时，Wrapper 会将 `sandbox_timeout` 作为
`timeout` 传给 E2B SDK。

使用 `persistent=False`（默认值）时，CrewAI 会为每次 Tool 调用创建一个新的 MicroVM，
并在调用结束后将其销毁。

## 可运行 Demo

将以下内容保存为 `main.py`：

```python
import os

from crewai import Agent, Crew, Process, Task
from crewai_tools import E2BExecTool
from dotenv import load_dotenv


load_dotenv()

required = ("E2B_API_URL", "E2B_API_KEY", "CUBE_TEMPLATE_ID", "OPENAI_API_KEY")
missing = [name for name in required if not os.getenv(name)]
if missing:
    raise SystemExit(f"Missing required environment variables: {', '.join(missing)}")

sandbox_tool = E2BExecTool(
    template=os.environ["CUBE_TEMPLATE_ID"],
    persistent=False,
    sandbox_timeout=120,
)

operator = Agent(
    role="Sandbox Operator",
    goal="Run requested diagnostics in an isolated CubeSandbox MicroVM",
    backstory=(
        "You verify runtime environments and report only output returned by the "
        "sandbox tool."
    ),
    tools=[sandbox_tool],
    allow_delegation=False,
    verbose=True,
)

diagnostic = Task(
    description=(
        "Use the E2B Sandbox Exec tool to run exactly this command inside the "
        "sandbox: `uname -srm && python3 --version`. Report both output lines and "
        "state that they came from the sandbox. Do not infer or invent values."
    ),
    expected_output="The OS kernel and Python version returned by the Cube sandbox.",
    agent=operator,
)

crew = Crew(
    agents=[operator],
    tasks=[diagnostic],
    process=Process.sequential,
    verbose=True,
)

print(crew.kickoff())
```

先在不消耗 LLM 请求的情况下验证沙箱连接：

```bash
python - <<'PY'
import os
from dotenv import load_dotenv
from crewai_tools import E2BExecTool

load_dotenv()
tool = E2BExecTool(template=os.environ["CUBE_TEMPLATE_ID"])
print(tool.run(command="uname -srm && python3 --version"))
PY
```

然后运行 CrewAI Agent：

```bash
python main.py
```

Verbose 日志中应出现 `E2B Sandbox Exec` 调用，以及 Cube MicroVM 返回的内核和 Python
版本。

## 进阶用法

- **跨 Tool 调用保留状态：**设置 `persistent=True`，并在 Crew 结束后显式调用
  `sandbox_tool.close()`。持久模式提供更强的连续性，但也会扩大提示注入的影响范围。
- **运行 Python Cell：**使用 `E2BPythonTool`，并准备同时暴露 envd（`49983`）与 Jupyter
  （`49999`）的 Cube Code Interpreter 模板。
- **限制出站访问：**为模板或工作负载配置 Cube [网络策略](/zh/guide/network-policy)或
  [安全代理](/zh/guide/security-proxy)。
- **使用文件或持久数据：**增加 `E2BFileTool`；若数据需要超过沙箱生命周期，则配置 Cube
  [持久化存储](/zh/guide/persistent-storage)。
- **限制执行时间：**把 `sandbox_timeout` 和每次 Tool 调用的 `timeout` 都设置为满足任务的
  最短时间。

## 注意事项与凭据处理

- `E2B_API_KEY` 用于本地 E2B SDK 向 CubeAPI 鉴权。CubeAPI 开启鉴权后，必须把占位值替换为
  鉴权回调接受的真实凭据。
- `OPENAI_API_KEY` 保留在本地 CrewAI 进程中。示例没有通过 `E2BExecTool(envs=...)` 将其传入
  沙箱，因此 Agent 生成的命令无法读取 LLM 凭据。
- 通过 Tool 的 `envs` 字段传入的值，或写入沙箱的文件，对 Agent 生成的代码可见；不要注入
  长期生产凭据。
- 仅设置 `E2B_API_URL` 不会完成数据平面路由配置；还需验证 CubeProxy 与 `*.cube.app` 的
  可达性，或使用开发 sidecar。
- Tool 会在正常完成后清理临时沙箱；若客户端进程意外中断，Cube 的超时机制是最后的清理边界。

## 参考资料

- [CrewAI E2B Sandbox Tools](https://docs.crewai.com/en/tools/ai-ml/e2bsandboxtools)
- [CrewAI 1.15.18 `E2BBaseTool` 源码](https://github.com/crewAIInc/crewAI/blob/4bc5d2924218e892bd0bc91b46352b49b0d3a740/lib/crewai-tools/src/crewai_tools/tools/e2b_sandbox_tool/e2b_base_tool.py)
- [CrewAI LLM 连接](https://docs.crewai.com/en/learn/llm-connections)
- [CubeSandbox 快速开始](/zh/guide/quickstart)
- [从客户端连接 CubeSandbox 集群](/zh/guide/multi-node-deploy#从客户端连接集群)
- [CubeSandbox 鉴权](/zh/guide/authentication)
