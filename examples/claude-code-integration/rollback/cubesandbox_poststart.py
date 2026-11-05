#!/usr/bin/env python3
"""PostToolUse/Bash hook: take baseline snapshot after the first command."""

from __future__ import annotations

import json
import os
import sys
import time

SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, SCRIPT_DIR)

from cubesandbox_lib import (
    get_sandbox_client,
    load_hook_env,
    read_my_state,
    read_sandbox_state,
    write_my_state,
)


def main() -> int:
    load_hook_env()

    try:
        payload = json.load(sys.stdin)
    except (json.JSONDecodeError, UnicodeDecodeError):
        print("{}")
        return 0

    if payload.get("hook_event_name") != "PostToolUse":
        print("{}")
        return 0
    if payload.get("tool_name") != "Bash":
        print("{}")
        return 0

    session_id = payload.get("session_id", "default")
    my = read_my_state(session_id)

    # Baseline already exists → nothing to do (auto snapshots don't count:
    # an `auto` snapshot from a risky first command must not suppress the
    # baseline, or `rollback last` would skip it forever)
    if any(s.get("kind") == "baseline" for s in my.get("snapshots", [])):
        print("{}")
        return 0

    # First command: we need a sandbox (cubesandbox-hook should have created one)
    sstate = read_sandbox_state(session_id)
    if not sstate or not sstate.get("sandbox_id"):
        print("[cubesandbox-poststart] no sandbox yet", file=sys.stderr)
        print("{}")
        return 0

    try:
        client = get_sandbox_client(sstate["sandbox_id"])
        snapshot = client.create_snapshot()
    except Exception as exc:
        print(f"[cubesandbox-poststart] snapshot error: {exc}", file=sys.stderr)
        print("{}")
        return 0

    my.setdefault("snapshots", []).append({
        "snapshot_id": snapshot.snapshot_id,
        "kind": "baseline",
        "name": "<initial>",
        "command": "<initial>",
        "timestamp": int(time.time()),
    })
    try:
        write_my_state(session_id, my)
    except Exception as exc:
        print(f"[cubesandbox-poststart] state write error: {exc}", file=sys.stderr)
        print("{}")
        return 0

    print("[cubesandbox-poststart] baseline snapshot taken", file=sys.stderr)
    print("{}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
