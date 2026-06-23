---
title: CrewAI Integration Guide
author: ruirui6946
date: 2026-06-23
tags:
  - integration
  - crewai
lang: en-US
---

# CrewAI Integration Guide

## Integration Target and Version

[CrewAI](https://www.crewai.com/) is a framework for orchestrating role-based AI agents. This guide connects CrewAI's official `E2BPythonTool` to Cube Sandbox, so agents can execute Python inside isolated MicroVMs instead of on the host.

The integration uses Cube Sandbox's E2B-compatible API. Existing CrewAI code keeps using `E2BPythonTool`; the only infrastructure change is pointing `E2B_API_URL` at CubeAPI and selecting a Cube template.

- Tested CrewAI version: `1.14.7`
- Python: `3.10+`
- Cube client: `e2b-code-interpreter`, installed by `crewai-tools[e2b]`
- Integration type: isolated Python execution tool

## Prerequisites

1. Deploy Cube Sandbox by following one of the [deployment guides](../bare-metal-deploy.md).
2. Create a code-interpreter template containing Python and the packages your agent needs. Cube template IDs use the `tpl-<hex>` format:

   ```bash
   cubemastercli tpl create-from-image \
     --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
     --writable-layer-size 1G \
     --expose-port 49999 \
     --expose-port 49983 \
     --probe 49999
   ```

   Use `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest` (recommended for international access). If you are in mainland China, use `cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest` instead.

3. Install the dependencies:

   ```bash
   pip install "crewai>=1.14.7,<2" "crewai-tools[e2b]>=1.14.7,<2" python-dotenv
   ```

4. Configure CubeAPI and your LLM:

   ```bash
   export E2B_API_URL="http://<cube-api-host>:3000"
   export E2B_API_KEY="<cube-api-key>"
   export CUBE_TEMPLATE_ID="tpl-xxxxxxxxxxxxxxxxxxxxxxxx"

   export OPENAI_API_KEY="<your-llm-api-key>"
   export MODEL="openai/gpt-4o-mini"
   # Optional for an OpenAI-compatible provider:
   # export OPENAI_BASE_URL="https://your-provider.example/v1"
   ```

`E2B_API_URL` must point to the Cube API Server, normally on port `3000`. Do not point it at CubeProxy.

::: warning Protect the Cube API key
The `http://` examples above are only suitable for local development on a trusted machine. Production deployments should either configure TLS on CubeAPI and use `https://`, or bind CubeAPI to loopback and use `http://127.0.0.1:3000` so `E2B_API_KEY` is not sent across the network in plaintext.
:::

## Integration Steps

### 1. Create the Cube-backed CrewAI tool

CrewAI already provides `E2BPythonTool`, so no custom `BaseTool` implementation is required:

```python
import os

from crewai_tools import E2BPythonTool

cube_python = E2BPythonTool(
    template=os.environ["CUBE_TEMPLATE_ID"],
    persistent=False,
    sandbox_timeout=120,
)
```

With `persistent=False`, every tool call receives a fresh Cube MicroVM that is destroyed after execution. This is the safest default for agent-generated code.

### 2. Attach the tool to an agent

```python
import os

from crewai import Agent, Crew, LLM, Process, Task
from crewai_tools import E2BPythonTool

llm_options = {"model": os.getenv("MODEL", "openai/gpt-4o-mini")}
if os.getenv("OPENAI_API_KEY"):
    llm_options["api_key"] = os.environ["OPENAI_API_KEY"]
if os.getenv("OPENAI_BASE_URL"):
    llm_options["base_url"] = os.environ["OPENAI_BASE_URL"]

cube_python = E2BPythonTool(
    template=os.environ["CUBE_TEMPLATE_ID"],
    persistent=False,
    sandbox_timeout=120,
)

analyst = Agent(
    role="Sandboxed data analyst",
    goal="Use isolated Python execution to produce reproducible answers",
    backstory=(
        "You verify every numerical result by running Python in Cube Sandbox. "
        "Never execute generated code on the host."
    ),
    tools=[cube_python],
    llm=LLM(**llm_options),
    verbose=os.getenv("CREWAI_VERBOSE", "").lower() == "true",
)

task = Task(
    description=(
        "Use the sandbox Python tool to simulate 10,000 rolls of two fair dice "
        "with random seed 7. Report the estimated probability that the sum is 8 "
        "and compare it with the exact probability 5/36."
    ),
    expected_output=(
        "A short report containing the simulated probability, exact probability, "
        "absolute error, and the Python method used."
    ),
    agent=analyst,
)

result = Crew(
    agents=[analyst],
    tasks=[task],
    process=Process.sequential,
).kickoff()

print(result)
```

### 3. Verify Cube before involving an LLM

When debugging connectivity, invoke the tool directly:

```python
result = cube_python.run(
    code="print({'runtime': 'cube', 'sum': sum(range(10))})",
    timeout=30,
)
if not all(fragment in str(result) for fragment in ("runtime", "cube", "45")):
    raise RuntimeError(f"Unexpected Cube smoke test result: {result}")
print(result)
```

This isolates CubeAPI, template, and SDK configuration from any LLM or CrewAI orchestration issue.

## Near-Zero Migration from E2B

If a crew already uses `E2BPythonTool`, the Python code does not change:

```diff
 from crewai_tools import E2BPythonTool

 tool = E2BPythonTool(
-    template="base",
+    template=os.environ["CUBE_TEMPLATE_ID"],
 )
```

Only the environment changes:

```diff
-E2B_API_URL=https://api.e2b.dev
+E2B_API_URL=http://<cube-api-host>:3000
```

The agent, task, and tool-calling logic stay the same while execution moves to Cube's MicroVM isolation.

## Going Further

### Persist state across tool calls

Use persistent mode when an agent must reuse imports, variables, or generated files:

```python
cube_python = E2BPythonTool(
    template=os.environ["CUBE_TEMPLATE_ID"],
    persistent=True,
    sandbox_timeout=300,
)

try:
    # Use cube_python in one or more agents.
    result = crew.kickoff()
finally:
    cube_python.close()
```

Persistent sandboxes increase the effect of prompt injection because state survives between calls. Keep timeouts short and do not inject broad credentials.

### Restrict network access

Cube extends the E2B create API with network policy controls. For workloads that need these controls, create the sandbox directly or expose the same arguments from a small custom CrewAI tool:

```python
from e2b_code_interpreter import Sandbox

with Sandbox.create(
    template=os.environ["CUBE_TEMPLATE_ID"],
    allow_internet_access=False,
    network={"allow_out": ["10.0.1.0/24"]},
) as sandbox:
    execution = sandbox.run_code("print('isolated execution')")
```

`allow_out` and `deny_out` accept IP/CIDR values. Domain-name rules are not supported.

### Mount host data

Host mounts are a Cube-specific extension encoded in sandbox metadata:

```python
import json

mounts = json.dumps([
    {
        "hostPath": "/srv/agent-input",
        "mountPath": "/mnt/input",
        "readOnly": True,
    }
])

with Sandbox.create(
    template=os.environ["CUBE_TEMPLATE_ID"],
    metadata={"host-mount": mounts},
) as sandbox:
    execution = sandbox.run_code(
        "from pathlib import Path; print(list(Path('/mnt/input').iterdir()))"
    )
```

The host path must already exist on the Cubelet node. Prefer read-only mounts for agent inputs.

### Bound execution time

There are two different timeout controls:

- `sandbox_timeout` on `E2BPythonTool` controls the sandbox idle lifetime.
- `timeout` passed to `tool.run(...)` controls an individual code execution.

Use both so an infinite script cannot hold a sandbox indefinitely.

## Caveats

- The template must include Cube's `envd` service. A plain image such as `python:3.12-slim` is not a valid Cube template by itself.
- Cube template IDs are generated IDs such as `tpl-...`, not Docker image names.
- The LLM API key belongs to the CrewAI process. Only pass task-scoped secrets into the sandbox.
- Treat code and output generated from untrusted prompts as untrusted, even though the MicroVM protects the host.
- Use ephemeral mode unless the task explicitly needs state across calls.

## Runnable Example

The complete bilingual example is available under [`examples/crewai-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/crewai-integration). Run `smoke_test.py` first to validate Cube, then run `main.py` to start the CrewAI agent.

## References

- [CrewAI E2B Sandbox Tools](https://docs.crewai.com/en/tools/ai-ml/e2bsandboxtools)
- [CrewAI custom tools](https://docs.crewai.com/en/learn/create-custom-tools)
- [Cube Sandbox Python examples](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/code-sandbox-quickstart)
- [Cube network policy example](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/network-policy)
- [Cube host-mount example](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/host-mount)
