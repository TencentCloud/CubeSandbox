# CubeSandbox Rollback for Claude Code

Snapshot and rollback protection for Claude Code sessions that run inside
CubeSandbox MicroVMs. Before risky commands a sandbox snapshot is taken
automatically; if the command breaks the environment, the agent runs
`cubesandbox-rollback last` to restore the previous filesystem state. This
example is a companion layer to the CubeSandbox Claude Code hooks
(`examples/claude-code-integration/hooks/`, below: **cubesandbox-hook**
(PR #765)), which redirect every Bash tool call into an isolated sandbox.

## Why

Claude Code's `/rewind` restores the conversation and file edits but not Bash
side effects in a sandbox: a destructive `rm`, `git reset`, or `npm install`
cannot be unwound. This plugin snapshots before risky commands and lets the
agent roll back to the last good state on demand.

## Architecture

- **Companion layer to cubesandbox-hook.** It never imports cubesandbox-hook
  modules and never writes cubesandbox-hook's state. It only *reads*
  `~/.cache/cubesandbox-hook/<sha256(session_id)>.json` (fields `sandbox_id`,
  `mount`, `state_token`) to find the session's sandbox. If cubesandbox-hook
  is absent or the sandbox does not exist yet, the hooks degrade gracefully
  (skip / no-op). The digest function is duplicated to match cubesandbox-hook
  exactly, so both plugins address the same session.
- **Parallel-hook safety.** Claude Code runs multiple PreToolUse hooks in
  parallel; each receives the original tool input, `deny` has highest
  precedence, and `updatedInput` is not seen by other hooks. Our hooks return
  only `{}` (no opinion) or `deny` — never `updatedInput` — so they coexist
  with cubesandbox-hook's rewrite hook without fighting over the command.
- **Deny channel.** Verified with live sessions: Claude Code renders
  `permissionDecisionReason` to the agent but not `hookSpecificOutput.stdout`.
  All user-facing output (rollback results, snapshot lists, errors) is carried
  in `permissionDecisionReason`.
- **Fail-open vs fail-closed.** The snapshot hook is fail-open (any error
  returns `{}` and the command proceeds). The rollback hook is fail-closed
  (errors deny the command with a message).

## Files

| File | Role |
|---|---|
| `cubesandbox_lib.py` | Shared utilities: session digest (must match cubesandbox-hook), atomic state I/O with `flock`, sandbox client, loads `~/.claude/hooks/cubesandbox.env` into the hook process at startup (without overriding existing env vars), pure helpers for snapshot find/prune/evict, orphan cleanup |
| `cubesandbox_risky.py` | Risk classifier. AST-based (bashlex): parses the whole command with bash's own grammar instead of regex. Unwraps `sudo`/`env`/`nohup`/`nice`/`timeout`/`exec` prefixes and leading `VAR=value` assignments, evaluates every pipeline/compound segment and nested `$()`/`<( )` substitution independently, detects destructive redirects (`/dev/null` and fd refs `&1`/`&2` excluded), flags `curl`/`wget` `-o`/`-O`/`--output`, `curl|bash`-style download-to-interpreter pipes, and fork bombs. Unparseable commands are treated as risky (fail-safe) |
| `cubesandbox_snapshot.py` | PreToolUse/Bash: risky command → `create_snapshot()` → record it → ring-buffer eviction of auto snapshots and undo-point invalidation → return `{}` (fail-open) |
| `cubesandbox_rollback.py` | PreToolUse/Bash: sentinel command → `deny` + multi-point rollback (`last` / index / snapshot id), `checkpoint`, `undo`, `list`, `drop` (fail-closed) |
| `cubesandbox_session.py` | SessionStart: injects rollback awareness into the agent's context |
| `cubesandbox_poststart.py` | PostToolUse/Bash: `<initial>` baseline snapshot (`kind: baseline`, never auto-evicted) after the first command |
| `install_rollback.sh` | Idempotent install/uninstall. Registers hooks in the project's `.claude/settings.json` and installs `SKILL.md` as a skill. Never touches user-level `~/.claude/settings.json` (cubesandbox-hook's domain) |
| `SKILL.md` | Agent-facing command reference (installed to `.claude/skills/cubesandbox-rollback/`) |
| `.env.example` | Optional `CUBE_ROLLBACK_SAFE` whitelist and `CUBE_ROLLBACK_MAX_AUTO_SNAPSHOTS` ring-buffer cap; API/template config is read from `~/.claude/hooks/cubesandbox.env` at hook startup (cubesandbox-hook's env file), or from the shell environment |

Own state lives in `~/.cache/cubesandbox-rollback/<sha256(session_id)>.json`
(written atomically, serialized with `flock`); if cubesandbox-hook's state
file disappears, the rollback state is treated as orphaned and cleaned up.

## Installation

Prerequisites: a running CubeSandbox deployment, the cubesandbox-hook hooks
installed, and `claude` launched **from the project root** (project-level
`settings.json` and the skill only load there).

```bash
# from the project you want protected
bash examples/claude-code-integration/rollback/install_rollback.sh

# remove again
bash examples/claude-code-integration/rollback/install_rollback.sh --uninstall
```

The installer copies the hook scripts to `~/.claude/hooks/rollback/`, registers
them in `<project>/.claude/settings.json`, and creates the skill at
`.claude/skills/cubesandbox-rollback/`. It is idempotent — re-running it does
not duplicate hooks.

## Agent commands

Snapshots are taken automatically before risky commands. The agent (or you)
can then use:

| Command | Effect |
|---|---|
| `cubesandbox-rollback last` | Roll back to the most recent snapshot (default) |
| `cubesandbox-rollback <N>` | Roll back to the snapshot at list index N |
| `cubesandbox-rollback <snapshot-id>` | Roll back to a specific snapshot |
| `cubesandbox-rollback list` | List snapshots: `[N] id time [kind] command` |
| `cubesandbox-rollback checkpoint <name>` | Explicit milestone snapshot, never auto-evicted |
| `cubesandbox-rollback undo` | Undo the last rollback (invalidated once a new snapshot is taken) |
| `cubesandbox-rollback drop <last\|N\|snapshot-id>` | Delete a snapshot (local + backend) |

Snapshots come in three kinds: `baseline` (taken at session start, never
auto-evicted), `auto` (taken before risky commands, kept in a ring buffer
capped by `CUBE_ROLLBACK_MAX_AUTO_SNAPSHOTS`), and `checkpoint` (user-named,
never auto-evicted).

### Snapshot lifecycle

Auto snapshots are kept in a ring buffer of `CUBE_ROLLBACK_MAX_AUTO_SNAPSHOTS`
(default 30, env-configurable); when the cap is exceeded, the oldest auto
snapshot is deleted from the backend. Baseline snapshots and explicit
checkpoints are never auto-evicted by the ring buffer — drop them manually.

Rolling back to an early snapshot deletes all later snapshots from the
plugin's index AND the backend — including named checkpoints; only the
immediately preceding state is preserved as the hidden undo point, so
`cubesandbox-rollback undo` restores the pre-rollback state. The discarded
list is shown in the rollback message. The undo point is deleted as soon as a
new snapshot is taken (either auto or checkpoint).

### Risk classification

The classifier parses each command with [bashlex](https://pypi.org/project/bashlex/)
(a Python port of bash's own parser) and evaluates every segment of the AST —
this is the same approach as OpenHands SDK, toad, and connectonion. Regex
parsing is deliberately not used: regex-based classifiers are trivially
bypassed by prefixes, quoted targets, or unspaced operators.

Snapshot-triggering commands include `rm`, `rmdir`, `chmod`, `chown`, `mv`,
`dd`, `shred`, `mkfs`, `fdisk`, `apt*`, `dnf`, `yum`, `brew`, `snap`; risky
subcommands such as `git reset`/`git clean`, `npm install`/`uninstall`/`update`,
`pip install`/`uninstall`, `yarn add`/`remove`, `cargo install`, `go install`,
`make install`; `curl`/`wget` writing via `-o`/`-O`/`--output`; `curl|bash`-
style download-to-interpreter pipes; and any redirect to a real file
(including quoted and unspaced targets like `> "out file.txt"` or `echo x>f`).
The following are *not* snapshot triggers:

- wrapper prefixes (`sudo`/`env`/`nohup`/`nice`/`timeout`/`exec`/`setsid`)
  and leading `VAR=value` assignments — the real command is extracted from
  behind them, so `sudo git reset --hard` and `DEBUG=1 npm install` trigger;
- redirects to `/dev/null` or file descriptors (`2>&1`, `>&1`, `>/dev/null`);
- the `cubesandbox-rollback` sentinel itself.

**Fail-safe**: commands bashlex cannot parse (arithmetic expansion
`$((1 > 0))`, `[[ ]]` tests, fork bombs) are treated as risky rather than
silently safe — an extra snapshot is harmless, a missed one is not.

To whitelist commands that must never trigger a snapshot, export
`CUBE_ROLLBACK_SAFE` (comma-separated), e.g.:

```bash
export CUBE_ROLLBACK_SAFE=echo,cat,ls,pwd,cd,git,make
```

## Verified on real hardware

Tested end-to-end on a bare-metal CubeSandbox v0.6.0 deployment (11 services,
KVM) with real Claude Code 2.1.142 sessions:

```
1. touch /tmp/demo.txt            # file created
2. rm /tmp/demo.txt               # deleted; pre-snapshot taken automatically
3. cubesandbox-rollback last      # "Rollback complete. State restored to snapshot `snap-xxx`"
4. ls /tmp/demo.txt               # file restored, confirmed inside the sandbox
```

Cross-verified via the CubeAPI: the snapshot id reported by Claude Code appears
both in `GET /cubeapi/v1/snapshots` (status `READY`) and in the sandbox's
`metadata.cube.master.runtime.restore.snapshot.id` — three-way alignment
between the agent output, the backend snapshot, and the restored sandbox.

## Known limitations

1. **Host-mount and snapshot are mutually exclusive (platform constraint).**
   Cubelet's `validateCommitSandboxTarget` rejects `CommitSandbox` for any
   sandbox with host-path volume mounts (`has volume mounts without persisted
   volume sources`). Adding the project dir to CubeMaster's
   `allowed_host_mount_prefixes` gives the read-only host mount but breaks
   snapshot/rollback with error 130400. Keep the default prefix list (only
   `/data/shared/`) — no host mount, snapshots work.
2. **Rollback on long-active sandboxes can hit a TAP `EBUSY` race.** Rollback
   restarts the VM and rebuilds its virtio-net TAP devices; on sandboxes with
   persistent exec connections this can fail with
   `CreateVirtioNet → Device or resource busy`. Retry after a few seconds or
   start a fresh session (known platform race; related fix in v0.3.1, #442).
3. **The first Bash command of a session has no prior snapshot.** The
   `<initial>` baseline is taken *after* the first command, so the very first
   command cannot be rolled back.
4. **PostToolUse is skipped for failed tool calls.** Claude Code does not fire
   PostToolUse hooks when a Bash tool call returns an error, so the baseline is
   only created when the first command succeeds. If it fails,
   `cubesandbox-rollback last` reports "No snapshots available to roll back."
5. **Project-level hooks only apply when `claude` runs from the project root.**
   Run `claude` in the directory that contains `.claude/settings.json`.

## Contributing

This README documents the example itself. Per `CONTRIBUTING.md`, a community
integration guide may also be submitted under `docs/guide/integrations/` (and
`docs/zh/guide/integrations/`), following its frontmatter and bilingual rules.
