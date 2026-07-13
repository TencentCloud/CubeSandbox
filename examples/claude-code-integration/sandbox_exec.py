"""
Sandbox execution backend for Claude Code.

Claude Code runs on the HOST and calls this script to execute
untrusted code inside an isolated CubeSandbox MicroVM.

Usage from Claude Code:
    python sandbox_exec.py --code "print(1+1)"
    python sandbox_exec.py --file /path/to/script.py
    python sandbox_exec.py --cmd "ls -la /etc"
    python sandbox_exec.py --pip "requests" --code "import requests; ..."
"""

import argparse
import os
import shlex
import stat
import sys
from pathlib import Path
import tempfile
from dotenv import load_dotenv
from e2b_code_interpreter import Sandbox
from e2b.sandbox.commands.command_handle import CommandExitException

load_dotenv()

TEMPLATE_ID = os.getenv("CUBE_TEMPLATE_ID", "")
E2B_API_URL = os.getenv("E2B_API_URL", "http://127.0.0.1:3000")
E2B_API_KEY = os.getenv("E2B_API_KEY", "e2b_000000")

# Session file for cross-process sandbox reuse (used with --keep-alive)
SESSION_FILE = Path(tempfile.gettempdir()) / f"cubesandbox_claude_session_{os.getuid()}"


def _write_session(sandbox_id):
    flags = os.O_WRONLY | os.O_CREAT | os.O_TRUNC
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    fd = os.open(SESSION_FILE, flags, 0o600)
    with os.fdopen(fd, "w") as session_file:
        if not stat.S_ISREG(os.fstat(session_file.fileno()).st_mode):
            raise OSError(f"unsafe session file: {SESSION_FILE}")
        os.fchmod(session_file.fileno(), 0o600)
        session_file.write(sandbox_id)


def _read_session():
    flags = os.O_RDONLY
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        fd = os.open(SESSION_FILE, flags)
        with os.fdopen(fd, "r", encoding="utf-8") as session_file:
            if not stat.S_ISREG(os.fstat(session_file.fileno()).st_mode):
                return None
            return session_file.read().strip()
    except (OSError, UnicodeDecodeError):
        return None


# In-process cache: keep one sandbox alive for reuse, avoid cold starts
_sandbox = None


def _get_sandbox(timeout=300):
    """Get or create a reusable sandbox.

    Tries to reconnect to a sandbox saved by a previous --keep-alive run
    before creating a new one, so the cache effectively works across
    CLI invocations (each of which is a new process).
    """
    global _sandbox
    if _sandbox is None:
        # Try to reconnect to a session from a previous --keep-alive invocation
        sandbox_id = _read_session()
        if sandbox_id:
            try:
                _sandbox = Sandbox.connect(sandbox_id)
                _sandbox.set_timeout(timeout)  # refresh TTL
                return _sandbox
            except Exception:
                SESSION_FILE.unlink(missing_ok=True)  # stale session, clean up
        _sandbox = Sandbox.create(TEMPLATE_ID, timeout=timeout)
        _write_session(_sandbox.sandbox_id)
    return _sandbox


def _run(sandbox, cmd, timeout=120):
    """Run a command in the sandbox."""
    try:
        return sandbox.commands.run(cmd, timeout=timeout)
    except CommandExitException as e:
        return e


def exec_code(code, pip_packages=None, timeout=120):
    """Execute Python code in the sandbox."""
    sandbox = _get_sandbox(timeout + 60)

    if pip_packages:
        r = _run(
            sandbox,
            f"pip install {' '.join(shlex.quote(p) for p in pip_packages)}",
            timeout=60,
        )
        if r.exit_code != 0:
            return f"[pip error] {r.stderr}"

    result = _run(sandbox, f"python3 -c {shlex.quote(code)}", timeout=timeout)
    if result.exit_code == 0:
        return result.stdout
    else:
        return f"[error] {result.stderr}"


def exec_file(filepath, timeout=120):
    """Copy a local file into the sandbox and execute it."""
    sandbox = _get_sandbox(timeout + 60)
    with open(filepath, "r") as f:
        content = f.read()

    sandbox.files.write("/tmp/script.py", content)
    result = _run(sandbox, "python3 /tmp/script.py", timeout=timeout)
    if result.exit_code == 0:
        return result.stdout
    else:
        return f"[error] {result.stderr}"


def exec_cmd(command, timeout=120):
    """Execute an arbitrary shell command in the sandbox."""
    sandbox = _get_sandbox(timeout + 60)
    result = _run(sandbox, command, timeout=timeout)
    if result.exit_code == 0:
        return result.stdout
    else:
        return f"[error] {result.stderr}"


def cleanup():
    """Destroy the cached sandbox and clear the session file."""
    global _sandbox
    if _sandbox:
        try:
            _sandbox.kill()
        except Exception:
            pass
        _sandbox = None
    SESSION_FILE.unlink(missing_ok=True)


def main():
    parser = argparse.ArgumentParser(
        description="Execute code in a CubeSandbox sandbox"
    )
    parser.add_argument("--code", help="Python code to execute")
    parser.add_argument("--file", help="Local Python file to execute in sandbox")
    parser.add_argument("--cmd", help="Shell command to execute")
    parser.add_argument("--pip", nargs="+", help="Pip packages to install first")
    parser.add_argument("--timeout", type=int, default=120, help="Execution timeout")
    parser.add_argument(
        "--keep-alive",
        action="store_true",
        help="Keep sandbox alive after execution; next invocation "
        "reconnects to it via session file",
    )
    args = parser.parse_args()

    if not TEMPLATE_ID:
        print("Error: CUBE_TEMPLATE_ID not set in .env")
        sys.exit(1)

    try:
        if args.code:
            print(exec_code(args.code, args.pip, args.timeout))
        elif args.file:
            print(exec_file(args.file, args.timeout))
        elif args.cmd:
            print(exec_cmd(args.cmd, args.timeout))
        else:
            parser.print_help()
    finally:
        if not args.keep_alive:
            cleanup()


if __name__ == "__main__":
    main()
