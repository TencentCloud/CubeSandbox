---
title: Pydantic AI Integration Guide
author: WuShang-d
date: 2026-09-21
tags:
  - integration
  - pydantic-ai
  - agent
lang: en-US
---

# Pydantic AI Integration Guide

[中文](../../zh/guide/integrations/pydantic-ai.md)

Give a [Pydantic AI](https://ai.pydantic.dev) agent a code-execution tool that
runs inside a CubeSandbox MicroVM. The model decides when to run Python; the code
executes in an isolated MicroVM, and its real stdout / stderr / exit code flow
back to the model as the tool result.

```text
user prompt
  -> Pydantic AI Agent (agent.run_sync)
  -> the model calls the run_python tool
  -> the tool writes the snippet + runs it inside the CubeSandbox MicroVM
  -> stdout / stderr / exit code returned to the model
  -> the model continues reasoning
  -> final answer
```

This guide wires Pydantic AI **function tools** to the official
[`cubesandbox` Python SDK](https://pypi.org/project/cubesandbox/). It does not add
a new Pydantic AI model backend and does not modify CubeSandbox — it is a small,
focused function-tool integration. A complete runnable example lives in
[`examples/pydantic-ai-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/pydantic-ai-integration).

## Integration Target and Version

| Component | Baseline used by the bundled example |
| --- | --- |
| Pydantic AI | `pydantic-ai` 2.x (`Agent`, `@agent.tool`, `RunContext`, `OpenAIChatModel`) |
| CubeSandbox SDK | `cubesandbox` 0.7.0 (`Sandbox`, `commands.run`, `files.write`) |
| CubeSandbox platform | E2B-compatible CubeAPI with a reachable CubeProxy data plane |
| Python | 3.10+ (required by `pydantic-ai`) |
| LLM | Any OpenAI-compatible chat endpoint |

The example was authored and statically validated (imports, tool wiring, sandbox
reuse, and failure paths) against `pydantic-ai` 2.46.0 and `cubesandbox` 0.7.0.
Pin the resolved versions after validating them against your own deployment.

## Prerequisites

- A running [CubeSandbox deployment](/guide/quickstart) with CubeAPI reachable,
  normally at `http://<cube-host>:3000`.
- A Python-capable sandbox template. The default demo only uses the Python
  standard library, so the official `sandbox-code` template is sufficient — no
  custom image is required.
- `cubemastercli` connected to the cluster to register the template.
- Python 3.10+ on the machine running the agent.
- An OpenAI-compatible LLM endpoint and API key.

::: tip Agent host vs. Cube host
The machine running the Pydantic AI agent and the machine running CubeSandbox do
not have to be the same. The agent talks to CubeAPI/CubeProxy over the network,
so a laptop can drive a remote Linux Cube host. This example uses the native
`cubesandbox` SDK, which connects straight to a CubeProxy IP via
`CUBE_PROXY_NODE_IP` — no wildcard DNS required. See
[CubeSandbox SDK: direct CubeProxy access](/guide/multi-node-deploy#cubesandbox-sdk-direct-cubeproxy-access).
:::

## Integration Steps

### 1. Register the official code sandbox template

The default task is standard-library only, so the prebuilt `sandbox-code` image
is enough:

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
# In mainland China, use cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest
```

Copy the `template_id` from the output.

### 2. Install the example and configure the environment

```bash
cd examples/pydantic-ai-integration
python3 -m venv .venv && source .venv/bin/activate
pip install -r requirements.txt
cp .env.example .env
```

Fill in `.env`:

| Variable | Required | Purpose |
| --- | --- | --- |
| `CUBE_TEMPLATE_ID` | yes | Template ID from step 1 |
| `CUBE_API_URL` | no | CubeAPI URL; defaults to `http://127.0.0.1:3000` |
| `CUBE_API_KEY` | no | Only for an auth-enabled CubeAPI |
| `CUBE_PROXY_NODE_IP` / `CUBE_PROXY_PORT_HTTP` | no | Reach CubeProxy directly when DNS is not configured |
| `OPENAI_API_KEY` | yes | Key for the OpenAI-compatible endpoint |
| `OPENAI_BASE_URL` | no | Endpoint URL; defaults to `https://tokenhub.tencentmaas.com/v1` |
| `MODEL_NAME` | no | Model name; defaults to `deepseek-v3` (`CHAT_MODEL` also accepted) |
| `CUBE_SSL_CERT_FILE` | no | CA bundle for a self-signed CubeAPI; exported process-globally (see Caveats) |

## Key Code Snippets

Pydantic AI passes typed dependencies into every tool through `RunContext`. Put
the sandbox on the dependency object so one MicroVM is shared across the run:

```python
from dataclasses import dataclass, field
import itertools, shlex
from collections.abc import Iterator

from cubesandbox import Sandbox, CubeSandboxError
from pydantic_ai import Agent, RunContext


@dataclass
class Deps:
    sandbox: Sandbox
    _script_counter: Iterator[int] = field(default_factory=lambda: itertools.count())


agent = Agent(deps_type=Deps, instructions="...")


@agent.tool
def run_python(ctx: RunContext[Deps], code: str) -> str:
    """Execute a Python 3 snippet inside the CubeSandbox MicroVM and return its output."""
    sandbox = ctx.deps.sandbox
    # Unique per-call path from a host-side counter — no model input is
    # interpolated into the shell command.
    script_path = f"/workspace/agent_step_{next(ctx.deps._script_counter)}.py"
    try:
        sandbox.files.write(script_path, code)
        result = sandbox.commands.run(
            f"python3 {shlex.quote(script_path)}", timeout=120, cwd="/workspace"
        )
    except CubeSandboxError as exc:
        return f"[cube-sandbox error] {type(exc).__name__}: {exc}"
    except Exception as exc:  # noqa: BLE001 - e.g. an envd request timeout
        return f"[execution failed] {type(exc).__name__}: {exc}"

    out = result.stdout or ""
    if result.stderr:
        out += "\n--- stderr ---\n" + result.stderr
    if result.exit_code != 0:
        out += f"\n[non-zero exit code: {result.exit_code}]"
    return out or "[no output]"
```

The outer lifecycle creates the MicroVM once and hands it to the agent as `deps`:

```python
# 1800s is a generous idle backstop; each tool call refreshes it, and the
# with-block's kill() is the normal teardown (see Going Further → Timeouts).
with Sandbox.create(template=template_id, timeout=1800,
                    allow_internet_access=False) as sandbox:
    # The stock sandbox-code image ships no /workspace; create it once.
    sandbox.commands.run("mkdir -p /workspace")
    result = agent.run_sync(question, deps=Deps(sandbox=sandbox), model=model)
print(result.output)
```

Configure any OpenAI-compatible endpoint with the current provider API:

```python
from pydantic_ai.models.openai import OpenAIChatModel
from pydantic_ai.providers.openai import OpenAIProvider

model = OpenAIChatModel(
    "deepseek-v3",
    provider=OpenAIProvider(base_url="https://<endpoint>/v1", api_key="<key>"),
)
```

## Runnable Demo

```bash
python pydantic_ai_agent_demo.py
# or pass your own standard-library-only task:
python pydantic_ai_agent_demo.py "Compute the first 15 prime numbers and their sum."
```

The default task asks the agent to generate and verify a Fibonacci sequence. The
final-answer wording varies by model, so verify the behavior rather than a fixed
sentence:

- `run_python` was actually called (the `Sandbox <id> created` line appears, then
  work happens).
- The reported numbers come from real execution and are correct (F(20) = 6765).
- The agent finishes with a final answer instead of looping.

## Going Further

- **Sandbox reuse.** One MicroVM is created per run and reused across every tool
  call. This avoids a per-call MicroVM boot and lets files written in one call
  persist for later calls in the same run. Prefer this over creating a sandbox
  inside each tool invocation.
- **Timeouts.** `commands.run(timeout=...)` bounds a single execution.
  `Sandbox.create(timeout=...)` is an **idle** timeout, reset only when the
  sandbox receives a request — each `run_python` call refreshes it, so ordinary
  model latency is fine, but a very long tool-free stretch could get the MicroVM
  reclaimed mid-run. The example uses a generous `1800`s backstop: normal
  teardown is the `with` block's `kill()`, and the timeout only bounds an
  orphaned MicroVM if the agent host dies first. `NEVER_TIMEOUT` removes the
  backstop entirely (and that orphan risk with it). For a hard wall-clock ceiling
  on the whole run, bound it on the agent side (a Pydantic AI
  [usage limit](https://ai.pydantic.dev/agents/#usage-limits) plus your own
  deadline).
- **Error handling.** The tool returns `CubeSandboxError` and transport timeouts
  to the model as text (with stderr delimited and non-zero exit codes reported)
  so it can retry, while `Sandbox.create()` failures propagate and abort the run
  cleanly.
- **Network isolation.** The demo creates the sandbox with
  `allow_internet_access=False`. Combine this with
  [network policy](/guide/network-policy) and the
  [security proxy](/guide/security-proxy) to allowlist only the egress a task needs.
- **Persistent files.** For state that must outlive a single run, mount a
  [persistent volume](/guide/persistent-storage) instead of relying on the
  ephemeral `/workspace`.

## Caveats

- Treat the MicroVM as untrusted execution. Keep LLM credentials in the agent
  host; do not pass them into the sandbox unless the task requires it.
- The default demo assumes standard-library-only code. If your task needs
  third-party packages, bake them into a template image or enable internet
  access and install them at runtime.
- `CUBE_SSL_CERT_FILE` is exported process-globally (as `SSL_CERT_FILE` /
  `REQUESTS_CA_BUNDLE`) and therefore also applies to the LLM HTTPS client; the
  bundle must include public root CAs.
- `pydantic-ai` requires Python 3.10+, so the agent host cannot run on 3.9 even
  though the CubeSandbox SDK itself supports 3.9.

## References

- [Runnable example](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/pydantic-ai-integration)
- [Pydantic AI documentation](https://ai.pydantic.dev)
- [Pydantic AI function tools](https://ai.pydantic.dev/tools/)
- [CubeSandbox quickstart](/guide/quickstart)
- [Connecting clients to a CubeSandbox cluster](/guide/multi-node-deploy#connect-clients-to-the-cluster)
