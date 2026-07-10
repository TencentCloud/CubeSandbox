# OpenCode × CubeSandbox Integration

Run [OpenCode](https://www.npmjs.com/package/opencode-ai) — a terminal-native AI
coding agent — inside CubeSandbox MicroVMs with hardware-level isolation,
snapshot-based session persistence, and optional default-deny egress with
on-the-wire credential injection.

> Full integration guide: [`docs/guide/integrations/opencode.md`](../../docs/guide/integrations/opencode.md)

## Prerequisites

| Requirement | Notes |
|---|---|
| CubeSandbox deployment | CubeAPI reachable at `http://<node>:3000` |
| `cubemastercli` | On `$PATH`, connected to the cluster |
| Python | 3.9+ |
| Docker | Build workstation + registry the Cube nodes can pull from |
| LLM provider API key | `anthropic`, `openai`, `deepseek`, or `openrouter` |

## Quick Start

### 1. Build and register the template

```bash
# All environment variables are optional; defaults shown
# REGISTRY=ghcr.io/tencentcloud  IMAGE_NAME=opencode-cube  IMAGE_TAG=latest
./build-template.sh
```

The script builds the Docker image, pushes it, registers it with
`cubemastercli tpl create-from-image`, watches the build job, and prints
the final `template_id` once it reaches `READY`.

### 2. Configure `.env`

```bash
cp .env.example .env
# fill in CUBE_API_URL, CUBE_TEMPLATE_ID, and your provider key
pip install -r requirements.txt
```

### 3. Run the one-shot example

```bash
python run_opencode.py
```

This creates a MicroVM, seeds a deterministic calculator project, runs an
OpenCode coding task, verifies the result contains the expected marker, and
tears the sandbox down.

## Files

| File | Description |
|---|---|
| `build-template.sh` | Build, push, and register the template image |
| `Dockerfile` | Stacks Node.js 24 + OpenCode CLI on `cubesandbox-base` |
| `run_opencode.py` | One-shot example: seed → run → verify |
| `snapshot_restore.py` | Pause/resume: run turn 1 → pause → reconnect → run turn 2 |
| `network_policy.py` | Default-deny egress with CubeEgress credential vault |
| `env_utils.py` | Configuration, provider specs, secret redaction helpers |
| `_opencode_common.py` | Shared command builder, result parser, session ID extraction |
| `test_commands.py` | Unit tests for `_opencode_common.py` |
| `test_env_utils.py` | Unit tests for `env_utils.py` |
| `.env.example` | Template for required environment variables |
| `requirements.txt` | Python dependencies (`cubesandbox`, `python-dotenv`) |

## Environment Variables

See `.env.example` for the full template. Key variables:

| Variable | Required | Default | Description |
|---|---|---|---|
| `CUBE_API_URL` | Yes | — | CubeAPI address (`http://<node>:3000`) |
| `CUBE_TEMPLATE_ID` | Yes | — | Template ID from `build-template.sh` |
| `OPENCODE_PROVIDER` | Yes | `anthropic` | LLM provider: `anthropic`, `openai`, `deepseek`, `openrouter` |
| `OPENCODE_MODEL` | Yes | — | Must use `provider/model` form (e.g. `anthropic/claude-sonnet-4-6`) |
| `ANTHROPIC_API_KEY` | Provider | — | Provider key (only the selected provider's key is needed) |
| `OPENAI_API_KEY` | Provider | — | — |
| `DEEPSEEK_API_KEY` | Provider | — | — |
| `OPENROUTER_API_KEY` | Provider | — | — |
| `OPENCODE_LLM_HOST` | No | Provider default | Override LLM API hostname for CubeEgress rules |
| `OPENCODE_WORKSPACE` | No | `/workspace` | Workspace path inside the sandbox |
| `OPENCODE_SANDBOX_TIMEOUT` | No | `1800` | Sandbox lifetime in seconds |
| `OPENCODE_EXEC_TIMEOUT` | No | `900` | Per-command timeout in seconds |
| `OPENCODE_NODE_CA_BUNDLE` | No | `/etc/ssl/certs/ca-certificates.crt` | CA bundle for CubeEgress TLS (vault flavor) |
| `OPENCODE_PLACEHOLDER_KEY` | No | `cube-egress-managed-placeholder` | Placeholder key value inside the VM (vault flavor) |
| `CUBE_PROXY_NODE_IP` | No | — | Direct node routing for SDK data path |

## Snapshot and Restore

```bash
python snapshot_restore.py
```

Runs a two-turn workflow:

1. **Turn 1** — OpenCode creates `plan.md` describing the intended work.
2. **Pause** — `sandbox.pause()` snapshots the running VM (memory + rootfs).
3. **Reconnect** — `Sandbox.connect(sandbox_id)` restores with `/workspace`
   and OpenCode's state directories intact.
4. **Turn 2** — OpenCode continues with `--continue`, implementing the plan
   and running tests.

> Use `try/finally` — not a `with` context manager — to manage the sandbox
> lifecycle. A context manager kills the sandbox on `__exit__`, undoing the
> pause.

## Default-Deny Egress and Credential Vault

```bash
python network_policy.py
```

Creates a sandbox with `allow_internet_access=False` and a single allow rule
for the configured LLM host. The real provider key stays on the host; only a
placeholder is placed inside the VM. CubeEgress injects the real auth header
on the wire when OpenCode calls the LLM API.

- `printenv ANTHROPIC_API_KEY` inside the sandbox shows only the placeholder.
- Every request to the allowed LLM host gets the auth header attached.
- All other destinations are dropped at L3/L4 by CubeVS.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `opencode: command not found` | Template not rebuilt after CLI change | Rebuild the image and re-register the template |
| Provider auth failure | Key not forwarded (direct) or missing inject rule (vault) | Pass `envs={...}` or fix the rule's `sni`/`host` |
| `403 Forbidden - CubeEgress` | Default-deny with no matching allow rule | Add the LLM host to the network rules |
| `Connection error` / TLS failure (vault) | Node.js ignores system CA store | Example sets `NODE_EXTRA_CA_CERTS`; override with `OPENCODE_NODE_CA_BUNDLE` |
| Template stuck in `PULLING` | Registry unreachable from Cube nodes | Push to an accessible registry; supply auth if needed |
| Readiness probe timeout | Base image missing envd | Use `FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| `Missing required environment variable` | `.env` not configured | Copy `.env.example` and fill in the required values |
| `OPENCODE_MODEL` validation error | Model missing `provider/model` form | Use e.g. `anthropic/claude-sonnet-4-6` |

## Full Guide

For deeper coverage of architecture, best practices, and caveats, see the
[OpenCode Integration Guide](../../docs/guide/integrations/opencode.md).
