# Claude Code + CubeSandbox Example

[中文文档](README_zh.md)

Run [Claude Code](https://docs.anthropic.com/en/docs/claude-code) — Anthropic's
official CLI coding agent — inside a CubeSandbox MicroVM. The agent edits files,
runs commands, and reaches the Anthropic API entirely within an isolated,
reproducible sandbox.

This example ships:

- A `Dockerfile` that stacks Node.js 22 + the Claude Code CLI on top of the
  CubeSandbox base image (envd already listens on `:49983`).
- `run_claude_agent.py` — a headless one-shot run inside `/workspace`.
- `resume_claude_agent.py` — pause/resume across two turns, proving `/workspace`
  survives the snapshot.
- `env_utils.py`, `.env.example`, `requirements.txt`.

## Directory layout

```
claude-code-sandbox/
├── Dockerfile               # CubeSandbox template image (Node.js + Claude Code CLI)
├── .env.example             # Copy to .env and fill in
├── .gitignore
├── requirements.txt         # Host driver deps (e2b, python-dotenv)
├── env_utils.py             # .env loading, key management, claude command builder
├── _claude_common.py        # Shared sandbox command helpers (run/ensure/id)
├── run_claude_agent.py      # One-shot Claude Code task
├── resume_claude_agent.py   # Pause / resume session persistence
├── README.md                # English docs (this file)
└── README_zh.md             # Chinese docs
```

## Prerequisites

- A running CubeSandbox deployment; CubeAPI reachable at `http://<node>:3000`.
- `cubemastercli` on `$PATH`, connected to the cluster.
- Docker on the build workstation, plus a registry the Cube nodes can pull from.
- An Anthropic API key.
- Python 3.10+ for the host driver scripts.

## 1. Build the template image

```bash
docker build --platform linux/amd64 \
  -t <your-registry>/claude-code-cube:latest \
  examples/claude-code-sandbox
docker push <your-registry>/claude-code-cube:latest
```

The image installs `@anthropic-ai/claude-code`, plus `git`, `python3`,
`ripgrep`, `jq`, and cleans apt/npm caches. The Claude Code version is pinned
via `--build-arg CLAUDE_CODE_VERSION=x.y.z`.

## 2. Register as a Cube template

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/claude-code-cube:latest \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health

cubemastercli tpl watch --job-id <job_id>
```

Note the `template_id` once the job reaches `READY`.

## 3. Configure the host driver

```bash
cd examples/claude-code-sandbox
cp .env.example .env
# fill in E2B_API_URL, CUBE_TEMPLATE_ID, and ANTHROPIC_API_KEY
pip install -r requirements.txt
```

| Variable | Where it flows | Notes |
|---|---|---|
| `E2B_API_URL` | Local process | CubeAPI address (`http://<node>:3000`) |
| `E2B_API_KEY` | Local process | Any non-empty string in local dev |
| `CUBE_TEMPLATE_ID` | `Sandbox.create(template=...)` | From step 2 |
| `ANTHROPIC_API_KEY` | `envs=...` (injected per command) | Provider key |

## 4. One-shot run

```bash
python run_claude_agent.py --prompt "Create hello.py that prints 'Hello from CubeSandbox' and run it."
```

The key is forwarded per-command via `sandbox.commands.run(..., envs=...)`, so
it lives only for the lifetime of that exec call.

## 5. Pause / resume (session persistence)

```bash
python resume_claude_agent.py
```

Turn 1 asks Claude Code to write `/workspace/plan.md`, then `sandbox.pause()`
snapshots the VM. The script reconnects with `Sandbox.connect(sandbox_id)`,
verifies `/workspace/plan.md` survived, then runs turn 2 to continue the work.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `claude: command not found` | Template not rebuilt after CLI change | Rebuild the image, re-register the template |
| Auth error from Anthropic | Key not forwarded | Pass `envs={...}` with `ANTHROPIC_API_KEY` |
| Readiness probe timeout | Image without envd | Ensure `FROM ghcr.io/tencentcloud/cubesandbox-base:...` |
| `pause()`/`connect()` errors | Platform too old for snapshots | Upgrade the CubeSandbox platform |

## References

- Claude Code docs: <https://docs.anthropic.com/en/docs/claude-code>
- CubeSandbox snapshot / clone: [`docs/guide/snapshot-rollback-clone.md`](../../docs/guide/snapshot-rollback-clone.md)
- Pi agent example: [`examples/pi-agent-integration`](../pi-agent-integration)
