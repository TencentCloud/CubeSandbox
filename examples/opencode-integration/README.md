# OpenCode + CubeSandbox Example

[中文文档](README_zh.md)

Run the [OpenCode CLI](https://opencode.ai/) — an open-source, terminal-native
AI coding agent — inside a CubeSandbox MicroVM. The agent edits files, runs
commands, and reaches an LLM API entirely within an isolated, reproducible
sandbox.

This example ships:

- A `Dockerfile` that stacks Node.js 20 + the OpenCode CLI on top of the
  CubeSandbox base image (envd already listens on `:49983`).
- `run_opencode.py` — a headless one-shot run inside `/workspace`.
- `resume_opencode.py` — pause/resume across two turns, proving `/workspace`,
  OpenCode's config dir (`/workspace/.opencode/config/`) and data dir
  (`/workspace/.opencode/data/`, where sessions live) all survive the snapshot.
- `network_policy.py` — a default-deny egress policy where CubeEgress injects
  the API key on the wire, so the key never enters the VM.
- `sandbox_exec.py` — a host-side CLI executor that lets you run arbitrary
  Python or shell code inside a disposable MicroVM (`--code`, `--file`,
  `--cmd`, `--pip`). Reuses a cached sandbox across invocations via a
  UID-scoped session file under `/tmp`.
- `mcp_server.py` — exposes the same execution backend as an MCP server
  (JSON-RPC over stdio) so any MCP client (Claude Desktop, Cursor, …) can
  sandbox its code through OpenCode's toolchain.
- `hooks/` — an OpenCode JavaScript plugin (`cubesandbox-sandbox.js`) that
  routes the in-agent `bash` tool through the host-side executor, plus a
  shell installer (`install.sh`) that drops the plugin into
  `~/.config/opencode/plugins/`. Mirrors the PreToolUse-Bash pattern from
  the Claude Code integration.
- `env_utils.py`, `_opencode_common.py`, `.env.example`, `requirements.txt`,
  `tests/`.

## Directory layout

```
opencode-integration/
├── Dockerfile            # CubeSandbox template image (Node.js + OpenCode CLI)
├── .env.example          # Copy to .env and fill in
├── .gitignore
├── requirements.txt      # Host driver deps (e2b, cubesandbox, python-dotenv)
├── env_utils.py          # .env loading, provider keys, OpenCode command builder
├── _opencode_common.py   # Shared sandbox command helpers (run/ensure/id)
├── run_opencode.py       # One-shot OpenCode task
├── resume_opencode.py    # Pause / resume session persistence
├── network_policy.py     # Default-deny egress + on-the-wire key injection
├── sandbox_exec.py       # Host-side CLI: --code / --file / --cmd / --pip
├── mcp_server.py         # JSON-RPC stdio MCP server with 5 sandbox tools
├── hooks/
│   ├── install.sh                       # Copy plugin + sanitized config into ~/.config/opencode
│   └── cubesandbox-sandbox.js           # tool.execute.before plugin that forwards bash to a sandbox
├── tests/                # pytest suite (mirrors the Claude Code test layout)
│   ├── conftest.py
│   ├── test_env_utils.py
│   ├── test_opencode_common.py
│   ├── test_sandbox_exec.py
│   └── test_mcp_server.py
├── README.md             # English docs (this file)
└── README_zh.md          # Chinese docs
```

## Prerequisites

- A running CubeSandbox deployment; CubeAPI reachable at `http://<node>:3000`.
- `cubemastercli` on `$PATH`, connected to the cluster.
- Docker on the build workstation, plus a registry the Cube nodes can pull from.
- An OpenCode-compatible LLM provider key — Anthropic, OpenAI, Google Gemini,
  DeepSeek, OpenRouter, Groq, Mistral, or any provider OpenCode ships a
  built-in preset for. See `.env.example`.
- Python 3.10+ for the host driver scripts.

## 1. Build the template image

```bash
docker build --pull --platform linux/amd64 \
  -t <your-registry>/opencode-cube:latest \
  examples/opencode-integration
docker push <your-registry>/opencode-cube:latest
```

The image installs `opencode-ai` plus `git`, `python3`, `ripgrep`, `jq`, and
cleans apt/npm caches. The OpenCode version is pinned via
`--build-arg OPENCODE_VERSION=x.y.z`. **Tested against `opencode-ai@1.17.20`**
— the script relies on the `--dangerously-skip-permissions` flag that landed in
OpenCode 1.17.x. Older releases need the equivalent
`OPENCODE_PERMISSION='{"*":"allow"}'` env var instead.

## 2. Register as a Cube template

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/opencode-cube:latest \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health

cubemastercli tpl watch --job-id <job_id>
```

Note the `template_id` once the job reaches `READY`.

## 3. Configure the host driver

```bash
cd examples/opencode-integration
cp .env.example .env
# fill in E2B_API_URL, CUBE_TEMPLATE_ID, and your provider key
pip install -r requirements.txt
```

| Variable | Where it flows | Notes |
|---|---|---|
| `E2B_API_URL` | Local process | CubeAPI address (`http://<node>:3000`) |
| `E2B_API_KEY` | Local process | Any non-empty string in local dev |
| `CUBE_TEMPLATE_ID` | `Sandbox.create(template=...)` | From step 2 |
| `OPENCODE_PROVIDER` | `env_utils.provider()` | Optional — inferred from the active key when unset |
| `OPENCODE_MODEL` | OpenCode CLI | Model id for the active provider |
| `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` / `GOOGLE_API_KEY` / ... | `envs=...` (direct) or CubeEgress inject (vault) | Provider key |
| `OPENCODE_BASE_URL` / `ANTHROPIC_BASE_URL` | OpenCode CLI | Custom upstream endpoint (e.g. DeepSeek via Anthropic-compatible gateway) |
| `OPENCODE_LLM_HOST` | `network_policy.py` | LLM API host to allow; defaults to the parsed `*_BASE_URL` host or the provider default |

## 4. One-shot run (direct key flavor)

```bash
python run_opencode.py --prompt "Create hello.py that prints 'Hello from CubeSandbox' and run it."
```

OpenCode is invoked headlessly with `opencode run "..."` (process the prompt
and exit, no TUI). The script appends `--dangerously-skip-permissions`
(OpenCode 1.17+, `--yolo` alias) so every tool call is auto-approved for the
duration of the run. Without it, the CLI blocks on a permission prompt that
cannot be answered over the exec channel. The provider key is forwarded
per-command via `sandbox.commands.run(..., envs=...)`, so it lives only for
the lifetime of that exec call — never written to a persistent file inside
the VM. Pass `--no-approve` to drop the flag and run with OpenCode's
permission prompts active (only safe if you've tightened the tool allow-list
via `opencode.json`).

> **Security:** this direct flavor leaves egress open, so a compromised agent
> could exfiltrate the injected key. For shared clusters use the vault flavor
> (step 6): default-deny egress + on-the-wire key injection.

## 5. Pause / resume (session persistence)

```bash
python resume_opencode.py
```

Turn 1 asks OpenCode to write `/workspace/plan.md`, then `sandbox.pause()`
snapshots the VM. The script reconnects with `Sandbox.connect(sandbox_id)`,
verifies `/workspace/plan.md` and OpenCode's data dir
(`/workspace/.opencode/data/storage/`, where session messages live) survived,
then runs turn 2 with `-c` to continue the most recent session. The sandbox
lifecycle is managed manually with `try/finally` (not a context manager), so
the pause is not undone by an early `kill`.

## 6. Restricted egress + key vault (recommended for shared clusters)

```bash
python network_policy.py
```

- Egress is default-deny — only the LLM host (`OPENCODE_LLM_HOST`) is reachable.
- CubeEgress attaches the provider key as an HTTP header on the wire
  (`x-api-key` for Anthropic, `Authorization: Bearer` otherwise), so
  `printenv` inside the sandbox never shows the real key — it only sees a
  placeholder.
- Because OpenCode ships as a Node.js bundle that ignores the system CA
  store, the script sets `NODE_EXTRA_CA_CERTS` so OpenCode trusts the
  CubeEgress interception CA; without it the vault path fails with
  "unable to verify the first certificate". Override the bundle path via
  `OPENCODE_NODE_EXTRA_CA_CERTS` if your image keeps the CA elsewhere.
- Any other destination returns `403 Forbidden - CubeEgress`.

If the agent needs extra hosts (package registries, MCP servers), add matching
allow rules or preinstall those dependencies into the template.

## 7. Host-side executor (`sandbox_exec.py`)

For tasks that need to run untrusted code but do not need the OpenCode
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
`/tmp/cubesandbox_opencode_session_<uid>` (`O_NOFOLLOW` + `0600` +
`symlink → S_ISREG` check) so a different user on a shared host cannot
hijack your `Sandbox.connect()` on the next call.

## 8. MCP server (`mcp_server.py`)

The same execution backend is also exposed as a newline-delimited
JSON-RPC MCP server so any MCP client (Claude Desktop, Cursor,
Windsurf, VS Code, …) can run untrusted code in the OpenCode
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
    "cubesandbox-opencode": {
      "command": "python3",
      "args": ["/abs/path/to/examples/opencode-integration/mcp_server.py"],
      "env": {
        "E2B_API_URL": "http://<cube-host>:3000",
        "E2B_API_KEY": "<api-key>",
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

## 9. Host OpenCode + bash-routing plugin (`hooks/`)

For the opposite workflow — keep OpenCode on the host but route every
`bash` tool call through CubeSandbox — install the JavaScript plugin
in `hooks/`:

```bash
cd examples/opencode-integration
python3 -m pip install -r requirements.txt
cp .env.example .env
# Fill in E2B_API_URL and CUBE_TEMPLATE_ID

cd hooks
./install.sh
```

The installer copies `cubesandbox-sandbox.js` into
`~/.config/opencode/plugins/`, drops a sibling `package.json` declaring
`"type": "module"` (so OpenCode's Bun runtime can resolve the ESM
`import` statements), and merges only an allow-listed subset of
`CUBE_*` settings into `~/.config/opencode/opencode.json`. LLM
provider API keys (`ANTHROPIC_API_KEY`, …) are never copied.

Restart OpenCode after install. The plugin's
`tool.execute.before` hook intercepts `bash` calls, spawns the host
`python3 sandbox_exec.py --cmd <quoted>` against the cached session
sandbox, and replaces the host-shell command with the sandbox output.
Other tools (`read`, `edit`, `write`, …) still run on the host as
usual — the plugin only intercepts `bash`, mirroring the bash-only
scope of the Claude Code PreToolUse hook.

Uninstall with `hooks/install.sh --uninstall`.

## Running the tests

```bash
cd examples/opencode-integration
python3 -m pip install pytest
python3 -m pytest tests -v
```

The suite is fully offline: no CubeSandbox cluster or LLM credentials
are needed. `sandbox_exec` and `mcp_server` are exercised via
`unittest.mock` against the e2b SDK; the env-var helpers are exercised
via `monkeypatch` so test order cannot leak state.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `opencode: command not found` in preflight | Template not rebuilt after CLI change | Rebuild the image, re-register the template |
| Permission prompt hangs the run | `--dangerously-skip-permissions` is missing on a run that touches files or commands | The default invocation passes it; use `opencode.json` permissions if you want a safer default |
| `unknown flag: --dangerously-skip-permissions` | OpenCode older than 1.17 | Rebuild the image with `--build-arg OPENCODE_VERSION=1.17.20`, or run with `OPENCODE_PERMISSION='{"*":"allow"}'` instead |
| Provider auth failure | Key not forwarded (direct) or missing inject rule (vault) | Pass `envs={...}` or fix the rule's `sni`/`host` |
| `403 Forbidden - CubeEgress` | Default-deny with no matching allow rule | Add the LLM host (and any extra hosts) to the rules |
| `Connection error` / TLS failure from OpenCode (vault) | OpenCode is a Node.js bundle that ignores the system CA store, so it won't trust the CubeEgress interception CA | The script sets `NODE_EXTRA_CA_CERTS` to the system bundle; override with `OPENCODE_NODE_EXTRA_CA_CERTS` if your CA lives elsewhere |
| Template creation stuck in `PULLING` | Registry unreachable from Cube nodes | Push to a registry the cluster can reach; supply auth if needed |
| Readiness probe timeout | Base image without envd | Ensure `FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| `pause()` / `connect()` errors | Platform too old for snapshots | Upgrade the CubeSandbox platform |
| `opencode run` errors with `model not found` | `OPENCODE_MODEL` does not match the provider | Set `OPENCODE_MODEL` explicitly per provider; OpenCode's `provider/model` shorthand is also accepted via `-m anthropic/claude-sonnet-4-6` |

## References

- Integration guide: [`docs/guide/integrations/opencode.md`](../../docs/guide/integrations/opencode.md)
- Snapshot / Clone / Rollback: [`docs/guide/snapshot-rollback-clone.md`](../../docs/guide/snapshot-rollback-clone.md)
- Network / egress policy examples: [`examples/network-policy`](../network-policy)
- Credential vault + egress control: [`docs/guide/security-proxy.md`](../../docs/guide/security-proxy.md)
- OpenCode CLI: <https://opencode.ai/>
- OpenCode docs: <https://opencode.ai/docs/>
