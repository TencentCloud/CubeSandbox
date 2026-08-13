# Claude Code + CubeSandbox — Transparent Bash Isolation

[中文文档](./README_zh.md)

Keep [Claude Code](https://docs.anthropic.com/en/docs/claude-code) running on your
host, but transparently redirect **every Bash command it runs** into an isolated
**CubeSandbox** MicroVM. A `PreToolUse` hook rewrites each `Bash` tool call before
it executes — the model never sees the sandbox layer, and no workflow changes are
needed.

```
Claude Code (host)
    ├── Read / Write / Edit ─────────────► host project files
    │
    └── Bash ──► PreToolUse hook ──► cubesandbox_exec ──► CubeAPI ──► MicroVM
                 (cubesandbox_rewrite.py)                 (:3000)     └─ reusable per session
```

Only the `Bash` tool is redirected. `Read`, `Write`, and `Edit` keep operating on
host files, so Claude Code edits your project locally while its shell commands run
in a throwaway kernel/filesystem/network.

## Why a hook

MCP- or SDK-based sandboxing relies on the agent *choosing* a sandbox tool; a plain
`Bash` call still lands on the host. A `PreToolUse` hook closes that gap: it
intercepts the tool call itself, so isolation is **transparent and complete** for
Bash — there is no path for a command to skip it.

| Property | Behaviour |
|----------|-----------|
| **Transparent** | The model issues normal `Bash` calls; the hook rewrites them. No prompt or tool changes. |
| **Per-session sandbox** | One MicroVM is reused per Claude Code `session_id`, so a session's commands share state. |
| **Shell state persists** | `cd` and exported variables carry across Bash calls within a session. |
| **Read-only host mount** | The session's first call can mount the project at the same path, read-only, so sandbox commands can inspect (not mutate) host files. |
| **Fail-closed** | If the hook cannot rewrite a call safely, it exits non-zero and blocks the command rather than letting it run on the host. |
| **Injection-safe** | The original command is passed as a single `shlex`-quoted argument, so shell metacharacters and newlines can never break out onto the host. |
| **Unconditional wrapping** | Every Bash call is rewritten; an already-wrapped executor invocation fed back through the hook simply fails inside the sandbox, never on the host. |
| **Auto-approval** | The hook answers `permissionDecision: "allow"` for rewritten Bash calls, so Claude Code's per-command approval prompt is suppressed; use `--permission-mode` / hooks policy accordingly. |

## Prerequisites

- A running CubeSandbox deployment (CubeAPI reachable, e.g. `http://127.0.0.1:3000`)
- Python 3.9+ on the host running Claude Code
- A CubeSandbox template to launch sandboxes from (`cubemastercli tpl list`)

## Quick Start

### 1 — Install dependencies and configure

```bash
python3 -m pip install -r requirements.txt
cp .env.example .env
# Edit .env: set CUBE_API_URL and CUBE_TEMPLATE_ID
```

### 2 — Install the hook

```bash
cd hooks
./install.sh
```

The installer registers the hook in `~/.claude/settings.json` **without** replacing
your other settings, and copies only the whitelisted `CUBE_*` values from `../.env`
into the installed hook config. Restart Claude Code so it picks up the hook.

### 3 — Use Claude Code normally

Just run Claude Code. Every Bash command it issues now executes inside a MicroVM:

```
> Run `uname -a && whoami` and tell me where it ran
```

The command runs in the sandbox — a different kernel and a sandbox user — while
Claude Code's file edits stay on your host.

## How it works

1. **`cubesandbox_rewrite.py`** (the hook) receives the `PreToolUse` JSON payload.
   For a `Bash` call it rewrites `tool_input.command` to:

   ```
   <python> <hooks>/cubesandbox_exec.py --session=<id> --mount=<cwd> --timeout=<s> -- <original command>
   ```

   and returns it via `updatedInput`. The original command is a single quoted
   argument, so nothing in it can execute on the host. Non-`Bash` tools pass
   through untouched. The hook wraps every Bash command unconditionally; an
   already-wrapped executor invocation fed back through it simply fails inside
   the sandbox, never on the host.

2. **`cubesandbox_exec.py`** (the executor) reuses one sandbox per `session_id`
   (mapping stored under `~/.cache/cubesandbox-hook/`, guarded by a per-session
   file lock), replays the persisted working directory and environment, runs the
   command in the MicroVM, and returns stdout/stderr and the exit code after the
   command finishes (buffered, not streamed). Concurrent Bash calls within one
   session are serialized through the per-session lock — they run one at a time,
   not in parallel.

## Host project mount

The first Bash call in a session can mount Claude Code's project directory at the
same absolute path inside the sandbox, **read-only** — sandbox commands can read
host project files but cannot edit them or write build artifacts there.

`hostPath` is resolved on the scheduled Cubelet node, not on the machine running
Claude Code. The shared view therefore works only when Claude Code is co-located
with that Cubelet, or the project already lives at the identical absolute path on
every eligible Cubelet. The hook never uploads or syncs a local project to a remote
deployment — do not allowlist a client-only path that could point at unrelated data
on a Cubelet.

The project path must be allowed by CubeMaster:

```yaml
extra_conf:
  allowed_host_mount_prefixes:
    - "/data/shared/"
    - "/home/you/projects/"
```

If the mount is rejected, execution falls back to an isolated sandbox without the
mount: Bash stays isolated, but no longer shares a file view with Claude Code's
host-side file tools.

## Resetting and uninstalling

```bash
# Drop the sandbox bound to a session (starts fresh next call)
python3 ~/.claude/hooks/cubesandbox_exec.py --reset --session <session-id>

# Remove the hook from ~/.claude/settings.json
cd hooks
./install.sh --uninstall
```

## Directory Structure

```
claude-code-integration/
├── hooks/
│   ├── cubesandbox_rewrite.py   # PreToolUse hook: rewrite Bash → sandbox exec
│   ├── cubesandbox_exec.py      # Executor: per-session MicroVM + state persistence
│   └── install.sh               # Idempotent install / uninstall
├── tests/
│   ├── conftest.py
│   ├── test_cubesandbox_rewrite.py
│   ├── test_cubesandbox_exec.py
│   └── test_hook_install.py
├── requirements.txt
├── .env.example
├── TROUBLESHOOTING.md
├── README.md                    # This file
└── README_zh.md                 # Chinese documentation
```

## Tests

```bash
python3 -m pip install -r requirements.txt pytest
pytest tests
```

## Troubleshooting

| Symptom | Likely Cause | Fix |
|---------|-------------|-----|
| Bash commands still run on the host | Hook not registered / Claude Code not restarted | Re-run `hooks/install.sh`, restart Claude Code |
| `CUBE_TEMPLATE_ID is not set` | Missing template in `.env` | Set `CUBE_TEMPLATE_ID` (see `cubemastercli tpl list`), then re-run `hooks/install.sh` |
| `the cubesandbox SDK is required` | Dependencies not installed | `pip install -r requirements.txt` |
| `Template not found` | Wrong template ID | Check `cubemastercli tpl list` |
| Host mount rejected (warning) | Path not in `allowed_host_mount_prefixes` | Add the prefix to CubeMaster `extra_conf`, or accept the no-mount fallback |

See [TROUBLESHOOTING.md (中文)](./TROUBLESHOOTING.md) for more.
