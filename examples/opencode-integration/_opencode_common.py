# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Shared command and JSONL helpers for the OpenCode integration examples."""

from __future__ import annotations

import json
import os
import sys
from collections.abc import Callable
from typing import Any, TextIO

JsonEventHandler = Callable[[dict[str, Any]], None]


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


def _notify_json_event(line: str, handler: JsonEventHandler | None) -> None:
    if handler is None:
        return
    try:
        event = json.loads(line)
    except (TypeError, ValueError):
        return
    if isinstance(event, dict):
        handler(event)


def render_jsonl_line(
    line: str,
    event_handler: JsonEventHandler | None = None,
) -> None:
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
    if event_handler is not None:
        event_handler(event)

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
    elif os.environ.get("OPENCODE_STREAM_VERBOSE", "").lower() in {
        "1",
        "true",
        "yes",
    }:
        print(f"  [event:{event_type or 'unknown'}] omitted", file=sys.stderr)


def jsonl_render_writer(
    event_handler: JsonEventHandler | None = None,
) -> Callable[[object], None]:
    buffer = {"text": ""}

    def write(chunk: object) -> None:
        text = getattr(chunk, "line", chunk)
        buffer["text"] += text if isinstance(text, str) else str(text)
        while "\n" in buffer["text"]:
            line, buffer["text"] = buffer["text"].split("\n", 1)
            render_jsonl_line(line, event_handler)

    return write


def raw_jsonl_writer(
    stream: TextIO,
    event_handler: JsonEventHandler | None = None,
) -> Callable[[object], None]:
    buffer = {"text": ""}

    def write(chunk: object) -> None:
        text = getattr(chunk, "line", chunk)
        text = text if isinstance(text, str) else str(text)
        stream.write(text)
        stream.flush()
        if event_handler is None:
            return
        buffer["text"] += text
        while "\n" in buffer["text"]:
            line, buffer["text"] = buffer["text"].split("\n", 1)
            _notify_json_event(line, event_handler)

    return write


def _is_unsupported_envs_error(exc: TypeError) -> bool:
    message = str(exc)
    return (
        "unexpected keyword argument 'envs'" in message
        or 'unexpected keyword argument "envs"' in message
    )


def run_command(
    sandbox: Any,
    command: str,
    *,
    cwd: str | None = None,
    envs: dict[str, str] | None = None,
    timeout: float | None = None,
    stream: bool = False,
    user: str = "root",
    json_event_handler: JsonEventHandler | None = None,
) -> Any:
    kwargs: dict[str, Any] = {"user": user}
    stdout_seen = False
    stderr_seen = False
    stdout_callback: Callable[[object], None] | None = None
    stderr_callback: Callable[[object], None] | None = None

    def track_stdout(chunk: object) -> None:
        nonlocal stdout_seen
        stdout_seen = True
        if stdout_callback is not None:
            stdout_callback(chunk)

    def track_stderr(chunk: object) -> None:
        nonlocal stderr_seen
        stderr_seen = True
        if stderr_callback is not None:
            stderr_callback(chunk)

    if cwd is not None:
        kwargs["cwd"] = cwd
    if timeout is not None:
        kwargs["timeout"] = timeout
    if envs:
        kwargs["envs"] = envs
    if stream:
        raw = os.environ.get("OPENCODE_STREAM_RAW", "").lower() in {"1", "true", "yes"}
        stdout_callback = (
            raw_jsonl_writer(sys.stdout, json_event_handler)
            if raw
            else jsonl_render_writer(json_event_handler)
        )
        stderr_callback = stream_writer(sys.stderr)
        kwargs["on_stdout"] = track_stdout
        kwargs["on_stderr"] = track_stderr
    try:
        result = sandbox.commands.run(command, **kwargs)
    except TypeError as exc:
        if "envs" not in kwargs:
            raise
        if not _is_unsupported_envs_error(exc):
            raise
        kwargs["env"] = kwargs.pop("envs")
        result = sandbox.commands.run(command, **kwargs)

    # Some compatible SDKs accept callback kwargs but do not invoke them.
    # Replay the collected result once so output and event capture still work.
    if stream and not stdout_seen:
        stdout = getattr(result, "stdout", "")
        if stdout and stdout_callback is not None:
            stdout_callback(stdout)
    if stream and not stderr_seen:
        stderr = getattr(result, "stderr", "")
        if stderr and stderr_callback is not None:
            stderr_callback(stderr)
    return result


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


def _session_ids_from_output(output: str) -> set[str]:
    session_ids: set[str] = set()
    for line in output.splitlines():
        try:
            event = json.loads(line)
        except (TypeError, ValueError):
            continue
        if isinstance(event, dict) and isinstance(event.get("sessionID"), str):
            session_ids.add(event["sessionID"])
    return session_ids


def _require_one_session_id(session_ids: set[str]) -> str:
    if not session_ids:
        raise SystemExit("OpenCode JSONL did not contain a sessionID")
    if len(session_ids) != 1:
        raise SystemExit(
            f"OpenCode JSONL contained multiple session IDs: {session_ids}"
        )
    return session_ids.pop()


def extract_session_id(output: str) -> str:
    return _require_one_session_id(_session_ids_from_output(output))


class SessionIdCapture:
    """Capture one authoritative session ID while JSONL chunks are streamed."""

    def __init__(self) -> None:
        self._session_ids: set[str] = set()

    def observe(self, event: dict[str, Any]) -> None:
        session_id = event.get("sessionID")
        if isinstance(session_id, str):
            self._session_ids.add(session_id)

    def resolve(self, fallback_output: str = "") -> str:
        session_ids = set(self._session_ids)
        session_ids.update(_session_ids_from_output(fallback_output))
        return _require_one_session_id(session_ids)
