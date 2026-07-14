# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Shared sandbox command helpers for the Claude Code example scripts.

Kept SDK-agnostic (duck-typed on ``sandbox.commands.run`` and the result's
attributes) so the same helpers work with both the e2b-compatible SDK used by
``run_claude_code.py`` / ``resume_claude_code.py`` and the native
``cubesandbox`` SDK used by ``network_policy.py``.
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
    """Render one Claude Code ``--output-format stream-json`` event as concise,
    human-readable output.

    Claude Code stream-json events include assistant text, tool use, and
    result messages. We extract the key content for a concise transcript.
    Non-JSON lines are printed verbatim. Set CLAUDE_CODE_STREAM_RAW=1 (or pass
    ``--raw``) to see the raw JSONL stream instead.
    """
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

    # Claude Code stream-json has different event types; render the key ones.
    event_type = event.get("type")

    if event_type == "assistant":
        # Assistant text content
        message = event.get("message", event)
        content = message.get("content", [])
        if isinstance(content, str) and content.strip():
            print(content.strip())
        elif isinstance(content, list):
            for item in content:
                if not isinstance(item, dict):
                    continue
                itype = item.get("type")
                if itype == "text":
                    text = str(item.get("text", "")).strip()
                    if text:
                        print(text)
                elif itype == "tool_use":
                    name = item.get("name") or "tool"
                    input_data = item.get("input", {})
                    brief = _tool_brief(input_data)
                    print(f"  -> [tool] {name}{': ' + brief if brief else ''}")

    elif event_type == "result":
        # Final result message
        result_text = event.get("result", "")
        if isinstance(result_text, str) and result_text.strip():
            print(result_text.strip())

    elif event_type == "error":
        error_msg = event.get("error", event.get("message", ""))
        print(f"  [error] {str(error_msg)[:300]}")

    elif event_type == "tool_result":
        # Tool execution result — only show errors
        content = event.get("content", "")
        is_error = event.get("is_error", False)
        if is_error:
            tool_name = event.get("tool_use_id", "tool")
            print(f"  x [tool] {tool_name} failed")


def _tool_brief(arguments: dict) -> str:
    for key in ("command", "path", "pattern", "query", "url", "file_path"):
        value = arguments.get(key)
        if value:
            return str(value).replace("\n", " ")[:120]
    return ""


def stream_json_render_writer() -> Callable[[object], None]:
    # e2b delivers stdout as arbitrary chunks (often several JSONL events, or a
    # partial line, per callback), not one event per call. Buffer and split on
    # newlines so each event is rendered exactly once. Claude Code newline-
    # terminates every event, so nothing important is left dangling in the buffer.
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
    # Run as root: /workspace is root-owned, and the default e2b exec user
    # ("user") cannot write to it.
    kwargs = {"cwd": cwd, "timeout": timeout, "user": user}
    kwargs = {key: value for key, value in kwargs.items() if value is not None}
    if envs:
        kwargs["envs"] = envs
    if stream:
        # Default to a concise transcript (assistant text + tool calls + errors).
        # Set CLAUDE_CODE_STREAM_RAW=1 (or pass --raw) to dump Claude Code's
        # raw stream-json instead.
        if raw is None:
            raw = os.environ.get("CLAUDE_CODE_STREAM_RAW", "").strip().lower() in (
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
    if exit_code is not None and int(exit_code) != 0:
        stdout = getattr(result, "stdout", "")
        stderr = getattr(result, "stderr", "")
        raise SystemExit(
            f"Failed to {action} (exit {exit_code}).\nSTDOUT:\n{stdout}\nSTDERR:\n{stderr}"
        )


def sandbox_identifier(sandbox: Any) -> str:
    return getattr(sandbox, "sandbox_id", getattr(sandbox, "id", "unknown"))
