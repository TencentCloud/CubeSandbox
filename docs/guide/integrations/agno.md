---
title: Agno Integration Guide
author: Lion-Leporidae
date: 2026-09-24
tags:
  - integration
  - agno
  - agent
  - sandbox
lang: en-US
---

# Agno Integration Guide

[中文](../../zh/guide/integrations/agno.md)

Run an [Agno](https://github.com/agno-agi/agno) Agent with a custom tool whose
Python execution happens in a CubeSandbox MicroVM. Agno turns an ordinary Python
function into a tool; the function below uses the native `cubesandbox` SDK, so
the model harness stays on the host while generated code stays in the MicroVM.

## Integration target and verified versions

| Component | Version used for validation |
| --- | --- |
| Agno | `3.0.11` |
| OpenAI Python SDK | `2.54.0` |
| CubeSandbox Python SDK | `0.7.0` |
| Python | `3.12` |

The example is expected to work with Agno 3.x and Python 3.10+. Pin the resolved
versions before using it in production.

## Prerequisites

- A running CubeSandbox deployment with reachable CubeAPI and CubeProxy.
- A template that includes `python3` and envd on port `49983`.
- `CUBE_TEMPLATE_ID`; set `CUBE_API_URL` and `CUBE_PROXY_NODE_IP` when their
  defaults do not match your deployment.
- An OpenAI-compatible endpoint and `OPENAI_API_KEY` for the full Agent run.
  The key belongs to the host-side harness and is not copied into the sandbox.

## Setup and runnable demo

The repository contains a complete sample in
[`examples/agno-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/agno-integration).
Build its Python template, create a Cube template, then install and configure
the host dependencies:

```bash
cd examples/agno-integration
docker build --platform linux/amd64 -t <your-registry>/agno-cube:latest .
docker push <your-registry>/agno-cube:latest
cubemastercli tpl create-from-image \
  --image <your-registry>/agno-cube:latest \
  --writable-layer-size 1G --expose-port 49983 --probe 49983 --probe-path /health

python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
cp .env.example .env
python agno_agent_demo.py --sandbox-only
python agno_agent_demo.py
```

`--sandbox-only` validates the Cube execution path with a deterministic Python
calculation and does not call an LLM. The ordinary run asks the Agent to call
the same tool.

## Integration pattern

Agno accepts a Python function in `Agent(tools=[...])`. Bind that function to
one already-created sandbox so all Agent tool calls reuse one MicroVM, while the
context manager performs lifecycle cleanup:

```python
from agno.agent import Agent
from agno.models.openai import OpenAIChat
from cubesandbox import Sandbox

with Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"], timeout=600) as sandbox:
    run_python = make_run_python(sandbox)
    agent = Agent(
        model=OpenAIChat(id="gpt-4o-mini", api_key=os.environ["OPENAI_API_KEY"]),
        tools=[run_python],
        instructions=["Use run_python for every code-execution task."],
    )
    agent.print_response("Calculate the sum of squares from 1 to 10.")
```

`make_run_python` in the runnable example writes each snippet to a unique path,
executes `python3` with a command timeout, and returns stdout, stderr, and a
non-zero exit code distinctly. It never evaluates the `code` argument on the
host.

## Production controls

- **Network:** for code that needs no network, create the sandbox with
  `allow_internet_access=False`. For required egress, use a narrow native
  CubeSandbox allow rule rather than placing an LLM key in the MicroVM.
- **Timeout and size:** the sample limits a command to 120 seconds and an input
  snippet to 16 KiB. Set stricter limits for your workload and platform policy.
- **Persistent state:** use `Volume.create(...)` and
  `volume_mounts={"/workspace": volume}` only when Agent state must survive a
  run; otherwise a fresh sandbox limits cross-task state leakage.
- **Cleanup:** keep `Sandbox.create(...)` inside a `with` block. If you need to
  preserve a session, use CubeSandbox pause/resume deliberately and record the
  sandbox identity outside the Agent's prompt.

## Caveats

- `CUBE_API_URL` configures the control plane; the SDK also needs a reachable
  CubeProxy data plane. On a deployment without wildcard DNS, set
  `CUBE_PROXY_NODE_IP` (and `CUBE_PROXY_PORT_HTTP` when needed).
- The template must contain Python. Selecting a template ID does not install
  Python or start a code-interpreter service automatically.
- The Agent may generate harmful code. Isolation reduces host exposure but does
  not make untrusted code safe for internal networks or data; enforce network,
  identity, and resource controls at the cluster boundary.

## References

- [Agno custom tools](https://docs.agno.com/tools/creating-tools/python-functions)
- [Agno Agent tools](https://docs.agno.com/tools/agent)
- [CubeSandbox network policy](/guide/network-policy)
- [CubeSandbox persistent storage](/guide/persistent-storage)
