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
- `sandbox_exec.py` — a host-side CLI executor that lets you run arbitrary
  Python or shell code inside a disposable MicroVM (`--code`, `--file`,
  `--cmd`, `--pip`). Reuses a cached sandbox across invocations via a
  UID-scoped session file under `/tmp`.
- `mcp_server.py` — exposes the same execution backend as an MCP server
  (JSON-RPC over stdio) so any MCP client (Claude Desktop, Cursor, …) can
  sandbox its code through CodeBuddy's toolchain.
- `hooks/` — a CodeBuddy JavaScript plugin (`cubesandbox-sandbox.js`) that
  routes the in-agent `bash` tool through the host-side executor, plus a
  shell installer (`install.sh`) that drops the plugin into
  `~/.config/codebuddy/plugins/`.
- `env_utils.py`, `_codebuddy_common.py`, `.env.example`, `requirements.txt`,
  `tests/`.

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
├── sandbox_exec.py       # Host-side CLI: --code / --file / --cmd / --pip
├── mcp_server.py         # JSON-RPC stdio MCP server with 5 sandbox tools
├── hooks/
│   ├── install.sh                       # Copy plugin + sanitized config into ~/.config/codebuddy
│   └── cubesandbox-sandbox.js           # tool.execute.before plugin that forwards bash to a sandbox
├── tests/                # pytest suite (fully offline, SDK mocked)
│   ├── conftest.py
│   ├── test_sandbox_exec.py
│   ├── test_mcp_server.py
│   └── test_codebuddy_common.py
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

## 7. Host-side executor (`sandbox_exec.py`)

For tasks that need to run untrusted code but do not need the CodeBuddy
agent itself, the `sandbox_exec.py` CLI runs code directly inside a
disposable MicroVM. The host stays clean; the sandbox is destroyed (or
its session cached) at the end of each call.

```bash
python sandbox_exec.py --code "print(1+1)"
python sandbox_exec.py --file ./script.py
python sandbox_exec.py --cmd "ls -la /workspace"
python sandbox_exec.py --pip requests --code "import requests; print(requests.__version__)"

# Reuse the same sandbox on the next call instead of cold-starting
python sandbox_exec.py --keep-alive --code "state = 42"
python sandbox_exec.py --cmd "echo state still alive"

# Force a fresh sandbox
python sandbox_exec.py --reset
```

The cross-process cache uses a UID-scoped session file under
`/tmp/cubesandbox_codebuddy_session_<uid>` (`O_NOFOLLOW` + `0600` +
`symlink → S_ISREG` check) so a different user on a shared host cannot
hijack your `Sandbox.connect()` on the next call.

## 8. MCP server (`mcp_server.py`)

The same execution backend is also exposed as a newline-delimited
JSON-RPC MCP server so any MCP client (Claude Desktop, Cursor,
Windsurf, VS Code, …) can run untrusted code in the CodeBuddy
sandbox instead of locally. Five tools are exposed:

| Tool | Purpose |
| --- | --- |
| `sandbox_run_code` | Run a Python snippet in the sandbox |
| `sandbox_run_command` | Run an arbitrary shell command in the sandbox |
| `sandbox_write_file` | Write a file into the sandbox |
| `sandbox_read_file` | Read a file back from the sandbox |
| `sandbox_reset` | Destroy the cached sandbox and start over |

Wire it into an MCP client (Claude Desktop example shown):

```json
{
  "mcpServers": {
    "cubesandbox-codebuddy": {
      "command": "python3",
      "args": ["/abs/path/to/examples/codebuddy-integration/mcp_server.py"],
      "env": {
        "CUBE_API_URL": "http://<cube-host>:3000",
        "CUBE_API_KEY": "<api-key>",
        "CUBE_TEMPLATE_ID": "<template-id>"
      }
    }
  }
}
```

The server lifecycle is process-global: a single sandbox is created on
first use, its TTL is refreshed on every tool call, and an `atexit`
handler kills it when the MCP process exits. `sandbox_reset` is the
only way to force a fresh one mid-session.

## 9. Host CodeBuddy + bash-routing plugin (`hooks/`)

For the opposite workflow — keep CodeBuddy on the host but route every
`bash` tool call through CubeSandbox — install the JavaScript plugin
in `hooks/`:

```bash
cd examples/codebuddy-integration
pip install -r requirements.txt
cp .env.example .env
# Fill in CUBE_API_URL and CUBE_TEMPLATE_ID

cd hooks
./install.sh
```

The installer copies `cubesandbox-sandbox.js` into
`~/.config/codebuddy/plugins/`, drops a sibling `package.json` declaring
`"type": "module"`, and merges only an allow-listed subset of
`CUBE_*` settings into the CodeBuddy config. LLM provider API keys are
never copied.

Restart CodeBuddy after install. The plugin's
`tool.execute.before` hook intercepts `bash` calls, spawns the host
`python3 sandbox_exec.py --cmd <quoted>` against the cached session
sandbox, and replaces the host-shell command with the sandbox output.
Other tools still run on the host as usual — the plugin only intercepts `bash`.

Uninstall with `hooks/install.sh --uninstall`.

## Running the tests

```bash
cd examples/codebuddy-integration
pip install pytest
pytest tests -v
```

The suite is fully offline: no CubeSandbox cluster or LLM credentials
are needed. `sandbox_exec` and `mcp_server` are exercised via
`unittest.mock` against the e2b SDK; the helpers are exercised
directly so test order cannot leak state.

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
