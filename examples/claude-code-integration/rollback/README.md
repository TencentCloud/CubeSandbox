# CubeSandbox Rollback for Claude Code

Snapshot and rollback protection for Claude Code sessions that run inside
CubeSandbox MicroVMs. Before risky commands a sandbox snapshot is taken
automatically; if the command breaks the environment, the agent runs
`cubesandbox-rollback last` to restore the previous filesystem state. This
example is a companion layer to the CubeSandbox Claude Code hooks
(`examples/claude-code-integration/hooks/`, below: **#765**), which redirect
every Bash tool call into an isolated sandbox.

## Why

Claude Code's `/rewind` restores the conversation and file edits but not Bash
side effects in a sandbox: a destructive `rm`, `git reset`, or `npm install`
cannot be unwound. This plugin snapshots before risky commands and lets the
agent roll back to the last good state on demand.

## Architecture

- **Companion layer to #765.** It never imports #765 modules and never writes
  #765's state. It only *reads* `~/.cache/cubesandbox-hook/<sha256(session_id)>.json`
  (fields `sandbox_id`, `mount`, `state_token`) to find the session's sandbox.
  If #765 is absent or the sandbox does not exist yet, the hooks degrade
  gracefully (skip / no-op). The digest function is duplicated to match #765
  exactly, so both plugins address the same session.
- **Parallel-hook safety.** Claude Code runs multiple PreToolUse hooks in
  parallel; each receives the original tool input, `deny` has highest
  precedence, and `updatedInput` is not seen by other hooks. Our hooks return
  only `{}` (no opinion) or `deny` — never `updatedInput` — so they coexist
  with #765's rewrite hook without fighting over the command.
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
| `cubesandbox_lib.py` | Shared utilities: session digest (must match #765), atomic state I/O with `flock`, sandbox client, config-hash staleness check, orphan cleanup |
| `cubesandbox_risky.py` | Risk classifier. Strict: compound commands are split per segment (quote-aware, `&&`/`\|\|`/`;`/`\|`), `sudo`/`env`/`nohup`/`nice` stripped, destructive redirects detected (`/dev/null`, `/dev/*` and fd refs `&1`/`&2` excluded), `curl`/`wget` `-o`/`-O`/`--output` flagged |
| `cubesandbox_snapshot.py` | PreToolUse/Bash: risky command → `create_snapshot()` → record it → return `{}` (fail-open) |
| `cubesandbox_rollback.py` | PreToolUse/Bash: sentinel command → `deny` + rollback / list / drop (fail-closed) |
| `cubesandbox_session.py` | SessionStart: injects rollback awareness into the agent's context |
| `cubesandbox_poststart.py` | PostToolUse/Bash: `<initial>` baseline snapshot after the first command |
| `install_rollback.sh` | Idempotent install/uninstall. Registers hooks in the project's `.claude/settings.json` and installs `SKILL.md` as a skill. Never touches user-level `~/.claude/settings.json` (#765's domain) |
| `SKILL.md` | Agent-facing command reference (installed to `.claude/skills/cubesandbox-rollback/`) |
| `.env.example` | Optional `CUBE_ROLLBACK_SAFE` whitelist; API/template config is shared from #765's `.env` |

Own state lives in `~/.cache/cubesandbox-rollback/<sha256(session_id)>.json`
(written atomically, serialized with `flock`); if #765's state file disappears,
the rollback state is treated as orphaned and cleaned up.

## Installation

Prerequisites: a running CubeSandbox deployment, the #765 hooks installed, and
`claude` launched **from the project root** (project-level `settings.json` and
the skill only load there).

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
| `cubesandbox-rollback last` | Restore the sandbox to the most recent snapshot |
| `cubesandbox-rollback list` | List available snapshots (index, id, time, command) |
| `cubesandbox-rollback drop <snapshot-id>` | Delete a snapshot |

### Risk classification

Snapshot-triggering commands include `rm`, `rmdir`, `chmod`, `chown`, `mv`,
`dd`, `shred`, `mkfs`, `fdisk`, `apt*`, `dnf`, `yum`, `brew`, `snap`; risky
subcommands such as `git reset`/`git clean`, `npm install`/`uninstall`/`update`,
`pip install`/`uninstall`, `yarn add`/`remove`, `cargo install`, `go install`,
`make install`; `curl`/`wget` writing via `-o`/`-O`/`--output`; and any
redirect to a real file. Redirects to `/dev/null` or file descriptors (`2>&1`)
are excluded, and the `cubesandbox-rollback` sentinel itself is never
snapshotted.

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
