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
import sys
from dotenv import load_dotenv
from e2b_code_interpreter import Sandbox
from e2b.sandbox.commands.command_handle import CommandExitException

load_dotenv()

TEMPLATE_ID = os.getenv("CUBE_TEMPLATE_ID", "")
E2B_API_URL = os.getenv("E2B_API_URL", "http://127.0.0.1:3000")
E2B_API_KEY = os.getenv("E2B_API_KEY", "e2b_000000")

# Cache: keep one sandbox alive for reuse within a single process.
# NOTE: Each CLI invocation (``python sandbox_exec.py ...``) is a separate
# Python process, so this cache only helps when sandbox_exec is imported
# and called repeatedly as a module (e.g. by mcp_server.py).  For
# cross-process sandbox reuse — where the same sandbox survives across
# many CLI calls — use ``cubesandbox_exec.py``, which persists sandbox
# IDs to disk via a file-based session cache.
_sandbox = None


def _get_sandbox(timeout=300):
    """Get or create a reusable sandbox."""
    global _sandbox
    if _sandbox is None:
        _sandbox = Sandbox.create(TEMPLATE_ID, timeout=timeout)
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
        r = _run(sandbox, f"pip install {' '.join(shlex.quote(p) for p in pip_packages)}", timeout=60)
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
    """Destroy the cached sandbox."""
    global _sandbox
    if _sandbox:
        _sandbox.kill()
        _sandbox = None


def main():
    parser = argparse.ArgumentParser(
        description="Execute code in a CubeSandbox sandbox"
    )
    parser.add_argument("--code", help="Python code to execute")
    parser.add_argument("--file", help="Local Python file to execute in sandbox")
    parser.add_argument("--cmd", help="Shell command to execute")
    parser.add_argument("--pip", nargs="+", help="Pip packages to install first")
    parser.add_argument("--timeout", type=int, default=120, help="Execution timeout")
    parser.add_argument("--keep-alive", action="store_true",
                        help="Keep sandbox alive after execution")
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
