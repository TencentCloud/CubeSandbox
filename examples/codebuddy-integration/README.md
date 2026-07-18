# CodeBuddy + CubeSandbox Example

[中文文档](README_zh.md)

Run the [Tencent CodeBuddy Code CLI](https://www.codebuddy.ai/docs/cli/README)
— a terminal-native AI coding agent — inside a CubeSandbox MicroVM. The agent
edits files, runs commands, and reaches an LLM API entirely within an isolated,
reproducible sandbox.

This example ships:

- A `Dockerfile` that stacks Node.js 20 + the CodeBuddy CLI on top of the
  CubeSandbox base image (envd already listens on `:49983`).
- `run_codebuddy.py` — a headless one-shot run inside `/workspace`.
- `resume_codebuddy.py` — pause/resume across two turns, proving `/workspace`
  and CodeBuddy's state directory (`/workspace/.codebuddy`) survive the snapshot.
- `network_policy.py` — a default-deny egress policy where CubeEgress injects
  the API key on the wire, so the key never enters the VM.
- `env_utils.py`, `_codebuddy_common.py`, `.env.example`, `requirements.txt`.

## Directory layout

```
codebuddy-integration/
├── Dockerfile            # CubeSandbox template image (Node.js + CodeBuddy CLI)
├── .env.example          # Copy to .env and fill in
├── .gitignore
├── requirements.txt      # Host driver deps (e2b, cubesandbox, python-dotenv)
├── env_utils.py          # .env loading, provider keys, CodeBuddy command builder
├── _codebuddy_common.py  # Shared sandbox command helpers (run/ensure/id)
├── run_codebuddy.py      # One-shot CodeBuddy task
├── resume_codebuddy.py   # Pause / resume session persistence
├── network_policy.py     # Default-deny egress + on-the-wire key injection
├── README.md             # English docs (this file)
└── README_zh.md          # Chinese docs
```

## Prerequisites

- A running CubeSandbox deployment; CubeAPI reachable at `http://<node>:3000`.
- `cubemastercli` on `$PATH`, connected to the cluster.
- Docker on the build workstation, plus a registry the Cube nodes can pull from.
- A CodeBuddy Code account (or a custom upstream API key — Anthropic, OpenAI,
  DeepSeek, Google Gemini, ...). See `.env.example`.
- Python 3.10+ for the host driver scripts.

## 1. Build the template image

```bash
docker build --pull --platform linux/amd64 \
  -t <your-registry>/codebuddy-cube:latest \
  examples/codebuddy-integration
docker push <your-registry>/codebuddy-cube:latest
```

The image installs `@tencent-ai/codebuddy-code` plus `git`, `python3`,
`ripgrep`, `jq`, and cleans apt/npm caches. The CodeBuddy version is pinned
via `--build-arg CODEBUDDY_VERSION=x.y.z`.

## 2. Register as a Cube template

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/codebuddy-cube:latest \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health

cubemastercli tpl watch --job-id <job_id>
```

Note the `template_id` once the job reaches `READY`.

## 3. Configure the host driver

```bash
cd examples/codebuddy-integration
cp .env.example .env
# fill in CUBE_API_URL, CUBE_TEMPLATE_ID, CODEBUDDY_INTERNET_ENVIRONMENT, and your key
pip install -r requirements.txt
```

| Variable | Where it flows | Notes |
|---|---|---|
| `CUBE_API_URL` | Local process | CubeAPI address (`http://<node>:3000`) |
| `CUBE_API_KEY` | Local process | Any non-empty string in local dev |
| `CUBE_TEMPLATE_ID` | `Sandbox.create(template=...)` | From step 2 |
| `CODEBUDDY_INTERNET_ENVIRONMENT` | CodeBuddy CLI | `io` (default, international), `internal` (China), `ioa` (Tencent enterprise) |
| `CODEBUDDY_MODEL` | CodeBuddy CLI | Model id for the active provider |
| `CODEBUDDY_API_KEY` / `ANTHROPIC_API_KEY` / ... | `envs=...` (direct) or CubeEgress inject (vault) | Provider key |
| `CODEBUDDY_BASE_URL` / `ANTHROPIC_BASE_URL` | CodeBuddy CLI | Custom upstream endpoint (e.g. DeepSeek via Anthropic-compatible gateway) |
| `CODEBUDDY_LLM_HOST` | `network_policy.py` | LLM API host to allow; defaults to the parsed `*_BASE_URL` host or the provider default |

## 4. One-shot run (direct key flavor)

```bash
python run_codebuddy.py --prompt "Create hello.py that prints 'Hello from CubeSandbox' and run it."
```

CodeBuddy is invoked headlessly with `-p` (process the prompt and exit, no TUI)
plus `-y` (`--dangerously-skip-permissions`, required for any non-interactive
run that touches files or runs commands — without it the CLI blocks on a
permission prompt that cannot be answered over the exec channel). The provider
key is forwarded per-command via `sandbox.commands.run(..., envs=...)`, so it
lives only for the lifetime of that exec call — never written to a persistent
file inside the VM.

> **Security:** this direct flavor leaves egress open, so a compromised agent
> could exfiltrate the injected key. For shared clusters use the vault flavor
> (step 6): default-deny egress + on-the-wire key injection.

## 5. Pause / resume (session persistence)

```bash
python resume_codebuddy.py
```

Turn 1 asks CodeBuddy to write `/workspace/plan.md`, then `sandbox.pause()`
snapshots the VM. The script reconnects with `Sandbox.connect(sandbox_id)`,
verifies `/workspace/plan.md` and CodeBuddy's state directory
(`/workspace/.codebuddy/projects/...`) survived, then runs turn 2 with
`-c` to continue the most recent session. The sandbox lifecycle is managed
manually with `try/finally` (not a context manager), so the pause is not
undone by an early `kill`.

## 6. Restricted egress + key vault (recommended for shared clusters)

```bash
python network_policy.py
```

- Egress is default-deny — only the LLM host (`CODEBUDDY_LLM_HOST`) is reachable.
- CubeEgress attaches the provider key as an HTTP header on the wire
  (`x-api-key` for Anthropic, `Authorization: Bearer` otherwise), so
  `printenv` inside the sandbox never shows the real key — it only sees a
  placeholder.
- Because CodeBuddy ships as a Node.js bundle that ignores the system CA
  store, the script sets `NODE_EXTRA_CA_CERTS` so CodeBuddy trusts the
  CubeEgress interception CA; without it the vault path fails with
  "unable to verify the first certificate". Override the bundle path via
  `CODEBUDDY_NODE_EXTRA_CA_CERTS` if your image keeps the CA elsewhere.
- Any other destination returns `403 Forbidden - CubeEgress`.

If the agent needs extra hosts (package registries, MCP servers), add matching
allow rules or preinstall those dependencies into the template.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `codebuddy: command not found` in preflight | Template not rebuilt after CLI change | Rebuild the image, re-register the template |
| Permission prompt hangs the run | Forgot `-y` / `--dangerously-skip-permissions` on a run that touches files or commands | The default invocation already passes `-y`; use `settings.json` permissions if you want a safer default |
| Provider auth failure | Key not forwarded (direct) or missing inject rule (vault) | Pass `envs={...}` or fix the rule's `sni`/`host` |
| `403 Forbidden - CubeEgress` | Default-deny with no matching allow rule | Add the LLM host (and any extra hosts) to the rules |
| `Connection error` / TLS failure from CodeBuddy (vault) | CodeBuddy is a Node.js bundle that ignores the system CA store, so it won't trust the CubeEgress interception CA | The script sets `NODE_EXTRA_CA_CERTS` to the system bundle; override with `CODEBUDDY_NODE_EXTRA_CA_CERTS` if your CA lives elsewhere |
| Template creation stuck in `PULLING` | Registry unreachable from Cube nodes | Push to a registry the cluster can reach; supply auth if needed |
| Readiness probe timeout | Base image without envd | Ensure `FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| `pause()` / `connect()` errors | Platform too old for snapshots | Upgrade the CubeSandbox platform |
| Login browser popup blocks the run | Default mode is interactive; `-p` requires a pre-set API key | Set `CODEBUDDY_API_KEY` (or the matching provider env) — the non-interactive mode never falls back to a browser flow |

## References

- Integration guide: [`docs/guide/integrations/codebuddy.md`](../../docs/guide/integrations/codebuddy.md)
- Snapshot / Clone / Rollback: [`docs/guide/snapshot-rollback-clone.md`](../../docs/guide/snapshot-rollback-clone.md)
- Network / egress policy examples: [`examples/network-policy`](../network-policy)
- Credential vault + egress control: [`docs/guide/security-proxy.md`](../../docs/guide/security-proxy.md)
- CodeBuddy Code CLI: <https://www.codebuddy.ai/docs/cli/README>
