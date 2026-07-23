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

- LangChain: `0.3.x` 及兼容的 Tool 调用运行时
- Cube API：控制面 0.4.x
- SDK：`e2b` 客户端（或自定义 Cube API 代理）

## 前置条件

- 已部署可用的 CubeSandbox（API 与鉴权链路已确认）。
- Python 已安装并具备 LangChain、LLM provider 依赖。
- 已创建可执行模板（含 Python 与执行工具链）。

## 接入步骤

### 1. 安装依赖

```bash
pip install langchain e2b
```

### 2. 将执行路径封装成统一工具

将 LangChain 的 tool 调用统一落到一个沙箱入口：

```python
import os
from e2b import Sandbox

CUBE_API_URL = os.environ["CUBE_API_URL"]
CUBE_API_KEY = os.environ.get("CUBE_API_KEY", "")
CUBE_TEMPLATE_ID = os.environ["CUBE_TEMPLATE_ID"]

def run_in_cube(code: str, timeout: int = 60) -> str:
    with Sandbox(
        api_url=CUBE_API_URL,
        api_key=CUBE_API_KEY,
        template=CUBE_TEMPLATE_ID,
        timeout=timeout,
    ) as sandbox:
        execution = sandbox.commands.run(
            f"python - <<'PY'\n{code}\nPY",
            timeout=timeout,
        )
        return execution.stdout
```

### 3. 注册为 LangChain 工具

```python
from langchain.tools import Tool

tool = Tool.from_function(
    name="cube_exec",
    description="在 CubeSandbox 内执行受限代码",
    func=run_in_cube,
)
```

### 4. 在 Agent 中使用

```python
from langchain.agents import initialize_agent
from langchain_openai import ChatOpenAI

agent = initialize_agent(
    tools=[tool],
    llm=ChatOpenAI(model="gpt-4o-mini", temperature=0),
    agent="zero-shot-react-description",
)

print(agent.run("生成一个排序列表的 Python 脚本并执行。"))
```

## 关键代码片段

### 每次任务都创建短生命周期沙箱

```python
class CubeTool:
    def __init__(self, template_id: str):
        self.template_id = template_id

    def run(self, query: str) -> str:
        with Sandbox(template=self.template_id, api_key=CUBE_API_KEY, api_url=CUBE_API_URL) as s:
            s.filesystem.write("/workspace/input.txt", query)
            res = s.commands.run("python /workspace/run.py")
            return res.stdout
```

## 注意事项

- 严格控制超时（建议 10–120 秒），避免模型任务跑飞。
- 涉及重依赖场景请分离模板（避免污染通用模板）。
- 不要将长期密钥写入模板镜像。
- 建议将 LangChain Tool 日志与 `cubecli logs` 一起聚合，便于追溯。

## 参考资料

- 相关文档：
  - [生态集成索引](./index.md)
  - [模板实践文档](../templates.md)
- 示例仓库：
  - 企业内部 PoC 仓库（Agent 执行器）
- 上游项目：
  - https://github.com/langchain-ai/langchain
