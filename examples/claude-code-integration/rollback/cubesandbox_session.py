#!/usr/bin/env python3
"""SessionStart hook: inject rollback awareness into agent context."""

from __future__ import annotations

import json
import sys

MESSAGE = (
    "CubeSandbox Rollback is active. A snapshot is taken automatically "
    "before every risky command (rm, npm install, git reset, etc.), and a "
    "baseline snapshot marks the session start. Run `cubesandbox-rollback "
    "list` to see all snapshots; roll back to any of them with "
    "`cubesandbox-rollback last`, `cubesandbox-rollback <N>`, or "
    "`cubesandbox-rollback <snapshot-id>`. Use `cubesandbox-rollback "
    "checkpoint <name>` to save a named milestone before a refactor. "
    "After a rollback, `cubesandbox-rollback undo` restores the previous "
    "state until a new snapshot is taken, and `cubesandbox-rollback drop "
    "<last|N|snapshot-id>` deletes a snapshot."
)


def main() -> int:
    try:
        payload = json.load(sys.stdin)
    except (json.JSONDecodeError, UnicodeDecodeError):
        print("{}")
        return 0

    if payload.get("hook_event_name") != "SessionStart":
        print("{}")
        return 0

    output = {
        "hookSpecificOutput": {
            "hookEventName": "SessionStart",
            "additionalContext": MESSAGE,
        }
    }
    print(json.dumps(output))
    return 0


if __name__ == "__main__":
    sys.exit(main())
