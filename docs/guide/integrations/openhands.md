---
title: OpenHands Integration Guide
author: Fan-hr
date: 2026-07-28
tags:
  - integration
  - openhands
  - coding-agent
  - agent
lang: en-US
---

# OpenHands Integration Guide

[中文文档](../../zh/guide/integrations/openhands.md)

## Integration Target and Version

[OpenHands](https://www.openhands.dev/) is an autonomous software-development
agent platform. Its [Agent SDK](https://github.com/OpenHands/software-agent-sdk)
executes agent actions (bash, file edits, scripts) through an **agent server**
that normally runs in a local Docker container.

This guide wires the agent server into a Cube Sandbox MicroVM instead: the
server is pre-installed in a Cube template and frozen *live* into the
template's boot snapshot, so every sandbox hot-starts with a running agent
server and full hardware isolation.

Tested versions: `openhands-sdk` / `openhands-tools` /
`openhands-agent-server` **1.38.0**, Cube Sandbox **v0.6.0**.

## Prerequisites

- Cube Sandbox deployment: any working deployment (single-node is fine) with
  `cubemastercli` and the E2B-compatible API reachable.
- SDK or CLI dependencies: Docker (image build), [`uv`](https://docs.astral.sh/uv/)
  or a current pip >= 26 (host scripts — older pips such as Ubuntu 24.04's
  stock 24.0 fail on an upstream `lmnr`/`opentelemetry` conflict).
- Required environment variables: `E2B_API_URL`, `E2B_API_KEY`,
  `CUBE_TEMPLATE_ID`; plus `LLM_MODEL`, `LLM_API_KEY`, optional
  `LLM_BASE_URL` (any OpenAI-compatible endpoint) for the full agent demo.
  To just verify the integration, `smoke_test.py` and `pause_resume.py`
  need **no LLM configuration at all**.

## Integration Steps

1. **Build the template image** —
   [`examples/openhands-integration/Dockerfile`](https://github.com/tencentcloud/CubeSandbox/blob/master/examples/openhands-integration/Dockerfile)
   extends `cubesandbox-base` (envd on `:49983` preserved), installs a
   standalone Python 3.12 via uv, pins `openhands-agent-server`, and starts it
   on `:8000` as the image CMD under the unprivileged `user` account.

2. **Register it as a template**, probing the agent server itself so the boot
   snapshot is taken only after the server is ready:

   ```bash
   cubemastercli tpl create-from-image \
     --image <registry>/openhands-sandbox:latest \
     --writable-layer-size 2G \
     --expose-port 8000 --expose-port 49983 \
     --probe 8000 --probe-path /ready
   ```

3. **Connect the OpenHands SDK** through `CubeSandboxWorkspace`, a
   `RemoteWorkspace` subclass (the same extension point the SDK's own
   `DockerWorkspace` uses) that creates the sandbox via the E2B-compatible
   SDK and points the workspace at the proxied agent-server URL.

4. **Run demos** from `examples/openhands-integration/`: `smoke_test.py`
   (no LLM; hot-start latency, bash/file round-trips), `pause_resume.py`
   (no LLM; whole-VM freeze/thaw under a live server), `main.py` (full agent
   coding task with in-sandbox result verification).

## Key Code Snippets

The minimal refactoring for an existing OpenHands SDK program is replacing
the workspace — everything else is unchanged:

```python
from openhands.sdk import LLM, Conversation
from openhands.tools.preset.default import get_default_agent
from cubesandbox_workspace import CubeSandboxWorkspace  # this example

agent = get_default_agent(
    llm=LLM(model="openai/deepseek-chat", api_key=..., base_url=...),
    cli_mode=True,  # bash + file editor; no browser stack in the template
)

with CubeSandboxWorkspace(template="tpl-...") as workspace:  # the template from step 2
    conversation = Conversation(agent=agent, workspace=workspace)
    conversation.send_message("Create fib.py, run it, fix any errors.")
    conversation.run()

    workspace.pause()   # freeze agent server + shells + in-flight processes
    workspace.resume()  # thaw bit-for-bit, session continues
```

The workspace core (abridged from `cubesandbox_workspace.py`):

```python
class CubeSandboxWorkspace(RemoteWorkspace):
    template: str
    agent_server_port: int = 8000

    def model_post_init(self, context):
        self._sandbox = Sandbox.create(template=self.template, ...)
        host = self._sandbox.get_host(self.agent_server_port)
        object.__setattr__(self, "host", f"http://{host}")
        self._wait_for_ready(timeout=self.health_check_timeout)
        super().model_post_init(context)
```

## Caveats

- **The agent loop runs inside the MicroVM.** LLM calls originate from the
  sandbox, so it needs egress to your LLM endpoint; a network allowlist can
  block everything else ([Network Policy](../network-policy.md)).
- **The LLM key travels into the sandbox.** The conversation payload carries
  `LLM(api_key=...)` to the in-VM server — upstream's agent-server design,
  `DockerWorkspace` included. To keep the raw key out of the VM, give the
  agent a placeholder and let CubeEgress
  [credential injection](../security-proxy.md) attach the real header on
  the wire; the template already trusts the interception CA.
- **Inbound access control.** `private_traffic=True` gates the workspace API
  behind per-sandbox traffic tokens, and `SESSION_API_KEY` adds server-side
  auth. Scopes and limitations are covered in the example README's
  [Security alignment](https://github.com/tencentcloud/CubeSandbox/blob/master/examples/openhands-integration/README.md#security-alignment)
  section.
- **Version pairing.** Keep host `openhands-sdk`/`openhands-tools` and the
  template's `openhands-agent-server` on the same release (tested: 1.38.0);
  `workspace.get_server_info()` reveals mismatches.
- **Host installs.** Use uv or a current pip (>= 26); older pips fail on
  the upstream `lmnr`/`opentelemetry` pins.

## References

- Related docs: [Templates Overview](../templates.md) ·
  [Creating Templates from OCI Images](../tutorials/template-from-image.md) ·
  [Network Policy](../network-policy.md) ·
  [Restrict Public Access](../restrict-public-access.md) ·
  [Security Proxy](../security-proxy.md)
- Sample repository: [`examples/openhands-integration`](https://github.com/tencentcloud/CubeSandbox/tree/master/examples/openhands-integration)
- Upstream project: [OpenHands](https://github.com/All-Hands-AI/OpenHands) ·
  [OpenHands Agent SDK](https://github.com/OpenHands/software-agent-sdk)
