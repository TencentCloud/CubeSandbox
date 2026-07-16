# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Shared sandbox command helpers for the CodeBuddy example scripts.

Kept SDK-agnostic (duck-typed on ``sandbox.commands.run`` and the result's
attributes) so the same helpers work with both the e2b-compatible SDK used by
``run_codebuddy.py`` / ``resume_codebuddy.py`` and the native ``cubesandbox``
SDK used by ``network_policy.py``.
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


def _tool_brief(arguments: dict) -> str:
    for key in ("command", "path", "pattern", "query", "url", "file_path"):
        value = arguments.get(key)
        if value:
            return str(value).replace("\n", " ")[:120]
    return ""


def _render_message(message: object) -> None:
    if not isinstance(message, dict):
        return
    role = message.get("role")
    content = message.get("content")
    if role == "assistant":
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
                elif itype == "toolCall":
                    name = item.get("name") or "tool"
                    arguments = (
                        item.get("arguments")
                        if isinstance(item.get("arguments"), dict)
                        else {}
                    )
                    brief = _tool_brief(arguments)
                    print(f"  \u2192 [tool] {name}{': ' + brief if brief else ''}")
        if message.get("stopReason") == "error":
            print(f"  [error] {str(message.get('errorMessage', ''))[:300]}")
    elif role == "toolResult" and message.get("isError"):
        print(f"  \u2717 [tool] {message.get('toolName', 'tool')} failed")


def _render_jsonl_line(line: str) -> None:
    """Render one CodeBuddy ``--output-format stream-json`` event as concise,
    human-readable output.

    CodeBuddy streams a lot of envelopes (per-token deltas, thinking traces,
    duplicate message snapshots). We ignore all of those and render only the
    authoritative transcript: assistant text, tool calls, and any failures, in
    order. Non-JSON lines are printed verbatim. Set CODEBUDDY_STREAM_RAW=1 (or
    pass ``--raw``) to see the raw JSONL stream instead.
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
        return
    # CodeBuddy stream-json events have a "type" field; we render the
    # final transcript event (type "result" or "end") and skip incremental
    # deltas. For other event types, only render if CODEBUDDY_STREAM_RAW is set.
    event_type = event.get("type")
    if event_type in ("result", "end"):
        for message in event.get("messages") or []:
            _render_message(message)


def jsonl_render_writer() -> Callable[[object], None]:
    # e2b delivers stdout as arbitrary chunks (often several JSONL events, or a
    # partial line, per callback), not one event per call. Buffer and split on
    # newlines so each event is rendered exactly once. CodeBuddy newline-terminates
    # every event, so nothing important is left dangling in the buffer.
    buffer = {"parts": []}

    def write(chunk: object) -> None:
        text = getattr(chunk, "line", chunk)
        text = text if isinstance(text, str) else str(text)
        lines = text.split("\n")
        if len(lines) == 1:
            buffer["parts"].append(text)
            return
        _render_jsonl_line("".join(buffer["parts"]) + lines[0])
        for line in lines[1:-1]:
            _render_jsonl_line(line)
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
    # Run as root: /workspace and CodeBuddy's state dir (/root/.codebuddy) are
    # root-owned, and the default e2b exec user ("user") cannot write to them.
    kwargs = {"cwd": cwd, "timeout": timeout, "user": user}
    kwargs = {key: value for key, value in kwargs.items() if value is not None}
    if envs:
        kwargs["envs"] = envs
    if stream:
        # Default to a concise transcript (assistant text + tool calls + errors).
        # Set CODEBUDDY_STREAM_RAW=1 (or pass --raw) to dump CodeBuddy's raw
        # JSONL instead.
        if raw is None:
            raw = os.environ.get("CODEBUDDY_STREAM_RAW", "").strip().lower() in (
                "1", "true", "yes",
            )
        kwargs["on_stdout"] = stream_writer(sys.stdout) if raw else jsonl_render_writer()
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
