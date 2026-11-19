# CubeSandbox Rollback

This session has CubeSandbox rollback protection. Snapshots are taken
automatically before risky commands.

## Available commands
- `cubesandbox-rollback last` — Roll back to the most recent snapshot (default)
- `cubesandbox-rollback <N>` — Roll back to the snapshot at list index N
- `cubesandbox-rollback <snapshot-id>` — Roll back to a specific snapshot
- `cubesandbox-rollback list` — List snapshots: `[N] id time [kind] command`
- `cubesandbox-rollback checkpoint <name>` — Create a named milestone snapshot (never auto-evicted)
- `cubesandbox-rollback undo` — Undo the last rollback (invalidated by the next snapshot)
- `cubesandbox-rollback drop <last|N|snapshot-id>` — Delete a snapshot (local + backend)

## When to use
If a command (e.g. npm install, rm, git reset) breaks the environment,
use rollback to restore the previous state. To go back further, roll back
to an arbitrary checkpoint by index (`cubesandbox-rollback <N>`) or by
snapshot id. If you rolled back too far, `cubesandbox-rollback undo`
restores the pre-rollback state.

## Limitations
- The first Bash command of a session cannot be rolled back (no prior snapshot)
- Rollback **only** restores the sandbox filesystem state,
  not process memory or the Claude Code transcript
- Auto snapshots beyond the ring-buffer cap are evicted; use
  `cubesandbox-rollback checkpoint <name>` for milestones you need to keep
- Rolling back to an early snapshot deletes all later snapshots from the
  plugin's index and the backend, including named checkpoints — only the
  immediately preceding state is kept as the undo point
- This plugin must coexist with the cubesandbox-hook CubeSandbox hooks
  (`examples/claude-code-integration/hooks/`)
