# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Shared sandbox command helpers for the OpenCode example scripts.

Kept SDK-agnostic (duck-typed on ``sandbox.commands.run`` and the result's
attributes) so the same helpers work with both the e2b-compatible SDK used by
``run_opencode.py`` / ``resume_opencode.py`` and the native ``cubesandbox`` SDK
used by ``network_policy.py``.
"""

from __future__ import annotations

import json
import os
import sys
from collections.abc import Callable
from typing import Any


def stream_writer(stream) -> Callable[[object], None]:
    def write(chunk: object) -> None:
        text = getattr(chunk, "line", chunk)
        stream.write(str(text))
        stream.flush()

    return write


def _render_stream_json_line(line: str) -> None:
    """Render one OpenCode JSON event, falling back to the original line."""
    line = line.rstrip("\r\n")
    if not line.strip():
        return
    try:
        event = json.loads(line)
    except (ValueError, TypeError):
        print(line)
        return
    if not isinstance(event, dict):
        print(line)
        return

    part = event.get("part")
    payload = part if isinstance(part, dict) else event
    event_type = str(payload.get("type", event.get("type", "")))
    if event_type in ("text", "assistant", "message"):
        text = payload.get("text", payload.get("content", ""))
        if isinstance(text, str) and text.strip():
            print(text.strip())
            return
    if event_type in ("tool", "tool_use", "tool-call"):
        name = payload.get("tool", payload.get("name", "tool"))
        print(f"  -> [tool] {name}")
        return
    if event_type in ("error", "failed"):
        message = payload.get("error", payload.get("message", payload))
        print(f"  [error] {str(message)[:300]}")
        return
    # Preserve unfamiliar events so new OpenCode versions remain observable.
    print(line)


def stream_json_render_writer() -> Callable[[object], None]:
    buffer = {"parts": []}

    def write(chunk: object) -> None:
        text = getattr(chunk, "line", chunk)
        text = text if isinstance(text, str) else str(text)
        lines = text.split("\n")
        if len(lines) == 1:
            buffer["parts"].append(text)
            return
        _render_stream_json_line("".join(buffer["parts"]) + lines[0])
        for line in lines[1:-1]:
            _render_stream_json_line(line)
        buffer["parts"] = [lines[-1]]

    return write


def run_command(
    sandbox: Any,
    command: str,
    *,
    cwd: str | None = None,
    envs: dict[str, str] | None = None,
    timeout: int | float | None = None,
    stream: bool = False,
    raw: bool | None = None,
    user: str = "root",
):
    # Run as root: /workspace and OpenCode's state dir (/root/.opencode) are
    # root-owned, and the default e2b exec user ("user") cannot write to them.
    kwargs = {"cwd": cwd, "timeout": timeout, "user": user}
    kwargs = {key: value for key, value in kwargs.items() if value is not None}
    if envs:
        kwargs["envs"] = envs
    if stream:
        if raw is None:
            raw = os.environ.get("OPENCODE_STREAM_RAW", "").strip().lower() in (
                "1", "true", "yes",
            )
        kwargs["on_stdout"] = stream_writer(sys.stdout) if raw else stream_json_render_writer()
        kwargs["on_stderr"] = stream_writer(sys.stderr)

    try:
        return sandbox.commands.run(command, **kwargs)
    except TypeError as exc:
        # Older SDKs name the parameter ``env`` instead of ``envs``. Only retry
        # for that specific signature mismatch; re-raise any other TypeError so
        # real bugs (e.g. a wrong-type command or timeout) are not masked.
        if "envs" not in kwargs or "envs" not in str(exc):
            raise
        kwargs["env"] = kwargs.pop("envs")
        return sandbox.commands.run(command, **kwargs)


def ensure_success(result, action: str) -> None:
    exit_code = getattr(result, "exit_code", None)
    if exit_code is None:
        return
    try:
        code = int(exit_code)
    except (TypeError, ValueError) as exc:
        raise SystemExit(f"Cannot parse exit code {exit_code!r} for: {action}") from exc
    if code != 0:
        stdout = getattr(result, "stdout", "")
        stderr = getattr(result, "stderr", "")
        raise SystemExit(
            f"Failed to {action} (exit {exit_code}).\nSTDOUT:\n{stdout}\nSTDERR:\n{stderr}"
        )


def sandbox_identifier(sandbox: Any) -> str:
    return getattr(sandbox, "sandbox_id", getattr(sandbox, "id", "unknown"))
