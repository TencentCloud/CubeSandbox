# LangChain + CubeSandbox Integration Examples

[中文文档](README_zh.md)

Run a LangChain agent whose code-execution tool runs inside a
[CubeSandbox](https://github.com/TencentCloud/CubeSandbox) MicroVM. These examples use
the official **`cubesandbox` Python SDK** (`from cubesandbox import Sandbox`) to create a
MicroVM and run code — no raw HTTP plumbing, and the sandbox is torn down automatically
by a `with` context manager.

This directory ships **two major-version variants** of the same agent. Pick the one
that matches your LangChain / Python version:

| Variant | LangChain | Python | Agent API | Path |
|---|---|---|---|---|
| **0.x** | `0.3.x` (legacy) | 3.9+ | `AgentExecutor` + `create_react_agent` | [`0.x/`](0.x) |
| **1.x** | `1.x` (modern) | 3.10+ | `langchain.agents.create_agent` + `@tool` | [`1.x/`](1.x) |

Both variants share the same code-execution tool (`run_python`, built on the `cubesandbox`
SDK) and the same sandbox template. The only differences are the LangChain
agent-construction API and the Python version requirement.

## Which one should I use?

- On **Python < 3.10** (>= 3.9), or an existing LangChain 0.3.x codebase → use **`0.x/`**.
- On **Python 3.10+** and starting fresh, or already on LangChain 1.x → use **`1.x/`**.

## Shared resources

These live at the top level and are used by both variants:

| File | Purpose |
|---|---|
| `Dockerfile` | Data-science template image (on `cubesandbox-base`) |
| `.env.example` | Environment variable template (copy to `.env` inside `0.x/` or `1.x/`) |
| `sales.csv` | Seed dataset preloaded into the template image |

## Quick start (either variant)

```bash
# Build & register the shared template image (top level)
docker build --platform linux/amd64 -t <your-registry>/langchain-cube:latest .
docker push <your-registry>/langchain-cube:latest
cubemastercli tpl create-from-image \
  --image <your-registry>/langchain-cube:latest \
  --writable-layer-size 2G --expose-port 49983 --probe 49983 --probe-path /health

# Then pick a variant, e.g. 1.x:
cd 1.x
cp ../.env.example .env        # fill in the values
pip install -r requirements.txt
python langchain_agent_demo.py
```

See each variant's `README.md` for full details.
