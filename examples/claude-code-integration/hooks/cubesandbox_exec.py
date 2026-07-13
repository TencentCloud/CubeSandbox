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


def _connect(sandbox_id: str) -> Any:
    """Reconnect and refresh the sandbox TTL through the SDK connect call."""
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


def _try_cached_sandbox(session_id: str) -> Optional[Any]:
    sandbox_id = _load_state(session_id).get("sandbox_id")
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
        print(
            f"[cubesandbox-exec] warning: reconnect failed ({exc}); "
            "creating a new sandbox",
            file=sys.stderr,
        )
        return None


def _get_sandbox_locked(session_id: str, mount: Optional[str]) -> Any:
    """Return the session sandbox while the caller holds its session lock."""
    sandbox = _try_cached_sandbox(session_id)
    if sandbox is not None:
        return sandbox

    if not TEMPLATE_ID:
        raise BootstrapError("CUBE_TEMPLATE_ID is not set")

    sandbox = _create_sandbox(mount)
    _save_state(
        session_id,
        {
            "sandbox_id": sandbox.sandbox_id,
            "mount": mount,
            "state_token": secrets.token_urlsafe(24),
        },
    )
    return sandbox


def get_sandbox(session_id: str, mount: Optional[str]) -> Any:
    with _session_lock(session_id):
        return _get_sandbox_locked(session_id, mount)


def _state_shell(command: str, state_token: str, mount: Optional[str]) -> str:
    """Wrap a command so its cwd and exported environment persist."""
    state_dir = f"/tmp/.cubesandbox-state-{state_token}"
    cwd_file = f"{state_dir}/cwd"
    env_file = f"{state_dir}/env"
    state_dir_q = shlex.quote(state_dir)
    cwd_file_q = shlex.quote(cwd_file)
    env_file_q = shlex.quote(env_file)
    default_cwd = shlex.quote(mount) if mount else '"$HOME"'
    command_q = shlex.quote(command)

    return (
        f"umask 077; mkdir -p -- {state_dir_q}; "
        f"[ -d {state_dir_q} ] && [ ! -L {state_dir_q} ] || exit 1; "
        f"if [ -f {env_file_q} ] && [ ! -L {env_file_q} ]; then "
        f"source {env_file_q} >/dev/null 2>&1; fi; "
        f"if [ -f {cwd_file_q} ] && [ ! -L {cwd_file_q} ]; then "
        f'cd -- "$(cat -- {cwd_file_q})" 2>/dev/null || '
        f'cd -- {default_cwd} 2>/dev/null || cd -- "$HOME" || exit 1; '
        f'else cd -- {default_cwd} 2>/dev/null || cd -- "$HOME" || exit 1; fi; '
        f"eval -- {command_q}\n"
        "__CBX_STATUS__=$?; "
        f"if [ ! -e {cwd_file_q} ] || [ -f {cwd_file_q} ] && [ ! -L {cwd_file_q} ]; "
        f"then pwd > {cwd_file_q} 2>/dev/null; fi; "
        f"if [ ! -e {env_file_q} ] || [ -f {env_file_q} ] && [ ! -L {env_file_q} ]; "
        f"then export -p > {env_file_q} 2>/dev/null; fi; "
        'exit "$__CBX_STATUS__"'
    )


def run(
    command: str,
    session_id: str,
    timeout: Optional[float],
    mount: Optional[str],
) -> int:
    with _session_lock(session_id):
        sandbox = _get_sandbox_locked(session_id, mount)
        state_token = _load_state(session_id).get("state_token")
        if (
            not isinstance(state_token, str)
            or not state_token
            or not all(
                character.isalnum() or character in "-_" for character in state_token
            )
        ):
            print("[cubesandbox-exec] error: sandbox state is invalid", file=sys.stderr)
            return 1

        try:
            result = sandbox.commands.run(
                _state_shell(command, state_token, mount),
                timeout=timeout,
                user=SANDBOX_USER,
            )
        except CubeSandboxError as exc:
            print(f"[cubesandbox-exec] error: {exc}", file=sys.stderr)
            return 1

    if result.stdout:
        sys.stdout.write(result.stdout)
    if result.stderr:
        sys.stderr.write(result.stderr)
    return result.exit_code


def reset(session_id: str) -> None:
    with _session_lock(session_id):
        sandbox_id = _load_state(session_id).get("sandbox_id")
        if isinstance(sandbox_id, str) and sandbox_id:
            try:
                _connect(sandbox_id).kill()
            except CubeSandboxError:
                pass
        _clear_state(session_id)
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


if __name__ == "__main__":
    sys.exit(main())
