#!/usr/bin/env python3
"""
PreToolUse hook: transparently redirect every Bash command Claude Code runs
into an isolated CubeSandbox MicroVM.

Registered (by install.sh) in ~/.claude/settings.json as:

{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {"type": "command", "command": "~/.claude/hooks/cubesandbox_rewrite.py"}
        ]
      }
    ]
  }
}

For every Bash tool call, Claude Code pipes a JSON payload like:

{
  "session_id": "abc123",
  "cwd": "/home/user/myproject",
  "hook_event_name": "PreToolUse",
  "tool_name": "Bash",
  "tool_input": {"command": "npm test", "timeout": 120000}
}

This script rewrites tool_input.command to:

  cubesandbox-exec --session abc123 --mount /home/user/myproject --timeout 120.000 'npm test'

and returns it via `updatedInput`, so Claude Code executes the *rewritten*
command with its normal Bash tool -- the AI never sees the sandbox layer.
"""

from __future__ import annotations

import json
import shlex
import sys

EXEC_BIN = "cubesandbox-exec"


def _has_unquoted_newline(command: str) -> bool:
    """Return True if *command* contains a \\n or \\r outside of quotes.

    Bash treats unquoted newlines as command separators, so a command
    like ``cubesandbox-exec\\nrm -rf /`` would execute ``rm -rf /`` on
    the host — we must never let such a command bypass rewriting.
    """
    in_single = in_double = False
    for ch in command:
        if ch == "'" and not in_double:
            in_single = not in_single
        elif ch == '"' and not in_single:
            in_double = not in_double
        elif ch in "\n\r" and not in_single and not in_double:
            return True
    return False


def _already_sandboxed(command: str) -> bool:
    """Return True if *command* is already a cubesandbox-exec invocation.

    Rejects commands containing unquoted newlines (see
    ``_has_unquoted_newline``) so that multi-line injection like
    ``cubesandbox-exec\\nrm -rf /`` is NOT mistaken for an already-sandboxed
    call.  Uses ``shlex.split()`` to correctly tokenize single-line commands,
    and matches the binary by basename so that full-path invocations (e.g.
    ``/home/user/.local/bin/cubesandbox-exec``) are also recognized,
    preventing recursive wrapping.
    """
    if _has_unquoted_newline(command):
        return False
    try:
        tokens = shlex.split(command)
    except ValueError:
        return False
    if not tokens:
        return False
    first = tokens[0]
    return first == EXEC_BIN or first.rsplit("/", 1)[-1] == EXEC_BIN


def main() -> None:
    try:
        payload = json.load(sys.stdin)
    except json.JSONDecodeError:
        sys.exit(0)  # malformed input -- don't block Claude Code

    if payload.get("tool_name") != "Bash":
        sys.exit(0)

    tool_input = payload.get("tool_input") or {}
    command = tool_input.get("command")
    if not isinstance(command, str) or not command or _already_sandboxed(command):
        sys.exit(0)

    session_id = payload.get("session_id") or "default"
    cwd = payload.get("cwd")

    rewritten = [EXEC_BIN, "--session", session_id]
    if cwd:
        rewritten += ["--mount", cwd]

    timeout_ms = tool_input.get("timeout")
    if isinstance(timeout_ms, (int, float)) and timeout_ms > 0:
        rewritten += ["--timeout", f"{timeout_ms / 1000:.3f}"]

    rewritten.append(command)
    new_command = " ".join(shlex.quote(a) for a in rewritten)

    updated_input = dict(tool_input)
    updated_input["command"] = new_command

    output = {
        "hookSpecificOutput": {
            "hookEventName": "PreToolUse",
            "permissionDecision": "allow",
            "updatedInput": updated_input,
        }
    }
    print(json.dumps(output))
    sys.exit(0)


if __name__ == "__main__":
    main()
