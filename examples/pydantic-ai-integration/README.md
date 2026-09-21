# Pydantic AI + CubeSandbox Integration Example

[中文文档](README_zh.md)

A [Pydantic AI](https://ai.pydantic.dev) agent whose `run_python` function tool
executes code inside a [CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
MicroVM. The model decides when to run code; the code runs in an isolated
MicroVM, and its **real** stdout / stderr / exit code flow back to the model.

It uses the official **`cubesandbox` Python SDK** (`from cubesandbox import Sandbox`)
— no raw HTTP. **One MicroVM is created per agent run and reused across every
tool call**; a `with` context manager tears it down when the run finishes.

```text
user prompt
  -> Pydantic AI Agent (agent.run_sync)
  -> model calls the run_python tool
  -> tool writes the snippet + runs it inside the CubeSandbox MicroVM
  -> stdout / stderr / exit code returned to the model
  -> model continues reasoning
  -> final answer
```

## What you need

- A CubeSandbox deployment with CubeAPI reachable (e.g. `http://<node>:3000`).
- A Python-capable sandbox template. The default demo only uses the Python
  standard library, so the official **`sandbox-code`** template is enough — no
  custom image required.
- **Python 3.10+** on the machine running the agent (required by `pydantic-ai`).
- An OpenAI-compatible LLM endpoint and API key.

> The agent host and the CubeSandbox host do not need to be the same machine.
> The agent talks to CubeAPI/CubeProxy over the network, so you can drive a
> remote Linux Cube host from your laptop.

## Setup

```bash
# 1. Register the official code sandbox template (once, on a Cube host).
cubemastercli tpl create-from-image \
  --image cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
# In mainland China, use cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest
# Copy the resulting template_id.

# 2. Configure and install (on the agent host).
cp .env.example .env        # fill CUBE_TEMPLATE_ID + the LLM key/endpoint
pip install -r requirements.txt
```

Key variables in `.env`:

| Variable | Required | Purpose |
|---|---|---|
| `CUBE_TEMPLATE_ID` | yes | Sandbox template ID from step 1 |
| `CUBE_API_URL` | no | CubeAPI URL; defaults to `http://127.0.0.1:3000` |
| `CUBE_API_KEY` | no | Only for an auth-enabled CubeAPI |
| `CUBE_PROXY_NODE_IP` / `CUBE_PROXY_PORT_HTTP` | no | Reach CubeProxy directly when DNS is not set up |
| `OPENAI_API_KEY` | yes | Key for the OpenAI-compatible endpoint |
| `OPENAI_BASE_URL` | no | Endpoint URL; defaults to `https://tokenhub.tencentmaas.com/v1` |
| `MODEL_NAME` | no | Model name; defaults to `deepseek-v3` (`CHAT_MODEL` also accepted) |

## Run

```bash
python pydantic_ai_agent_demo.py
# or pass your own task (still standard-library only):
python pydantic_ai_agent_demo.py "Compute the first 15 prime numbers and their sum."
```

## What to expect

The default task asks the agent to generate and verify a Fibonacci sequence. A
successful run:

1. Prints `Sandbox <id> created. Running agent...`.
2. The model calls `run_python` at least once; the snippet executes **inside the
   MicroVM** (not locally).
3. Prints a `=== Final answer ===` block whose numbers match what the executed
   code printed (e.g. the 20th Fibonacci number is `6765`, and all recurrence
   checks pass).

The exact wording of the final answer varies by model, so verify the behavior,
not a fixed sentence:

- `run_python` was actually called (the sandbox id line appears, then work happens).
- The reported numbers come from real execution — they are internally consistent
  and correct (F(20) = 6765).
- The agent finishes with a final answer instead of looping.

## How the sandbox is used

- **One MicroVM per run, reused across tool calls.** The sandbox is created once
  in `main()` and passed to the agent via typed `deps`; every `run_python` call
  reuses it, so there is no per-call MicroVM boot cost and files persist within a run.
- **Working directory.** The stock `sandbox-code` image ships no `/workspace`, so
  `main()` runs `mkdir -p /workspace` once right after the sandbox boots.
- **Unique script per call.** Each call writes `/workspace/agent_step_<n>.py`
  (the index comes from a host-side counter), so repeated calls never overwrite
  each other.
- **Failures come back as tool output.** A `CubeSandboxError` or transport
  timeout is returned to the model as text so it can retry, instead of aborting
  the run. stderr is delimited and non-zero exit codes are reported.
- **Network isolation by default.** The demo creates the sandbox with
  `allow_internet_access=False` because the task needs no network. Set it to
  `True` in the script if your own task does.

## Files

| File | Purpose |
|---|---|
| `pydantic_ai_agent_demo.py` | The runnable agent + `run_python` Cube tool |
| `requirements.txt` | Host driver deps (`pydantic-ai`, `cubesandbox`, `python-dotenv`) |
| `.env.example` | Environment variable template |
