---
title: Google ADK Integration Guide
author: ztt0216
date: 2026-08-25
tags:
  - integration
  - google-adk
  - agent
lang: en-US
---

# Google ADK Integration Guide

[中文](../../zh/guide/integrations/google-adk.md)

Use CubeSandbox as the code execution environment for a
[Google Agent Development Kit](https://github.com/google/adk-python) agent.
Google ADK supports Python function tools, and CubeSandbox exposes an
E2B-compatible API, so an ADK agent can call a normal Python tool while the
generated code runs inside an isolated CubeSandbox MicroVM.

This guide follows the "agent on the host, code in the sandbox" pattern. The
ADK runtime, model credentials, and local project stay on the host; only the
tool's code execution crosses into CubeSandbox.

## Integration Target and Version

| Component | Baseline |
| --- | --- |
| Google ADK | Python package `google-adk>=2.0.0` |
| Python | 3.10+ |
| CubeSandbox | E2B-compatible CubeAPI and a template that supports `run_code` |
| SDK path | `e2b-code-interpreter` pointed at CubeAPI through `E2B_API_URL` |

Google ADK is evolving quickly. Pin the resolved `google-adk` and
`e2b-code-interpreter` versions after validating them against your CubeSandbox
deployment.

## Prerequisites

- A running [CubeSandbox deployment](/guide/quickstart) with CubeAPI reachable,
  normally at `http://<cube-host>:3000`.
- A CubeSandbox template ID. Use a template that supports the E2B code
  interpreter `run_code` path.
- Python 3.10+ on the machine running the ADK agent.
- A Google API key or another ADK-supported model configuration.

::: warning Control plane and data plane
`E2B_API_URL` points the E2B-compatible SDK at CubeAPI. If your deployment uses
TLS with a self-signed certificate, set `CUBE_SSL_CERT_FILE` so the example can
export `SSL_CERT_FILE` before opening the sandbox.
:::

## Setup

Install the example:

```bash
cd examples/google-adk-integration
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
cp .env.example .env
```

Configure `.env`:

| Variable | Purpose |
| --- | --- |
| `E2B_API_URL` | CubeAPI control-plane URL, for example `http://<cube-host>:3000` |
| `E2B_API_KEY` | E2B-compatible key accepted by your CubeAPI auth callback, or `e2b_000000` when auth is disabled |
| `CUBE_TEMPLATE_ID` | CubeSandbox template ID used for temporary tool sandboxes |
| `GOOGLE_API_KEY` | Google model API key used by ADK |
| `GOOGLE_ADK_MODEL` | ADK model name, for example `gemini-2.5-flash` |
| `CUBE_SSL_CERT_FILE` | Optional CubeSandbox CA bundle for self-signed deployments |

## Integration Snippet

Expose a CubeSandbox execution function as an ADK tool:

```python
from google.adk import Agent

from cube_code_tool import run_python_in_cube

root_agent = Agent(
    name="cube_code_agent",
    model="gemini-2.5-flash",
    instruction=(
        "When Python code needs to run, call run_python_in_cube so execution "
        "happens inside an isolated CubeSandbox MicroVM."
    ),
    tools=[run_python_in_cube],
)
```

The tool itself keeps the Cube-specific logic in one place:

```python
from e2b_code_interpreter import Sandbox

def run_python_in_cube(code: str) -> dict:
    template_id = os.environ["CUBE_TEMPLATE_ID"]
    with Sandbox.create(template=template_id, timeout=300) as sandbox:
        execution = sandbox.run_code(code)
        return {
            "stdout": execution.text,
            "error": str(execution.error) if execution.error else None,
        }
```

Compared with a host-side Python tool, only the tool body changes. The ADK agent
still sees a regular function tool with structured inputs and outputs.

### Migrating a local ADK code tool

```diff
- def run_python(code: str) -> dict:
-     completed = subprocess.run(["python3", "-c", code], capture_output=True, text=True)
-     return {"stdout": completed.stdout, "stderr": completed.stderr}
+ def run_python(code: str) -> dict:
+     return run_python_in_cube(code)
```

The agent definition and prompts can remain unchanged if they already reference
the original code execution tool.

## Runnable Demo

Run the offline wiring check first:

```bash
cd examples/google-adk-integration
python smoke_test.py
```

Expected output:

```text
GOOGLE_ADK_CUBE_SMOKE_OK
```

Then run the ADK agent from the parent directory:

```bash
cd examples
adk run google-adk-integration
```

Ask:

```text
Run Python in the sandbox to calculate the first 10 Fibonacci numbers.
```

The agent calls `run_python_in_cube`, creates a temporary CubeSandbox from
`CUBE_TEMPLATE_ID`, executes the generated Python code, returns the tool output,
and deletes the temporary sandbox when the tool call finishes.

## Going Further

- **Reuse a sandbox per session:** the example creates one temporary sandbox per
  tool call for simplicity. For multi-step notebooks, keep a sandbox handle in a
  session-scoped service and delete it when the ADK session ends.
- **Timeouts:** set `CUBE_SANDBOX_TIMEOUT` or pass a fixed timeout to
  `Sandbox.create(...)` for longer analysis tasks.
- **Network policy:** create sandboxes with Cube-specific egress controls when
  generated code must only reach approved hosts. See
  [network policy](/guide/network-policy).
- **Credential handling:** keep model provider keys on the host. If sandboxed
  code must call an external API, prefer Cube's security proxy and credential
  injection flow instead of placing raw secrets in the VM.
- **Templates:** preinstall heavy dependencies in the template image so each ADK
  tool call starts quickly from a ready snapshot.

## Caveats

- The example validates imports and wiring offline. A full live run requires a
  reachable CubeSandbox deployment and a `run_code`-capable template.
- Google ADK's native Agent Runtime Code Execution tool targets Google Cloud
  Agent Runtime. This guide uses a custom ADK function tool so CubeSandbox can
  provide the execution backend.
- The default example creates and deletes a sandbox for each tool call. That is
  easiest to review, but not the lowest-latency shape for long multi-step tasks.

## References

- Runnable example:
  [`examples/google-adk-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/google-adk-integration)
- Google ADK Python:
  <https://github.com/google/adk-python>
- ADK custom tools:
  <https://adk.dev/tools-custom/>
- ADK Agent Runtime Code Execution:
  <https://adk.dev/integrations/code-exec-agent-runtime/>
