#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Sandbox execution backend for OpenCode.

OpenCode itself runs on the HOST (or inside an outer sandbox) and calls this
script whenever it needs to execute untrusted code or shell commands inside
an isolated CubeSandbox MicroVM. The pattern mirrors the same idea behind
Claude Code's Bash hook or Codex's sandbox backend — keep the LLM agent
outside the danger zone, route every executable action through a fresh,
disposable VM.

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

# The session file lives under gettempdir() so `mktemp`/`/tmp` rotations can
# reclaim it, but is scoped to the invoking UID and locked down with 0600 so
# other users on a shared host cannot hijack `Sandbox.connect()` on the next
# invocation. The O_NOFOLLOW + S_ISREG double-check guards against symlink
# races that would otherwise let an attacker point us at an arbitrary file
# before the write happens.
SESSION_FILE = Path(tempfile.gettempdir()) / f"cubesandbox_opencode_session_{os.getuid()}"


def _write_session(sandbox_id: str) -> None:
    flags = os.O_WRONLY | os.O_CREAT | os.O_TRUNC
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    fd = os.open(SESSION_FILE, flags, 0o600)
    with os.fdopen(fd, "w") as fp:
        if not stat.S_ISREG(os.fstat(fp.fileno()).st_mode):
            raise OSError(f"unsafe session file: {SESSION_FILE}")
        # Re-affirm 0600 in case the file already existed with lax perms.
        os.fchmod(fp.fileno(), 0o600)
        fp.write(sandbox_id)


def _read_session() -> str | None:
    flags = os.O_RDONLY
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        fd = os.open(SESSION_FILE, flags)
    except OSError:
        return None
    try:
        with os.fdopen(fd, "r", encoding="utf-8") as fp:
            if not stat.S_ISREG(os.fstat(fp.fileno()).st_mode):
                return None
            value = fp.read().strip()
            return value or None
    except (OSError, UnicodeDecodeError):
        return None


# In-process cache so consecutive --keep-alive calls inside the same Python
# interpreter don't pay cold-start cost. Cross-process reuse goes through
# SESSION_FILE.
_sandbox: Sandbox | None = None


def _get_sandbox(timeout: int = 300) -> Sandbox:
    """Get or create a reusable sandbox, reconnecting via session file when possible."""
    global _sandbox
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
            _sandbox = Sandbox.connect(sandbox_id)
            _sandbox.set_timeout(timeout)
            return _sandbox
        except Exception:
            SESSION_FILE.unlink(missing_ok=True)

    if not TEMPLATE_ID:
        raise SystemExit(
            "CUBE_TEMPLATE_ID is not set. Set it in your .env or pass it via the environment."
        )
    _sandbox = Sandbox.create(TEMPLATE_ID, timeout=timeout)
    _write_session(_sandbox.sandbox_id)
    return _sandbox


def _run(sandbox: Sandbox, cmd: str, timeout: int = 120):
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
            return f"[pip error] {r.stderr}"
    result = _run(sandbox, f"python3 -c {shlex.quote(code)}", timeout=timeout)
    if result.exit_code == 0:
        return result.stdout
    return f"[error] {r_stderr(result)}"


def exec_file(filepath: str, timeout: int = 120) -> str:
    """Copy a local file into the sandbox and execute it."""
    sandbox = _get_sandbox(timeout + 60)
    try:
        with open(filepath, "r", encoding="utf-8") as fp:
            content = fp.read()
    except OSError as exc:
        return f"[error] cannot read {filepath}: {exc}"
    sandbox.files.write("/tmp/opencode_script.py", content)
    result = _run(sandbox, "python3 /tmp/opencode_script.py", timeout=timeout)
    if result.exit_code == 0:
        return result.stdout
    return f"[error] {r_stderr(result)}"


def exec_cmd(command: str, timeout: int = 120) -> str:
    """Execute an arbitrary shell command in the sandbox."""
    sandbox = _get_sandbox(timeout + 60)
    result = _run(sandbox, command, timeout=timeout)
    if result.exit_code == 0:
        return result.stdout
    return f"[error] {r_stderr(result)}"


def r_stderr(result) -> str:
    """Extract stderr from a CommandExitException (which has no .stderr attr) or a result."""
    return getattr(result, "stderr", "") or str(result)


def cleanup() -> None:
    """Destroy the cached sandbox and clear the session file."""
    global _sandbox
    if _sandbox is not None:
        try:
            _sandbox.kill()
        except Exception:
            pass
        _sandbox = None
    SESSION_FILE.unlink(missing_ok=True)


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
             "(e.g. one per Claude Code session_id) do not break callers.",
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