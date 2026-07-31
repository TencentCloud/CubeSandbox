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
    get_config_hash,
    get_sandbox_client,
    read_my_state,
    read_sandbox_state,
    session_digest,
    write_my_state,
)
from cubesandbox_risky import is_risky, is_sentinel, load_safe_whitelist


def _fail_open(reason: str) -> int:
    """Log error and return empty decision (command proceeds)."""
    print(f"[cubesandbox-snapshot] {reason}", file=sys.stderr)
    print("{}")
    return 0


def main() -> int:
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

    # 5. Config hash check (从严: stale → skip; empty = first run, allow)
    stored = read_my_state(session_id).get("config_hash", "")
    current = get_config_hash()
    if stored and stored != current:
        return _fail_open("config stale — reinstall rollback hooks")

    # 6. Connect + snapshot
    try:
        client = get_sandbox_client(sstate["sandbox_id"])
        snapshot = client.create_snapshot()
    except Exception as exc:
        return _fail_open(f"sandbox error: {exc}")

    # 7. Persist
    my = read_my_state(session_id)
    my.setdefault("snapshots", []).append({
        "snapshot_id": snapshot.snapshot_id,
        "name": getattr(snapshot, "name", ""),
        "command": command[:256],
        "timestamp": int(time.time()),
    })
    my["config_hash"] = get_config_hash()
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
