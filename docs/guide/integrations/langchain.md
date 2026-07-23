---
title: LangChain Integration Guide
author: March-77
date: 2026-07-23
tags:
  - integration
  - langchain
lang: en-US
---

# LangChain Integration Guide

## Integration Target and Version

- LangChain: `0.3.x` and compatible tool calling runtimes
- Cube API endpoint: 0.4.x compatible control plane
- SDK: `e2b` client layer or direct Cube API proxy

## Prerequisites

- Working CubeSandbox deployment (API + auth path tested).
- Python environment with LangChain and your preferred LLM provider.
- A prebuilt template for runtime execution (Python + shell helpers).

## Integration Steps

### 1. Install dependencies

```bash
pip install langchain e2b
```

### 2. Create an adapter wrapper around E2B tool execution

LangChain tool calls should be funneled through a single sandbox launcher:

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

### 3. Bind to LangChain tool interface

```python
from langchain.tools import Tool

tool = Tool.from_function(
    name="cube_exec",
    description="Execute untrusted user code inside CubeSandbox",
    func=run_in_cube,
)
```

### 4. Use in an agent

```python
from langchain.agents import initialize_agent
from langchain_openai import ChatOpenAI

agent = initialize_agent(
    tools=[tool],
    llm=ChatOpenAI(model="gpt-4o-mini", temperature=0),
    agent="zero-shot-react-description",
)

print(agent.run("Generate a tiny Python script that sorts a list and run it."))
```

## Key Code Snippets

### Keep per-task isolation with short-lived sandboxes

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

## Caveats

- Keep execution timeout strict (`10-120s`) to avoid long-lived runaway jobs.
- For Python packages requiring GPU/compiler toolchain, isolate those workloads into a dedicated template.
- Do not persist long-lived secrets inside the template image.
- Route logs from LangChain run and `cubecli logs` together for complete traceability.

## References

- Related docs:
  - [Integrations index](./index.md)
  - [Template best practices](../templates.md)
- Sample repository:
  - Internal PoC repository in your org (agent-side execution runner)
- Upstream project:
  - https://github.com/langchain-ai/langchain
