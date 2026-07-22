#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Sandbox execution backend for CodeBuddy.

CodeBuddy runs on the HOST (or inside an outer sandbox) and calls this
script whenever it needs to execute untrusted code or shell commands inside
an isolated CubeSandbox MicroVM. The pattern mirrors the same idea behind
Claude Code's Bash hook — keep the LLM agent outside the danger zone,
route every executable action through a fresh, disposable VM.

Usage from a Python orchestrator or a shell:

    python sandbox_exec.py --code "print(1+1)"
    python sandbox_exec.py --file /path/to/script.py
    python sandbox_exec.py --cmd "ls -la /workspace"
    python sandbox_exec.py --pip requests --code "import requests; print(requests.__version__)"
    python sandbox_exec.py --keep-alive --code "state = 42"     # reuse on next call
    python sandbox_exec.py --reset --session session-id          # force a fresh one
"""

from __future__ import annotations

import argparse
import os
import shlex
import stat
import sys
import tempfile
import threading
from pathlib import Path

from dotenv import load_dotenv
from e2b_code_interpreter import Sandbox

load_dotenv()

# --- Configuration -----------------------------------------------------------

# CUBE_API_URL / CUBE_API_KEY are the canonical names (documented in .env.example).
# E2B_API_URL / E2B_API_KEY are accepted as legacy aliases so existing deployments
# that only set the E2B_ names continue to work without changes.
E2B_API_URL = os.getenv("CUBE_API_URL") or os.getenv("E2B_API_URL", "http://127.0.0.1:3000")
E2B_API_KEY = os.getenv("CUBE_API_KEY") or os.getenv("E2B_API_KEY", "e2b_000000")
TEMPLATE_ID = os.getenv("CUBE_TEMPLATE_ID", "")

# Maximum command length to prevent resource exhaustion.
MAX_COMMAND_LENGTH = 65536

# Allowed directories for file operations (restrictive by default).
# Can be extended via ALLOWED_READ_DIRS environment variable (colon-separated).
_ALLOWED_READ_DIRS = [Path.cwd()]
if os.getenv("ALLOWED_READ_DIRS"):
    for d in os.getenv("ALLOWED_READ_DIRS", "").split(":"):
        if d:
            _ALLOWED_READ_DIRS.append(Path(d).resolve())

# The session file lives under gettempdir() so `mktemp`/`/tmp` rotations can
# reclaim it, but is scoped to the invoking UID and locked down with 0600 so
# other users on a shared host cannot hijack `Sandbox.connect()` on the next
# invocation. The O_NOFOLLOW + S_ISREG double-check guards against symlink
# races that would otherwise let an attacker point us at an arbitrary file
# before the write happens.
SESSION_FILE = Path(tempfile.gettempdir()) / f"cubesandbox_codebuddy_session_{os.getuid()}"

# Thread lock for sandbox access (prevents race conditions in multi-threaded usage).
_sandbox_lock = threading.Lock()

# In-process cache so consecutive --keep-alive calls inside the same Python
# interpreter don't pay cold-start cost. Cross-process reuse goes through
# SESSION_FILE.
_sandbox: "Sandbox | None" = None


def _write_session(sandbox_id: str) -> None:
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        fd = os.open(SESSION_FILE, flags, 0o600)
    except FileExistsError:
        # O_EXCL fails if file exists — unlink and retry.
        SESSION_FILE.unlink(missing_ok=True)
        fd = os.open(SESSION_FILE, flags, 0o600)
    except OSError as e:
        if hasattr(e, "errno") and e.errno == 40:  # ELOOP - symlink
            raise OSError(f"session file is a symlink: {SESSION_FILE}") from e
        raise

    try:
        # Immediately verify it's a regular file before writing
        st = os.fstat(fd)
        if not stat.S_ISREG(st.st_mode):
            raise OSError(f"session file is not a regular file: {SESSION_FILE}")
        os.ftruncate(fd, 0)
        os.write(fd, sandbox_id.encode("utf-8"))
        os.fsync(fd)
    finally:
        os.close(fd)


def _read_session() -> str | None:
    flags = os.O_RDONLY
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        fd = os.open(SESSION_FILE, flags)
    except OSError:
        return None
    try:
        st = os.fstat(fd)
        if not stat.S_ISREG(st.st_mode):
            return None
        with os.fdopen(fd, "r", encoding="utf-8") as fp:
            value = fp.read().strip()
            return value or None
    except (OSError, UnicodeDecodeError):
        return None


def _get_sandbox(timeout: int = 300) -> "Sandbox":
    """Get or create a reusable sandbox, reconnecting via session file when possible."""
    global _sandbox
    with _sandbox_lock:
        if _sandbox is not None:
            try:
                _sandbox.set_timeout(timeout)  # refresh TTL
                return _sandbox
            except Exception:
                try:
                    _sandbox.kill()
                except Exception:
                    pass
                _sandbox = None

        # Try cross-process reuse first.
        sandbox_id = _read_session()
        if sandbox_id:
            try:
                from e2b_code_interpreter import Sandbox
                _sandbox = Sandbox.connect(sandbox_id)
                _sandbox.set_timeout(timeout)
                return _sandbox
            except Exception:
                SESSION_FILE.unlink(missing_ok=True)

        if not TEMPLATE_ID:
            raise SystemExit(
                "CUBE_TEMPLATE_ID is not set. Set it in your .env or pass it via the environment."
            )
        from e2b_code_interpreter import Sandbox
        _sandbox = Sandbox.create(TEMPLATE_ID, timeout=timeout)
        _write_session(_sandbox.sandbox_id)
        return _sandbox


def _run(sandbox: "Sandbox", cmd: str, timeout: int = 120):
    """Run a command in the sandbox, normalizing non-zero exits into a result object.

    The e2b / cubesandbox SDK raises CommandExitException on non-zero exits;
    swallowing it here lets callers handle success / failure uniformly instead
    of catching at every call site.
    """
    from e2b.sandbox.commands.command_handle import CommandExitException
    try:
        return sandbox.commands.run(cmd, timeout=timeout)
    except CommandExitException as exc:
        return exc


# --- Public API --------------------------------------------------------------

def exec_code(code: str, pip_packages: list[str] | None = None, timeout: int = 120) -> str:
    """Execute Python code in the sandbox and return stdout (or stderr on failure)."""
    sandbox = _get_sandbox(timeout + 60)
    if pip_packages:
        r = _run(
            sandbox,
            "pip install " + " ".join(shlex.quote(p) for p in pip_packages),
            timeout=60,
        )
        if r.exit_code != 0:
            return f"[pip error] command failed (exit {r.exit_code})"
    result = _run(sandbox, f"python3 -c {shlex.quote(code)}", timeout=timeout)
    if result.exit_code == 0:
        return result.stdout or ""
    return f"[error] exit code {result.exit_code}"


def exec_file(filepath: str, timeout: int = 120) -> str:
    """Copy a local file into the sandbox and execute it.

    Security: filepath is validated to be within allowed directories before reading.
    Symlinks and absolute paths outside allowed directories are rejected.
    """
    # Resolve and validate the filepath
    try:
        resolved = Path(filepath).resolve()
    except (ValueError, OSError):
        return "[error] invalid filepath"

    # Reject symlinks - check if the original path is a symlink
    # (realpath resolves all symlinks, so check original first)
    try:
        if Path(filepath).is_symlink():
            return "[error] symlinks not allowed"
    except (ValueError, OSError):
        pass

    # Also check if any component in the resolved path is a symlink
    # (this catches cases like /allowed/dir -> /malicious where /allowed/dir itself is a symlink)
    parts = Path(filepath).parts
    for i in range(1, len(parts) + 1):
        partial = Path(*parts[:i])
        try:
            if partial.is_symlink():
                return "[error] symlinks not allowed"
        except (ValueError, OSError):
            pass

    # Check if path is within any allowed directory
    if not any(str(resolved).startswith(str(d)) for d in _ALLOWED_READ_DIRS):
        return "[error] filepath not in allowed directories"

    sandbox = _get_sandbox(timeout + 60)
    try:
        with open(filepath, "r", encoding="utf-8") as fp:
            content = fp.read()
    except OSError:
        return "[error] cannot read file"
    except UnicodeDecodeError:
        return "[error] file is not valid UTF-8"

    sandbox.files.write("/tmp/codebuddy_script.py", content)
    result = _run(sandbox, "python3 /tmp/codebuddy_script.py", timeout=timeout)
    if result.exit_code == 0:
        return result.stdout or ""
    return f"[error] exit code {result.exit_code}"


def exec_cmd(command: str, timeout: int = 120) -> str:
    """Execute an arbitrary shell command in the sandbox.

    Note: Commands are executed inside an isolated MicroVM, so the blast
    radius of a malicious command is limited to that sandbox. However, be
    aware that:
    - The sandbox may have access to API keys injected via environment.
    - Resource exhaustion attacks (infinite loops, memory allocation) are possible.
    """
    if not isinstance(command, str):
        return "[error] command must be a string"
    if len(command) > MAX_COMMAND_LENGTH:
        return f"[error] command exceeds maximum length of {MAX_COMMAND_LENGTH} bytes"
    if not command.strip():
        return "[error] empty command"

    sandbox = _get_sandbox(timeout + 60)
    result = _run(sandbox, command, timeout=timeout)
    if result.exit_code == 0:
        return result.stdout or ""
    return f"[error] exit code {result.exit_code}"


def r_stderr(result) -> str:
    """Extract stderr from a result object, sanitizing internal details."""
    stderr = getattr(result, "stderr", None)
    if stderr:
        # Truncate to prevent large error output from leaking internal details
        return stderr[:2048].strip()
    exit_code = getattr(result, "exit_code", None)
    if exit_code is not None:
        return f"exit code {exit_code}"
    return "unknown error"


def cleanup() -> None:
    """Destroy the cached sandbox and clear the session file."""
    global _sandbox
    with _sandbox_lock:
        if _sandbox is not None:
            try:
                _sandbox.kill()
            except Exception:
                pass
            _sandbox = None
        try:
            SESSION_FILE.unlink(missing_ok=True)
        except OSError:
            pass


# --- CLI ---------------------------------------------------------------------

def main() -> None:
    parser = argparse.ArgumentParser(
        description="Execute code or shell commands in an isolated CubeSandbox MicroVM."
    )
    parser.add_argument("--code", help="Python code to execute")
    parser.add_argument("--file", help="Local Python file to copy into the sandbox and execute")
    parser.add_argument("--cmd", help="Shell command to execute")
    parser.add_argument("--pip", nargs="+", help="Pip packages to install before --code")
    parser.add_argument("--timeout", type=int, default=120, help="Execution timeout in seconds")
    parser.add_argument(
        "--keep-alive",
        action="store_true",
        help="Keep the sandbox alive after this invocation so the next call "
             "can reconnect via the session file instead of cold-starting.",
    )
    parser.add_argument(
        "--reset",
        action="store_true",
        help="Destroy the cached sandbox before running, then exit.",
    )
    parser.add_argument(
        "--session",
        default=None,
        help="(Reserved) per-session identifier. Currently unused — session reuse "
             "is process-global. Documented so future per-session sandboxes "
             "do not break callers.",
    )
    args = parser.parse_args()

    if args.reset:
        cleanup()
        print("Sandbox destroyed. A new one will be created on next use.")
        return

    if not any((args.code, args.file, args.cmd)):
        parser.print_help()
        sys.exit(2)

    if not TEMPLATE_ID:
        print("Error: CUBE_TEMPLATE_ID not set in .env", file=sys.stderr)
        sys.exit(1)

    try:
        if args.code:
            print(exec_code(args.code, args.pip, args.timeout))
        elif args.file:
            print(exec_file(args.file, args.timeout))
        elif args.cmd:
            print(exec_cmd(args.cmd, args.timeout))
    finally:
        if not args.keep_alive:
            cleanup()


if __name__ == "__main__":
    main()
