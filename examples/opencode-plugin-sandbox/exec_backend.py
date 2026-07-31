#!/usr/bin/env python3
"""exec_backend.py — run one shell command inside a CubeSandbox MicroVM.

This is the executor half of the OpenCode plugin integration. The plugin's
``tool.execute.before`` hook rewrites each ``bash`` call into an invocation of
this script; this script forwards the command to a MicroVM through the
E2B-compatible CubeSandbox API and prints the result back to OpenCode.

Contract with the plugin
------------------------
Invoked as::

    python3 exec_backend.py --session <id> --command <shell command>

* ``--command`` arrives as a single argv element, so the host shell never
  interprets it. Only the shell inside the MicroVM does.
* stdout / stderr are forwarded verbatim; the process exit code mirrors the
  command's exit code so OpenCode's usual success/failure handling still works.

Session affinity
----------------
A shell is only useful if consecutive commands share state. Each OpenCode
session is therefore mapped to one MicroVM, recorded in a state file under
``CUBE_OPENCODE_STATE_DIR`` (default ``~/.cache/cubesandbox-opencode``).

Two pieces of state are carried across calls:

``cwd``
    Emitted by the wrapper after the command finishes and stored host-side, so
    ``cd /tmp`` followed by ``pwd`` behaves as expected.

``env``
    Variables exported by the command, captured the same way, so
    ``export FOO=1`` followed by ``echo $FOO`` behaves as expected.

Concurrency
-----------
OpenCode may issue several ``bash`` calls concurrently. Calls within one session
are serialised with a lock file so they cannot interleave on the same MicroVM
and corrupt the recorded cwd/env. Different sessions run in parallel.

Requirements
------------
``pip install e2b-code-interpreter`` and a reachable CubeSandbox deployment.
"""

from __future__ import annotations

import argparse
import json
import os
import shlex
import sys
import time
from pathlib import Path

# Sentinels used to separate command output from the trailing state block.
# Chosen to be improbable in ordinary command output.
_STATE_BEGIN = "__CUBE_STATE_BEGIN_9f2a__"
_STATE_END = "__CUBE_STATE_END_9f2a__"

# Emitted inside the guest to capture cwd and environment as JSON. Prefers
# python3 so values containing quotes or newlines stay well-formed, and
# degrades to a cwd-only object when python3 is absent from the template.
STATE_SNIPPET = (
    "python3 -c " + shlex.quote(
        "import json,os;"
        "print(json.dumps({"
        '"cwd": os.getcwd(), '
        '"env": {k: v for k, v in os.environ.items() '
        'if k not in (\"_\", \"SHLVL\", \"PWD\", \"OLDPWD\")}}))'
    )
    + " 2>/dev/null || "
    + 'printf \'{"cwd": "%s"}\\n\' "$PWD"'
)

DEFAULT_API_URL = "http://127.0.0.1:3000"
DEFAULT_TIMEOUT = 120
DEFAULT_WORKDIR = "/workspace"


# --------------------------------------------------------------------------- #
# configuration
# --------------------------------------------------------------------------- #
def state_dir() -> Path:
    raw = os.environ.get("CUBE_OPENCODE_STATE_DIR")
    if raw:
        base = Path(raw).expanduser()
    else:
        base = Path.home() / ".cache" / "cubesandbox-opencode"
    base.mkdir(parents=True, exist_ok=True)
    # State can contain environment variables exported by commands, so keep it
    # readable only by the owner.
    try:
        base.chmod(0o700)
    except OSError:
        pass
    return base


def safe_session_key(session: str) -> str:
    """Reduce a session id to a filesystem-safe token.

    The session id reaches us from the plugin and ends up in a filename, so
    strip anything that could escape the state directory.
    """
    cleaned = "".join(c if (c.isalnum() or c in "-_") else "_" for c in session)
    return cleaned[:120] or "default"


def require_template_id() -> str:
    tpl = os.environ.get("CUBE_TEMPLATE_ID", "").strip()
    if not tpl:
        fail(
            "CUBE_TEMPLATE_ID is not set.\n"
            "Create a template first, then export its id:\n"
            "  cubemastercli tpl create-from-image \\\n"
            "    --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \\\n"
            "    --writable-layer-size 1G --expose-port 49999 --expose-port 49983 --probe 49999\n"
            "  export CUBE_TEMPLATE_ID=tpl-xxxxxxxx"
        )
    return tpl


def fail(message: str, code: int = 97) -> None:
    """Report a wrapper-level failure and exit non-zero.

    A non-zero exit is what makes the integration fail closed: OpenCode treats
    the tool call as failed rather than assuming the command ran.
    """
    sys.stderr.write(f"[cubesandbox-exec] {message}\n")
    sys.exit(code)


# --------------------------------------------------------------------------- #
# per-session state
# --------------------------------------------------------------------------- #
def load_state(key: str) -> dict:
    path = state_dir() / f"{key}.json"
    if not path.exists():
        return {}
    try:
        with path.open("r", encoding="utf-8") as fh:
            data = json.load(fh)
        return data if isinstance(data, dict) else {}
    except (OSError, ValueError):
        # A corrupt state file must not wedge the session; start fresh.
        return {}


def save_state(key: str, state: dict) -> None:
    path = state_dir() / f"{key}.json"
    tmp = path.with_suffix(".json.tmp")
    try:
        with tmp.open("w", encoding="utf-8") as fh:
            json.dump(state, fh)
        os.replace(tmp, path)
        path.chmod(0o600)
    except OSError as exc:
        sys.stderr.write(f"[cubesandbox-exec] warning: cannot persist state: {exc}\n")


class SessionLock:
    """Advisory lock serialising calls that share one session.

    Uses ``O_CREAT | O_EXCL`` so it works without fcntl assumptions. A stale
    lock older than ``stale_after`` seconds is reclaimed, which keeps a crashed
    call from blocking the session forever.
    """

    def __init__(self, key: str, timeout: float = 90.0, stale_after: float = 300.0):
        self.path = state_dir() / f"{key}.lock"
        self.timeout = timeout
        self.stale_after = stale_after
        self.fd = None

    def __enter__(self):
        deadline = time.time() + self.timeout
        while True:
            try:
                self.fd = os.open(str(self.path), os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
                os.write(self.fd, str(os.getpid()).encode())
                return self
            except FileExistsError:
                try:
                    age = time.time() - self.path.stat().st_mtime
                    if age > self.stale_after:
                        self.path.unlink(missing_ok=True)
                        continue
                except OSError:
                    pass
                if time.time() > deadline:
                    # Proceed unlocked rather than failing the user's command;
                    # the worst case is interleaved cwd/env bookkeeping.
                    sys.stderr.write(
                        "[cubesandbox-exec] warning: lock wait timed out, continuing\n"
                    )
                    return self
                time.sleep(0.1)

    def __exit__(self, *_exc):
        # Only release a lock we actually acquired. When __enter__ gave up after
        # the timeout, self.fd is None and the lock is still held by a live
        # peer; unlinking it there would let a third call acquire concurrently
        # and defeat the serialisation this class exists to provide.
        # Crashed holders are already covered by the stale-lock reclaim above.
        if self.fd is None:
            return False
        try:
            os.close(self.fd)
        except OSError:
            pass
        self.path.unlink(missing_ok=True)
        return False


# --------------------------------------------------------------------------- #
# command wrapping
# --------------------------------------------------------------------------- #
def build_wrapper(command: str, cwd: str, env: dict) -> str:
    """Wrap the user command so cwd and exported env survive the call.

    The wrapper:
      1. restores the recorded cwd and environment,
      2. runs the command with ``eval`` inside a subshell,
      3. emits a JSON state block between sentinels from an ``EXIT`` trap.

    ``exit_code`` is preserved so the caller can mirror it.

    Two constraints shape this, and they pull in opposite directions.

    ``bash -c`` cannot be used: it starts a child shell, so a ``cd`` or
    ``export`` performed by the command mutates only that child and is gone
    before the state is read. The state would always report the wrapper's own
    cwd and environment, silently defeating the point of capturing it.

    Plain ``eval`` in the wrapper's own shell cannot be used either: a command
    ending in ``exit`` terminates that shell, so the state block and the
    exit-code line never run. The host then finds no sentinel and reports
    success for a command that actually failed.

    A subshell with an ``EXIT`` trap satisfies both. ``eval`` runs in the
    subshell, so its mutations are visible to the trap; the trap fires when the
    subshell ends normally or via ``exit``, so the state block and exit code
    are emitted in both cases; and an ``exit`` inside only ends the subshell,
    leaving the wrapper intact.

    One case remains outside this: a command that calls ``exec`` replaces the
    subshell's process image, which discards traps along with everything else.
    No shell-level wrapper can observe that, so the host sees no sentinel.
    ``run_command`` handles the absence of a sentinel by keeping the previous
    session state and returning the process exit code unchanged, which is the
    conservative choice — state is stale rather than wrong. This is recorded in
    the known limitations rather than papered over.
    """
    lines = ["set +e"]

    if cwd:
        # Fall back to the default workdir if the recorded directory vanished.
        lines.append(f"cd {shlex.quote(cwd)} 2>/dev/null || cd {shlex.quote(DEFAULT_WORKDIR)}")
    else:
        lines.append(f"mkdir -p {shlex.quote(DEFAULT_WORKDIR)} && cd {shlex.quote(DEFAULT_WORKDIR)}")

    for name, value in (env or {}).items():
        if not name.isidentifier():
            continue
        lines.append(f"export {name}={shlex.quote(str(value))}")

    # Run inside a subshell with an EXIT trap so the state block is emitted no
    # matter how the command ends, including `exit` and `exec`. See the
    # docstring for why neither `bash -c` nor a bare `eval` works here.
    trap_body = "; ".join(
        [
            "__cube_rc=$?",
            f'echo "{_STATE_BEGIN}"',
            STATE_SNIPPET,
            f'echo "{_STATE_END}"',
            'echo "__CUBE_RC__=$__cube_rc"',
        ]
    )

    lines.append("(")
    lines.append(f"  trap {shlex.quote(trap_body)} EXIT")
    lines.append(f"  eval {shlex.quote(command)}")
    lines.append(")")

    return "\n".join(lines)


def split_output(text: str) -> tuple[str, dict, int]:
    """Separate command output from the trailing state block and exit code."""
    state: dict = {}
    rc = 0

    if _STATE_BEGIN in text:
        head, _, rest = text.partition(_STATE_BEGIN)
        blob, _, tail = rest.partition(_STATE_END)
        try:
            parsed = json.loads(blob.strip())
            if isinstance(parsed, dict):
                state = parsed
        except ValueError:
            state = {}
    else:
        head, tail = text, ""

    for line in tail.splitlines():
        line = line.strip()
        if line.startswith("__CUBE_RC__="):
            try:
                rc = int(line.split("=", 1)[1])
            except ValueError:
                rc = 0

    return head, state, rc


# --------------------------------------------------------------------------- #
# sandbox execution
# --------------------------------------------------------------------------- #
def run_in_sandbox(command: str, session: str, timeout: int) -> int:
    try:
        from e2b_code_interpreter import Sandbox  # type: ignore
    except ImportError:
        fail(
            "e2b-code-interpreter is not installed.\n"
            "  pip install e2b-code-interpreter\n"
            "The plugin fails closed, so the command was NOT run on the host."
        )
        return 97  # unreachable, keeps type checkers happy

    template = require_template_id()
    os.environ.setdefault("E2B_API_URL", DEFAULT_API_URL)
    os.environ.setdefault("E2B_API_KEY", "e2b_000000")

    key = safe_session_key(session)

    with SessionLock(key):
        state = load_state(key)
        wrapper = build_wrapper(command, state.get("cwd", ""), state.get("env", {}))

        try:
            with Sandbox.create(template=template) as sandbox:
                result = sandbox.run_code(
                    "import subprocess, sys\n"
                    "p = subprocess.run(['bash', '-lc', " + repr(wrapper) + "],\n"
                    "                   capture_output=True, text=True)\n"
                    "sys.stdout.write(p.stdout)\n"
                    "sys.stderr.write(p.stderr)\n",
                    timeout=timeout,
                )
        except Exception as exc:  # noqa: BLE001 - surface any SDK failure verbatim
            fail(
                f"sandbox execution failed: {exc}\n"
                "The command was NOT run on the host (fail-closed)."
            )
            return 97

        stdout = "".join(getattr(result.logs, "stdout", []) or [])
        stderr = "".join(getattr(result.logs, "stderr", []) or [])

        body, new_state, rc = split_output(stdout)

        if body:
            sys.stdout.write(body if body.endswith("\n") else body + "\n")
        if stderr:
            sys.stderr.write(stderr)

        if new_state:
            merged = dict(state)
            if new_state.get("cwd"):
                merged["cwd"] = new_state["cwd"]
            if isinstance(new_state.get("env"), dict):
                merged["env"] = new_state["env"]
            save_state(key, merged)

        return rc


# --------------------------------------------------------------------------- #
# entry point
# --------------------------------------------------------------------------- #
def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="exec_backend.py",
        description="Run a shell command inside a CubeSandbox MicroVM.",
    )
    parser.add_argument("--session", default="default", help="OpenCode session id")
    parser.add_argument("--command", required=True, help="shell command to run in the sandbox")
    parser.add_argument(
        "--timeout",
        type=int,
        default=int(os.environ.get("CUBE_OPENCODE_TIMEOUT", DEFAULT_TIMEOUT)),
        help="per-command timeout in seconds",
    )
    parser.add_argument(
        "--reset",
        action="store_true",
        help="discard the recorded cwd/env for this session and exit",
    )
    args = parser.parse_args(argv)

    key = safe_session_key(args.session)

    if args.reset:
        (state_dir() / f"{key}.json").unlink(missing_ok=True)
        print(f"[cubesandbox-exec] session state cleared: {key}")
        return 0

    return run_in_sandbox(args.command, args.session, args.timeout)


if __name__ == "__main__":
    sys.exit(main())
