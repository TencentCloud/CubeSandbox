---
title: CrewAI Integration Guide
author: eatLaoJun
date: 2026-08-28
tags:
  - integration
  - crewai
  - agent
lang: en-US
---

# CrewAI Integration Guide

[中文](../../zh/guide/integrations/crewai.md)

Run a CrewAI agent's shell tool inside a CubeSandbox MicroVM. CrewAI's built-in
`E2BExecTool` uses the official E2B SDK, while CubeSandbox exposes an E2B-compatible API.
The integration therefore keeps the Agent, Task, and Tool definitions intact and changes
only the sandbox endpoint, API key, and template ID.

## Integration Target and Version

| Component | Baseline used by this guide |
| --- | --- |
| CrewAI / `crewai-tools` | `1.15.18` with the `e2b` extra |
| Python | 3.10–3.13 |
| Sandbox tool | `crewai_tools.E2BExecTool` |
| CubeSandbox | E2B-compatible CubeAPI and a reachable CubeProxy data plane |
| Cube template | Linux image with envd listening on port `49983` |

`E2BExecTool` is used instead of the code-interpreter tool so the demo works with a
standard CubeSandbox template. `E2BPythonTool` additionally requires a template that
runs the code-interpreter service on port `49999`.

## Prerequisites

- A running [CubeSandbox deployment](/guide/quickstart) with CubeAPI reachable, normally
  at `http://<cube-host>:3000`.
- A ready Cube template ID. The template must expose and probe envd on port `49983`.
- Python 3.10–3.13 on the machine running CrewAI.
- An OpenAI API key, or another LLM provider configured according to CrewAI's LLM docs.

::: warning Control plane and data plane
`E2B_API_URL` points to the CubeAPI control plane. The E2B SDK also connects to a
per-sandbox data-plane hostname through CubeProxy. Configure wildcard DNS in production,
or use the [E2B development sidecar](/guide/multi-node-deploy#official-e2b-sdk-without-wildcard-dns-dev-sidecar)
for local development without wildcard DNS.
:::

## Setup

### 1. Prepare a Cube template

You can reuse any Linux template that runs envd on port `49983`. For example:

```bash
cubemastercli tpl create-from-image \
  --image ghcr.io/tencentcloud/cubesandbox-base:2026.16 \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

Wait for the build to become `READY`, then copy its `template_id`:

```bash
cubemastercli tpl watch --job-id <job_id>
```

### 2. Install dependencies

```bash
python3 -m venv .venv
source .venv/bin/activate
pip install "crewai-tools[e2b]==1.15.18" "python-dotenv>=1,<2"
```

The `e2b` extra installs both the E2B SDK and the matching CrewAI package.

### 3. Configure environment variables

Create a `.env` file:

```dotenv
E2B_API_URL=http://<cube-host>:3000
E2B_API_KEY=e2b_000000
CUBE_TEMPLATE_ID=<ready-template-id>

OPENAI_API_KEY=<your-openai-key>
OPENAI_MODEL_NAME=gpt-4o-mini
```

| Variable | Used by | Purpose |
| --- | --- | --- |
| `E2B_API_URL` | E2B SDK | CubeAPI control-plane URL |
| `E2B_API_KEY` | E2B SDK | CubeAPI bearer credential; use `e2b_000000` only when authentication is disabled |
| `CUBE_TEMPLATE_ID` | Demo | Passed to `E2BExecTool(template=...)` |
| `OPENAI_API_KEY` | CrewAI | LLM credential kept in the local Agent process |
| `OPENAI_MODEL_NAME` | CrewAI | LLM model; defaults to `gpt-4o-mini` when omitted |
| `OPENAI_API_BASE` | CrewAI | Optional base URL for an OpenAI-compatible provider |

Do not set the tool's `domain` argument for this setup. The underlying E2B SDK reads
`E2B_API_URL` directly and sends sandbox creation requests to CubeAPI.

## Integration Snippet

An existing E2B-backed CrewAI application only needs its connection settings changed:

```diff
- E2B_API_URL=https://api.e2b.dev
- E2B_API_KEY=<e2b-cloud-key>
- SANDBOX_TEMPLATE=<e2b-template>
+ E2B_API_URL=http://<cube-host>:3000
+ E2B_API_KEY=e2b_000000
+ SANDBOX_TEMPLATE=<cube-template-id>
```

The Tool remains the same:

```python
import os

from crewai_tools import E2BExecTool

sandbox_tool = E2BExecTool(
    template=os.environ["SANDBOX_TEMPLATE"],
    persistent=False,
    sandbox_timeout=120,
)
```

With `persistent=False` (the default), CrewAI creates a fresh MicroVM for each tool call
and kills it when that call finishes.

## Runnable Demo

Save the following as `main.py`:

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

First verify the sandbox connection without spending an LLM request:

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

Then run the CrewAI Agent:

```bash
python main.py
```

The verbose trace should show `E2B Sandbox Exec` being called, followed by the kernel
and Python version returned from the Cube MicroVM.

## Going Further

- **Keep state across tool calls:** set `persistent=True` and call `sandbox_tool.close()`
  explicitly when the crew finishes. Persistent mode trades stronger continuity for a
  larger prompt-injection blast radius.
- **Run Python cells:** use `E2BPythonTool` with a Cube code-interpreter template that
  exposes both envd (`49983`) and Jupyter (`49999`).
- **Restrict outbound access:** apply a Cube [network policy](/guide/network-policy) or
  [security proxy](/guide/security-proxy) policy to the template/workload.
- **Use files or durable data:** add `E2BFileTool`, or configure Cube
  [persistent storage](/guide/persistent-storage) when data must outlive a sandbox.
- **Bound execution:** keep both `sandbox_timeout` and each tool call's `timeout` as short
  as the workload permits.

## Caveats and Credential Handling

- `E2B_API_KEY` authenticates the local E2B SDK to CubeAPI. When CubeAPI authentication
  is enabled, replace the placeholder with the real credential accepted by the auth
  callback.
- `OPENAI_API_KEY` stays in the local CrewAI process. The example does not pass it through
  `E2BExecTool(envs=...)`, so agent-generated commands cannot read the LLM credential.
- Values supplied through a tool's `envs` field or files written into the sandbox are
  visible to agent-generated code. Do not inject long-lived production secrets.
- `E2B_API_URL` does not configure data-plane routing by itself. Verify CubeProxy and
  `*.cube.app` reachability, or use the development sidecar.
- The tool cleans up ephemeral sandboxes on normal completion. Cube's timeout remains the
  final cleanup boundary if the client process is interrupted.

## References

- [CrewAI E2B Sandbox Tools](https://docs.crewai.com/en/tools/ai-ml/e2bsandboxtools)
- [CrewAI LLM connections](https://docs.crewai.com/en/learn/llm-connections)
- [CubeSandbox quick start](/guide/quickstart)
- [Connecting clients to a CubeSandbox cluster](/guide/multi-node-deploy#connect-clients-to-the-cluster)
- [CubeSandbox authentication](/guide/authentication)
