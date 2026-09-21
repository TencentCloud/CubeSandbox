---
title: Pydantic AI 集成指南
author: WuShang-d
date: 2026-09-21
tags:
  - integration
  - pydantic-ai
  - agent
lang: zh-CN
---

# Pydantic AI 集成指南

[English](../../../guide/integrations/pydantic-ai.md)

为 [Pydantic AI](https://ai.pydantic.dev) Agent 添加一个在 CubeSandbox MicroVM 中
执行代码的工具。由模型决定何时运行 Python；代码在隔离的 MicroVM 中执行，其真实的
stdout / stderr / 退出码作为工具结果回传给模型。

```text
用户提问
  -> Pydantic AI Agent (agent.run_sync)
  -> 模型调用 run_python 工具
  -> 工具在 CubeSandbox MicroVM 中写入脚本并执行
  -> stdout / stderr / 退出码回传给模型
  -> 模型继续推理
  -> 最终答案
```

本指南把 Pydantic AI 的**函数工具**接到官方
[`cubesandbox` Python SDK](https://pypi.org/project/cubesandbox/)。它**不**新增
Pydantic AI 模型后端，也**不**修改 CubeSandbox——这是一个小而聚焦的函数工具集成。
完整可运行示例位于
[`examples/pydantic-ai-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/pydantic-ai-integration)。

## 集成对象与版本

| 组件 | 示例所用基线 |
| --- | --- |
| Pydantic AI | `pydantic-ai` 2.x（`Agent`、`@agent.tool`、`RunContext`、`OpenAIChatModel`） |
| CubeSandbox SDK | `cubesandbox` 0.7.0（`Sandbox`、`commands.run`、`files.write`） |
| CubeSandbox 平台 | 兼容 E2B 的 CubeAPI，且 CubeProxy 数据面可达 |
| Python | 3.10+（`pydantic-ai` 的要求） |
| LLM | 任意 OpenAI 兼容的对话端点 |

示例基于 `pydantic-ai` 2.46.0 与 `cubesandbox` 0.7.0 编写，并做了静态验证
（导入、工具接线、沙箱复用与失败路径）。在你自己的部署上验证后，请锁定解析出的版本。

## 前置条件

- 一个 CubeAPI 可达的 [CubeSandbox 部署](/zh/guide/quickstart)，通常位于
  `http://<cube-host>:3000`。
- 一个具备 Python 的沙箱模板。默认示例只用 Python 标准库，因此官方 `sandbox-code`
  模板即可满足，无需自定义镜像。
- 已连接集群的 `cubemastercli`，用于注册模板。
- 运行 Agent 的机器需 Python 3.10+。
- 一个 OpenAI 兼容的 LLM 端点及其 API Key。

::: tip Agent 主机与 Cube 主机
运行 Pydantic AI Agent 的机器和运行 CubeSandbox 的机器不必是同一台。Agent 通过网络
访问 CubeAPI/CubeProxy，因此可以用笔记本驱动一台远程 Linux Cube 主机。若在没有通配
DNS 的情况下使用官方 E2B SDK 的数据面主机名，请参见
[E2B 开发 sidecar](/zh/guide/multi-node-deploy#官方-e2b-sdk-无泛域名-dns开发-sidecar)。
:::

## 接入步骤

### 1. 注册官方 code sandbox 模板

默认任务仅依赖标准库，因此预构建的 `sandbox-code` 镜像即可：

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 2G --expose-port 49983 --probe 49983
# 中国大陆请使用 cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest
```

复制输出中的 `template_id`。

### 2. 安装示例并配置环境

```bash
cd examples/pydantic-ai-integration
python3 -m venv .venv && source .venv/bin/activate
pip install -r requirements.txt
cp .env.example .env
```

填写 `.env`：

| 变量 | 必填 | 用途 |
| --- | --- | --- |
| `CUBE_TEMPLATE_ID` | 是 | 步骤 1 得到的模板 ID |
| `CUBE_API_URL` | 否 | CubeAPI 地址；默认 `http://127.0.0.1:3000` |
| `CUBE_API_KEY` | 否 | 仅在开启鉴权的 CubeAPI 上需要 |
| `CUBE_PROXY_NODE_IP` / `CUBE_PROXY_PORT_HTTP` | 否 | 未配置 DNS 时直连 CubeProxy |
| `OPENAI_API_KEY` | 是 | OpenAI 兼容端点的 API Key |
| `OPENAI_BASE_URL` | 否 | 端点地址；默认 `https://tokenhub.tencentmaas.com/v1` |
| `MODEL_NAME` | 否 | 模型名；默认 `deepseek-v3` |

## 关键代码片段

Pydantic AI 通过 `RunContext` 把带类型的依赖传入每个工具。把沙箱放在依赖对象上，
即可让整个运行共享同一个 MicroVM：

```python
from dataclasses import dataclass, field
import itertools, shlex
from typing import Iterator

from cubesandbox import Sandbox, CubeSandboxError
from pydantic_ai import Agent, RunContext


@dataclass
class Deps:
    sandbox: Sandbox
    _script_counter: Iterator[int] = field(default_factory=lambda: itertools.count())


agent = Agent(deps_type=Deps, instructions="...")


@agent.tool
def run_python(ctx: RunContext[Deps], code: str) -> str:
    """在 CubeSandbox MicroVM 中执行一段 Python 3 代码并返回其输出。"""
    sandbox = ctx.deps.sandbox
    # 每次调用使用唯一路径，编号来自宿主侧计数器——不会把模型输入拼进 shell 命令。
    script_path = f"/workspace/agent_step_{next(ctx.deps._script_counter)}.py"
    try:
        sandbox.files.write(script_path, code)
        result = sandbox.commands.run(
            f"python3 {shlex.quote(script_path)}", timeout=120, cwd="/workspace"
        )
    except CubeSandboxError as exc:
        return f"[cube-sandbox error] {type(exc).__name__}: {exc}"

    out = result.stdout or ""
    if result.stderr:
        out += "\n--- stderr ---\n" + result.stderr
    if result.exit_code != 0:
        out += f"\n[non-zero exit code: {result.exit_code}]"
    return out or "[no output]"
```

外层生命周期只创建一次 MicroVM，并作为 `deps` 交给 Agent：

```python
with Sandbox.create(template=template_id, timeout=600,
                    allow_internet_access=False) as sandbox:
    result = agent.run_sync(question, deps=Deps(sandbox=sandbox), model=model)
print(result.output)
```

用当前的 provider API 配置任意 OpenAI 兼容端点：

```python
from pydantic_ai.models.openai import OpenAIChatModel
from pydantic_ai.providers.openai import OpenAIProvider

model = OpenAIChatModel(
    "deepseek-v3",
    provider=OpenAIProvider(base_url="https://<endpoint>/v1", api_key="<key>"),
)
```

## 运行示例

```bash
python pydantic_ai_agent_demo.py
# 或传入你自己的（仅标准库）任务：
python pydantic_ai_agent_demo.py "计算前 15 个质数及它们的和。"
```

默认任务让 Agent 生成并校验斐波那契数列。最终答案的措辞因模型而异，因此请验证
**行为**而非固定句子：

- `run_python` 确实被调用（先出现 `Sandbox <id> created` 那一行，再开始干活）。
- 报告的数字来自真实执行且正确（F(20) = 6765）。
- Agent 以最终答案收尾，而不是陷入循环。

## 进阶

- **沙箱复用。** 每次运行只创建一个 MicroVM，并在所有工具调用间复用。这样避免了
  逐次调用的 MicroVM 启动开销，也让某次调用写入的文件在同一次运行的后续调用中仍然
  存在。请优先采用这种方式，而不要在每次工具调用内部创建沙箱。
- **超时。** `commands.run(timeout=...)` 限制单次执行；`Sandbox.create(timeout=...)`
  限制的是 MicroVM 允许**空闲**多久后被回收——活跃的沙箱会不断重置该计时，因此它
  并不是墙钟意义上的存活上限。若还需要为整次运行设定硬性上限，请在 Agent 侧强制
  （Pydantic AI 的[用量限制](https://ai.pydantic.dev/agents/#usage-limits)加上你
  自己的截止时间）。
- **错误处理。** 工具会把 `CubeSandboxError` 与传输超时以文本形式返回给模型
  （stderr 分隔展示、非零退出码单独报告），便于重试；而 `Sandbox.create()` 失败会
  向上传播并干净地中断运行。
- **网络隔离。** 示例以 `allow_internet_access=False` 创建沙箱。可结合
  [网络策略](/zh/guide/network-policy) 与[安全代理](/zh/guide/security-proxy)，
  只放行任务真正需要的出站流量。
- **持久文件。** 若状态需要跨运行保留，请挂载
  [持久卷](/zh/guide/persistent-storage)，而不要依赖临时的 `/workspace`。

## 注意事项

- 把 MicroVM 视为不可信执行环境。LLM 凭证应保留在 Agent 主机；除非任务确有需要，
  不要传入沙箱。
- 默认示例假设代码仅依赖标准库。若任务需要第三方包，请把它们打进模板镜像，或开启
  网络访问在运行时安装。
- `CUBE_SSL_CERT_FILE` 会被进程级导出（作为 `SSL_CERT_FILE` / `REQUESTS_CA_BUNDLE`），
  因此也会作用于 LLM 的 HTTPS 客户端；该 bundle 必须同时包含公共根 CA。
- `pydantic-ai` 需要 Python 3.10+，因此即便 CubeSandbox SDK 本身支持 3.9，Agent
  主机也不能在 3.9 上运行。

## 参考资料

- [可运行示例](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/pydantic-ai-integration)
- [Pydantic AI 文档](https://ai.pydantic.dev)
- [Pydantic AI 函数工具](https://ai.pydantic.dev/tools/)
- [CubeSandbox 快速开始](/zh/guide/quickstart)
- [将客户端连接到 CubeSandbox 集群](/zh/guide/multi-node-deploy#从客户端连接集群)
