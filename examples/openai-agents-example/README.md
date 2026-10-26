# OpenAI Agents SDK + CubeSandbox Example

[中文文档](README_zh.md)

This directory contains two example scripts showing how to connect the OpenAI Agents SDK's `E2BSandboxClient` to [CubeSandbox](https://github.com/TencentCloud/CubeSandbox).

| Script | Scenario | Description |
|--------|----------|-------------|
| [`simple_demo.py`](simple_demo.py) | Quick start | Minimal Shell Agent + Pause/Resume demo |
| [`main.py`](main.py) | SWE-bench debugging | Full streaming output + LLM preflight + end-to-end tracing |

## Prerequisites

- Python 3.10+
- CubeSandbox platform deployed and CubeAPI reachable
- A sandbox template created (see "Create a sandbox template" below)
- An API key for TokenHub or any OpenAI-compatible LLM service

## Quick Start

### 1. Install dependencies

```bash
pip install -r requirements.txt
```

### 2. Configure environment variables

```bash
cp .env.example .env
```

Edit `.env`:

| Variable | Description |
|----------|-------------|
| `TOKENHUB_API_KEY` | TokenHub API key (automatically mapped to `OPENAI_API_KEY`) |
| `OPENAI_BASE_URL` | LLM endpoint (defaults to `https://tokenhub.tencentmaas.com/v1`) |
| `E2B_API_URL` | CubeAPI URL, e.g. `http://<cube-host>:3000` |
| `E2B_API_KEY` | CubeAPI auth key |
| `CUBE_TEMPLATE_ID` | Sandbox template ID |
| `CUBE_SSL_CERT_FILE` | (Optional) path to the CubeSandbox CA bundle |

### 3. Create a sandbox template

**simple_demo.py** works with any Linux template. **main.py** requires a SWE-bench image with the Django source code preinstalled:

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-image.tencentcloudcr.com/demo/django_1776_django-13447:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --cpu 4000 --memory 8192 \
  --probe 49983
```

Copy the template ID from the output into `CUBE_TEMPLATE_ID` in your `.env`.

---

## simple_demo.py — Minimal Example

A minimal Shell Agent that demonstrates the core steps of integrating with CubeSandbox.

### Usage

```bash
# Basic Agent Q&A
python simple_demo.py
python simple_demo.py --question "What Linux distro is this?"

# Pause / Resume demo (write file → pause → resume → verify file)
python simple_demo.py --pause-resume

# SSL debug modes
python simple_demo.py --no-ssl-patch        # Disable all SSL customisations
python simple_demo.py --llm-cube-ssl        # Let LLM use the cube CA as well
```

### Arguments

| Argument | Default | Description |
|----------|---------|-------------|
| `--model` | `openai/glm-5.1` | LLM model name |
| `--question` | Check OS version | Prompt sent to the Agent |
| `--template` | `CUBE_TEMPLATE_ID` | Sandbox template ID |
| `--timeout` | `300` | Sandbox timeout (seconds) |
| `--pause-resume` | — | Switch to Pause/Resume demo mode |
| `--no-ssl-patch` | — | Disable SSL customisations |
| `--llm-cube-ssl` | — | Make the LLM client also use the cube CA |

### Core code

```python
agent = SandboxAgent(
    name="Cube Demo Agent",
    model=make_model("openai/glm-5.1"),
    instructions="You are a helpful assistant running inside a cloud sandbox.",
    default_manifest=Manifest(),
    capabilities=[Shell()],
)

run_config = RunConfig(
    sandbox=SandboxRunConfig(
        client=E2BSandboxClient(),
        options=E2BSandboxClientOptions(
            sandbox_type=E2BSandboxType.E2B,
            template=os.environ["CUBE_TEMPLATE_ID"],
            timeout=300,
        ),
    ),
)

result = await Runner.run(agent, "What OS is running?", run_config=run_config)
```

### Pause/Resume flow

```
[step 1] Create sandbox
[step 2] Write marker file pause-resume-test.txt
[step 3] Pause sandbox (stop + shutdown, pause_on_exit=True)
[step 4] Resume sandbox (client.resume)
[step 5] Read the file and compare contents → PASS / FAIL
[cleanup] Destroy sandbox
```

---

## main.py — SWE-bench Django Debugging

A full SWE-bench Agent that autonomously analyses a Django bug inside the sandbox and proposes a fix. Ships with an LLM preflight, streaming output, and end-to-end tracing.

### Usage

```bash
# Analyse the Django bug (django__django-13447)
python main.py

# Custom question
python main.py --question "What Python version is installed? Show the Django version too."

# Specify model
python main.py --model openai/deepseek-v3.2

# Only test sandbox connectivity (do not call the LLM)
python main.py --sandbox-only
python main.py --sandbox-only --timeout 60

# Limit tool-call turns
python main.py --max-turns 20
```

### Arguments

| Argument | Default | Description |
|----------|---------|-------------|
| `--model` | `openai/glm-5.1` | LLM model name (TokenHub uses the `openai/` prefix) |
| `--question` | Analyse and fix the bug | Prompt sent to the Agent |
| `--template` | `CUBE_TEMPLATE_ID` | SWE-bench Django template ID |
| `--timeout` | `300` | Sandbox timeout (seconds) |
| `--max-turns` | `50` | Maximum tool-call turns |
| `--sandbox-only` | — | Only create/destroy the sandbox to verify connectivity |

### Built-in features

**LLM preflight**: Before the real run, validates LLM connectivity (plain text → tool-calling → streaming, three tests).

**Streaming output**: Live-prints each step of the Agent:

```
[preflight] 1/3 plain ok — glm-5.1 @ https://tokenhub.tencentmaas.com/v1/  856 ms
[preflight] 2/3 tool-call ok — 1204 ms: get_info({"cmd":"uname"})
[preflight] 3/3 streaming ok — 623 ms, 12 chunks
[status] creating sandbox & starting session ...
[agent] SWE-bench Agent running
[step 1] tool_call: exec_command({"cmd": "cat /testbed/django/contrib/admin/..."})
  → output: ...
[step 2] tool_call: exec_command({"cmd": "grep -n 'items_for_result' ..."})
  → output: ...
[answer] The bug is in the `items_for_result` function ...
[done] 8 tool calls, 42350 ms total
```

**End-to-end tracing**: Automatically records the E2B lifecycle (create/start/exec/shutdown) and the LLM calls (HTTP request/response, time to first token).

**Hang-guard timeout**: If no new event is observed for 30 seconds after the LLM answers, the run exits automatically — this prevents the Runner's internal finalisation from hanging forever.

### About the SWE-bench scenario

The default task is `django__django-13447`:

> When `ModelAdmin` sets `list_display_links = None`, the Django admin still renders the first field as a link (it should render as plain text).

The Agent will:
1. Locate the `items_for_result` function in `/testbed/django/contrib/admin/templatetags/admin_list.py`
2. Analyse the root cause
3. Propose a fix

The sandbox template image contains the full Django source code under `/testbed`.

---

## CubeSandbox Adaptations

Both scripts include the following runtime patches:

| Patch | Reason |
|-------|--------|
| `default_username = "root"` + Filesystem method wrappers | CubeSandbox envd only serves the `root` user |
| `Commands.run` strips the `stdin` argument | Compatibility with older envd versions |
| LLM client uses the system CA bundle | Prevents `SSL_CERT_FILE` (cube gRPC) from contaminating TokenHub HTTPS |
| Force `OpenAIChatCompletionsModel` | TokenHub does not support the Responses API |

See the [Integration Guide](openai-agents-sandbox-cube-integration.md) for details.

## Related Documents

- [OpenAI Agents SDK × CubeSandbox Integration Guide](openai-agents-sandbox-cube-integration.md)
- [OpenAI Sandbox Agents Concepts](openai-agents.md)
- [OpenAI Agents SDK on GitHub](https://github.com/openai/openai-agents-python)
- [CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
