---
title: LangChain Integration Guide
author: peerless-hero
date: 2026-07-07
tags:
  - integration
  - langchain
  - agent
lang: en-US
---

# LangChain Integration Guide

Run a LangChain agent that calls a Python tool inside a
[CubeSandbox](https://github.com/TencentCloud/CubeSandbox) MicroVM. Because Cube exposes an
**E2B-compatible API**, migrating a LangChain app from E2B to Cube usually means changing a few
environment variables, while giving every line of agent-generated code KVM-level isolation. This guide
ships a runnable
[`examples/langchain-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/langchain-integration)
sample project.

## Two major LangChain versions (0.x and 1.x)

This guide provides **two variants** that differ only in the LangChain Agent API and the required
Python version. Both share the same `run_python` tool (built on the official `cubesandbox` SDK) and
the same sandbox template.

| Variant | LangChain | langchain-openai | Python | Agent API | Example path |
|---|---|---|---|---|---|
| **0.x** (legacy) | `0.3.x` | `0.3.x` | 3.9+ | `AgentExecutor` + `create_react_agent` | `examples/langchain-integration/0.x` |
| **1.x** (modern) | `1.x` | `1.x` | 3.10+ | `langchain.agents.create_agent` + `@tool` | `examples/langchain-integration/1.x` |

If you are on Python 3.9, or already have LangChain 0.3.x code → use **0.x**. If you are on
Python 3.10+ and starting fresh (or already on LangChain 1.x) → use **1.x**.

### Components and versions

| Component | 0.x | 1.x |
|---|---|---|
| LangChain | `langchain==0.3.23` | `langchain>=1.3.14,<2.0` |
| langchain-openai | `langchain-openai==0.3.12` | `langchain-openai>=1.0,<2.0` |
| LangGraph (create_agent runtime) | — (not used; `AgentExecutor` + `create_react_agent` have no LangGraph dep) | `langgraph>=0.2` (declared in `1.x/requirements.txt` so the sample is self-contained) |
| cubesandbox SDK (main driver) | `cubesandbox>=0.6.0` | `cubesandbox>=0.6.0` |
| CubeSandbox base image | `ghcr.io/tencentcloud/cubesandbox-base:2026.16` | same |
| CubeSandbox platform | `>= 0.3.0` (core); higher for optional features below | `>= 0.3.0` (core); higher for optional features below |

Per-feature platform minimums (the SDK floor above covers the base workflow; raise
`cubesandbox`/platform only when you use the feature):

| Feature | Minimum platform | Minimum SDK |
|---|---|---|
| Base workflow (`commands.run` / `files.write` over the envd process API on 49983, context-manager teardown) | `>= 0.3.0` | `>= 0.6.0` |
| CubeEgress secret injection (advanced usage, default-deny egress) | `>= 0.4.0` | `>= 0.3.0` |
| Volume mounts (`volume_mounts=`, `Volume` API) | `>= 0.6.0` | `>= 0.6.0` |

## Prerequisites

- CubeSandbox is deployed and CubeAPI is reachable at `http://<node>:3000`.
- `cubemastercli` is on your `$PATH` and connected to the cluster.
- The build machine has Docker and the registry is pullable by Cube nodes.
- Python 3.10+ (for the 1.x variant). The 0.x variant also runs on Python 3.9+.
- Reachability to the Cube cluster node. The `cubesandbox` SDK reaches CubeAPI via `CUBE_API_URL`
  and the data plane via `CUBE_PROXY_NODE_IP` (so `*.cube.app` resolves without DNS); if the proxy
  does not listen on port 80, set `CUBE_PROXY_PORT_HTTP`.
- An OpenAI-compatible LLM endpoint (the sample uses TokenHub; any OpenAI-compatible endpoint works
  via `OPENAI_BASE_URL` / `OPENAI_API_KEY`).

## Why run the LangChain agent in a sandbox

LangChain agents often expose a code-execution tool (data analysis, file conversion, shell calls).
Running that tool on the host mixes the agent's blast radius with your dev machine. Inside
CubeSandbox you get:

| Concern | What CubeSandbox provides |
|---|---|
| **Isolation** | One KVM MicroVM per session, with its own guest kernel — agent code cannot touch the host |
| **Reproducibility** | Every session boots from the same template snapshot |
| **Fast startup** | Cold start under 60ms, so running many agents in parallel is cheap |
| **Long tasks** | `sandbox.pause()` snapshots the VM + root filesystem for later resume |
| **Secret hygiene** | CubeEgress can inject LLM auth headers at the network layer — the real secret is never visible inside the VM |
| **Egress audit** | Every outbound request is logged to the egress audit log |

## Integration steps

### 1. Build the template image

Layer a Python data-science stack on top of `cubesandbox-base` (envd listens on `:49983`).

```dockerfile
# examples/langchain-integration/Dockerfile
ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        python3 python3-pip ca-certificates git \
    && rm -rf /var/lib/apt/lists/*

# Pin versions for reproducible builds; upgrade as needed
RUN python3 -m pip install --no-cache-dir --upgrade pip \
    && python3 -m pip install --no-cache-dir --break-system-packages \
        pandas==2.2.3 numpy==1.26.4 matplotlib==3.9.3 scikit-learn==1.6.1

WORKDIR /workspace

# Seed a demo dataset so the sample agent runs without external input
COPY sales.csv /workspace/sales.csv

EXPOSE 49983
```

Build and push:

```bash
docker build --platform linux/amd64 \
  -t <your-registry>/langchain-cube:latest \
  examples/langchain-integration
docker push <your-registry>/langchain-cube:latest
```

### 2. Register as a Cube template

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/langchain-cube:latest \
  --writable-layer-size 2G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health
```

`create-from-image` blocks and watches the job to completion by default; pass `--detach` if you
want to submit-and-exit and poll later with `cubemastercli tpl watch --job-id <job_id>`.

Once the job reaches `READY`, note the `template_id` — you must pass it to every
`Sandbox.create()`. The `2G` writable layer suits medium analysis tasks; raise it to `4G+` if the
agent installs large packages at runtime.

### 3. Install dependencies and configure env vars

```bash
cd examples/langchain-integration/1.x      # or 0.x, depending on your LangChain version
cp ../.env.example .env
# Fill in CUBE_API_URL, CUBE_API_KEY, CUBE_TEMPLATE_ID, CUBE_PROXY_NODE_IP, and your LLM key
pip install -r requirements.txt
```

| Variable | Flows to | Notes |
|---|---|---|
| `CUBE_API_URL` | `cubesandbox` SDK | CubeAPI address (`http://<node>:3000`) |
| `CUBE_API_KEY` | `cubesandbox` SDK | (optional) `X-API-Key` header; only set it for an auth-enabled CubeAPI backend — the SDK sends no auth header when unset |
| `CUBE_TEMPLATE_ID` | `Sandbox.create(template=...)` | From step 2 |
| `CUBE_PROXY_NODE_IP` | `cubesandbox` SDK | CubeProxy node IP so `*.cube.app` resolves without DNS |
| `CUBE_PROXY_PORT_HTTP` | `cubesandbox` SDK | Proxy HTTP port (default `80`; set `8081` if the proxy listens on 8081) |
| `OPENAI_API_KEY` / `TOKENHUB_API_KEY` | LLM client | OpenAI-compatible key |
| `OPENAI_BASE_URL` | LLM client | e.g. `https://tokenhub.tencentmaas.com/v1` |
| `CHAT_MODEL` | LLM client | e.g. `deepseek-v3` |
| `CUBE_SSL_CERT_FILE` | demo | (optional) self-signed CA cert path for HTTPS CubeAPI. The demo exports it as `SSL_CERT_FILE` (process-global) so the gRPC client trusts it — the bundle should include public root CAs or the LLM endpoint is affected too |

### 4. Wire the code-execution tool to Cube

The agent logic is identical across versions; only the tool wrapper changes from a local REPL to the
Cube sandbox. The snippet below is the **shared `run_python` tool for both `0.x` and `1.x`** —
built on the official **`cubesandbox` Python SDK** (`from cubesandbox import Sandbox`), using
`sandbox.files.write` to upload code and `sandbox.commands.run` to execute, so **no raw HTTP** is
needed.

```python
# run_python tool — official cubesandbox SDK (used by the snippets below;
# the shipped demos inline equivalent logic inside build_agent)
import itertools
from cubesandbox import Sandbox

def make_run_python(sandbox: Sandbox):
    """Return a run_python tool bound to the given created `sandbox`."""
    script_counter = itertools.count()
    def run_python(code: str) -> str:
        """Execute Python inside the Cube Sandbox MicroVM; return stdout, with
        stderr delimited below it when present.

        The image preinstalls pandas / numpy / matplotlib / scikit-learn. Each call
        writes the snippet to a unique /workspace/_agent_<n>.py and runs it, so
        concurrent tool calls don't overwrite each other. Charts can be saved under
        /workspace (e.g. /workspace/revenue.png).
        """
        script = f"/workspace/_agent_{next(script_counter)}.py"
        sandbox.files.write(script, code)
        result = sandbox.commands.run(f"python3 {script}", timeout=120, cwd="/workspace")
        out = result.stdout
        # Keep stderr delimited from stdout so library warnings (exit_code 0)
        # don't blur the real output seen by the LLM.
        if result.stderr:
            out += "\n--- stderr ---\n" + result.stderr
        if result.exit_code != 0:
            out += f"\n[non-zero exit code: {result.exit_code}]"
        return out
    return run_python
```

> **One sandbox for the whole run, auto-torn-down.** The entire agent loop creates a single MicroVM
> (`Sandbox.create(template=...)`) and the context manager calls `sandbox.kill()`
> (`DELETE /sandboxes/:sandboxID`) when the `with` block exits, so no sandbox leaks. The bundled
> sample uses exactly this pattern.

### 5. Run the agent

Both variants build the same `ChatOpenAI` client and reuse the shared `run_python` tool; they differ
only in how the agent is constructed. Create one `Sandbox` for the whole run and bind the tool with
`make_run_python(sandbox)`:

#### 1.x (modern — `langchain.agents.create_agent`)

```python
import os
from dotenv import load_dotenv

from langchain_openai import ChatOpenAI
from langchain.agents import create_agent
from langchain_core.tools import tool
from cubesandbox import Sandbox

load_dotenv()
_llm_key = os.getenv("OPENAI_API_KEY") or os.getenv("TOKENHUB_API_KEY")
if not _llm_key:
    raise SystemExit("Missing LLM API key (set OPENAI_API_KEY or TOKENHUB_API_KEY)")

llm = ChatOpenAI(
    model=os.getenv("CHAT_MODEL") or "deepseek-v3",
    api_key=_llm_key,
    base_url=os.getenv("OPENAI_BASE_URL") or "https://tokenhub.tencentmaas.com/v1",
    timeout=60,
    max_retries=2,
    temperature=0,
)

SANDBOX_CONTEXT = (
    "You are a data analyst. You can execute Python inside a Cube Sandbox "
    "MicroVM via the run_python tool. Environment facts:\n"
    "- Working directory: /workspace\n"
    "- Demo dataset: /workspace/sales.csv with columns month,product,units,price\n"
    "  (6 rows: 3 months x 2 products; revenue is defined as units * price)\n"
    "- Preinstalled: pandas, numpy, matplotlib, scikit-learn\n"
    "- Save any charts/artifacts under /workspace\n"
    "When the user mentions 'the dataset' without a path, use /workspace/sales.csv.\n"
    "Modeling conventions for this tiny demo dataset (follow them unless the "
    "user explicitly specifies otherwise):\n"
    "- Regression/forecast target: monthly TOTAL revenue. Aggregate to one row "
    "per month, then use a numeric month index (0, 1, 2, ...) as the only "
    "feature.\n"
    "- Never use the target itself or its direct components (units, price) as "
    "features when predicting revenue - that is data leakage and yields a "
    "meaningless RMSE of 0.\n"
    "- The dataset is too small for a train/test split; fit and evaluate on "
    "all rows and explicitly state that the metric is in-sample.\n"
    "- Report only numbers actually printed by the executed code; never "
    "invent or estimate metric values."
)

with Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"], timeout=600) as sandbox:
    run_python = tool(make_run_python(sandbox))
    agent = create_agent(llm, [run_python], system_prompt=SANDBOX_CONTEXT)
    result = agent.invoke({"messages": [{"role": "user", "content":
        "Load sales.csv from /workspace, compute total revenue per month, "
        "and report the month -> revenue numbers in your final answer."}]})
    for msg in reversed(result["messages"]):
        if msg.content:
            print(msg.content)
            break
    else:
        print("(no final answer in messages)")
```

#### 0.x (legacy — `AgentExecutor` + `create_react_agent`)

```python
import os
from dotenv import load_dotenv

from langchain_openai import ChatOpenAI
from langchain.agents import AgentExecutor, create_react_agent
from langchain.tools import Tool
from langchain_core.prompts import PromptTemplate
from cubesandbox import Sandbox

load_dotenv()
_llm_key = os.getenv("OPENAI_API_KEY") or os.getenv("TOKENHUB_API_KEY")
if not _llm_key:
    raise SystemExit("Missing LLM API key (set OPENAI_API_KEY or TOKENHUB_API_KEY)")

llm = ChatOpenAI(  # same params as above
    model=os.getenv("CHAT_MODEL") or "deepseek-v3",
    api_key=_llm_key,
    base_url=os.getenv("OPENAI_BASE_URL") or "https://tokenhub.tencentmaas.com/v1",
    timeout=60, max_retries=2, temperature=0,
)

with Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"], timeout=600) as sandbox:
    run_python = make_run_python(sandbox)
    tools = [Tool(name="run_python", func=run_python,
                  description="Execute Python code in a Cube Sandbox MicroVM.")]
    agent = create_react_agent(llm, tools, PromptTemplate.from_template(
        "You are a data analyst. Answer the question using tools.\n\n"
        "Environment facts:\n"
        "- Demo dataset: /workspace/sales.csv with columns month,product,units,price\n"
        "  (6 rows: 3 months x 2 products; revenue is defined as units * price)\n"
        "- Preinstalled: pandas, numpy, matplotlib, scikit-learn\n"
        "- Save any charts/artifacts under /workspace\n"
        "When the user mentions 'the dataset' without a path, use /workspace/sales.csv.\n"
        "Modeling conventions (unless the user specifies otherwise):\n"
        "- Regression target: monthly TOTAL revenue; aggregate per month and use "
        "a numeric month index (0, 1, 2, ...) as the only feature.\n"
        "- Never use the target or its components (units, price) as features - "
        "that is data leakage and yields a meaningless RMSE of 0.\n"
        "- Too small for a train/test split; evaluate in-sample and say so.\n"
        "- Report only numbers actually printed by the executed code.\n\n"
        "Tools: {tools}\n\n"
        "Use the following format:\n\n"
        "Question: the input question you must answer\n"
        "Thought: you should always think about what to do\n"
        "Action: the action to take, should be one of [{tool_names}]\n"
        "Action Input: the input to the action\n"
        "Observation: the result of the action\n"
        "... (this Thought/Action/Action Input/Observation can repeat N times)\n"
        "Thought: I now know the final answer\n"
        "Final Answer: the final answer to the original input question\n\n"
        "Begin!\n\n"
        "Question: {input}\n\nThought: {agent_scratchpad}"))
    executor = AgentExecutor(agent=agent, tools=tools, verbose=True, handle_parsing_errors=True)
    result = executor.invoke({"input":
        "Load sales.csv from /workspace, compute total revenue per month, "
        "and report the month -> revenue numbers in your final answer."})
    print(result["output"])
```

Run (from the chosen variant directory):

```bash
python langchain_agent_demo.py
# Custom question:
python langchain_agent_demo.py "Train a linear model on the dataset and report the RMSE."
```

### Expected output

With the default prompt, the **agent's final answer** printed on the host includes the per-month
revenue numbers (2780.0 / 3375.5 / 3872.0). (The 1.x `create_agent` is not verbose by default; the
0.x `AgentExecutor(verbose=True)` also prints the full reasoning trace to the console.)

Teardown is best-effort: if `kill()` fails (e.g. the sandbox already expired), the SDK's context
manager **silently swallows** the error — no warning is printed. If you need to detect teardown
failures, call `sandbox.kill()` explicitly and handle `CubeSandboxError` yourself.

## Key snippets

### Migrate an E2B app to Cube

If your LangChain app already drives an E2B sandbox, the code is almost identical — just swap the
import for the `cubesandbox` SDK and point it at CubeAPI:

```diff
- from e2b_code_interpreter import Sandbox
- # E2B cloud (managed)
- export E2B_API_KEY="e2b_xxx"
- export E2B_API_URL="https://api.e2b.dev"   # default, usually omitted
+ from cubesandbox import Sandbox
+ # Cube Sandbox (self-hosted, MicroVM isolation)
+ export CUBE_API_URL="http://<your-cube-host>:3000"
+ # export CUBE_API_KEY="<your-api-key>"  # only for auth-enabled backends
+ export CUBE_TEMPLATE_ID="<your-cube-template-id>"
+ export CUBE_PROXY_NODE_IP="<your-cube-host>"
```

The `Sandbox` API (`Sandbox.create`, `commands.run`, `files.write`, `kill`) keeps the same shape, so
the rest of your tool code needs no changes. The sample above uses exactly this SDK.

### Local baseline (the part being replaced)

```python
# Before: code runs on the host via langchain_experimental
from langchain_experimental.tools import PythonREPLTool
tools = [PythonREPLTool()]
```

Swap `PythonREPLTool` for the `run_python` Cube tool above to get isolation for free.

## Caveats

- **envd user.** The `cubesandbox` SDK's `files.write` / `commands.run` run as `root` by default,
  so no extra config is needed. (The base image also provisions a non-root `uid=1000` user; if you
  switch the SDK to run as that user, file-permission behavior changes.)
- **Cleanup safety.** `with Sandbox.create(...) as sandbox:` calls `sandbox.kill()`
  (`DELETE /sandboxes/:sandboxID`, **no** `/kill` subpath) on exit, so no sandbox leaks even if the
  agent raises.
- **One sandbox for the whole run, reused across calls.** The entire agent loop creates a single
  MicroVM and reuses it between `run_python` calls, so you pay the lifecycle cost only once.
- **State does not persist across turns.** Each `commands.run` is a fresh `python` process;
  variables/imports do not survive between tool calls. Inline everything the snippet needs, or write
  intermediate state to `/workspace` and read it back.
- **The template must contain the required stack.** Preinstall pandas/numpy/matplotlib into the
  image; under a default-deny egress policy, a runtime `pip install` will fail.
- **Timeouts.** Set both the sandbox `timeout` (platform reclaim) and the per-command `timeout`; a
  long agent loop can exhaust either.

## Advanced usage

### Network isolation + secret injection (native SDK)

On a shared cluster, prefer the native `cubesandbox` SDK together with default-deny egress and
network-layer secret injection, so the LLM secret never enters the VM:

```python
import os
from cubesandbox import Sandbox, Rule, Match, Action, Inject

rules = [
    Rule(
        name="allow_llm",
        match=Match(scheme="https", sni="api.openai.com", host="api.openai.com"),
        action=Action(allow=True, audit="metadata", inject=[
            Inject(header="Authorization",
                   secret=os.environ["OPENAI_API_KEY"],
                   format="Bearer ${SECRET}"),
        ]),
    ),
]
with Sandbox.create(
    template=os.environ["CUBE_TEMPLATE_ID"],
    timeout=600,
    allow_internet_access=False,           # default-deny; hosts in rules are allowed
    network={"rules": rules},
) as sandbox:
    run_agent(sandbox)                     # sandbox.kill() runs on block exit
```

### File mounts

`Sandbox.create()` has no `mounts` parameter. Use the Cube-specific APIs instead:

- **Host directory mounts** go through `metadata["host-mount"]` (a JSON-encoded list of
  `{hostPath, mountPath, readOnly}` descriptors), so the agent reads/writes shared host data
  directly without round-tripping through `files.write`. Note that `hostPath` must live under an
  **allowed prefix** (by default `/data/shared/`; extend `allowed_host_mount_prefixes` in
  CubeMaster's config to use other paths):

```python
import json
from cubesandbox import Sandbox

sandbox = Sandbox.create(
    template=os.environ["CUBE_TEMPLATE_ID"],
    metadata={
        "host-mount": json.dumps([
            {"hostPath": "/data/shared/datasets", "mountPath": "/workspace/datasets",
             "readOnly": True},
        ]),
    },
)
```

- **Volume mounts** use `volume_mounts={mount_path: volume_id}` (see
  [`examples/volume`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/volume)):

```python
from cubesandbox import Sandbox, Volume

vol = Volume.create("my-workspace")
sandbox = Sandbox.create(
    template=os.environ["CUBE_TEMPLATE_ID"],
    volume_mounts={"/workspace": vol},
)
```

### Long-task pause / resume

`pause()` returns `None` — the sandbox ID never changes. Call `Sandbox.connect()` on the same ID to
resume:

```python
sandbox = Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"], timeout=1800)
try:
    turn_1(sandbox)
    sandbox.pause()
    sandbox = Sandbox.connect(sandbox.sandbox_id)   # /workspace intact after resume
    turn_2(sandbox)
finally:
    sandbox.kill()
```

## Troubleshooting

| Symptom | Possible cause | Fix |
|---|---|---|
| File op `permission denied` | envd user mismatch | The `cubesandbox` SDK's `files.write` / `commands.run` run as `root` by default. If using the E2B SDK, set `e2b.envd.rpc.default_username = "root"`. |
| `command not found: python` | Template missing Python | Rebuild the image with `python3` |
| `ModuleNotFoundError: pandas` | Stack not in image | Add pandas/numpy/matplotlib in the Dockerfile |
| `403 Forbidden - CubeEgress` | Default-deny with no allow rule | Allow the LLM host (and any other needed hosts) |
| CubeAPI `Connection refused` | Wrong `CUBE_API_URL` | Set `http://<node>:3000` |
| Template stuck in `PULLING` | Cluster cannot reach the registry | Push to a registry the cluster can reach |
| `run_python` returns nothing | Script has no stdout | Have the agent explicitly `print()` results |

## References

- Runnable samples — `0.x` (LangChain 0.3.x): [`examples/langchain-integration/0.x`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/langchain-integration/0.x) · `1.x` (LangChain 1.x): [`examples/langchain-integration/1.x`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/langchain-integration/1.x)
- Bring your own image: [`docs/guide/tutorials/bring-your-own-image.md`](../tutorials/bring-your-own-image.md)
- Create a template from an image: [`docs/guide/tutorials/template-from-image.md`](../tutorials/template-from-image.md)
- Snapshot / clone / rollback: [`docs/guide/snapshot-rollback-clone.md`](../snapshot-rollback-clone.md)
- Credential safekeeping + egress control: [`docs/guide/security-proxy.md`](../security-proxy.md)
- LangChain: <https://github.com/langchain-ai/langchain>
- E2B SDK: <https://github.com/e2b-dev/e2b>
- `langchain_e2b` (E2B official LangChain integration): <https://github.com/e2b-dev/langchain_e2b>
