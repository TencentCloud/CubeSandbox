#!/usr/bin/env python3
"""PreToolUse/Bash hook: detect rollback sentinel → deny + rollback.
Fail-closed."""

from __future__ import annotations

import json
import os
import sys
import time

SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, SCRIPT_DIR)

from cubesandbox_lib import (
    delete_snapshot_backend,
    find_snapshot_index,
    get_sandbox_client,
    load_hook_env,
    orphan_check,
    prune_after,
    read_my_state,
    read_sandbox_state,
    write_my_state,
)
from cubesandbox_risky import is_sentinel, parse_sentinel

USAGE = ("Usage: cubesandbox-rollback [last|<N>|<snapshot-id>|list|"
         "checkpoint <name>|undo|drop <last|N|snapshot-id>]")


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


def _kind_label(s: dict) -> str:
    """Human label for a snapshot's kind (backward-compat: missing → auto)."""
    kind = s.get("kind", "auto")
    if kind == "checkpoint":
        name = s.get("name", "")
        return f"checkpoint:{name}" if name else "checkpoint"
    return kind


def _cmd_list(session_id: str) -> int:
    my = read_my_state(session_id)
    snaps = my.get("snapshots", [])
    if not snaps:
        return _deny("No snapshots available.")
    lines = ["Available snapshots:", ""]
    for i, s in enumerate(snaps):
        ts = time.strftime("%H:%M:%S", time.localtime(s.get("timestamp", 0)))
        lines.append(
            f"  [{i}] {s.get('snapshot_id', '?')}  {ts}  "
            f"[{_kind_label(s)}]  {s.get('command', '?')[:60]}"
        )
    if my.get("undo"):
        lines.append("")
        lines.append("Undo available: run `cubesandbox-rollback undo`")
    return _deny("\n".join(lines))


def _cmd_checkpoint(session_id: str, command: str, arg: str | None) -> int:
    if not arg or not arg.strip():
        return _deny("Usage: cubesandbox-rollback checkpoint <name>")
    name = arg.strip()

    my = read_my_state(session_id)
    sstate = read_sandbox_state(session_id)
    if not sstate or not sstate.get("sandbox_id"):
        return _deny("No sandbox found. Is the cubesandbox-hook plugin configured?")

    try:
        client = get_sandbox_client(sstate["sandbox_id"])
        snapshot = client.create_snapshot()
    except Exception as exc:
        return _deny(f"Checkpoint failed: {exc}")

    # A checkpoint is new activity — invalidate any pending undo point
    old_undo = my.get("undo")
    if old_undo:
        my["undo"] = None

    my.setdefault("snapshots", []).append({
        "snapshot_id": snapshot.snapshot_id,
        "kind": "checkpoint",
        "name": name,
        "command": command[:256],
        "timestamp": int(time.time()),
    })
    try:
        write_my_state(session_id, my)
    except Exception as exc:
        return _deny(f"Checkpoint state write error: {exc}")
    # State persisted first — backend delete is best-effort, so a state-write
    # failure never leaves a local entry pointing at a deleted snapshot.
    if old_undo:
        delete_snapshot_backend(old_undo.get("snapshot_id"))
    return _deny(f"Checkpoint `{name}` saved: {snapshot.snapshot_id}")


def _cmd_undo(session_id: str) -> int:
    my = read_my_state(session_id)
    undo = my.get("undo")
    if not undo:
        return _deny("Nothing to undo.")

    sstate = read_sandbox_state(session_id)
    if not sstate or not sstate.get("sandbox_id"):
        return _deny("No sandbox found. Is the cubesandbox-hook plugin configured?")

    try:
        client = get_sandbox_client(sstate["sandbox_id"])
        client.rollback(undo["snapshot_id"])
    except Exception as exc:
        return _deny(f"Undo failed: {exc}")

    # The undo snapshot's job is done — drop it from the backend and clear
    # the undo point.  The snapshots list is NOT pruned on undo.
    delete_snapshot_backend(undo["snapshot_id"])
    my["undo"] = None
    try:
        write_my_state(session_id, my)
    except Exception as exc:
        return _deny(f"Undo state write error: {exc}")

    return _deny(
        f"Undo complete. State restored to pre-rollback snapshot "
        f"`{undo['snapshot_id']}`."
    )


def _cmd_drop(session_id: str, arg: str | None) -> int:
    if not arg:
        return _deny("Usage: cubesandbox-rollback drop <last|N|snapshot-id>")
    my = read_my_state(session_id)
    snaps = my.get("snapshots", [])
    idx = find_snapshot_index(snaps, arg)
    if idx is None:
        return _deny(f"Snapshot `{arg}` not found.")
    target = snaps[idx]
    my["snapshots"] = snaps[:idx] + snaps[idx + 1:]
    try:
        write_my_state(session_id, my)
    except Exception as exc:
        return _deny(f"Snapshot drop state write error: {exc}")
    # State persisted first — backend delete is best-effort, so a state-write
    # failure never leaves a local entry pointing at a deleted snapshot.
    delete_snapshot_backend(target.get("snapshot_id"))
    return _deny(f"Snapshot `{target.get('snapshot_id')}` dropped.")


def _cmd_rollback(session_id: str, target: str) -> int:
    my = read_my_state(session_id)
    snaps = my.get("snapshots", [])
    if not snaps:
        return _deny("No snapshots available to roll back.")

    idx = find_snapshot_index(snaps, target)
    if idx is None:
        return _deny("No such snapshot.")

    target_snap = snaps[idx]

    sstate = read_sandbox_state(session_id)
    if not sstate or not sstate.get("sandbox_id"):
        return _deny("No sandbox found. Is the cubesandbox-hook plugin configured?")

    # Capture the pre-rollback state as the new undo point BEFORE rolling
    # back.  On failure the old undo point must stay fully intact — so the
    # old undo's backend snapshot is only deleted AFTER rollback succeeds
    # (and the freshly captured undo snapshot is cleaned up on failure).
    try:
        client = get_sandbox_client(sstate["sandbox_id"])
        undo_snapshot = client.create_snapshot()
    except Exception as exc:
        return _deny(f"Could not capture pre-rollback snapshot: {exc}")

    new_undo = {
        "snapshot_id": undo_snapshot.snapshot_id,
        "command": "pre-rollback",
        "timestamp": int(time.time()),
    }

    try:
        client.rollback(target_snap["snapshot_id"])
    except Exception as exc:
        # Rollback failed — clean up the fresh undo snapshot and keep the
        # old undo point (backend + state) untouched.
        delete_snapshot_backend(new_undo["snapshot_id"])
        return _deny(f"Rollback failed: {exc}")

    # Rollback succeeded — old undo point (if any) is now stale
    old_undo = my.get("undo")

    # Prune snapshots taken after the rollback target
    kept, dropped = prune_after(snaps, idx)

    my["snapshots"] = kept
    my["undo"] = new_undo
    try:
        write_my_state(session_id, my)
    except Exception as exc:
        return _deny(f"Rollback state write error: {exc}")

    # State persisted first — backend deletes are best-effort, so a
    # state-write failure never leaves local entries pointing at deleted
    # snapshots.  The fresh undo snapshot (`new_undo`) stays in the backend
    # and is referenced by the persisted state, so it is NOT deleted here.
    if old_undo:
        delete_snapshot_backend(old_undo.get("snapshot_id"))
    for s in dropped:
        delete_snapshot_backend(s.get("snapshot_id"))

    msg = (f"Rollback complete. State restored to snapshot "
           f"`{target_snap['snapshot_id']}` from command "
           f"`{target_snap.get('command', '?')}`.")
    if dropped:
        ids = ", ".join(s.get("snapshot_id", "?") for s in dropped)
        msg += f"\nDropped {len(dropped)} snapshot(s) after this point: {ids}"
    return _deny(msg)


def main() -> int:
    load_hook_env()

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

    # --- subcommands ---
    if sub in ("list", "ls"):
        return _cmd_list(session_id)

    if sub == "checkpoint":
        return _cmd_checkpoint(session_id, command, arg)

    if sub == "undo":
        return _cmd_undo(session_id)

    if sub == "drop":
        return _cmd_drop(session_id, arg)

    # Rollback path: "last" | <N> | <snapshot-id> (bare sentinel → "last").
    # Any other token is treated as a snapshot-id candidate; unmatched ids
    # produce "No such snapshot." from _cmd_rollback.
    return _cmd_rollback(session_id, sub)


if __name__ == "__main__":
    sys.exit(main())
