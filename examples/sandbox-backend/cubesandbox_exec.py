#!/usr/bin/env python3
"""
CubeSandbox exec backend for the PreToolUse hook.

Runs a single shell command inside a CubeSandbox MicroVM and behaves like
``ssh host '<cmd>'`` / ``docker exec <container> bash -c '<cmd>'``:

- stdout / stderr are passed through byte-for-byte
- the command's real exit code is propagated via ``sys.exit()``
- one sandbox is created per Claude Code **session** and reused for every
  subsequent call (see ``--session``), so repeated invocations don't pay
  a cold-start cost
- the sandbox's cwd and exported environment variables are persisted
  *inside the sandbox filesystem* between calls, so a bare ``cd foo`` or
  ``export X=1`` issued by one Bash tool call is visible to the next one
  -- exactly like Claude Code's own persistent host shell session.

This script is normally invoked by ``cubesandbox_rewrite.py`` (the
PreToolUse hook), not by a human, but it is fully usable standalone:

    cubesandbox-exec "npm test"
    cubesandbox-exec --session my-session "git status"
    cubesandbox-exec --mount /data/shared/myproject "cd /data/shared/myproject && ls"
    cubesandbox-exec --reset --session my-session
"""

from __future__ import annotations

import argparse
import fcntl
import json
import os
import secrets
import sys
from pathlib import Path

# Stdlib-only at module level so that `--help` / `--reset` can at least
# parse args without the optional `cubesandbox` / `python-dotenv` deps
# being installed. The heavy imports happen in _bootstrap() below.
_SCRIPT_DIR = Path(__file__).resolve().parent

# Populated by _bootstrap() (called from main() after argparse).
ApiError = Config = CubeSandboxError = Sandbox = SandboxNotFoundError = None  # type: ignore[assignment]
TEMPLATE_ID = ""
SANDBOX_USER = "root"
SANDBOX_TTL = 1800
STATE_DIR = Path(os.getenv("CUBE_HOOK_STATE_DIR", os.path.expanduser("~/.cache/cubesandbox-hook")))
DEFAULT_SESSION = "default"

# One lock file per STATE_DIR; fcntl.flock serializes concurrent
# cubesandbox-exec processes that race to create a sandbox for the same
# brand-new session (Claude Code emits parallel Bash tool calls).
_LOCK_FILE = STATE_DIR / ".create.lock"


def _bootstrap() -> None:
    """Load .env (from the script's own dir, not the process CWD) and import
    the cubesandbox SDK + read config. Called lazily from main() so that
    argparse --help works even before deps are installed."""
    global ApiError, Config, CubeSandboxError, Sandbox, SandboxNotFoundError
    global TEMPLATE_ID, SANDBOX_USER, SANDBOX_TTL

    try:
        from dotenv import load_dotenv
    except ImportError:
        load_dotenv = None  # type: ignore[assignment]

    if load_dotenv:
        load_dotenv(_SCRIPT_DIR / ".env")
        load_dotenv()  # optional: let CWD .env override for dev/debug

    from cubesandbox import (  # noqa: E402
        ApiError as _ApiError,
        Config as _Config,
        CubeSandboxError as _CubeSandboxError,
        Sandbox as _Sandbox,
        SandboxNotFoundError as _SandboxNotFoundError,
    )
    ApiError, Config, CubeSandboxError, Sandbox, SandboxNotFoundError = (
        _ApiError, _Config, _CubeSandboxError, _Sandbox, _SandboxNotFoundError,
    )

    TEMPLATE_ID = os.getenv("CUBE_TEMPLATE_ID", "")
    SANDBOX_USER = os.getenv("CUBE_SANDBOX_USER", "root")
    SANDBOX_TTL = int(os.getenv("CUBE_SANDBOX_TIMEOUT", "1800"))


# ── Session -> sandbox_id persistence (on the host running this script) ─

def _state_path(session_id: str) -> Path:
    safe = "".join(c if c.isalnum() or c in "-_" else "_" for c in session_id) or DEFAULT_SESSION
    return STATE_DIR / f"{safe}.json"


def _load_state(session_id: str) -> dict:
    path = _state_path(session_id)
    if path.exists():
        try:
            return json.loads(path.read_text())
        except (json.JSONDecodeError, OSError):
            pass
    return {}


def _save_state(session_id: str, state: dict) -> None:
    STATE_DIR.mkdir(parents=True, exist_ok=True)
    _state_path(session_id).write_text(json.dumps(state))


def _clear_state(session_id: str) -> None:
    path = _state_path(session_id)
    if path.exists():
        path.unlink()


# ── Sandbox lifecycle ───────────────────────────────────────────────────

def get_sandbox(session_id: str, mount: str | None) -> Sandbox:
    """Reuse the sandbox already created for this Claude Code session, or
    create a fresh one on first use / after it has expired or been killed.

    The create+save critical section is guarded by a file lock so that
    concurrent cubesandbox-exec processes (Claude Code emits parallel Bash
    tool calls) don't each create their own sandbox and orphan the loser.
    """
    # Fast path: reuse an existing cached sandbox without locking.
    state = _load_state(session_id)
    sandbox_id = state.get("sandbox_id")
    if sandbox_id:
        try:
            return _connect(sandbox_id)
        except SandboxNotFoundError:
            pass  # sandbox is gone -- fall through and create a new one
        except CubeSandboxError as e:
            print(f"[cubesandbox-exec] warning: reconnect failed ({e}); creating a new sandbox", file=sys.stderr)

    if not TEMPLATE_ID:
        print("[cubesandbox-exec] error: CUBE_TEMPLATE_ID is not set", file=sys.stderr)
        sys.exit(127)

    # Slow path: create under an exclusive lock so only one process wins.
    STATE_DIR.mkdir(parents=True, exist_ok=True)
    with open(_LOCK_FILE, "w") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        # Re-read state inside the lock -- another process may have just
        # created the sandbox while we were waiting.
        state = _load_state(session_id)
        sandbox_id = state.get("sandbox_id")
        if sandbox_id:
            try:
                return _connect(sandbox_id)
            except SandboxNotFoundError:
                pass
            except CubeSandboxError as e:
                print(f"[cubesandbox-exec] warning: reconnect failed ({e}); creating a new sandbox", file=sys.stderr)

        sandbox = _create_sandbox(mount)
        _save_state(session_id, {
            "sandbox_id": sandbox.sandbox_id,
            "mount": mount,
            "state_token": secrets.token_urlsafe(24),
        })
        return sandbox


def _connect(sandbox_id: str) -> Sandbox:
    """Reconnect to an existing sandbox, refreshing its TTL to
    ``SANDBOX_TTL`` (Sandbox.connect's default uses Config.timeout=300,
    which would ignore the user's CUBE_SANDBOX_TIMEOUT setting)."""
    return Sandbox.connect(sandbox_id, config=Config(timeout=SANDBOX_TTL))


def _create_sandbox(mount: str | None) -> Sandbox:
    """Create a sandbox, optionally bind-mounting *mount* (Claude Code's
    project cwd) read-only at the same path. Falls back to a plain sandbox if the host-mount is rejected
    (e.g. the path isn't under CubeMaster's ``allowed_host_mount_prefixes``).
    """
    if mount:
        try:
            return Sandbox.create(
                TEMPLATE_ID,
                timeout=SANDBOX_TTL,
                metadata={
                    "host-mount": json.dumps([
                        {"hostPath": mount, "mountPath": mount, "readOnly": True},
                    ])
                },
            )
        except ApiError as e:
            print(
                f"[cubesandbox-exec] warning: host-mount of {mount!r} was rejected ({e}); "
                "falling back to a sandbox without a shared filesystem. Bash commands will "
                "run in isolation and won't see files created via the Read/Write/Edit tools. "
                "See README.md 'Filesystem consistency' section.",
                file=sys.stderr,
            )
    return Sandbox.create(TEMPLATE_ID, timeout=SANDBOX_TTL)


# ── Command execution ───────────────────────────────────────────────────

def run(command: str, session_id: str, timeout: float | None, mount: str | None) -> int:
    sandbox = get_sandbox(session_id, mount)
    state = _load_state(session_id)
    state_token = state.get("state_token")
    if not isinstance(state_token, str):
        print("[cubesandbox-exec] error: sandbox state is missing", file=sys.stderr)
        return 1

    default_cwd = mount or "$HOME"
    state_dir = f"/tmp/.cubesandbox-state-{state_token}"
    cwd_file = f"{state_dir}/cwd"
    env_file = f"{state_dir}/env"
    wrapped = (
        f"umask 077; mkdir -p -- {state_dir}; "
        f"[ -d {state_dir} ] && [ ! -L {state_dir} ] || exit 1; "
        f"[ -f {env_file} ] && [ ! -L {env_file} ] && source {env_file} >/dev/null 2>&1; "
        f'cd "$(cat {cwd_file} 2>/dev/null || echo {default_cwd})" 2>/dev/null; '
        f"{command}\n"
        "__CBX_STATUS__=$?; "
        f"pwd > {cwd_file} 2>/dev/null; "
        f"export -p > {env_file} 2>/dev/null; "
        "exit $__CBX_STATUS__"
    )

    try:
        result = sandbox.commands.run(wrapped, timeout=timeout, user=SANDBOX_USER)
    except CubeSandboxError as e:
        print(f"[cubesandbox-exec] error: {e}", file=sys.stderr)
        return 1

    if result.stdout:
        sys.stdout.write(result.stdout)
    if result.stderr:
        sys.stderr.write(result.stderr)
    return result.exit_code


def reset(session_id: str) -> None:
    state = _load_state(session_id)
    sandbox_id = state.get("sandbox_id")
    if sandbox_id:
        try:
            _connect(sandbox_id).kill()
        except CubeSandboxError:
            pass
    _clear_state(session_id)
    print(f"[cubesandbox-exec] sandbox for session {session_id!r} destroyed")


# ── CLI ──────────────────────────────────────────────────────────────────

def main() -> None:
    parser = argparse.ArgumentParser(description="Run a shell command inside a cached CubeSandbox MicroVM")
    parser.add_argument("command", nargs="?", help="Shell command to run (passed through to bash -c)")
    parser.add_argument(
        "--session", default=DEFAULT_SESSION,
        help="Session key used to cache/reuse the sandbox (typically Claude Code's session_id)",
    )
    parser.add_argument(
        "--timeout", type=float, default=float(os.getenv("CUBE_EXEC_TIMEOUT", "120")),
        help="Command timeout in seconds (default 120)",
    )
    parser.add_argument(
        "--mount", default=None,
        help="Host directory to bind-mount read-only into the sandbox at the same path "
             "(only used the first time a sandbox is created for --session). Must be under "
             "CubeMaster's allowed_host_mount_prefixes.",
    )
    parser.add_argument("--reset", action="store_true", help="Destroy the cached sandbox for this session and exit")
    args = parser.parse_args()

    if args.reset:
        _bootstrap()
        reset(args.session)
        return

    if not args.command:
        parser.print_help()
        sys.exit(2)

    _bootstrap()
    sys.exit(run(args.command, args.session, args.timeout, args.mount))


if __name__ == "__main__":
    main()
