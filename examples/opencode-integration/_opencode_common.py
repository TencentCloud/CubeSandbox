# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Shared command and JSONL helpers for the OpenCode integration examples."""

from __future__ import annotations

import json
import os
import sys
from collections.abc import Callable
from typing import Any, TextIO


def stream_writer(stream: TextIO) -> Callable[[object], None]:
    def write(chunk: object) -> None:
        text = getattr(chunk, "line", chunk)
        stream.write(str(text))
        stream.flush()

    return write


def _tool_brief(part: dict[str, Any]) -> str:
    state = part.get("state")
    if not isinstance(state, dict):
        return ""
    inputs = state.get("input")
    if not isinstance(inputs, dict):
        return ""
    # OpenCode emits filePath while URL-oriented tools emit url. Supporting
    # both keeps the concise renderer useful across tool event variants.
    for key in ("command", "path", "filePath", "pattern", "query", "url"):
        value = inputs.get(key)
        if value:
            return str(value).replace("\n", " ")[:120]
    return ""


def render_jsonl_line(line: str) -> None:
    line = line.rstrip("\r\n")
    if not line.strip():
        return
    try:
        event = json.loads(line)
    except (TypeError, ValueError):
        print(line)
        return
    if not isinstance(event, dict):
        return

    event_type = event.get("type")
    part = event.get("part")
    if event_type == "text" and isinstance(part, dict):
        text = str(part.get("text", "")).strip()
        if text:
            print(text)
    elif event_type == "tool_use" and isinstance(part, dict):
        tool = part.get("tool") or "tool"
        state = part.get("state") if isinstance(part.get("state"), dict) else {}
        status = state.get("status", "unknown")
        brief = _tool_brief(part)
        print(f"  -> [tool:{status}] {tool}{': ' + brief if brief else ''}")
        if status == "error" and state.get("error"):
            print(f"     {str(state['error'])[:300]}")
    elif event_type == "error":
        print(f"  [error] {str(event.get('error', 'unknown error'))[:300]}")


def jsonl_render_writer() -> Callable[[object], None]:
    buffer = {"text": ""}

    def write(chunk: object) -> None:
        text = getattr(chunk, "line", chunk)
        buffer["text"] += text if isinstance(text, str) else str(text)
        while "\n" in buffer["text"]:
            line, buffer["text"] = buffer["text"].split("\n", 1)
            render_jsonl_line(line)

    return write


def run_command(
    sandbox: Any,
    command: str,
    *,
    cwd: str | None = None,
    envs: dict[str, str] | None = None,
    timeout: float | None = None,
    stream: bool = False,
    user: str = "root",
) -> Any:
    kwargs: dict[str, Any] = {"user": user}
    if cwd is not None:
        kwargs["cwd"] = cwd
    if timeout is not None:
        kwargs["timeout"] = timeout
    if envs:
        kwargs["envs"] = envs
    if stream:
        raw = os.environ.get("OPENCODE_STREAM_RAW", "").lower() in {"1", "true", "yes"}
        kwargs["on_stdout"] = (
            stream_writer(sys.stdout) if raw else jsonl_render_writer()
        )
        kwargs["on_stderr"] = stream_writer(sys.stderr)
    try:
        return sandbox.commands.run(command, **kwargs)
    except TypeError as exc:
        if "envs" not in kwargs or "envs" not in str(exc):
            raise
        kwargs["env"] = kwargs.pop("envs")
        return sandbox.commands.run(command, **kwargs)


def ensure_success(result: Any, action: str) -> None:
    exit_code = getattr(result, "exit_code", None)
    if exit_code not in (None, 0):
        stdout = getattr(result, "stdout", "")
        stderr = getattr(result, "stderr", "")
        raise SystemExit(
            f"Failed to {action} (exit {exit_code}).\n"
            f"STDOUT:\n{stdout}\nSTDERR:\n{stderr}"
        )


def sandbox_identifier(sandbox: Any) -> str:
    return str(getattr(sandbox, "sandbox_id", getattr(sandbox, "id", "unknown")))


def extract_session_id(output: str) -> str:
    session_ids: set[str] = set()
    for line in output.splitlines():
        try:
            event = json.loads(line)
        except (TypeError, ValueError):
            continue
        if isinstance(event, dict) and isinstance(event.get("sessionID"), str):
            session_ids.add(event["sessionID"])
    if not session_ids:
        raise SystemExit("OpenCode JSONL did not contain a sessionID")
    if len(session_ids) != 1:
        raise SystemExit(
            f"OpenCode JSONL contained multiple session IDs: {session_ids}"
        )
    return session_ids.pop()
