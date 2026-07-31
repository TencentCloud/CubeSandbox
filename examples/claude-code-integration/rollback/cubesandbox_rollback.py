#!/usr/bin/env python3
"""PreToolUse/Bash hook: detect rollback sentinel → deny + rollback.  Fail-closed."""

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
    orphan_check,
    read_my_state,
    read_sandbox_state,
    write_my_state,
)
from cubesandbox_risky import is_sentinel, parse_sentinel


def _deny(reason_text: str) -> int:
    """Print deny decision and exit 0 (CC reads JSON, not exit code).

    Live Claude Code testing proved that only `permissionDecisionReason` is
    reliably rendered to the agent when a PreToolUse hook denies.  The full
    user-facing message therefore goes into the reason; `stdout` carries the
    same text as a harmless secondary copy (ignored by current CC, useful if a
    future version renders it).
    """
    print(json.dumps({
        "hookSpecificOutput": {
            "hookEventName": "PreToolUse",
            "permissionDecision": "deny",
            "permissionDecisionReason": reason_text,
            "stdout": reason_text,
        }
    }))
    return 0


def _pass() -> int:
    """Print empty decision — not our command."""
    print("{}")
    return 0


def main() -> int:
    try:
        payload = json.load(sys.stdin)
    except (json.JSONDecodeError, UnicodeDecodeError):
        return _pass()

    if payload.get("hook_event_name") != "PreToolUse":
        return _pass()
    if payload.get("tool_name") != "Bash":
        return _pass()

    command = payload.get("tool_input", {}).get("command", "")
    session_id = payload.get("session_id", "default")

    if not is_sentinel(command):
        return _pass()

    sub, arg = parse_sentinel(command)

    # Orphan check
    orphan_check(session_id)

    # Config hash check (从严: stale → deny; empty = first run, allow)
    stored = read_my_state(session_id).get("config_hash", "")
    current = get_config_hash()
    if stored and stored != current:
        return _deny("Config mismatch with #765. Reinstall rollback hooks.")

    # --- subcommands ---
    if sub in ("list", "ls"):
        my = read_my_state(session_id)
        snaps = my.get("snapshots", [])
        if not snaps:
            return _deny("No snapshots available.")
        lines = ["Available snapshots:", ""]
        for i, s in enumerate(snaps):
            ts = time.strftime("%H:%M:%S", time.localtime(s.get("timestamp", 0)))
            lines.append(f"  [{i}] {s.get('snapshot_id','?')}  {ts}  {s.get('command','?')[:60]}")
        return _deny("\n".join(lines))

    if sub == "drop":
        if not arg:
            return _deny("Usage: cubesandbox-rollback drop <snapshot-id>")
        my = read_my_state(session_id)
        snaps = my.get("snapshots", [])
        new_snaps = [s for s in snaps if s.get("snapshot_id") != arg]
        if len(new_snaps) == len(snaps):
            return _deny(f"Snapshot `{arg}` not found.")
        my["snapshots"] = new_snaps
        write_my_state(session_id, my)
        return _deny(f"Snapshot `{arg}` dropped.")

    # Anything other than `last` (or the implicit default) is an unknown
    # subcommand — deny with usage instead of silently rolling back.
    if sub not in (None, "", "last"):
        return _deny("Usage: cubesandbox-rollback [last|list|drop <snapshot-id>]")

    # "last" (default when no subcommand is given)
    my = read_my_state(session_id)
    snaps = my.get("snapshots", [])
    if not snaps:
        return _deny("No snapshots available to roll back.")

    latest = snaps[-1]

    sstate = read_sandbox_state(session_id)
    if not sstate or not sstate.get("sandbox_id"):
        return _deny("No sandbox found. Is #765 configured?")

    try:
        client = get_sandbox_client(sstate["sandbox_id"])
        client.rollback(latest["snapshot_id"])
    except Exception as exc:
        return _deny(f"Rollback failed: {exc}")

    # 从严: only keep snapshots up to and including the target
    idx = snaps.index(latest)
    my["snapshots"] = snaps[:idx + 1]
    write_my_state(session_id, my)

    return _deny(
        f"Rollback complete. State restored to snapshot `{latest['snapshot_id']}` "
        f"from command `{latest.get('command', '?')}`. "
        "You may re-run your intended command now."
    )


if __name__ == "__main__":
    sys.exit(main())
