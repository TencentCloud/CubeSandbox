---
title: OpenAI Agents SDK Integration Guide
author: ZedingZhang
date: 2026-08-19
tags:
  - integration
  - openai-agents-sdk
  - agent
lang: en-US
---

# OpenAI Agents SDK Integration Guide

[中文](../../zh/guide/integrations/openai-agents-sdk.md)

Use a CubeSandbox MicroVM as the sandbox execution environment for an
[OpenAI Agents SDK](https://developers.openai.com/api/docs/guides/agents/sandboxes)
`SandboxAgent`. CubeSandbox exposes an E2B-compatible API, so the SDK's built-in
`E2BSandboxClient` can provide the sandbox execution plane without a custom
provider implementation.

This page is the short integration entry point. The repository already ships
complete Shell Agent, SWE-bench, pause/resume, and Code Interpreter examples;
the links below let you run and inspect those implementations directly.

## Integration Target and Version

| Component | Baseline used by the bundled examples |
| --- | --- |
| OpenAI Agents SDK | Python package `openai-agents[e2b]` with Sandbox Agents support |
| Python | 3.10+ |
| CubeSandbox | E2B-compatible CubeAPI and a reachable CubeProxy data plane |
| Sandbox modes | Generic E2B (`E2BSandboxType.E2B`) and Code Interpreter (`E2BSandboxType.CODE_INTERPRETER`) |

Sandbox Agents are currently beta in the OpenAI Agents SDK. The example
requirements intentionally install the current SDK release; pin the resolved
versions after validating them for a production deployment.

## Prerequisites

- A running [CubeSandbox deployment](/guide/quickstart) with CubeAPI reachable,
  normally at `http://<cube-host>:3000`.
- `cubemastercli` connected to the cluster and a sandbox template ID.
- Python 3.10+ on the machine running the Agent harness.
- An API key and model name for TokenHub or another OpenAI-compatible LLM
  endpoint when running the full Agent demo.

::: warning Control plane and data plane
`E2B_API_URL` selects the CubeAPI control-plane endpoint. The official E2B SDK
also connects to per-sandbox data-plane hostnames. A one-click local deployment
includes CoreDNS; production deployments should configure wildcard DNS. If you
must use the official E2B SDK locally without wildcard DNS, use the
[E2B development sidecar](/guide/multi-node-deploy#official-e2b-sdk-without-wildcard-dns-dev-sidecar).
:::

## Setup

### 1. Choose a CubeSandbox template

`simple_demo.py` works with any Linux template that runs envd on port `49983`.
You can reuse an existing template or create the SWE-bench template used by the
bundled debugging demo:

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-image.tencentcloudcr.com/demo/django_1776_django-13447:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --cpu 4000 --memory 8192 \
  --probe 49983
```

The command starts an asynchronous build. Use the job ID from its output to
monitor the build:

```bash
cubemastercli tpl watch --job-id <job_id>
```

Wait until the status becomes `READY`, then copy the `template_id` from the
output.

### 2. Install the example dependencies

```bash
cd examples/openai-agents-example
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
cp .env.example .env
```

Configure `.env`:

| Variable | Purpose |
| --- | --- |
| `E2B_API_URL` | CubeAPI control-plane URL, for example `http://<cube-host>:3000` |
| `E2B_API_KEY` | Required by the E2B SDK; use the `e2b_`-prefixed key accepted by your CubeAPI auth callback, or `e2b_000000` when authentication is disabled |
| `CUBE_TEMPLATE_ID` | CubeSandbox template ID |
| `TOKENHUB_API_KEY` | TokenHub key used by the bundled demos |
| `OPENAI_API_KEY` / `OPENAI_BASE_URL` | Alternative OpenAI-compatible LLM credentials and endpoint |
| `CUBE_SSL_CERT_FILE` | Optional CubeSandbox CA bundle for a self-signed deployment |

Use a model name that exists at the configured LLM endpoint. The template
variable is application-owned: an existing E2B application can keep its current
variable name, while the bundled examples use `CUBE_TEMPLATE_ID` for clarity.

## Integration Snippet

Keep the Agent definition and replace only its sandbox connection settings:

```python
import asyncio
import os

from agents import Runner
from agents.run import RunConfig
from agents.sandbox import SandboxRunConfig
from agents.extensions.sandbox import (
    E2BSandboxClient,
    E2BSandboxClientOptions,
    E2BSandboxType,
)

async def main():
    run_config = RunConfig(
        sandbox=SandboxRunConfig(
            client=E2BSandboxClient(),
            options=E2BSandboxClientOptions(
                sandbox_type=E2BSandboxType.E2B,
                template=os.environ["CUBE_TEMPLATE_ID"],
                timeout=300,
            ),
        ),
        workflow_name="Cube shell agent",
    )

    result = await Runner.run(
        agent,
        "What OS is running? Show uname and /etc/os-release.",
        run_config=run_config,
    )
    print(result.final_output)


# `agent` is your existing SandboxAgent.
asyncio.run(main())
```

The checked-in [`simple_demo.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/openai-agents-example/simple_demo.py)
adds a complete `SandboxAgent`, model configuration, cleanup, and the current
CubeSandbox envd compatibility handling around this core snippet.

### Migrating an existing E2B-backed Agent

The client class does not change. Point the existing E2B configuration at Cube
and provide a Cube template ID:

```diff
- E2B_API_URL="https://api.e2b.dev"
- E2B_API_KEY="<e2b-cloud-key>"
- SANDBOX_TEMPLATE="<e2b-template>"
+ E2B_API_URL="http://<cube-host>:3000"
+ E2B_API_KEY="e2b_000000"
+ SANDBOX_TEMPLATE="<cube-template-id>"
```

The example uses `e2b_000000` for a deployment with CubeAPI authentication
disabled. If authentication is enabled, replace it with the `e2b_`-prefixed
credential accepted by your auth callback.

`SANDBOX_TEMPLATE` represents whatever environment variable your application
already passes to `E2BSandboxClientOptions(template=...)`; it does not need to
be renamed.

## Runnable Demo

First verify the sandbox path without making an LLM request:

```bash
cd examples/openai-agents-example
python main.py --sandbox-only --timeout 60
```

Verify that filesystem state survives a pause/resume cycle:

```bash
python simple_demo.py --pause-resume
```

Then run the Shell Agent against a real task:

```bash
python simple_demo.py \
  --question "What OS is running? Show uname and the first 3 lines of /etc/os-release."
```

For a larger workflow, `main.py` lets the Agent inspect a Django source tree and
analyze the SWE-bench `django__django-13447` bug. See the bilingual
[example README](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/openai-agents-example)
for its arguments and expected flow.

## Going Further

- **Longer runs:** set both the sandbox lifetime in
  `E2BSandboxClientOptions(timeout=...)` and an appropriate Agent turn limit.
- **Pause and resume:** set `pause_on_exit=True`, retain the session state, and
  call `E2BSandboxClient.resume(...)`. The bundled pause/resume demo performs a
  complete write, pause, resume, read, and cleanup cycle.
- **Code Interpreter:** use the
  [`openai-agents-code-interpreter`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/openai-agents-code-interpreter)
  examples. Generic execution needs envd on `49983`; Jupyter mode additionally
  needs the Code Interpreter service on `49999` in the template image.
- **Network and storage controls:** configure Cube-specific policies through
  [network policy](/guide/network-policy), [security proxy](/guide/security-proxy),
  and [persistent storage](/guide/persistent-storage). Features not represented
  by the E2B compatibility surface can be prepared in the template or managed
  through CubeSandbox's native APIs.

## Caveats

- The bundled scripts set the E2B envd username to `root` and remove the `stdin`
  argument when talking to older envd versions. Copy the compatibility block
  from the runnable example if your deployment requires it.
- `E2B_API_URL` alone does not replace data-plane DNS or sidecar configuration;
  verify both CubeAPI and CubeProxy reachability.
- `E2BSandboxType.CODE_INTERPRETER` requires a purpose-built template. Selecting
  that enum does not install or start Jupyter automatically.
- Treat the sandbox as untrusted execution. Keep LLM credentials in the Agent
  harness unless the task explicitly needs them inside the MicroVM.

## References

- [OpenAI Sandbox Agents documentation](https://developers.openai.com/api/docs/guides/agents/sandboxes)
- [Runnable Shell Agent and SWE-bench examples](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/openai-agents-example)
- [Runnable Code Interpreter examples](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/openai-agents-code-interpreter)
- [Detailed OpenAI Agents SDK × CubeSandbox implementation notes](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/openai-agents-example/openai-agents-sandbox-cube-integration.md)
- [Connecting clients to a CubeSandbox cluster](/guide/multi-node-deploy#connect-clients-to-the-cluster)
