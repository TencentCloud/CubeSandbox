# Agno + CubeSandbox integration example

[中文文档](README_zh.md)

This example keeps the Agno model harness on the host and exposes one custom
Python tool to it. The tool creates no host subprocess: it writes code to a
CubeSandbox MicroVM and invokes `python3` through the official `cubesandbox`
SDK. The context manager cleans the sandbox up after the Agent run.

## Prerequisites

- A deployed CubeSandbox cluster and a template with `python3` plus envd on
  port `49983`. Build the included `Dockerfile` if you do not already have one.
- Python 3.10+ on the machine running the Agent harness.
- An OpenAI-compatible LLM key for the full Agent run. This key stays on the
  host; the example never passes it to the MicroVM.

## Run

```bash
docker build --platform linux/amd64 -t <your-registry>/agno-cube:latest .
docker push <your-registry>/agno-cube:latest
cubemastercli tpl create-from-image \
  --image <your-registry>/agno-cube:latest \
  --writable-layer-size 1G --expose-port 49983 --probe 49983 --probe-path /health

python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
cp .env.example .env
# Fill CUBE_TEMPLATE_ID, CUBE_API_URL, CUBE_PROXY_NODE_IP, and LLM settings.
python agno_agent_demo.py --sandbox-only
python agno_agent_demo.py
```

`--sandbox-only` runs a deterministic Python calculation in the MicroVM without
calling an LLM. The default run asks the Agno Agent to invoke the same tool.

## Safety notes

- Treat all generated code as untrusted. This example bounds each tool input to
  16 KiB and each command to 120 seconds, but those are not a replacement for
  cluster policy.
- Keep `allow_internet_access=False` for code that does not need egress. Add a
  narrow CubeSandbox network policy only for required destinations.
- Use a persistent Cube volume only when an Agent needs state across runs;
  otherwise the context-manager cleanup gives each Agent run a fresh workspace.
