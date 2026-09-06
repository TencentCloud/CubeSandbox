# LangGraph + CubeSandbox Integration Examples

[中文文档](README_zh.md)

Run a [LangGraph](https://github.com/langchain-ai/langgraph) agent — an explicit `StateGraph` of
nodes and conditional edges — whose code-execution tool runs inside a
[CubeSandbox](https://github.com/TencentCloud/CubeSandbox) MicroVM via the official
**`cubesandbox` Python SDK**. These are the runnable counterparts of the
[LangGraph integration guide](../../docs/guide/integrations/langgraph.md).

| Script | Purpose |
|---|---|
| `langgraph_agent_demo.py` | A generate → execute → review → retry loop on one MicroVM |
| `langgraph_checkpoint_demo.py` | Resume across `pause()` / `connect()` with a LangGraph checkpointer |

## Prerequisites

- Python 3.10+
- CubeSandbox deployed, with a template image containing pandas / numpy / matplotlib /
  scikit-learn built and registered (see the `Dockerfile`).

## Quick start

```bash
# Build & register the shared template image
docker build --platform linux/amd64 -t <your-registry>/langgraph-cube:latest .
docker push <your-registry>/langgraph-cube:latest
cubemastercli tpl create-from-image \
  --image <your-registry>/langgraph-cube:latest \
  --writable-layer-size 2G --expose-port 49983 --probe 49983 --probe-path /health

# Configure env vars and run
cp .env.example .env        # fill in the values
pip install -r requirements.txt
python langgraph_agent_demo.py
python langgraph_checkpoint_demo.py
```

## Environment variables

| Variable | Required | Notes |
|---|---|---|
| `CUBE_API_URL` | — | CubeAPI base URL (defaults to `http://127.0.0.1:3000`) |
| `CUBE_TEMPLATE_ID` | ✓ | Sandbox template id used by the demo |
| `CUBE_PROXY_NODE_IP` | — | Direct IP to reach CubeProxy (SDK falls back to wildcard-DNS when unset) |
| `CUBE_API_KEY` | — | Only for an auth-enabled CubeAPI backend |
| `OPENAI_API_KEY` | ✓ | LLM API key (or `TOKENHUB_API_KEY`) |
| `OPENAI_BASE_URL` | — | OpenAI-compatible endpoint |
| `CHAT_MODEL` | — | Model name (defaults to `deepseek-v3`) |
