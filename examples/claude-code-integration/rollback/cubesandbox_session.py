#!/usr/bin/env python3
"""SessionStart hook: inject rollback awareness into agent context."""

from __future__ import annotations

import json
import sys

MESSAGE = (
    "CubeSandbox Rollback is active. Snapshots are taken automatically "
    "before risky commands (rm, npm install, git reset, etc.). If a "
    "command breaks the environment, run `cubesandbox-rollback last` to "
    "restore the previous state. Use `cubesandbox-rollback list` to see "
    "available snapshots. Use `cubesandbox-rollback drop <id>` to delete "
    "a snapshot."
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
