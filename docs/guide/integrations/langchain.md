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

- Python: `3.10+`
- LangChain: `>=1.0,<2.0`
- LangChain OpenAI integration: `>=1.0,<2.0`
- E2B Python SDK: `>=2.4.1,<3.0`
- Cube API endpoint: 0.4.x compatible control plane

## Prerequisites

- Working CubeSandbox deployment (API + auth path tested).
- An OpenAI API key, or equivalent configuration for another supported model provider.
- A prebuilt template for runtime execution (Python + shell helpers).

## Integration Steps

### 1. Install dependencies

```bash
python3 -m pip install \
  "langchain>=1,<2" \
  "langchain-openai>=1,<2" \
  "e2b>=2.4.1,<3"
```

### 2. Configure CubeSandbox and the model provider

The E2B-compatible SDK reads `E2B_API_URL`; point it at CubeAPI instead of the
E2B cloud endpoint:

```bash
export E2B_API_URL="http://<cubeapi-host>:3000"
export E2B_API_KEY="<cube-api-key>"
export CUBE_TEMPLATE_ID="<template-id>"
export OPENAI_API_KEY="<openai-api-key>"
```

For a local deployment without authentication, use a non-empty placeholder such
as `E2B_API_KEY=e2b_000000`.

### 3. Create a reusable CubeSandbox adapter

Keep the template, API key, and API URL together in the adapter. This makes the
target deployment explicit and avoids hidden module-level configuration.

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

### 4. Register the adapter as a LangChain tool

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
    """Execute Python code inside an isolated CubeSandbox."""
    return cube.run(code)
```

### 5. Run a real agent task

```python
from langchain.agents import create_agent
from langchain_openai import ChatOpenAI

agent = create_agent(
    model=ChatOpenAI(model="gpt-4o-mini", temperature=0),
    tools=[cube_exec],
    system_prompt="Use cube_exec whenever you need to run Python code.",
)

response = agent.invoke(
    {
        "messages": [
            {
                "role": "user",
                "content": "Sort [5, 2, 9, 1] in Python and report the result.",
            }
        ]
    }
)
print(response["messages"][-1].content)
```

## Caveats

- Keep execution timeout strict (`10-120s`) to avoid long-lived runaway jobs.
- For Python packages requiring GPU/compiler toolchain, isolate those workloads into a dedicated template.
- Do not persist long-lived secrets inside the template image.
- Treat model-generated code as untrusted and restrict sandbox network access and file mounts accordingly.
- Route logs from LangChain run and `cubecli logs` together for complete traceability.

## References

- Related docs:
  - [Integrations index](./index.md)
  - [Template best practices](../templates.md)
  - [CubeSandbox quick start](../quickstart.md)
- Upstream docs:
  - [LangChain agents](https://docs.langchain.com/oss/python/langchain/agents)
  - [E2B Python SDK](https://e2b.dev/docs/sdk-reference/python-sdk/v2.0.1/sandbox_sync)
