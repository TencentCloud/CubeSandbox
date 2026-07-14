# Claude Code + CubeSandbox Example

[中文文档](README_zh.md)

Run [Claude Code](https://docs.anthropic.com/en/docs/claude-code)
— Anthropic's official CLI coding agent — inside a CubeSandbox MicroVM. The
agent edits files, runs commands, and reaches the Anthropic LLM API entirely
within an isolated, reproducible sandbox.

This example ships:

- A `Dockerfile` that stacks Node.js 24 + the Claude Code CLI on top of the
  CubeSandbox base image (envd already listens on `:49983`).
- `run_claude_code.py` — a headless one-shot run inside `/workspace`.
- `resume_claude_code.py` — pause/resume across two turns, proving `/workspace`
  survived the snapshot.
- `network_policy.py` — a default-deny egress policy where CubeEgress injects
  the API key on the wire, so the key never enters the VM.
- `env_utils.py`, `.env.example`, `requirements.txt`.

## Directory layout

```
claude-code-integration/
├── Dockerfile            # CubeSandbox template image (Node.js + Claude Code CLI)
├── .env.example          # Copy to .env and fill in
├── .gitignore
├── requirements.txt      # Host driver deps (e2b, cubesandbox, python-dotenv)
├── env_utils.py          # .env loading, Anthropic key, Claude Code command builder
├── _common.py            # Shared sandbox command helpers (run/ensure/id)
├── run_claude_code.py    # One-shot Claude Code task
├── resume_claude_code.py # Pause / resume session persistence
├── network_policy.py     # Default-deny egress + on-the-wire key injection
├── README.md             # English docs (this file)
└── README_zh.md          # Chinese docs
```

## Prerequisites

- A running CubeSandbox deployment; CubeAPI reachable at `https://<node>:3000`.
  Use plain HTTP only for isolated local development on loopback.
- `cubemastercli` on `$PATH`, connected to the cluster.
- Docker on the build workstation, plus a registry the Cube nodes can pull from.
- An Anthropic API key (or an Anthropic-compatible endpoint via `ANTHROPIC_BASE_URL`).
- Python 3.10+ for the host driver scripts.

## 1. Build the template image

```bash
docker build --platform linux/amd64 \
  -t <your-registry>/claude-code-cube:latest \
  examples/claude-code-integration
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
cd examples/claude-code-integration
cp .env.example .env
# fill in E2B_API_URL, CUBE_TEMPLATE_ID, and your Anthropic key
pip install -r requirements.txt
```

| Variable | Where it flows | Notes |
|---|---|---|
| `E2B_API_URL` | Local process | TLS-protected CubeAPI address (`https://<node>:3000`) |
| `E2B_API_KEY` | Local process | Any non-empty string in local dev |
| `CUBE_TEMPLATE_ID` | `Sandbox.create(template=...)` | From step 2 |
| `ANTHROPIC_API_KEY` | `envs=...` (direct) or CubeEgress inject (vault) | Anthropic key |
| `ANTHROPIC_BASE_URL` | Passed into the exec env | Anthropic-compatible gateways (e.g. DeepSeek) |
| `CLAUDE_CODE_MODEL` | Claude CLI `--model` flag | Defaults to `claude-sonnet-4-6` |
| `CLAUDE_CODE_LLM_HOST` | `network_policy.py` | LLM host allowed under default-deny egress |

## 4. One-shot run (direct key flavor)

```bash
python run_claude_code.py --prompt "Create hello.py that prints 'Hello from CubeSandbox' and run it."
```

The key is forwarded per-command via `sandbox.commands.run(..., envs=...)`, so
it lives only for the lifetime of that exec call — never written to a
persistent file inside the VM.

> **Security:** this direct flavor leaves egress open, so a compromised agent
> could exfiltrate the injected key. For shared clusters use the vault flavor
> (step 6): default-deny egress + on-the-wire key injection.

All three scripts run Claude Code with `--output-format stream-json` and render
a concise transcript by default (assistant text, tool calls, and any failures).
Pass `--raw` (or set `CLAUDE_CODE_STREAM_RAW=1`) to stream Claude Code's raw
JSONL events instead.

## 5. Pause / resume (session persistence)

```bash
python resume_claude_code.py
```

Turn 1 asks Claude Code to write `/workspace/plan.md`, then
`sandbox.pause()` snapshots the VM. The script reconnects with
`Sandbox.connect(sandbox_id)`, verifies `/workspace/plan.md` survived, then
runs turn 2 to continue the work. The sandbox lifecycle is managed manually
with `try/finally` (not a context manager), so the pause is not undone by an
early `kill`.

## 6. Restricted egress + key vault (recommended for shared clusters)

```bash
python network_policy.py
```

- Egress is default-deny — only the Anthropic LLM host is reachable.
- CubeEgress attaches the Anthropic key as HTTP headers on the wire (`x-api-key`
  and `anthropic-version`), so `printenv` inside the sandbox never shows the
  real key — it only sees a placeholder.
- Because Claude Code runs on Node.js (which ignores the system CA store), the
  script sets `NODE_EXTRA_CA_CERTS` so Claude Code trusts the CubeEgress
  interception CA; without it the vault path fails with `Connection error`.
  Override the bundle path via `CLAUDE_CODE_NODE_EXTRA_CA_CERTS` if your image
  keeps the CA elsewhere.
- Any other destination returns `403 Forbidden - CubeEgress`.

If the agent needs extra hosts (package registries, MCP servers), add matching
allow rules or preinstall those dependencies into the template.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `claude: command not found` in preflight | Template not rebuilt after CLI change | Rebuild the image, re-register the template |
| Auth error from Anthropic | Key not forwarded (direct) or missing inject rule (vault) | Pass `envs={...}` or fix the rule's `sni`/`host` |
| `403 Forbidden - CubeEgress` | Default-deny with no matching allow rule | Add the LLM host (and any extra hosts) to the rules |
| `Connection error` / TLS failure on the vault path | Claude Code runs on Node, which ignores the system CA store and won't trust the CubeEgress interception CA | The script points `NODE_EXTRA_CA_CERTS` at the dedicated CubeEgress anchor; override with `CLAUDE_CODE_NODE_EXTRA_CA_CERTS` if your CA lives elsewhere |
| Readiness probe timeout | Image without envd | Ensure `FROM ghcr.io/tencentcloud/cubesandbox-base:...` |
| `pause()`/`connect()` errors | Platform too old for snapshots | Upgrade the CubeSandbox platform |

## References

- Integration guide: [`docs/guide/integrations/claude-code.md`](../../docs/guide/integrations/claude-code.md)
- Snapshot / Clone / Rollback: [`docs/guide/snapshot-rollback-clone.md`](../../docs/guide/snapshot-rollback-clone.md)
- Network / egress policy examples: [`examples/network-policy`](../network-policy)
- Claude Code: <https://docs.anthropic.com/en/docs/claude-code>
