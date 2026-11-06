---
title: browser-use Integration Guide
author: tunsuy
date: 2026-10-19
tags:
  - integration
  - browser-use
  - agent
  - browser-automation
lang: en-US
---

# browser-use Integration Guide

[中文](../../zh/guide/integrations/browser-use.md)

Run a [browser-use](https://github.com/browser-use/browser-use) agent on your host while the
browser it drives — a real Chromium — lives inside a
[CubeSandbox](https://github.com/TencentCloud/CubeSandbox) MicroVM. The agent loop and your LLM
credentials stay local; every web page, script, ad, and download the agent touches is confined to a
disposable KVM virtual machine. The integration builds on the CDP-over-CubeProxy pattern already
shipped in [`examples/browser-sandbox`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/browser-sandbox),
so no custom browser-use fork or provider is needed.

```
Host                                     Cube Sandbox (MicroVM)
┌──────────────────────────┐             ┌────────────────────────────┐
│ browser-use Agent        │   CDP over  │ Chromium                   │
│  - LLM calls (your keys) │  WebSocket  │  --remote-debugging-port   │
│  - DOM/vision processing │────────────►│  :9000                     │
└──────────────────────────┘  CubeProxy  └────────────────────────────┘
```

## Integration Target and Version

| Component | Version referenced by this guide |
| --- | --- |
| browser-use | `>= 0.3` (`Agent` + `BrowserSession` + `browser_use.ChatOpenAI`); a legacy `0.1.x` (`Browser` + `BrowserConfig`) variant is also shown |
| Python | 3.11+ (required by browser-use) |
| cubesandbox SDK | `>= 0.6.0` |
| Browser template image | `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest` (Chromium serving CDP on port `9000`) |
| CubeSandbox platform | `>= 0.6.0` (template aliases and `sandbox.pause()` used by this guide) |

browser-use evolves quickly and its connection API differs across major releases; pin the version
you validate and check the [Caveats](#caveats) section when upgrading.

## Prerequisites

- A running [CubeSandbox deployment](/guide/quickstart) with CubeAPI reachable, normally at
  `http://<cube-host>:3000`.
- `cubemastercli` on your `$PATH` and connected to the cluster.
- Python 3.11+ on the machine running the browser-use agent. **No local Chromium is required** —
  the browser runs inside the sandbox.
- The sandbox domain (`*.cube.app` by default) must resolve to CubeProxy **from the machine running
  browser-use**. The CDP connection is opened by browser-use itself, not by the `cubesandbox` SDK,
  so `CUBE_PROXY_NODE_IP` does not cover it — configure wildcard DNS (or dnsmasq) as described in
  [HTTPS and domain configuration](/guide/https-and-domain).
- An OpenAI-compatible LLM endpoint and API key. browser-use sends page screenshots to the model,
  so pick a strong **vision-capable** model.

## Why run the browser in a sandbox

Web agents consume untrusted content by design: arbitrary pages, arbitrary JavaScript, arbitrary
files. Running that browser on your host mixes the agent's blast radius with your machine. Inside
CubeSandbox you get:

| Concern | What CubeSandbox provides |
| --- | --- |
| **Untrusted web content** | Pages, scripts, and downloads execute inside a disposable KVM MicroVM with its own guest kernel — never on the host |
| **Prompt injection** | Even if a malicious page steers the agent into dangerous clicks or downloads, the damage is confined to the VM and dies with it |
| **Clean state per task** | One fresh MicroVM per agent run: no cookie, cache, or session leakage between tasks; cold start is under 60 ms |
| **Secret hygiene** | The LLM key stays in the host agent process; the browsing VM never sees it |
| **Long tasks** | `sandbox.pause()` snapshots the VM (browser state included) for later resume |
| **Egress control** | CubeEgress can allowlist exactly which hosts the browser may reach, with an audit log |

## Integration steps

### 1. Create the browser template

The official browser image ships Chromium started in remote-debugging mode on port `9000`, with the
CDP endpoint served under the `/cdp` path prefix (the readiness probe hits `/cdp/json/version`):

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest \
  --alias browser-use \
  --writable-layer-size 1G \
  --expose-port 9000 \
  --probe 9000 \
  --probe-path /cdp/json/version \
  --allow-internet-access
```

Notes:

- From mainland China, use
  `cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest` instead.
- `--allow-internet-access` lets the in-VM browser reach public sites. On a hardened cluster, drop
  it and define allow rules instead (see [network policy](/guide/network-policy) and
  [security proxy](/guide/security-proxy)). If the node needs explicit resolvers, add
  `--dns <dns1> --dns <dns2>`.
- `create-from-image` blocks and watches the build job by default; pass `--detach` and poll later
  with `cubemastercli tpl watch --job-id <job_id>`. Once the job reaches `READY`, note the
  `template_id` (with `--alias browser-use` you can also pass the alias to `Sandbox.create`).
- Heavy pages render better with more resources — raise `--cpu` / `--memory` if the agent works on
  media-rich sites. `--cpu` is in **millicores** (`--cpu 4000` requests 4 cores; the default is
  `2000`, i.e. 2 cores) and `--memory` is in MB (default `2000`).

Any custom image that runs Chromium with `--remote-debugging-port` works too; see
[bring your own image](../tutorials/bring-your-own-image.md) and
[template from image](../tutorials/template-from-image.md).

### 2. Install host dependencies

```bash
python3 -m venv .venv
source .venv/bin/activate
pip install "browser-use>=0.3" "cubesandbox>=0.6.0" python-dotenv
```

::: warning Legacy browser-use 0.1.x needs a separate venv
The legacy 0.1.x snippet under [Key code snippets](#key-code-snippets) cannot coexist with
`browser-use>=0.3` in the same virtualenv (the `Browser`/`BrowserConfig` API was removed in 0.3),
and it additionally imports `langchain_openai`, which the install above does not provide. To try
that path, create a second virtualenv with a downgraded install:

```bash
python3 -m venv .venv-legacy
source .venv-legacy/bin/activate
pip install "browser-use>=0.1,<0.2" langchain-openai "cubesandbox>=0.6.0" python-dotenv
```
:::

### 3. Configure environment variables

```bash
cat > .env <<'EOF'
CUBE_API_URL=http://<cube-host>:3000
CUBE_TEMPLATE_ID=browser-use
OPENAI_API_KEY=<your-llm-key>
OPENAI_BASE_URL=https://tokenhub.tencentmaas.com/v1
CHAT_MODEL=hunyuan-vision
EOF
```

| Variable | Flows to | Notes |
| --- | --- | --- |
| `CUBE_API_URL` | `cubesandbox` SDK | CubeAPI address (`http://<node>:3000`) |
| `CUBE_API_KEY` | `cubesandbox` SDK | (optional) only for an auth-enabled CubeAPI backend |
| `CUBE_TEMPLATE_ID` | `Sandbox.create(template=...)` | Template ID or alias from step 1 |
| `OPENAI_API_KEY` / `TOKENHUB_API_KEY` | LLM client | OpenAI-compatible key; stays on the host |
| `OPENAI_BASE_URL` | LLM client | e.g. `https://tokenhub.tencentmaas.com/v1` |
| `CHAT_MODEL` | LLM client | Must be a **vision-capable** model, e.g. `hunyuan-vision` (or any other multimodal model your endpoint offers); text-only models like `deepseek-v3` will fail |
| `SSL_CERT_FILE` | Python TLS | CA bundle for a self-signed/mkcert CubeProxy deployment |
| `NODE_EXTRA_CA_CERTS` | Playwright driver | Only for the legacy browser-use `0.1.x` path |

### 4. Attach browser-use to the sandboxed browser

Create one MicroVM per agent run and hand its CDP endpoint to `BrowserSession`:

```python
# browser_use_cube_demo.py
import asyncio
import os

from browser_use import Agent, BrowserSession, ChatOpenAI
from dotenv import load_dotenv

from cubesandbox import Sandbox

load_dotenv()


async def main() -> None:
    # The context manager calls sandbox.kill() on exit, so no VM leaks.
    with Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"], timeout=900) as sandbox:
        # Chromium inside the VM serves CDP on port 9000; CubeProxy publishes
        # it as https://9000-<sandbox-id>.cube.app. The trailing "?" matches
        # the URL shape verified by examples/browser-sandbox.
        cdp_url = f"https://{sandbox.get_host(9000)}/cdp?"

        agent = Agent(
            task=(
                "Go to https://news.ycombinator.com and return the title of "
                "the current top post."
            ),
            llm=ChatOpenAI(
                model=os.getenv("CHAT_MODEL", "hunyuan-vision"),
                base_url=os.getenv("OPENAI_BASE_URL",
                                   "https://tokenhub.tencentmaas.com/v1"),
                api_key=os.getenv("OPENAI_API_KEY") or os.getenv("TOKENHUB_API_KEY"),
            ),
            browser_session=BrowserSession(cdp_url=cdp_url),
        )
        history = await agent.run()
        print("Final result:", history.final_result())


asyncio.run(main())
```

### 5. Run it

```bash
python browser_use_cube_demo.py
```

browser-use prints a per-step log (`📍 Step N: ...` with the action it takes) while it drives the
in-VM Chromium, then the script prints the agent's final answer — with the default task, the title
of the current Hacker News top post (the exact value obviously depends on the live site). When the
`with` block exits, the sandbox is killed and the browser state is discarded.

## Key code snippets

### Legacy browser-use 0.1.x (Playwright-based)

This path requires the downgraded install in a separate virtualenv described in step 2
(`browser-use>=0.1,<0.2` plus `langchain-openai`); it does not run against a `browser-use>=0.3`
install. The connection itself is identical to the bundled
[`examples/browser-sandbox/browser.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/browser-sandbox/browser.py)
— `BrowserConfig(cdp_url=...)` is passed straight to Playwright's `connect_over_cdp`, including the
trailing `?` on the URL:

```python
from browser_use import Agent, Browser, BrowserConfig
from langchain_openai import ChatOpenAI

browser = Browser(config=BrowserConfig(
    cdp_url=f"https://{sandbox.get_host(9000)}/cdp?",
))
agent = Agent(
    task="...",
    llm=ChatOpenAI(model=os.environ.get("CHAT_MODEL", "hunyuan-vision"),
                   base_url="https://tokenhub.tencentmaas.com/v1",
                   api_key=os.environ["OPENAI_API_KEY"]),
    browser=browser,
)
history = await agent.run()
```

### One sandbox per task (parallel agents)

Because a cold start is under 60 ms, the natural pattern is a fresh MicroVM per task — run several
agents in parallel without any shared browser state:

```python
# One shared vision-capable LLM client, reused by all parallel agents.
llm = ChatOpenAI(
    model=os.getenv("CHAT_MODEL", "hunyuan-vision"),
    base_url=os.getenv("OPENAI_BASE_URL", "https://tokenhub.tencentmaas.com/v1"),
    api_key=os.getenv("OPENAI_API_KEY") or os.getenv("TOKENHUB_API_KEY"),
)

async def run_task(task: str) -> str:
    with Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"], timeout=900) as sandbox:
        agent = Agent(
            task=task,
            llm=llm,
            browser_session=BrowserSession(
                cdp_url=f"https://{sandbox.get_host(9000)}/cdp?"),
        )
        history = await agent.run()
        return history.final_result() or ""

results = await asyncio.gather(*(run_task(t) for t in tasks))
```

### Getting files out of the VM

Anything the browser downloads stays inside the sandbox. Pull artifacts back before teardown (or
let them die with the VM — often the point). `files.read` is text-only, so fetch binary artifacts
through `base64` in a command:

```python
import base64

result = sandbox.commands.run("base64 -w0 /tmp/screenshot.png")
data = base64.b64decode(result.stdout)          # raw PNG bytes
```

## Caveats

- **CDP endpoint discovery varies by browser-use version.** `BrowserSession(cdp_url=...)` expects
  the browser's CDP endpoint; newer releases connect with a pure-Python CDP client instead of
  Playwright. The snippets above use the trailing-`?` form (`https://<host>/cdp?`) — the exact URL
  shape verified against this image by
  [`examples/browser-sandbox`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/browser-sandbox).
  If your browser-use release cannot resolve the WebSocket endpoint from that URL, query
  `https://<host>/cdp/json/version` (the same URL the readiness probe uses) and pass a WebSocket
  URL built from its `webSocketDebuggerUrl` field — replacing the loopback host with the proxy
  host — as `cdp_url`.
- **DNS is on you for the CDP leg.** `CUBE_PROXY_NODE_IP` only rewrites hostnames inside
  `cubesandbox` SDK calls. browser-use opens the CDP WebSocket itself, so the host machine must
  resolve `*.cube.app` (wildcard DNS or dnsmasq entry pointing at CubeProxy).
- **Self-signed proxy certs.** Export your CA bundle as `SSL_CERT_FILE` (modern, pure-Python CDP
  client) or `NODE_EXTRA_CA_CERTS` (0.1.x Playwright path) so the WebSocket handshake passes TLS
  verification.
- **Vision model required.** browser-use grounds each step in a screenshot; text-only models will
  fail or perform poorly regardless of the sandbox.
- **Egress.** Without `--allow-internet-access` (or explicit CubeEgress allow rules) the in-VM
  browser cannot load public sites; with a default-deny policy every target domain must be
  allowlisted.
- **Timeouts, twice.** The sandbox `timeout` reclaims the whole VM; browser-use's
  `agent.run(max_steps=...)` bounds the agent loop. A long browsing task can exhaust either.
- **Sizing.** Chromium is memory-hungry: raise the template's `--memory` (and `--cpu`) for
  media-rich pages, or tabs will be killed by the guest OOM killer mid-run.

## Troubleshooting

| Symptom | Possible cause | Fix |
| --- | --- | --- |
| `Name or service not known` on `9000-<id>.cube.app` | Sandbox domain not resolvable from the host | Point wildcard DNS for `*.cube.app` at CubeProxy; `CUBE_PROXY_NODE_IP` does not help this connection |
| `SSL: CERTIFICATE_VERIFY_FAILED` | Self-signed/mkcert proxy certificate | Export the CA via `SSL_CERT_FILE` (or `NODE_EXTRA_CA_CERTS` on 0.1.x) |
| WebSocket handshake fails / 404 | `cdp_url` shape mismatch across browser-use versions | `curl -k https://<host>/cdp/json/version` and compare with the URL you pass; see Caveats |
| CDP connect times out right after create | Chromium not up yet | The readiness probe gates `READY`; retry the connect once before debugging further |
| Pages never load, agent stalls on navigation | VM has no egress | Recreate the template with `--allow-internet-access` (plus `--dns` if needed), or add CubeEgress allow rules |
| `403 Forbidden - CubeEgress` | Default-deny egress without a rule for the target site | Allowlist the target hosts in the network policy |
| Agent clicks blindly / misses elements | Non-vision or weak model | Switch `CHAT_MODEL` to a strong vision-capable model |
| Sandbox killed mid-run | Sandbox `timeout` too short | Raise `Sandbox.create(timeout=...)` |

## References

- Runnable CDP baseline: [`examples/browser-sandbox`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/browser-sandbox)
- browser-use upstream: <https://github.com/browser-use/browser-use>
- browser-use docs: <https://docs.browser-use.com>
- GUI-desktop alternative (full Ubuntu desktop in a sandbox): [Ubuntu Desktop Sandbox Guide](./ubuntu-desktop.md)
- Custom template images: [`docs/guide/tutorials/bring-your-own-image.md`](../tutorials/bring-your-own-image.md)
- Create a template from an image: [`docs/guide/tutorials/template-from-image.md`](../tutorials/template-from-image.md)
- Egress allowlisting and audit: [`docs/guide/network-policy.md`](../network-policy.md)
- Credential safekeeping + egress control: [`docs/guide/security-proxy.md`](../security-proxy.md)
