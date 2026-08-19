#!/usr/bin/env python3
"""Execute a command in a CubeSandbox cached by Claude Code session."""

from __future__ import annotations

import argparse
import contextlib
import fcntl
import hashlib
import json
import math
import os
import secrets
import shlex
import stat
import sys
import tempfile
import traceback
from pathlib import Path
from typing import Any, Dict, Iterator, Optional


SCRIPT_DIR = Path(__file__).resolve().parent
CONFIG_KEYS = (
    "CUBE_API_URL",
    "CUBE_TEMPLATE_ID",
    "CUBE_PROXY_NODE_IP",
    "CUBE_PROXY_PORT_HTTP",
    "CUBE_SANDBOX_DOMAIN",
    "CUBE_SANDBOX_USER",
    "CUBE_SANDBOX_TIMEOUT",
    "CUBE_EXEC_TIMEOUT",
    "CUBE_HOOK_STATE_DIR",
)
DEFAULT_SESSION = "default"

# SDK symbols are populated lazily so --help remains available before setup.
ApiError = Config = CubeSandboxError = Sandbox = SandboxNotFoundError = None
TEMPLATE_ID = ""
SANDBOX_USER = "root"
SANDBOX_TTL = 1800
STATE_DIR = Path(os.path.expanduser("~/.cache/cubesandbox-hook"))


class BootstrapError(RuntimeError):
    """Raised when hook dependencies or configuration cannot be loaded."""


def _load_config_values(dotenv_values: Any) -> None:
    """Load only hook-owned CubeSandbox values without replacing exports."""
    config_paths = [SCRIPT_DIR / "cubesandbox.env"]
    if (
        SCRIPT_DIR.name == "hooks"
        and SCRIPT_DIR.parent.name == "claude-code-integration"
    ):
        config_paths.append(SCRIPT_DIR.parent / ".env")

    for path in config_paths:
        if not path.is_file():
            continue
        values = dotenv_values(path)
        for key in CONFIG_KEYS:
            value = values.get(key)
            if key not in os.environ and isinstance(value, str):
                os.environ[key] = value


def _positive_int_env(name: str, default: int) -> int:
    raw_value = os.getenv(name, str(default))
    try:
        value = int(raw_value)
    except ValueError as exc:
        raise BootstrapError(f"{name} must be a positive integer") from exc
    if value <= 0:
        raise BootstrapError(f"{name} must be a positive integer")
    return value


def _bootstrap() -> None:
    """Load sanitized configuration and import the native CubeSandbox SDK."""
    global ApiError, Config, CubeSandboxError, Sandbox, SandboxNotFoundError
    global TEMPLATE_ID, SANDBOX_USER, SANDBOX_TTL, STATE_DIR

    try:
        from dotenv import dotenv_values
    except ImportError as exc:
        raise BootstrapError(
            "python-dotenv is required; install dependencies from ../requirements.txt"
        ) from exc

    _load_config_values(dotenv_values)

    try:
        from cubesandbox import (
            ApiError as _ApiError,
            Config as _Config,
            CubeSandboxError as _CubeSandboxError,
            Sandbox as _Sandbox,
            SandboxNotFoundError as _SandboxNotFoundError,
        )
    except ImportError as exc:
        raise BootstrapError(
            "the cubesandbox SDK is required; install dependencies from ../requirements.txt"
        ) from exc

    ApiError = _ApiError
    Config = _Config
    CubeSandboxError = _CubeSandboxError
    Sandbox = _Sandbox
    SandboxNotFoundError = _SandboxNotFoundError
    TEMPLATE_ID = os.getenv("CUBE_TEMPLATE_ID", "")
    SANDBOX_USER = os.getenv("CUBE_SANDBOX_USER", "root")
    SANDBOX_TTL = _positive_int_env("CUBE_SANDBOX_TIMEOUT", 1800)
    STATE_DIR = Path(
        os.path.expanduser(
            os.getenv("CUBE_HOOK_STATE_DIR", "~/.cache/cubesandbox-hook")
        )
    )


def _session_digest(session_id: str) -> str:
    key = session_id or DEFAULT_SESSION
    return hashlib.sha256(key.encode("utf-8")).hexdigest()


def _ensure_state_dir() -> None:
    STATE_DIR.mkdir(mode=0o700, parents=True, exist_ok=True)
    if STATE_DIR.is_symlink() or not STATE_DIR.is_dir():
        raise OSError(f"unsafe state directory: {STATE_DIR}")
    os.chmod(STATE_DIR, 0o700)


def _state_path(session_id: str) -> Path:
    return STATE_DIR / f"{_session_digest(session_id)}.json"


def _lock_path(session_id: str) -> Path:
    return STATE_DIR / f"{_session_digest(session_id)}.lock"


def _load_state(session_id: str) -> Dict[str, Any]:
    if not STATE_DIR.exists():
        return {}
    path = _state_path(session_id)
    try:
        _ensure_state_dir()
        flags = os.O_RDONLY
        if hasattr(os, "O_NOFOLLOW"):
            flags |= os.O_NOFOLLOW
        descriptor = os.open(path, flags)
        with os.fdopen(descriptor, "r", encoding="utf-8") as state_file:
            if not stat.S_ISREG(os.fstat(state_file.fileno()).st_mode):
                return {}
            os.fchmod(state_file.fileno(), 0o600)
            state = json.load(state_file)
    except (FileNotFoundError, json.JSONDecodeError, OSError, UnicodeDecodeError):
        return {}
    return state if isinstance(state, dict) else {}


def _save_state(session_id: str, state: Dict[str, Any]) -> None:
    _ensure_state_dir()
    destination = _state_path(session_id)
    fd, temporary_name = tempfile.mkstemp(
        dir=STATE_DIR,
        prefix=f".{destination.stem}.",
        suffix=".tmp",
    )
    temporary = Path(temporary_name)
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as state_file:
            json.dump(state, state_file)
            state_file.flush()
            os.fsync(state_file.fileno())
        os.replace(temporary, destination)
        os.chmod(destination, 0o600)
    finally:
        with contextlib.suppress(FileNotFoundError):
            temporary.unlink()


def _clear_state(session_id: str) -> None:
    with contextlib.suppress(FileNotFoundError):
        _state_path(session_id).unlink()


@contextlib.contextmanager
def _session_lock(session_id: str) -> Iterator[None]:
    _ensure_state_dir()
    flags = os.O_CREAT | os.O_RDWR
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    descriptor = os.open(_lock_path(session_id), flags, 0o600)
    try:
        os.fchmod(descriptor, 0o600)
        fcntl.flock(descriptor, fcntl.LOCK_EX)
        yield
    finally:
        fcntl.flock(descriptor, fcntl.LOCK_UN)
        os.close(descriptor)


def _remove_lock(session_id: str) -> None:
    """Delete a session lock file once no other process is using it.

    Must be called with the session lock already released. Removal is gated
    on a non-blocking acquire: if another process holds the lock the file is
    left in place, because unlinking it would let a later caller create a
    second inode and defeat mutual exclusion.
    """
    path = _lock_path(session_id)
    flags = os.O_RDWR
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        descriptor = os.open(path, flags)
    except (FileNotFoundError, OSError):
        return
    try:
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            return  # still in use by a concurrent run; leave it for that owner
        try:
            with contextlib.suppress(FileNotFoundError):
                path.unlink()
        finally:
            fcntl.flock(descriptor, fcntl.LOCK_UN)
    finally:
        os.close(descriptor)


def _connect(sandbox_id: str) -> Any:
    """Reconnect to fetch data-plane metadata and verify the sandbox lives."""
    return Sandbox.connect(sandbox_id, config=Config(timeout=SANDBOX_TTL))


def _create_sandbox(mount: Optional[str]) -> Any:
    """Create a sandbox, falling back when a read-only host mount is denied."""
    if mount:
        metadata = {
            "host-mount": json.dumps(
                [{"hostPath": mount, "mountPath": mount, "readOnly": True}]
            )
        }
        try:
            return Sandbox.create(TEMPLATE_ID, timeout=SANDBOX_TTL, metadata=metadata)
        except ApiError as exc:
            print(
                f"[cubesandbox-exec] warning: read-only host mount {mount!r} "
                f"was rejected ({exc}); creating a sandbox without the mount",
                file=sys.stderr,
            )
    return Sandbox.create(TEMPLATE_ID, timeout=SANDBOX_TTL)


def _valid_state_token(token: Any) -> bool:
    """Return True for a token that is safe to interpolate into shell code."""
    return (
        isinstance(token, str)
        and bool(token)
        and all(character.isalnum() or character in "-_" for character in token)
    )


def _try_cached_sandbox(state: Dict[str, Any]) -> Optional[Any]:
    sandbox_id = state.get("sandbox_id")
    if not isinstance(sandbox_id, str) or not sandbox_id:
        return None
    try:
        return _connect(sandbox_id)
    except SandboxNotFoundError:
        return None
    except CubeSandboxError as exc:
        print(
            f"[cubesandbox-exec] warning: reconnect failed ({exc}); "
            "creating a new sandbox",
            file=sys.stderr,
        )
        return None
    except Exception as exc:
        # Transport-level failures (requests/httpx) land here and should fall
        # back to a fresh sandbox, but so would a programming error — keep it
        # diagnosable instead of masking it behind the generic warning.
        traceback.print_exc()
        print(
            f"[cubesandbox-exec] warning: reconnect failed ({exc}); "
            "creating a new sandbox",
            file=sys.stderr,
        )
        return None


def _get_sandbox_locked(session_id: str, mount: Optional[str]) -> Any:
    """Return ``(sandbox, state)`` while the caller holds the session lock."""
    state = _load_state(session_id)
    sandbox_id = state.get("sandbox_id")
    if (
        isinstance(sandbox_id, str)
        and sandbox_id
        and not _valid_state_token(state.get("state_token"))
    ):
        # State from an older hook version or a hand-edited file: the token
        # cannot be trusted in shell code, so destroy the recorded sandbox
        # and start fresh instead of failing every subsequent run.
        print(
            "[cubesandbox-exec] warning: cached sandbox state is invalid; "
            "recreating the sandbox",
            file=sys.stderr,
        )
        with contextlib.suppress(Exception):
            _connect(sandbox_id).kill()
        _clear_state(session_id)
        state = {}

    sandbox = _try_cached_sandbox(state)
    if sandbox is not None:
        return sandbox, state

    if not TEMPLATE_ID:
        raise BootstrapError("CUBE_TEMPLATE_ID is not set")

    sandbox = _create_sandbox(mount)
    state = {
        "sandbox_id": sandbox.sandbox_id,
        "mount": mount,
        "state_token": secrets.token_urlsafe(24),
    }
    try:
        _save_state(session_id, state)
    except Exception:
        # Never leak the just-created sandbox when its ID cannot be recorded.
        with contextlib.suppress(Exception):
            sandbox.kill()
        raise
    return sandbox, state


def get_sandbox(session_id: str, mount: Optional[str]) -> Any:
    with _session_lock(session_id):
        sandbox, _state = _get_sandbox_locked(session_id, mount)
        return sandbox


def _state_shell(command: str, state_token: str, mount: Optional[str]) -> str:
    """Wrap a command so its cwd and exported environment persist.

    The returned snippet runs inside the sandbox. Every interpolated value
    is ``shlex.quote``d and the caller restricts ``state_token`` to
    ``[A-Za-z0-9_-]``, so the original command stays exactly one quoted
    argument and cannot break out onto the surrounding shell.
    """
    state_dir = f"/tmp/.cubesandbox-state-{state_token}"
    cwd_file = f"{state_dir}/cwd"
    env_file = f"{state_dir}/env"
    state_dir_q = shlex.quote(state_dir)
    cwd_file_q = shlex.quote(cwd_file)
    env_file_q = shlex.quote(env_file)
    default_cwd = shlex.quote(mount) if mount else '"$HOME"'
    command_q = shlex.quote(command)

    return (
        # State files must stay private to the sandbox user.
        f"umask 077; mkdir -p -- {state_dir_q}; "
        # Bail out if the state directory was replaced by a symlink.
        f"[ -d {state_dir_q} ] && [ ! -L {state_dir_q} ] || exit 1; "
        # Restore the persisted environment unless the env file is a link.
        f"if [ -f {env_file_q} ] && [ ! -L {env_file_q} ]; then "
        f"source {env_file_q} >/dev/null 2>&1; fi; "
        # Restore the previous cwd, falling back to the mount, then $HOME.
        f"if [ -f {cwd_file_q} ] && [ ! -L {cwd_file_q} ]; then "
        f'cd -- "$(cat -- {cwd_file_q})" 2>/dev/null || '
        f'cd -- {default_cwd} 2>/dev/null || cd -- "$HOME" || exit 1; '
        f'else cd -- {default_cwd} 2>/dev/null || cd -- "$HOME" || exit 1; fi; '
        # Run the original command as exactly one quoted argument.
        f"eval -- {command_q}\n"
        # Capture the exit status BEFORE any state bookkeeping clobbers $?.
        "__CBX_STATUS__=$?; "
        # Persist cwd/env for the next call. The symlink check leads so the
        # precedence is explicit: write only when the path is not a symlink
        # AND is either missing or a regular file — never through a symlink
        # a previous command may have planted.
        f"if [ ! -L {cwd_file_q} ] && "
        f"{{ [ ! -e {cwd_file_q} ] || [ -f {cwd_file_q} ]; }}; "
        f"then pwd > {cwd_file_q} 2>/dev/null; fi; "
        # `export -p` output is shell-generated, so re-sourcing it cannot
        # smuggle new syntax; still scrub the variables that would
        # re-execute attacker-controlled code on every later command.
        f"if [ ! -L {env_file_q} ] && "
        f"{{ [ ! -e {env_file_q} ] || [ -f {env_file_q} ]; }}; "
        f"then export -p 2>/dev/null | "
        f"grep -v -E '^(declare -x|export) (BASH_ENV|ENV|LD_PRELOAD|PROMPT_COMMAND)=' "
        f"> {env_file_q}; fi; "
        'exit "$__CBX_STATUS__"'
    )


def run(
    command: str,
    session_id: str,
    timeout: Optional[float],
    mount: Optional[str],
) -> int:
    try:
        with _session_lock(session_id):
            sandbox, state = _get_sandbox_locked(session_id, mount)
            result = sandbox.commands.run(
                _state_shell(command, state["state_token"], mount),
                timeout=timeout,
                user=SANDBOX_USER,
            )
    except BootstrapError:
        raise
    except (CubeSandboxError, RuntimeError, OSError) as exc:
        # The SDK raises RuntimeError for execution failures, requests
        # transport errors are OSError subclasses, and local state setup can
        # raise OSError too — report all of them as one-line errors instead
        # of tracebacks. The type name keeps the broad tuple diagnosable:
        # without it a bare message cannot be traced back to its origin.
        print(
            f"[cubesandbox-exec] error: {type(exc).__name__}: {exc}",
            file=sys.stderr,
        )
        return 1

    if result.stdout:
        sys.stdout.write(result.stdout)
    if result.stderr:
        sys.stderr.write(result.stderr)
    return result.exit_code


def reset(session_id: str) -> None:
    with _session_lock(session_id):
        try:
            sandbox_id = _load_state(session_id).get("sandbox_id")
            if isinstance(sandbox_id, str) and sandbox_id:
                try:
                    _connect(sandbox_id).kill()
                except Exception:
                    pass  # best-effort kill; state is cleared regardless
        finally:
            _clear_state(session_id)
    # Outside the lock: the file cannot be unlinked while we hold it open as
    # our own mutex, and reset() is the only point where a session is known
    # to be finished, so this is where accumulated lock files get reaped.
    _remove_lock(session_id)
    print(f"[cubesandbox-exec] sandbox for session {session_id!r} destroyed")


def _positive_float(raw_value: str) -> float:
    """Parse a finite positive number for argparse."""
    try:
        value = float(raw_value)
    except ValueError as exc:
        raise argparse.ArgumentTypeError("timeout must be positive") from exc
    if not math.isfinite(value) or value <= 0:
        raise argparse.ArgumentTypeError("timeout must be positive")
    return value


def _default_exec_timeout() -> float:
    try:
        return _positive_float(os.getenv("CUBE_EXEC_TIMEOUT", "120"))
    except argparse.ArgumentTypeError as exc:
        raise BootstrapError(
            "CUBE_EXEC_TIMEOUT must be a finite positive number"
        ) from exc


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="Run a shell command in a cached CubeSandbox MicroVM"
    )
    parser.add_argument("command", nargs="?", help="shell command to execute")
    parser.add_argument("--session", default=DEFAULT_SESSION, help="sandbox cache key")
    parser.add_argument(
        "--timeout",
        type=_positive_float,
        default=None,
        help="command timeout in seconds",
    )
    parser.add_argument(
        "--mount",
        default=None,
        help="host directory to mount read-only when creating the sandbox",
    )
    parser.add_argument(
        "--reset", action="store_true", help="destroy the cached sandbox"
    )
    return parser


def main() -> int:
    parser = _parser()
    args = parser.parse_args()
    if not args.reset and args.command is None:
        parser.error("a command is required unless --reset is used")

    try:
        _bootstrap()
        if args.reset:
            reset(args.session)
            return 0
        timeout = args.timeout if args.timeout is not None else _default_exec_timeout()
        return run(args.command, args.session, timeout, args.mount)
    except BootstrapError as exc:
        print(f"[cubesandbox-exec] error: {exc}", file=sys.stderr)
        return 127
    except Exception as exc:
        # A hook must never dump a raw traceback into the Claude Code
        # transcript; keep the exception type so it stays diagnosable.
        print(
            f"[cubesandbox-exec] error: {type(exc).__name__}: {exc}",
            file=sys.stderr,
        )
        return 1


if __name__ == "__main__":
    sys.exit(main())
