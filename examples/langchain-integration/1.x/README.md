# LangChain 1.x + CubeSandbox Integration Example

[中文文档](README_zh.md)

A [LangChain](https://github.com/langchain-ai/langchain) **1.x** agent whose
code-execution tool runs inside a [CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
MicroVM. This variant uses the **modern `langchain.agents.create_agent` API**
with the `@tool` decorator and requires **Python 3.10+**.

It uses the official **`cubesandbox` Python SDK** (`from cubesandbox import Sandbox`)
to create a MicroVM and run code — no raw HTTP, no LangChain 0.x legacy
`AgentExecutor` / `PromptTemplate` boilerplate. The sandbox is torn down automatically
by a `with` context manager.

> Still on LangChain 0.3.x / Python 3.9? See [`../0.x`](../0.x).

## What you need

- A CubeSandbox deployment with CubeAPI reachable at `http://<node>:3000`.
- A sandbox template with Python + pandas/numpy/matplotlib/scikit-learn preinstalled
  (build it from the top-level `../Dockerfile`).
- **Python 3.10+** (required by `langchain-openai` 1.x).
- An OpenAI-compatible LLM endpoint (TokenHub in this example).

## Setup

```bash
# 1. Build & push the data-science template image (from the parent dir)
cd ..
docker build --platform linux/amd64 \
  -t <your-registry>/langchain-cube:latest .
docker push <your-registry>/langchain-cube:latest

# 2. Register it as a Cube template
cubemastercli tpl create-from-image \
  --image <your-registry>/langchain-cube:latest \
  --writable-layer-size 2G \
  --expose-port 49983 --probe 49983 --probe-path /health
# copy the resulting template_id

# 3. Configure the host driver (run from this 1.x/ dir)
cd 1.x
cp ../.env.example .env        # fill CUBE_API_URL, CUBE_API_KEY, CUBE_TEMPLATE_ID, CUBE_PROXY_NODE_IP, LLM key
pip install -r requirements.txt
```

## Run

```bash
python langchain_agent_demo.py
# custom task:
python langchain_agent_demo.py "Train a linear model on the dataset and report RMSE."
```

> This example uses the official `cubesandbox` SDK to execute code inside the MicroVM.
> The proxy is configured via `CUBE_PROXY_NODE_IP` (and `CUBE_PROXY_PORT_HTTP`) in `.env`.

The template image pre-seeds `sales.csv` under `/workspace`, so the agent can
directly load and analyze it. The agent uses the `cubesandbox`-based `run_python` tool to compute
monthly revenue — all executed inside the MicroVM.

## Expected output

With the default prompt the host prints the agent's **final answer** — the
month → revenue numbers (2780.0 / 3375.5 / 3872.0). (1.x uses `create_agent`,
which is **not** verbose, so no per-step reasoning trace is shown.)

Log / cleanup behavior:

- Only the agent's final answer is printed to **stdout**.
- Teardown is best-effort: if `kill()` fails, the SDK's context manager silently swallows
  the error, so no warning is printed to **stderr**.

## Files

| File | Purpose |
|---|---|
| `langchain_agent_demo.py` | The runnable 1.x agent + `run_python` Cube tool |
| `requirements.txt` | Host driver deps (`langchain>=1.3.14,<2.0`, `langchain-openai>=1.0,<2.0`, `cubesandbox`) |
| `../Dockerfile` | Shared data-science template image (on `cubesandbox-base`) |
| `../.env.example` | Shared environment variable template |
