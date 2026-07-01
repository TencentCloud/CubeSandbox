# Claude Code + CubeSandbox

[中文文档](README_zh.md)

Run [Anthropic Claude Code](https://docs.anthropic.com/en/docs/claude-code) —
a terminal-native AI coding agent — **inside a CubeSandbox MicroVM**. The agent
gets a KVM-isolated rootfs with hardware-level isolation from the host and
your other tenants; the operator gets an audited egress path where API keys
never enter the VM.

```
┌───────────────────────┐          ┌───────────────────────┐
│  Host driver script    │  E2B     │  CubeSandbox MicroVM  │
│  (run_claude.py)       │  proto   │                       │
│                        │ ────────►│  envd  (:49983)       │
│  Anthropic key stays   │          │  claude CLI (Node 20) │
│  on the host OR is     │          │  git / python3 / rg   │
│  injected by CubeEgress│          │  /workspace           │
└──────────┬─────────────┘          └───────────┬───────────┘
           │                                    │ HTTPS
           │                                    ▼
           │                             ┌─────────────────┐
           └─── (optional inject rule) ─►│  CubeEgress     │───► api.anthropic.com
                                         └─────────────────┘
```

## What you get

| Feature | How CubeSandbox provides it |
|---|---|
| Hardware-isolated rootfs for the agent | KVM MicroVM (Cloud Hypervisor) with a dedicated guest kernel |
| Sub-second cold start of a fresh workspace | Boot from a template snapshot in <60 ms |
| Long-running agent tasks with checkpoint / resume | `sandbox.pause()` + `Sandbox.connect(sandbox_id)` |
| API key never touches the sandbox | CubeEgress `inject` rule attaches `x-api-key` on egress |
| Egress control | Domain allowlist limited to `api.anthropic.com` (or your gateway) |
| Full audit log of every LLM request | `/data/log/cube-egress/access.jsonl` per host |

## Directory layout

```
claude-code-integration/
├── README.md                # This file (English)
├── README_zh.md             # Chinese version
├── Dockerfile               # cubesandbox-base + Node 20 + @anthropic-ai/claude-code
├── env.example              # Copy to .env and fill in
├── env_utils.py             # dotenv + envs=... helper
├── requirements.txt         # e2b>=2.4.1, python-dotenv
├── run_claude.py            # Minimal one-shot demo
├── resume_claude.py         # pause / resume across two turns
└── network_policy.py        # CubeEgress inject rule (key-in-vault mode)
```

## 1. Build & register the template

Any x86_64 host with Docker will do — the image only ships in a registry the
Cube cluster can reach.

```bash
# 1) Build
docker build -t claude-code-cube:latest examples/claude-code-integration

# 2) Push to a registry your Cube nodes can pull from
docker tag  claude-code-cube:latest <your-registry>/claude-code-cube:latest
docker push                        <your-registry>/claude-code-cube:latest

# 3) Register as a Cube template
cubemastercli tpl create-from-image \
  --image <your-registry>/claude-code-cube:latest \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health

# 4) Wait for READY
cubemastercli tpl watch --job-id <job_id>
```

The image inherits `cubesandbox-base`, so envd is already listening on
`:49983`. Claude Code lands at `/usr/bin/claude` and its state directory at
`/root/.claude/`; nothing user-specific is baked into the image.

## 2. Configure the driver

```bash
cd examples/claude-code-integration
cp env.example .env      # fill in E2B_API_URL, CUBE_TEMPLATE_ID, ANTHROPIC_API_KEY
pip install -r requirements.txt
```

| Variable | Where it flows | Notes |
|---|---|---|
| `E2B_API_URL` | Local process | Points at CubeAPI (`http://<node>:3000`) |
| `E2B_API_KEY` | Local process | Any non-empty string |
| `CUBE_TEMPLATE_ID` | `Sandbox.create(template=...)` | From step 1 |
| `ANTHROPIC_API_KEY` | Forwarded per-command via `envs=...` | Or omit when using CubeEgress inject |
| `ANTHROPIC_MODEL` | Optional, e.g. `claude-sonnet-4-5` | Passed straight to `claude` |
| `ANTHROPIC_BASE_URL` | Optional custom Anthropic-compatible gateway | e.g. Bedrock proxy |
| `CLAUDE_CODE_USE_BEDROCK` | Optional, `true` to route through AWS Bedrock | Requires AWS creds in the sandbox exec env; **mutually exclusive with Vertex** |
| `CLAUDE_CODE_USE_VERTEX` | Optional, `true` to route through Google Vertex AI | Requires `GOOGLE_APPLICATION_CREDENTIALS` in the sandbox; **mutually exclusive with Bedrock** |

## 3. One-shot run

```bash
python run_claude.py --prompt "Create hello.py that prints 'Hello from CubeSandbox!' and run it."
```

Under the hood the script:

1. Boots a sandbox from the Claude Code template.
2. `claude --version` preflight to catch stale templates fast.
3. Runs `claude --print --allowedTools 'Bash(npm:*),Bash(node:*),Bash(python3:*),Edit,Write,Read' -- <prompt>`
   inside `/workspace` with the API key forwarded per-exec via `envs=...`.
4. Streams stdout/stderr back to the host in real time.
5. Lists `/workspace` so you can see the files the agent produced.

Add `--stream-json` for `--output-format stream-json` (each turn emitted as a
JSON event — handy for orchestration systems that want to watch the tool
calls). Add `--seed ./my_project.py` to upload a local file into the
workspace before the agent runs.

## 4. Long-running tasks with pause / resume

```bash
python resume_claude.py
```

The demo runs Claude Code, then calls `sandbox.pause()` — the VM is
snapshotted to disk, resources are released, and a later
`Sandbox.connect(sandbox_id)` brings it back with the rootfs, `/workspace`
files, and Claude Code's on-disk state (`~/.claude/`) intact. This is
particularly useful for:

- multi-hour refactors where you want to walk away and check in later
- interactive sessions that need to survive host maintenance
- forking N variants from a common baseline (via `sandbox.pause()` + clone)

The E2B protocol version of this call maps directly onto CubeSandbox's
[Snapshot / Clone / Rollback engine](../../docs/guide/snapshot-rollback-clone.md).

## 5. Credential vault mode (recommended for shared clusters)

`network_policy.py` demonstrates the **API-key-never-in-VM** pattern. The
sandbox is created with a CubeEgress rule set that:

- default-denies every outbound request,
- allows `https://api.anthropic.com`,
- injects `x-api-key: sk-ant-...` and `anthropic-version: 2023-06-01`
  headers on the wire.

```bash
python network_policy.py
```

Inside the sandbox, `printenv | grep ANTHROPIC_API_KEY` returns nothing —
the CLI still authenticates because CubeEgress attaches the header after
the sandbox emits the bare request. Every request lands in
`/data/log/cube-egress/access.jsonl` on the host with the rule name, sandbox
IP, method, path, latency, and TLS handshake outcome.

For gateway deployments (Bedrock, Vertex, in-house Anthropic proxies) point
`ANTHROPIC_BASE_URL` at the gateway and change the rule's `sni` / `host`
accordingly. Full grammar: [Security proxy guide](../../docs/guide/security-proxy.md).

## 6. Best practices

**Egress**: keep the sandbox default-denied. Add exactly the domains
Claude Code needs — usually only `api.anthropic.com`, plus `registry.npmjs.org`
if the task installs new npm packages.

**Tool whitelisting**: pass `--allowedTools` (or set `--dangerously-skip-permissions`
only inside a sandbox you're OK to lose). The CLI still enforces user
prompts by default, but a whitelist compresses one round-trip per call.

**Snapshot as commit**: after the agent completes a self-contained step,
`sandbox.pause()` yields a resumable checkpoint. If the next turn goes
sideways you can `Sandbox.connect(sid)` on the previous one and try again.

**Concurrent agents**: one sandbox per session. CubeSandbox's `<60 ms`
cold start makes N-parallel workflows cheap; hardware isolation means one
malfunctioning agent can't touch its neighbours.

**Working directory**: mount only `/workspace` for user files. Everything
under `/root/.claude/` is agent state — losing it just resets the agent's
short-term memory, not the code you produced.

## 7. Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `claude: command not found` in preflight | Wrong template | Re-check `CUBE_TEMPLATE_ID`; rebuild image; template must have Node ≥ 18 |
| `Invalid API key` from Claude Code | `ANTHROPIC_API_KEY` not forwarded (or CubeEgress rule missing) | See §5, or add the key to `envs=...` |
| Egress blocked with `403 Forbidden — CubeEgress` | Default-deny with no matching allow rule | Add `Match(sni="api.anthropic.com")` (or your gateway) |
| Template creation fails readiness probe | Base image without envd | Ensure `FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| `Cannot find /workspace` | Custom `WORKDIR` overridden | Pass `--workspace /some/dir` and `mkdir -p` before running |
| Agent hangs on `Bash(...)` prompt | No `--allowedTools` supplied and TTY is missing | Use `--print` + `--allowedTools` (both scripts already do this) |
| SSL handshake fails against `api.anthropic.com` | CubeEgress cert not trusted inside sandbox | Template must be built `--with-cube-ca=true` (default), or set `SSL_CERT_FILE` |

## 8. Related docs

- [Integration guide (English)](../../docs/guide/integrations/claude-code.md)
- [集成指南（中文）](../../docs/zh/guide/integrations/claude-code.md)
- [Bring Your Own Image (envd)](../../docs/guide/tutorials/bring-your-own-image.md)
- [Create Templates from OCI Images](../../docs/guide/tutorials/template-from-image.md)
- [Snapshot · Clone · Rollback](../../docs/guide/snapshot-rollback-clone.md)
- [Security Proxy · Credential Vault](../../docs/guide/security-proxy.md)
- [Claude Code CLI reference](https://docs.anthropic.com/en/docs/claude-code/cli-reference)
