#!/usr/bin/env python3
"""Shared utilities: digest, state I/O, sandbox connection, config hash."""

from __future__ import annotations

import contextlib
import fcntl
import hashlib
import json
import os
import sys
import tempfile
from pathlib import Path
from typing import Any, Dict, Optional

# ---------------------------------------------------------------------------
# Paths
# ---------------------------------------------------------------------------
DEFAULT_SESSION = "default"
STATE_DIR_765 = Path(os.path.expanduser(
    os.getenv("CUBE_HOOK_STATE_DIR", "~/.cache/cubesandbox-hook")
))
MY_STATE_DIR = Path(os.path.expanduser("~/.cache/cubesandbox-rollback"))

# #765's installed config file (written by #765's install.sh → CONFIG_PATH)
_ENV_PATH_765 = Path(os.path.expanduser("~/.claude/hooks/cubesandbox.env"))


# ---------------------------------------------------------------------------
# Digest (must match #765's _session_digest *exactly*)
# ---------------------------------------------------------------------------
def session_digest(session_id: str) -> str:
    key = session_id or DEFAULT_SESSION
    return hashlib.sha256(key.encode("utf-8")).hexdigest()


# ---------------------------------------------------------------------------
# Path helpers
# ---------------------------------------------------------------------------
def _ensure_dir(dirpath: Path) -> None:
    dirpath.mkdir(mode=0o700, parents=True, exist_ok=True)
    if dirpath.is_symlink() or not dirpath.is_dir():
        raise OSError(f"unsafe state directory: {dirpath}")
    os.chmod(dirpath, 0o700)


def _sandbox_state_path(session_id: str) -> Path:
    """Path to #765's state file: ~/.cache/cubesandbox-hook/<digest>.json"""
    return STATE_DIR_765 / f"{session_digest(session_id)}.json"


def _my_state_path(session_id: str) -> Path:
    """Path to our state file: ~/.cache/cubesandbox-rollback/<digest>.json"""
    return MY_STATE_DIR / f"{session_digest(session_id)}.json"


def _my_lock_path(session_id: str) -> Path:
    return MY_STATE_DIR / f"{session_digest(session_id)}.lock"


# ---------------------------------------------------------------------------
# #765 state I/O
# ---------------------------------------------------------------------------
def read_sandbox_state(session_id: str) -> Optional[Dict[str, Any]]:
    """Read #765's JSON state; return None if missing or unreadable."""
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
    """Read our state, returning an empty dict if not found."""
    path = _my_state_path(session_id)
    if not path.exists():
        return {"session_id": session_id, "snapshots": [], "config_hash": ""}
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (json.JSONDecodeError, UnicodeDecodeError, OSError):
        return {"session_id": session_id, "snapshots": [], "config_hash": ""}


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
# Sandbox connection
# ---------------------------------------------------------------------------
def get_sandbox_client(sandbox_id: str):
    """Connect to a CubeSandbox; import on demand."""
    from cubesandbox import Sandbox
    return Sandbox.connect(sandbox_id=sandbox_id)


# ---------------------------------------------------------------------------
# Config hash (detect stale #765 env)
# ---------------------------------------------------------------------------
def get_config_hash() -> str:
    """SHA-256 of #765's .env file content."""
    if _ENV_PATH_765.exists():
        content = _ENV_PATH_765.read_bytes()
    else:
        content = b""
    return hashlib.sha256(content).hexdigest()


def check_config_stale(session_id: str) -> bool:
    """Return True if our stored config hash differs from #765's current env."""
    stored = read_my_state(session_id).get("config_hash", "")
    current = get_config_hash()
    return stored and stored != current


# ---------------------------------------------------------------------------
# Orphan detection
# ---------------------------------------------------------------------------
def orphan_check(session_id: str) -> bool:
    """Delete our state if #765's state is gone. Return True if orphaned."""
    if not _sandbox_state_path(session_id).exists():
        our = _my_state_path(session_id)
        if our.exists():
            with contextlib.suppress(OSError):
                our.unlink()
        return True
    return False

