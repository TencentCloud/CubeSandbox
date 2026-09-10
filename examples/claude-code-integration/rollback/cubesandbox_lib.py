#!/usr/bin/env python3
"""Shared utilities: digest, state I/O, sandbox connection, hook env,
snapshot helpers."""

from __future__ import annotations

import contextlib
import fcntl
import hashlib
import json
import os
import sys
import tempfile
from pathlib import Path
from typing import Any, Dict

# ---------------------------------------------------------------------------
# Paths
# ---------------------------------------------------------------------------
DEFAULT_SESSION = "default"
HOOK_STATE_DIR = Path(os.path.expanduser(
    os.getenv("CUBE_HOOK_STATE_DIR", "~/.cache/cubesandbox-hook")
))
MY_STATE_DIR = Path(os.path.expanduser("~/.cache/cubesandbox-rollback"))

# cubesandbox-hook's installed config file (written by cubesandbox-hook's
# install.sh → CONFIG_PATH)
_HOOK_ENV_PATH = Path(os.path.expanduser("~/.claude/hooks/cubesandbox.env"))


# ---------------------------------------------------------------------------
# Digest (must match cubesandbox-hook's _session_digest *exactly*)
# ---------------------------------------------------------------------------
def session_digest(session_id: str) -> str:
    key = session_id or DEFAULT_SESSION
    return hashlib.sha256(key.encode("utf-8")).hexdigest()


# ---------------------------------------------------------------------------
# Hook env (cubesandbox-hook's installed .env — never clobber)
# ---------------------------------------------------------------------------
def load_hook_env() -> None:
    """Load KEY=VALUE entries from cubesandbox-hook's installed env file.

    Missing or unreadable file → no-op.  Already-set environment variables
    are NEVER clobbered (``os.environ.setdefault``).
    """
    path = _HOOK_ENV_PATH
    if not path.exists():
        return
    try:
        from dotenv import dotenv_values
        values = dotenv_values(str(path))
    except Exception:
        return
    for key, value in values.items():
        if key and isinstance(value, str):
            os.environ.setdefault(key, value)


# ---------------------------------------------------------------------------
# Path helpers
# ---------------------------------------------------------------------------
def _ensure_dir(dirpath: Path) -> None:
    dirpath.mkdir(mode=0o700, parents=True, exist_ok=True)
    if dirpath.is_symlink() or not dirpath.is_dir():
        raise OSError(f"unsafe state directory: {dirpath}")
    os.chmod(dirpath, 0o700)


def _sandbox_state_path(session_id: str) -> Path:
    """Path to cubesandbox-hook's state file:
    ~/.cache/cubesandbox-hook/<digest>.json"""
    return HOOK_STATE_DIR / f"{session_digest(session_id)}.json"


def _my_state_path(session_id: str) -> Path:
    """Path to our state file: ~/.cache/cubesandbox-rollback/<digest>.json"""
    return MY_STATE_DIR / f"{session_digest(session_id)}.json"


def _my_lock_path(session_id: str) -> Path:
    return MY_STATE_DIR / f"{session_digest(session_id)}.lock"


# ---------------------------------------------------------------------------
# cubesandbox-hook state I/O
# ---------------------------------------------------------------------------
def read_sandbox_state(session_id: str) -> Dict[str, Any] | None:
    """Read cubesandbox-hook's JSON state; return None if missing/unreadable."""
    path = _sandbox_state_path(session_id)
    if not path.exists():
        return None
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (json.JSONDecodeError, UnicodeDecodeError, OSError):
        return None


# ---------------------------------------------------------------------------
# Own state I/O (atomic + flock)
# ---------------------------------------------------------------------------
@contextlib.contextmanager
def _locked_state(session_id: str):
    """Open a lock file and yield when the exclusive lock is acquired."""
    _ensure_dir(MY_STATE_DIR)
    lock_path = _my_lock_path(session_id)
    fd = os.open(str(lock_path), os.O_CREAT | os.O_RDWR, 0o600)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX)
        yield
    finally:
        fcntl.flock(fd, fcntl.LOCK_UN)
        os.close(fd)


def read_my_state(session_id: str) -> Dict[str, Any]:
    """Read our state, returning a fresh default if not found."""
    path = _my_state_path(session_id)
    if not path.exists():
        return {"session_id": session_id, "snapshots": [], "undo": None}
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (json.JSONDecodeError, UnicodeDecodeError, OSError):
        return {"session_id": session_id, "snapshots": [], "undo": None}


def write_my_state(session_id: str, state: Dict[str, Any]) -> None:
    """Atomically write our state file with flock serialisation."""
    _ensure_dir(MY_STATE_DIR)
    path = _my_state_path(session_id)
    with _locked_state(session_id):
        fd, tmp = tempfile.mkstemp(dir=str(MY_STATE_DIR),
                                   prefix=".tmp_state_",
                                   suffix=".json")
        try:
            with os.fdopen(fd, "w", encoding="utf-8") as fh:
                json.dump(state, fh, indent=2, sort_keys=True)
            os.chmod(tmp, 0o600)
            os.rename(tmp, str(path))
        except Exception:
            with contextlib.suppress(OSError):
                os.unlink(tmp)
            raise


# ---------------------------------------------------------------------------
# Snapshot helpers (pure, unit-testable — no I/O)
# ---------------------------------------------------------------------------
def find_snapshot_index(snaps: list, target: str) -> int | None:
    """Locate a snapshot in the snapshots list.

    ``target``:
      - ``"last"``            → the final index (``len(snaps) - 1``)
      - decimal int string    → that index (out-of-range → None)
      - otherwise             → index of the entry whose ``snapshot_id``
                                matches, else None (non-numeric non-id → None)
    """
    if not snaps:
        return None
    if target == "last":
        return len(snaps) - 1
    if target.isdigit():
        idx = int(target)
        return idx if 0 <= idx < len(snaps) else None
    for i, s in enumerate(snaps):
        if s.get("snapshot_id") == target:
            return i
    return None


def prune_after(snaps: list, idx: int) -> tuple[list, list]:
    """Return (kept, dropped) — kept = snaps[:idx+1], dropped = snaps[idx+1:].

    Preserves order.
    """
    return snaps[:idx + 1], snaps[idx + 1:]


def evict_auto(snaps: list, max_auto: int) -> tuple[list, list]:
    """Ring eviction for ``kind == 'auto'`` snapshots only.

    Baseline and checkpoint snapshots are NEVER evicted.  If the number of
    auto snapshots exceeds ``max_auto``, the OLDEST auto (lowest index among
    autos) is removed, repeated until count(auto) <= max_auto (usually one
    iteration).  Snapshots missing a ``kind`` are treated as ``auto``
    (backward compat with pre-``kind`` state).  ``max_auto`` is clamped to
    >= 1.  Returns (new_list, evicted_list).
    """
    new = list(snaps)
    evicted: list = []
    limit = max(1, max_auto)
    while sum(1 for s in new if s.get("kind", "auto") == "auto") > limit:
        for i, s in enumerate(new):
            if s.get("kind", "auto") == "auto":
                evicted.append(new.pop(i))
                break
    return new, evicted


# ---------------------------------------------------------------------------
# Sandbox connection + backend deletion
# ---------------------------------------------------------------------------
def get_sandbox_client(sandbox_id: str):
    """Connect to a CubeSandbox; import on demand."""
    from cubesandbox import Sandbox
    return Sandbox.connect(sandbox_id=sandbox_id)


def delete_snapshot_backend(snapshot_id: str) -> None:
    """Best-effort backend deletion; swallow all exceptions (caller keeps
    local state consistent)."""
    if not snapshot_id:
        return
    try:
        from cubesandbox import Sandbox
        Sandbox.delete_snapshot(snapshot_id)
    except Exception:
        pass


# ---------------------------------------------------------------------------
# Rollback tunables
# ---------------------------------------------------------------------------
def get_max_auto_snapshots() -> int:
    """CUBE_ROLLBACK_MAX_AUTO_SNAPSHOTS, default 30, clamped to >= 1."""
    raw = os.environ.get("CUBE_ROLLBACK_MAX_AUTO_SNAPSHOTS", "")
    try:
        n = int(raw.strip())
    except (ValueError, AttributeError):
        n = 30
    return max(1, n)


# ---------------------------------------------------------------------------
# Orphan detection
# ---------------------------------------------------------------------------
def orphan_check(session_id: str) -> bool:
    """Delete our state if cubesandbox-hook's state is gone. Return True if
    orphaned."""
    if not _sandbox_state_path(session_id).exists():
        our = _my_state_path(session_id)
        if our.exists():
            with contextlib.suppress(OSError):
                our.unlink()
        return True
    return False
