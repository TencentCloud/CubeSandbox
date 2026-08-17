---
title: Claude Code Integration Guide
author: shsaihdsaiudh
date: 2026-07-06
tags:
  - integration
  - claude-code
  - coding-agent
lang: en-US
---

# Claude Code

[Claude Code](https://docs.anthropic.com/en/docs/claude-code) is a terminal-based AI coding agent developed by Anthropic. It runs commands, edits files, and executes code in your terminal.

This guide shows how to keep Claude Code running **on your host** while transparently redirecting **every Bash command it runs** into an isolated CubeSandbox MicroVM, using a `PreToolUse` hook. The model never sees the sandbox layer, and no prompt or workflow changes are required.

## Why a hook

MCP- or SDK-based sandboxing depends on the agent *choosing* a sandbox tool; a plain `Bash` tool call still lands on the host. A `PreToolUse` hook intercepts the tool call itself, so Bash isolation is **transparent and complete** — there is no path for a command to skip it.

## Architecture

```
Claude Code (host)
    ├── Read / Write / Edit ─────────────► host project files
    │
    └── Bash ──► PreToolUse hook ──► cubesandbox_exec ──► CubeAPI ──► MicroVM
                 (cubesandbox_rewrite.py)                 (:3000)     └─ reusable per session
```

Only the `Bash` tool is redirected. `Read`, `Write`, and `Edit` keep operating on host files, so Claude Code edits your project locally while its shell commands run in a throwaway kernel/filesystem/network.

## Integration Target and Version

| Component | Version |
|---|---|
| Claude Code | Any release with `PreToolUse` hook support |
| cubesandbox Python SDK | Installed via `requirements.txt` |
| Python | 3.9+ on the host running Claude Code |

## Prerequisites

- Running [CubeSandbox deployment](/guide/quickstart) with CubeAPI reachable (e.g. `http://127.0.0.1:3000`)
- Python 3.9+ on the host running Claude Code
- A CubeSandbox template to launch sandboxes from (see [Template](#template) below)

## Template

The hook runs arbitrary Bash commands in the sandbox, so the template only needs to be a general-purpose code sandbox — **Claude Code itself does not need to be installed inside it**. The stock `sandbox-code` image works:

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 2G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
```

Use the resulting template ID (`STATUS: READY` in `cubemastercli tpl list`) as `CUBE_TEMPLATE_ID` below.

## Install the hook

From the example directory:

```bash
python3 -m pip install -r requirements.txt
cp .env.example .env
# Set CUBE_API_URL and CUBE_TEMPLATE_ID in .env

cd hooks
./install.sh
```

Restart Claude Code after installation. The installer merges a `Bash` matcher into `~/.claude/settings.json` **without** replacing your other settings, and writes only whitelisted `CUBE_*` values to the hook configuration — it never copies LLM provider API keys.

Now use Claude Code normally: every Bash command it issues executes inside a MicroVM.

## How it works

1. **`cubesandbox_rewrite.py`** (the hook) receives the `PreToolUse` payload. For a `Bash` call it rewrites `tool_input.command` to an invocation of the executor with the original command as a single `shlex`-quoted argument, and returns it via `updatedInput`. Nothing in the original command can execute on the host. Non-`Bash` tools pass through. Every Bash command is wrapped unconditionally; if an already-wrapped executor invocation is fed back through the hook, the nested invocation simply fails inside the sandbox (the host hook path does not exist there), never on the host.

2. **`cubesandbox_exec.py`** (the executor) reuses one sandbox per Claude Code `session_id` (mapping stored under `~/.cache/cubesandbox-hook/`, guarded by a per-session file lock), replays the persisted working directory and exported environment, runs the command in the MicroVM, and returns stdout/stderr and the exit code after the command finishes (buffered, not streamed).

## Host project mount

On the first call in a session, the hook can request a **read-only** mount of Claude Code's project directory at the same path in the sandbox. Add the project root to CubeMaster's host-mount allowlist:

```yaml
extra_conf:
  allowed_host_mount_prefixes:
    - "/data/shared/"
    - "/home/you/projects/"
```

`hostPath` is resolved on the scheduled Cubelet node, not on the machine running Claude Code. The shared view works only when Claude Code is co-located with that Cubelet, or the project already lives at the identical absolute path on every eligible Cubelet. The hook does not upload or synchronize a local project to a remote deployment; do not allowlist a client-only path that could refer to unrelated Cubelet data.

This hook covers the `Bash` tool only. `Read`, `Write`, and `Edit` still access the host, and the mount is read-only so sandbox commands cannot write project files or build artifacts. If CubeMaster rejects the mount, execution falls back to a sandbox without it; Bash stays isolated but loses filesystem consistency with the host-side tools.

## Security properties

- **Fail-closed** — if the hook cannot rewrite a call safely, it exits non-zero and blocks the command rather than letting it run on the host.
- **Injection-safe** — the original command is passed as one `shlex`-quoted argument, so shell metacharacters and newlines can never break out onto the host.
- **Unconditional wrapping** — every Bash call is rewritten; an already-wrapped executor invocation fed back through the hook simply fails inside the sandbox (the host hook path does not exist there), never on the host.
- **Auto-approval** — the hook answers `permissionDecision: "allow"` for rewritten Bash calls, so Claude Code's per-command approval prompt is suppressed; use `--permission-mode` / hooks policy accordingly.
- **No credential leakage** — the installer copies only whitelisted `CUBE_*` values into the hook config.

## Reset and uninstall

```bash
# Drop the sandbox bound to a session (fresh sandbox next call)
python3 ~/.claude/hooks/cubesandbox_exec.py --reset --session <session-id>

# Remove the hook from ~/.claude/settings.json
cd hooks
./install.sh --uninstall
```

## Key Code Snippets

### Hook matcher in `~/.claude/settings.json`

The installer merges a `Bash` matcher group like this into your settings (with your absolute home path):

```json
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "/home/you/.claude/hooks/cubesandbox_rewrite.py || exit 2"
          }
        ]
      }
    ]
  }
}
```

### Manual hook test

```bash
echo '{"tool_name":"Bash","cwd":"/tmp","session_id":"t","tool_input":{"command":"whoami"}}' \
  | python3 ~/.claude/hooks/cubesandbox_rewrite.py
```

## Caveats

- **Read/Write/Edit stay on the host.** Only `Bash` tool calls are redirected; Claude Code's file edits keep hitting your host project files.
- **Serialized Bash within a session.** Concurrent Bash calls in one session are serialized through a per-session lock — they run one at a time, not in parallel.
- **Buffered output.** stdout/stderr are returned only after the command finishes; long-running commands show no incremental output.
- **Auto-approval.** The hook answers `permissionDecision: "allow"` for rewritten Bash calls, so Claude Code's per-command approval prompt is suppressed — set `--permission-mode` / hooks policy accordingly.
- **Persisted environment is scrubbed.** Exported variables persist between commands, but `BASH_ENV`, `ENV`, `LD_PRELOAD`, and `PROMPT_COMMAND` are scrubbed from the persisted environment.

## Troubleshooting

### Bash commands still run on the host

The hook is not registered, or Claude Code was not restarted. Verify `~/.claude/settings.json` has a `PreToolUse` `Bash` matcher pointing at `~/.claude/hooks/cubesandbox_rewrite.py`, then restart Claude Code. You can test the hook directly:

```bash
echo '{"tool_name":"Bash","cwd":"/tmp","session_id":"t","tool_input":{"command":"whoami"}}' \
  | python3 ~/.claude/hooks/cubesandbox_rewrite.py
```

### `the cubesandbox SDK is required`

Install dependencies into the Python environment Claude Code uses: `pip install -r requirements.txt`.

### `CUBE_TEMPLATE_ID is not set` / `Template not found`

Set `CUBE_TEMPLATE_ID` in `.env` to a `READY` template (`cubemastercli tpl list`), then re-run `hooks/install.sh`.

### Host mount rejected

The path is not in `allowed_host_mount_prefixes`, or does not exist on the scheduled Cubelet. Add the prefix to CubeMaster `extra_conf`, ensure co-location, or accept the no-mount fallback.

## Example Repository

See the full runnable example at [`examples/claude-code-integration/`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/claude-code-integration), which includes:

- `hooks/cubesandbox_rewrite.py` — the `PreToolUse` hook that rewrites Bash calls
- `hooks/cubesandbox_exec.py` — the executor: per-session MicroVM reuse and shell-state persistence
- `hooks/install.sh` — idempotent install / uninstall
- `tests/` — tests for the hook rewrite, executor, and install lifecycle

## References

- Claude Code hooks documentation: <https://docs.anthropic.com/en/docs/claude-code/hooks>
- Runnable example: [`examples/claude-code-integration/`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/claude-code-integration)
- Project quickstart: [Quickstart](/guide/quickstart)
