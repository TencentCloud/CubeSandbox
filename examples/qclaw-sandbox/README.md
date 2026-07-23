# OpenClaw (QClaw) + CubeSandbox Example

[中文文档](README_zh.md)

Run [OpenClaw](https://github.com/TencentCloud/CubeSandbox) — Tencent's AI
agent gateway — inside a CubeSandbox MicroVM. The agent gateway runs as a
persistent daemon inside the sandbox, providing an isolated, reproducible
runtime for AI agent workloads.

OpenClaw is the core agent runtime behind QClaw (Tencent's AI desktop app)
and the CubeOps AgentHub. It supports multiple LLM providers (DeepSeek by
default, Anthropic, OpenAI) and manages agent sessions, tool execution, and
workspace state within the sandbox.

This example ships:

- A `Dockerfile` that stacks Node.js 22 + the OpenClaw gateway on top of the
  CubeSandbox base image (envd already listens on `:49983`).
- `run_qclaw_agent.py` — a one-shot run: start gateway, send prompt, collect
  result.
- `resume_qclaw_agent.py` — pause/resume across two turns, proving `/workspace`
  and `/root/.openclaw/` survive the snapshot.
- `env_utils.py`, `.env.example`, `requirements.txt`.

## Directory layout

```
qclaw-sandbox/
├── Dockerfile               # CubeSandbox template image (Node.js + OpenClaw gateway)
├── .env.example             # Copy to .env and fill in
├── .gitignore
├── requirements.txt         # Host driver deps (e2b, python-dotenv)
├── env_utils.py             # .env loading, provider keys, env builder
├── _qclaw_common.py         # Shared helpers (gateway lifecycle, HTTP interaction)
├── run_qclaw_agent.py       # One-shot OpenClaw task
├── resume_qclaw_agent.py    # Pause / resume session persistence
├── README.md                # English docs (this file)
└── README_zh.md             # Chinese docs
```

## Prerequisites

- A running CubeSandbox deployment; CubeAPI reachable at `http://<node>:3000`.
- `cubemastercli` on `$PATH`, connected to the cluster.
- Docker on the build workstation, plus a registry the Cube nodes can pull from.
- An LLM provider API key (DeepSeek by default; Anthropic and OpenAI also supported).
- Python 3.10+ for the host driver scripts.

## 1. Build the template image

```bash
docker build --platform linux/amd64 \
  -t <your-registry>/qclaw-cube:latest \
  examples/qclaw-sandbox
docker push <your-registry>/qclaw-cube:latest
```

The image installs `openclaw`, plus `git`, `python3`, `ripgrep`, `jq`,
`supervisor`, and cleans apt/npm caches. The OpenClaw version is pinned
via `--build-arg QCLAW_VERSION=x.y.z`.

If your registry requires an internal npm source, pass `--build-arg NPM_REGISTRY=<url>`.

## 2. Register as a Cube template

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/qclaw-cube:latest \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health

cubemastercli tpl watch --job-id <job_id>
```

Note the `template_id` once the job reaches `READY`.

## 3. Configure the host driver

```bash
cd examples/qclaw-sandbox
cp .env.example .env
# fill in E2B_API_URL, CUBE_TEMPLATE_ID, and your provider key
pip install -r requirements.txt
```

| Variable | Where it flows | Notes |
|---|---|---|
| `E2B_API_URL` | Local process | CubeAPI address (`http://<node>:3000`) |
| `E2B_API_KEY` | Local process | Any non-empty string in local dev |
| `CUBE_TEMPLATE_ID` | `Sandbox.create(template=...)` | From step 2 |
| `QCLAW_PROVIDER` | Host script | `anthropic`, `deepseek` (default) |
| `QCLAW_MODEL` | OpenClaw config | Model id for the provider |
| `DEEPSEEK_API_KEY` | `envs=...` (injected per command) | Provider key (DeepSeek) |
| `ANTHROPIC_API_KEY` | `envs=...` (injected per command) | Provider key (Anthropic) |

## 4. One-shot run

```bash
python run_qclaw_agent.py --prompt "Create hello.py that prints 'Hello from CubeSandbox' and run it."
```

The driver script:
1. Creates a sandbox from the template
2. Starts the OpenClaw gateway via supervisor
3. Waits for gateway readiness (port 18789 + auth token)
4. Sends the prompt via the gateway REST API
5. Collects and displays the response
6. Destroys the sandbox

## 5. Pause / resume (session persistence)

```bash
python resume_qclaw_agent.py
```

Turn 1 asks OpenClaw to write `/workspace/plan.md`, then `sandbox.pause()`
snapshots the VM. The script reconnects, verifies both `/workspace/plan.md`
and `/root/.openclaw/` survived, restarts the gateway, and runs turn 2.

## Architecture notes

- **Gateway-daemon pattern**: Unlike one-shot CLI agents, OpenClaw runs as a
  persistent process managed by `supervisor`. The gateway handles agent
  sessions, tool execution, and LLM communication.
- **State directory**: `/root/.openclaw/` holds config, agent state, sessions,
  and workspace. This directory must survive pause/resume for session
  persistence.
- **Multi-provider**: Supports DeepSeek (default), Anthropic, and OpenAI via
  the `QCLAW_PROVIDER` env var.
- **AgentHub integration**: This template is a companion to the CubeOps
  AgentHub, which manages OpenClaw instances at scale with host-side state
  directories, egress credential injection, and snapshot/restore.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `openclaw: command not found` | Template not rebuilt after CLI change | Rebuild the image, re-register the template |
| Gateway not ready after 30s | Supervisor config missing or port conflict | Check `supervisorctl status openclaw` or `/var/log/openclaw.log` |
| Auth error from provider | Key not forwarded or wrong provider | Check `QCLAW_PROVIDER` and the matching `*_API_KEY` env var |
| `403 Forbidden` from gateway | Token mismatch between start and read | Verify `/root/.openclaw/openclaw.json` has a valid token |
| Readiness probe timeout | Image without envd | Ensure `FROM ghcr.io/tencentcloud/cubesandbox-base:...` |

## References

- CubeOps AgentHub OpenClaw service: `CubeOps/internal/service/openclaw.go`
- CubeSandbox snapshot / clone: [`docs/guide/snapshot-rollback-clone.md`](../../docs/guide/snapshot-rollback-clone.md)
- Pi agent example: [`examples/pi-agent-integration`](../pi-agent-integration)
