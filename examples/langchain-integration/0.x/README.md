# LangChain 0.x + CubeSandbox Integration Example

[中文文档](README_zh.md)

A [LangChain](https://github.com/langchain-ai/langchain) **0.3.x** agent whose
code-execution tool runs inside a [CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
MicroVM. This variant uses the **legacy `AgentExecutor` / `create_react_agent` API**
and works on **Python 3.9+**.

It uses the official **`cubesandbox` Python SDK** (`from cubesandbox import Sandbox`)
to create a MicroVM and run code — no raw HTTP. The sandbox is torn down automatically
by a `with` context manager.

> Want the modern LangChain 1.x API instead? See [`../1.x`](../1.x).

## What you need

- A CubeSandbox deployment with CubeAPI reachable at `http://<node>:3000`.
- A sandbox template with Python + pandas/numpy/matplotlib/scikit-learn preinstalled
  (build it from the top-level `../Dockerfile`).
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

# 3. Configure the host driver (run from this 0.x/ dir)
cd 0.x
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
directly load and analyze it. The agent uses the `run_python` tool to compute
monthly revenue — all executed inside the MicroVM.

## Expected output

With the default prompt the host prints the agent's **final answer** — the
month → revenue numbers (2780.0 / 3375.5 / 3872.0) — to **stdout**.
Because the 0.x executor sets `verbose=True`, it also prints the
**full reasoning trace** (thought → tool call → observation) to the console
before the answer.

Log / cleanup behavior:

- Final answer → **stdout**; reasoning trace → console (verbose).
- Teardown is best-effort: if `kill()` fails, the SDK's context manager silently swallows
  the error, so no warning is printed to **stderr**.

## Files

| File | Purpose |
|---|---|
| `langchain_agent_demo.py` | The runnable 0.x agent + `run_python` Cube tool |
| `requirements.txt` | Host driver deps (`langchain==0.3.23`, `langchain-openai==0.3.12`, `cubesandbox`) |
| `../Dockerfile` | Shared data-science template image (on `cubesandbox-base`) |
| `../.env.example` | Shared environment variable template |
