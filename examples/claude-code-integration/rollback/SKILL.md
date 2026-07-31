# CubeSandbox Rollback

This session has CubeSandbox rollback protection. Snapshots are taken
automatically before risky commands.

## Available commands
- `cubesandbox-rollback last` — Restore sandbox to the last snapshot
- `cubesandbox-rollback list` — List all available snapshots
- `cubesandbox-rollback drop <id>` — Delete a snapshot

## When to use
If a command (e.g. npm install, rm, git reset) breaks the environment,
use rollback to restore the previous state.

## Limitations
- The first Bash command of a session cannot be rolled back (no prior snapshot)
- Rollback **only** restores the sandbox filesystem state,
  not process memory or the Claude Code transcript
- This plugin must coexist with the #765 CubeSandbox hooks
  (`examples/claude-code-integration/hooks/`)
