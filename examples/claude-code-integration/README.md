# Claude Code Integration for CubeSandbox

Run [Claude Code](https://docs.anthropic.com/en/docs/claude-code) — Anthropic's
agentic coding CLI — inside CubeSandbox MicroVMs. This example covers image
build, key injection, egress control, and snapshot-based session persistence.

[中文文档](README_zh.md)

## Quick Start

```bash
# 1. Configure
cp .env.example .env
# Fill in: E2B_API_URL, CUBE_TEMPLATE_ID, ANTHROPIC_API_KEY

# 2. Install dependencies
pip install -r requirements.txt

# 3. Build and register the template image
docker build --platform linux/amd64 \
  -t <your-registry>/claude-code-cube:latest \
  examples/claude-code-integration
docker push <your-registry>/claude-code-cube:latest

cubemastercli tpl create-from-image \
  --image <your-registry>/claude-code-cube:latest \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health

# 4. Run a one-shot task
python run_claude_code.py

# 5. (Optional) Demonstrate pause/resume
python resume_claude_code.py

# 6. (Recommended for production) Default-deny egress + credential vault
python network_policy.py
```

## Files

| File | Purpose |
|---|---|
| `Dockerfile` | Node.js 24 + Claude Code CLI on `cubesandbox-base:2026.16` |
| `.env.example` | Environment variable reference |
| `requirements.txt` | Python dependencies (`e2b`, `cubesandbox`, `python-dotenv`) |
| `run_claude_code.py` | One-shot headless Claude Code task |
| `resume_claude_code.py` | Pause/resume persistence demo |
| `network_policy.py` | CubeEgress credential vault (default-deny egress + on-the-wire key injection) |
| `_cc_common.py` | Shared sandbox command helpers + JSON/JSONL output rendering |
| `env_utils.py` | Environment variable utilities + CLI command builder |
| `test_cli.py` | CLI verification test suite (run to validate Claude Code CLI behavior) |

## How It Works

### Architecture

```
┌──────────────────────────┐     ┌─────────────────────────────┐
│   Host (Python driver)   │     │   CubeSandbox MicroVM        │
│                          │     │                              │
│  Sandbox.create()   ─────┼────→│  envd (:49983)              │
│  sandbox.commands.run()  │────→│  claude -p "..." --json     │
│  sandbox.pause()         │────→│  /workspace (persistent)    │
│  Sandbox.connect()       │────→│  /root/.claude (state dir)  │
└──────────────────────────┘     └─────────────────────────────┘
```

### One-Shot Task (`run_claude_code.py`)

Creates a sandbox, seeds a demo project, runs `claude -p "<prompt>" --output-format json`,
parses the result, and kills the sandbox. The API key is injected per-command via
`envs=` — simple for development, but the key enters the VM.

### Session Persistence (`resume_claude_code.py`)

- **Turn 1**: Claude Code writes `/workspace/plan.md`, then the sandbox is paused.
- **Pause**: `sandbox.pause()` snapshots VM memory + rootfs and frees compute.
- **Turn 2**: `Sandbox.connect(sandbox_id)` resumes the sandbox with all files
  and Claude Code state intact, then Claude Code continues the work.

The sandbox lifecycle is managed manually with `try/finally` (not `with Sandbox.create()`)
to prevent the context manager from killing the sandbox on pause.

### Credential Vault (`network_policy.py`)

The recommended production pattern:

1. **Default-deny egress** (`allow_internet_access=False`) — blocks all outbound
   traffic except the Anthropic API host.
2. **On-the-wire injection** — CubeEgress attaches `x-api-key` and
   `anthropic-version` headers at the proxy layer. The real key never enters
   the VM; `printenv ANTHROPIC_API_KEY` shows only a placeholder.
3. **Node.js CA trust** — Claude Code runs on Node.js, which ignores the system
   CA store. `NODE_EXTRA_CA_CERTS` points Node at a bundle that includes the
   CubeEgress root CA (installed by the base image).

### Output Formats

Claude Code supports three output modes with `--output-format` (all require `-p`/`--print`):

| Format | Behavior | Best for |
|---|---|---|
| `json` | Single JSON object with `type`, `result`, `usage`, `is_error` fields | One-shot tasks, result capture |
| `stream-json` | JSONL events (`system`/`assistant`/`result`) — requires `--verbose` | Real-time streaming, multi-turn |
| `text` | Plain text (default) | Debugging, human reading |

The example scripts default to `json`; add `--output-format stream-json --verbose`
to `cc_command()` for streaming.

## Environment Variables

| Variable | Where it flows | Notes |
|---|---|---|
| `E2B_API_URL` | Local process | CubeAPI address (`http://<node>:3000`) |
| `E2B_API_KEY` | Local process | Any non-empty string in local dev |
| `CUBE_TEMPLATE_ID` | `Sandbox.create(template=...)` | From `cubemastercli tpl create-from-image` |
| `ANTHROPIC_API_KEY` | `envs=...` (direct) or CubeEgress inject (vault) | Anthropic API key |
| `ANTHROPIC_BASE_URL` | Passed into the exec env | For API gateways / compatible endpoints |
| `CC_MODEL` | `--model` flag | Default: `claude-sonnet-4-6` |
| `CC_EFFORT` | `--effort` flag | Effort level: low, medium, high, xhigh, max |
| `CC_LLM_HOST` | `network_policy.py` | API host allowed under default-deny egress |
| `CC_PERMISSION_MODE` | `--permission-mode` flag | plan, acceptEdits, auto, bypassPermissions |

## Requirements

- CubeSandbox deployment with CubeAPI reachable at `http://<node>:3000`
- `cubemastercli` on `$PATH`, connected to the cluster
- Docker with a registry the Cube nodes can pull from
- Python 3.10+ for the host driver scripts
- Anthropic API key

## Caveats

- **Node.js CA trust (vault flavor).** Claude Code's Node.js runtime uses its own
  bundled CA store. Without `NODE_EXTRA_CA_CERTS` pointing at a bundle that includes
  the CubeEgress root CA, all API calls through the vault path fail with TLS errors.
- **Direct-flavor key persistence.** With the direct flavor (`envs=`) the key is
  scoped to the exec call, but sandbox snapshots may capture the in-VM environment.
  For strict isolation prefer the vault flavor.
- **Headless only.** Claude Code's interactive TUI is not available over the E2B
  protocol. Use `-p` / `--print` with `--output-format json` for
  machine-readable output.
- **Permission mode.** In sandbox environments, use `--permission-mode auto` or
  `--dangerously-skip-permissions` for fully autonomous execution. `plan` mode
  requires human approval for each tool call, which is not practical in a
  headless sandbox.
- **Egress side-effects.** Tasks that `npm install` or fetch MCP tools need those
  hosts allowed or preinstalled into the template.
- **API rate limits.** Claude Code interacts with the Anthropic API; standard
  rate limits and token quotas apply. Use `--max-budget-usd` to cap spending per
  session.
- **State directory.** `/root/.claude` holds Claude Code's session state. Keep
  it empty in the image to avoid leaking sessions across tenants.

## References

- [Claude Code documentation](https://docs.anthropic.com/en/docs/claude-code)
- [E2B Claude Code integration](https://e2b.dev/docs/agents/claude-code)
- [CubeSandbox Bring Your Own Image](../../docs/guide/tutorials/bring-your-own-image.md)
- [CubeSandbox Snapshot / Clone / Rollback](../../docs/guide/snapshot-rollback-clone.md)
- [CubeSandbox Security Proxy (credential vault)](../../docs/guide/security-proxy.md)
