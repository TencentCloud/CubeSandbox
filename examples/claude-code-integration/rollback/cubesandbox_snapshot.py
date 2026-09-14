#!/usr/bin/env python3
"""PreToolUse/Bash hook: snapshot before risky commands.  Fail-open."""

from __future__ import annotations

import json
import os
import sys
import time

SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, SCRIPT_DIR)

from cubesandbox_lib import (
    delete_snapshot_backend,
    evict_auto,
    get_max_auto_snapshots,
    get_sandbox_client,
    load_hook_env,
    read_my_state,
    read_sandbox_state,
    write_my_state,
)
from cubesandbox_risky import is_risky, is_sentinel, load_safe_whitelist


def _fail_open(reason: str) -> int:
    """Log error and return empty decision (command proceeds)."""
    print(f"[cubesandbox-snapshot] {reason}", file=sys.stderr)
    print("{}")
    return 0


def main() -> int:
    load_hook_env()

    # 1. Parse hook input
    try:
        payload = json.load(sys.stdin)
    except (json.JSONDecodeError, UnicodeDecodeError) as exc:
        return _fail_open(f"invalid hook JSON: {exc}")

    if payload.get("hook_event_name") != "PreToolUse":
        print("{}")
        return 0
    if payload.get("tool_name") != "Bash":
        print("{}")
        return 0

    tool_input = payload.get("tool_input", {})
    command = tool_input.get("command", "")
    session_id = payload.get("session_id", "default")

    # 2. Skip sentinel
    if is_sentinel(command):
        print("{}")
        return 0

    # 3. Load whitelist and check risk
    safe = load_safe_whitelist()
    if not is_risky(command, safe):
        print("{}")
        return 0

    # 4. Risky — need a sandbox
    sstate = read_sandbox_state(session_id)
    if not sstate or not sstate.get("sandbox_id"):
        return _fail_open("no sandbox yet (first command?)")

    # 5. Connect + snapshot
    try:
        client = get_sandbox_client(sstate["sandbox_id"])
        snapshot = client.create_snapshot()
    except Exception as exc:
        return _fail_open(f"sandbox error: {exc}")

    # 6. New activity invalidates any pending undo point
    my = read_my_state(session_id)
    if my.get("undo"):
        undo = my["undo"]
        print(f"[cubesandbox-snapshot] new activity invalidates undo point "
              f"{undo.get('snapshot_id')}", file=sys.stderr)
        delete_snapshot_backend(undo.get("snapshot_id"))
        my["undo"] = None

    # 7. Append + ring-evict auto snapshots
    my.setdefault("snapshots", []).append({
        "snapshot_id": snapshot.snapshot_id,
        "kind": "auto",
        "name": getattr(snapshot, "name", ""),
        "command": command[:256],
        "timestamp": int(time.time()),
    })
    my["snapshots"], evicted = evict_auto(
        my["snapshots"], get_max_auto_snapshots()
    )
    for s in evicted:
        print(f"[cubesandbox-snapshot] evicting old auto snapshot "
              f"{s.get('snapshot_id')}", file=sys.stderr)
        delete_snapshot_backend(s.get("snapshot_id"))

    # 8. Persist
    try:
        write_my_state(session_id, my)
    except Exception as exc:
        return _fail_open(f"state write error: {exc}")

    print(f"[cubesandbox-snapshot] snapshot taken: {snapshot.snapshot_id}",
          file=sys.stderr)
    print("{}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
