---
title: LangChain 集成指南
author: March-77
date: 2026-07-23
tags:
  - integration
  - langchain
lang: zh-CN
---

# LangChain 集成指南

## 集成对象与版本

- Python：`3.10+`
- LangChain：`>=1.0,<2.0`
- LangChain OpenAI 集成：`>=1.0,<2.0`
- E2B Python SDK：`>=2.4.1,<3.0`
- Cube API：控制面 0.4.x

## 前置条件

- 已部署可用的 CubeSandbox（API 与鉴权链路已确认）。
- 已准备 OpenAI API Key，或其他受支持模型提供方的等效配置。
- 已创建可执行模板（含 Python 与执行工具链）。

## 接入步骤

### 1. 安装依赖

```bash
python3 -m pip install \
  "langchain>=1,<2" \
  "langchain-openai>=1,<2" \
  "e2b>=2.4.1,<3"
```

### 2. 配置 CubeSandbox 与模型提供方

E2B 兼容 SDK 从 `E2B_API_URL` 读取控制面地址；将其指向 CubeAPI，
而不是 E2B 云端：

```bash
export E2B_API_URL="http://<cubeapi-host>:3000"
export E2B_API_KEY="<cube-api-key>"
export CUBE_TEMPLATE_ID="<template-id>"
export OPENAI_API_KEY="<openai-api-key>"
```

本地部署未启用鉴权时，可使用 `E2B_API_KEY=e2b_000000` 这样的非空占位值。

### 3. 创建可复用的 CubeSandbox 适配器

将模板 ID、API Key 和 API URL 统一保存在适配器实例中，使目标部署显式可见，
避免依赖隐藏的模块级配置。

```python
from e2b import Sandbox

class CubeTool:
    def __init__(
        self,
        template_id: str,
        api_key: str,
        api_url: str,
        timeout: int = 120,
    ) -> None:
        self.template_id = template_id
        self.api_key = api_key
        self.api_url = api_url
        self.timeout = timeout

    def run(self, code: str) -> str:
        with Sandbox.create(
            template=self.template_id,
            api_key=self.api_key,
            api_url=self.api_url,
            timeout=self.timeout,
        ) as sandbox:
            script_path = "/tmp/langchain_task.py"
            sandbox.files.write(script_path, code)
            result = sandbox.commands.run(
                f"python3 {script_path}",
                timeout=self.timeout,
            )
            if result.exit_code != 0:
                raise RuntimeError(result.stderr)
            return result.stdout
```

### 4. 注册为 LangChain 工具

```python
import os
from langchain.tools import tool

cube = CubeTool(
    template_id=os.environ["CUBE_TEMPLATE_ID"],
    api_key=os.environ["E2B_API_KEY"],
    api_url=os.environ["E2B_API_URL"],
)

@tool
def cube_exec(code: str) -> str:
    """在隔离的 CubeSandbox 内执行 Python 代码。"""
    return cube.run(code)
```

### 5. 运行真实 Agent 任务

```python
from langchain.agents import create_agent
from langchain_openai import ChatOpenAI

agent = create_agent(
    model=ChatOpenAI(model="gpt-4o-mini", temperature=0),
    tools=[cube_exec],
    system_prompt="需要运行 Python 代码时，请使用 cube_exec。",
)

response = agent.invoke(
    {
        "messages": [
            {
                "role": "user",
                "content": "用 Python 排序 [5, 2, 9, 1] 并报告结果。",
            }
        ]
    }
)
print(response["messages"][-1].content)
```

## 注意事项

- 严格控制超时（建议 10–120 秒），避免模型任务跑飞。
- 涉及重依赖场景请分离模板（避免污染通用模板）。
- 不要将长期密钥写入模板镜像。
- 将模型生成代码视为不可信输入，并相应限制沙箱网络访问和文件挂载。
- 建议将 LangChain Tool 日志与 `cubecli logs` 一起聚合，便于追溯。

## 参考资料

- 相关文档：
  - [生态集成索引](./index.md)
  - [模板实践文档](../templates.md)
  - [CubeSandbox 快速开始](../quickstart.md)
- 上游文档：
  - [LangChain Agents](https://docs.langchain.com/oss/python/langchain/agents)
  - [E2B Python SDK](https://e2b.dev/docs/sdk-reference/python-sdk/v2.0.1/sandbox_sync)
