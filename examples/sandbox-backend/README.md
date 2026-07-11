# Hook Sandbox Backend

Transparently redirect **every** Claude Code Bash command into an isolated
CubeSandbox MicroVM, using a `PreToolUse` hook instead of an MCP tool the AI
has to opt into.

See [`DESIGN.md`](./DESIGN.md) for the design rationale (inspired by
[RTK](https://github.com/rtk-ai/rtk)'s command-rewrite hook pattern).

## Why not the MCP approach?

[`./mcp_server.py`](./mcp_server.py) (included in this directory)
exposes `sandbox_run_command` etc. as MCP tools, but Claude Code has to
**choose** to call them -- it can always fall back to its native `Bash` tool
and run on the host instead.

| | MCP server (opt-in) | Hook backend (this example) |
|---|---|---|
| Interception | AI explicitly calls `sandbox_run_command` | `PreToolUse` hook rewrites every `Bash` call |
| Transparency | AI knows it's using a sandbox | AI is unaware -- sees normal Bash output |
| Coverage | Depends on the AI's choice | 100%, cannot be bypassed |
| Granularity | Fine (opt-in per call) | Coarse (all-or-nothing by default) |

## Architecture

```
Claude Code (host)
    │  Bash tool call: "npm test"
    ▼
PreToolUse hook ──► cubesandbox_rewrite.py
    │  rewrites tool_input.command to:
    │  cubesandbox-exec --session <id> --mount <cwd> "npm test"
    ▼
Claude Code executes the rewritten command with its normal Bash tool
    │
    ▼
cubesandbox-exec (cubesandbox_exec.py)
    │  reuses (or creates) one sandbox per Claude Code session
    ▼
CubeAPI ──► CubeMaster ──► Cubelet ──► KVM MicroVM
                                          │
                                          stdout/stderr/exit code
                                          returned to Claude Code
```

The AI never sees `cubesandbox-exec` in its own reasoning -- it issued
`npm test`, and it gets back exactly the output/exit code `npm test` would
have produced, except it ran inside a disposable MicroVM instead of on your
machine.

## Quick Start

```bash
cd examples/sandbox-backend
cp .env.example .env
# edit .env: set CUBE_API_URL and CUBE_TEMPLATE_ID (see cubemastercli tpl list)

./install.sh
```

`install.sh` will:
1. Copy the two scripts into `~/.claude/hooks/`
2. Install a `cubesandbox-exec` shim on `$PATH` (default `~/.local/bin`)
3. Register the `PreToolUse` hook in `~/.claude/settings.json` (idempotent)
4. `pip install` the `cubesandbox` SDK + `python-dotenv`

Restart Claude Code, then try:

```
> Run `ls -la` and tell me what's here
```

Claude Code will show a normal Bash tool call and normal-looking output --
but it actually ran inside a CubeSandbox MicroVM. Verify with:

```bash
cubesandbox-exec --session <your-session-id> "hostname; cat /proc/1/cgroup | head -1"
```

To remove everything:

```bash
./install.sh --uninstall
```

## Filesystem consistency

Claude Code's `Read` / `Write` / `Edit` tools operate on the **host**
filesystem (wherever `claude` was launched), while `Bash` commands now run
**inside the sandbox**. Without extra care those are two different
filesystems and `cd $PROJECT && cat file.py` inside the sandbox would fail.

This example solves it with CubeSandbox's [`host-mount`](../host-mount)
extension: the first time a sandbox is created for a session,
`cubesandbox_exec.py` bind-mounts the project directory (the hook's `cwd`
field) **read-only at the same path** inside the sandbox. Sandbox-side Bash
commands can safely read the same source files as host-side tools, but host
tools remain the sole writer for project files.

This requires the project directory to be under one of CubeMaster's
`allowed_host_mount_prefixes` (default: `/data/shared/`):

```yaml
# CubeMaster config
extra_conf:
  allowed_host_mount_prefixes:
    - "/data/shared/"
    - "/home/you/projects/"   # add your project root
```

If the path isn't allowed, `cubesandbox-exec` **does not fail** -- it logs a
warning and falls back to a sandbox with its own isolated filesystem. Bash
commands still run safely sandboxed, they just won't see files created via
Read/Write/Edit (and vice versa). For a fully consistent view, add your
project root to `allowed_host_mount_prefixes` or keep it under
`/data/shared/`.

## Session & shell state

- **One sandbox per Claude Code session.** `cubesandbox_rewrite.py` passes
  `session_id` from the hook payload; `cubesandbox_exec.py` caches the
  resulting `sandbox_id` in `~/.cache/cubesandbox-hook/<session_id>.json`
  and reconnects to it on every subsequent Bash call, so you don't pay a
  cold-start cost per command.
- **`cd` / `export` persist across calls.** Each `commands.run()` invocation
  is otherwise stateless, so before running your command the wrapper
  restores `$PWD` and exported vars from a randomly named, owner-only
  directory inside the sandbox's `/tmp`, and updates them afterwards. From
  Claude Code's point of view this behaves like one
  continuous shell session, the same as its native (non-sandboxed) Bash
  tool.
- **Manual reset.** `cubesandbox-exec --reset --session <id>` kills the
  cached sandbox and clears its state, forcing a clean sandbox on the next
  call (handy between unrelated tasks in the same Claude Code session).

## Files

```
sandbox-backend/
├── DESIGN.md               # Design notes / rationale (RTK-inspired)
├── cubesandbox_exec.py     # Sandbox exec CLI (session cache, state persistence, host-mount)
├── cubesandbox_rewrite.py  # PreToolUse hook: rewrites Bash tool_input.command
├── sandbox_exec.py         # Standalone CLI (e2b SDK, opt-in)
├── mcp_server.py            # MCP server (opt-in sandbox tools for Claude Code)
├── install.sh              # Installs hooks + settings.json registration (+ --uninstall)
├── test_cubesandbox_exec.py # Session cache and sandbox lifecycle tests
├── test_cubesandbox_rewrite.py # Hook command-rewrite safety tests
├── test_mcp_server.py       # MCP framing and tool dispatch tests
├── requirements.txt
├── .env.example
└── README.md                # This file
```

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Bash output unchanged, no sandbox created | Hook not registered / Claude Code not restarted | Check `~/.claude/settings.json`, restart `claude` |
| `CUBE_TEMPLATE_ID is not set` | `.env` missing/not loaded | `cp .env.example .env` and fill in values; `cubesandbox-exec` loads `.env` from its own directory |
| `hostPath ... is not within an allowed mount prefix` (warning) | Project dir not under `allowed_host_mount_prefixes` | Add it to CubeMaster config, or ignore -- sandbox still works, just without shared FS |
| Commands run but `cd`/`export` don't stick between calls | The session's private state directory under `/tmp` is not writable in the template image | Ensure the template's `/tmp` is writable by `CUBE_SANDBOX_USER` |
| Every command spins up a brand-new sandbox | `session_id` missing from hook payload, or `~/.cache/cubesandbox-hook` not writable | Check hook stdin payload has `session_id`; check `CUBE_HOOK_STATE_DIR` permissions |
| `SandboxNotFoundError` / stale sandbox | Sandbox TTL (`CUBE_SANDBOX_TIMEOUT`) expired between calls | Increase `CUBE_SANDBOX_TIMEOUT`, or accept the automatic recreate (some cwd/env state is lost) |

## Related

- [`../sandboxed-claude/`](../sandboxed-claude/) -- run Claude Code itself
  inside a sandbox (pattern A)
- [`../host-mount/`](../host-mount/) -- the host-mount mechanism used here
- Claude Code Hooks reference: https://code.claude.com/docs/en/hooks
