#!/usr/bin/env python3
"""Claude Code PreToolUse hook that redirects Bash commands to CubeSandbox."""

from __future__ import annotations

import json
import math
import shlex
import sys
from pathlib import Path
from typing import Any, Dict, Optional


EXEC_SCRIPT = Path(__file__).resolve().with_name("cubesandbox_exec.py")
DEFAULT_SESSION = "default"
MAX_TIMEOUT_MS = 60 * 60 * 1000


class HookInputError(ValueError):
    """Raised when a matched Bash hook payload cannot be rewritten safely."""


def _already_wrapped(command: str) -> bool:
    """Return True if *command* is already a hook-rewritten invocation.

    The hook wraps every Bash command as::

        <python> <.../cubesandbox_exec.py> --session=... -- <original command>

    If Claude Code (or the user) ever feeds such a command back through the
    hook, wrapping it a second time would run the executor inside the
    executor.  We detect our own wrapper by tokenizing with ``shlex`` and
    matching the executor script among the leading tokens.

    Fails safe: anything we cannot confidently parse as an existing wrapper
    returns ``False`` so the caller wraps it (the secure default).  A command
    carrying an unquoted newline (e.g. ``python\\nrm -rf /``) tokenizes such
    that the executor script is absent, so it is wrapped as a single quoted
    argument rather than mistaken for an already-wrapped call.
    """
    try:
        tokens = shlex.split(command)
    except ValueError:
        return False
    exec_name = EXEC_SCRIPT.name
    for token in tokens[:2]:
        if token == str(EXEC_SCRIPT) or token.rsplit("/", 1)[-1] == exec_name:
            return True
    return False


def rewrite_payload(payload: Any) -> Optional[Dict[str, Any]]:
    """Return a Claude hook response for a valid Bash tool payload."""
    if not isinstance(payload, dict):
        raise HookInputError("hook payload must be a JSON object")

    tool_name = payload.get("tool_name")
    if not isinstance(tool_name, str) or not tool_name:
        raise HookInputError("hook payload must include tool_name")
    if tool_name != "Bash":
        return None

    tool_input = payload.get("tool_input")
    if not isinstance(tool_input, dict):
        raise HookInputError("Bash tool_input must be a JSON object")

    command = tool_input.get("command")
    if not isinstance(command, str):
        raise HookInputError("Bash tool_input.command must be a string")

    # Idempotency: never wrap a command the hook already rewrote, otherwise
    # the executor would run inside the executor.
    if _already_wrapped(command):
        return None

    raw_session = payload.get("session_id")
    session_id = (
        raw_session if isinstance(raw_session, str) and raw_session else DEFAULT_SESSION
    )

    argv = [
        sys.executable,
        str(EXEC_SCRIPT),
        f"--session={session_id}",
    ]

    cwd = payload.get("cwd")
    if isinstance(cwd, str) and cwd:
        argv.append(f"--mount={cwd}")

    timeout_ms = tool_input.get("timeout")
    try:
        positive_timeout = (
            isinstance(timeout_ms, (int, float))
            and not isinstance(timeout_ms, bool)
            and math.isfinite(timeout_ms)
            and timeout_ms > 0
            and timeout_ms <= MAX_TIMEOUT_MS
        )
    except OverflowError:
        positive_timeout = False
    if positive_timeout:
        argv.append(f"--timeout={timeout_ms / 1000:.3f}")

    # ``--`` keeps commands beginning with a dash from becoming executor flags.
    argv.extend(["--", command])
    updated_input = dict(tool_input)
    updated_input["command"] = shlex.join(argv)

    return {
        "hookSpecificOutput": {
            "hookEventName": "PreToolUse",
            "permissionDecision": "allow",
            "updatedInput": updated_input,
        }
    }


def _fail_closed(message: str) -> int:
    try:
        print(f"[cubesandbox-rewrite] error: {message}", file=sys.stderr)
    except Exception:
        pass
    return 2


def main() -> int:
    try:
        payload = json.load(sys.stdin)
        output = rewrite_payload(payload)
        if output is not None:
            print(json.dumps(output))
    except (json.JSONDecodeError, UnicodeDecodeError) as exc:
        return _fail_closed(f"invalid hook JSON: {exc}")
    except HookInputError as exc:
        return _fail_closed(str(exc))
    except Exception as exc:
        return _fail_closed(f"unexpected hook failure: {exc}")

    return 0


if __name__ == "__main__":
    sys.exit(main())
