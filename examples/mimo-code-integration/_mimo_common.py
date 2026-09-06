# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Shared command and MiMo Code NDJSON helpers."""

from __future__ import annotations

import json
import os
import re
import shlex
import sys
from collections.abc import Callable
from typing import Any

# CubeSandbox Commands.run buffers process output and does not invoke on_stdout
# callbacks. Capture MiMo NDJSON to a sandbox file and parse only a bounded
# prefix so long agent runs cannot grow unbounded host-side event lists.
DEFAULT_EVENTS_PATH = "/tmp/cube-mimo-events.ndjson"
DEFAULT_MAX_EVENT_BYTES = 2 * 1024 * 1024
DEFAULT_MAX_EVENTS = 5_000


def is_unexpected_keyword_error(error: TypeError, keyword: str) -> bool:
    """Return whether an SDK rejected one named keyword argument."""
    pattern = rf"unexpected keyword argument ['\"]{re.escape(keyword)}['\"]"
    return re.search(pattern, str(error)) is not None


def stream_writer(stream) -> Callable[[object], None]:
    def write(chunk: object) -> None:
        text = getattr(chunk, "line", chunk)
        stream.write(str(text))
        stream.flush()

    return write


def parse_jsonl(
    text: str,
    *,
    max_events: int = DEFAULT_MAX_EVENTS,
) -> list[dict[str, Any]]:
    events: list[dict[str, Any]] = []
    for line in text.splitlines():
        if not line.strip():
            continue
        try:
            event = json.loads(line)
        except (TypeError, ValueError):
            continue
        if isinstance(event, dict):
            events.append(event)
            if len(events) >= max_events:
                break
    return events


def _tool_summary(part: dict[str, Any]) -> str:
    tool = str(part.get("tool") or "tool")
    state = part.get("state") if isinstance(part.get("state"), dict) else {}
    status = str(state.get("status") or "completed")
    details = state.get("input")
    if isinstance(details, dict):
        for key in ("command", "file_path", "path", "pattern", "query", "url"):
            value = details.get(key)
            if value:
                return f"{tool} [{status}]: {str(value).replace(chr(10), ' ')[:120]}"
    return f"{tool} [{status}]"


def render_event(event: dict[str, Any]) -> None:
    event_type = event.get("type")
    part = event.get("part") if isinstance(event.get("part"), dict) else {}
    if event_type == "text":
        text = str(part.get("text") or "").strip()
        if text:
            print(text)
    elif event_type == "tool_use":
        print(f"  \u2192 [tool] {_tool_summary(part)}")
    elif event_type == "error":
        error = event.get("error")
        if isinstance(error, dict):
            data = error.get("data") if isinstance(error.get("data"), dict) else {}
            print(
                f"  [error] {data.get('message') or error.get('name') or error}",
                file=sys.stderr,
            )
        else:
            print(f"  [error] {error}", file=sys.stderr)


class JsonlCollector:
    """Collect arbitrary stdout chunks into complete MiMo NDJSON events."""

    def __init__(
        self,
        *,
        raw: bool = False,
        max_events: int = DEFAULT_MAX_EVENTS,
        max_bytes: int = DEFAULT_MAX_EVENT_BYTES,
    ) -> None:
        self.raw = raw
        self.max_events = max_events
        self.max_bytes = max_bytes
        self.events: list[dict[str, Any]] = []
        self.truncated = False
        self._buffer = ""
        self._bytes = 0

    def __call__(self, chunk: object) -> None:
        if self.truncated:
            return
        text = getattr(chunk, "line", chunk)
        piece = text if isinstance(text, str) else str(text)
        remaining = self.max_bytes - self._bytes
        if remaining <= 0:
            self.truncated = True
            return
        if len(piece) > remaining:
            piece = piece[:remaining]
            self.truncated = True
        self._buffer += piece
        self._bytes += len(piece)
        while "\n" in self._buffer:
            line, self._buffer = self._buffer.split("\n", 1)
            self._consume(line)
            if self.truncated:
                self._buffer = ""
                return

    def flush(self) -> None:
        if self._buffer and not self.truncated:
            self._consume(self._buffer)
        self._buffer = ""

    def _consume(self, line: str) -> None:
        if self.truncated or not line.strip():
            return
        if self.raw:
            print(line)
        try:
            event = json.loads(line)
        except (TypeError, ValueError):
            if not self.raw:
                print(line)
            return
        if not isinstance(event, dict):
            return
        self.events.append(event)
        if not self.raw:
            render_event(event)
        if len(self.events) >= self.max_events:
            self.truncated = True


def run_command(
    sandbox: Any,
    command: str,
    *,
    cwd: str | None = None,
    envs: dict[str, str] | None = None,
    timeout: int | float | None = None,
    on_stdout: Callable[[object], None] | None = None,
    user: str = "root",
):
    kwargs = {"cwd": cwd, "timeout": timeout, "user": user}
    kwargs = {key: value for key, value in kwargs.items() if value is not None}
    if envs:
        kwargs["envs"] = envs
    if on_stdout:
        kwargs["on_stdout"] = on_stdout
        kwargs["on_stderr"] = stream_writer(sys.stderr)
    try:
        return sandbox.commands.run(command, **kwargs)
    except TypeError as exc:
        if "envs" not in kwargs or not is_unexpected_keyword_error(exc, "envs"):
            raise
        kwargs["env"] = kwargs.pop("envs")
        return sandbox.commands.run(command, **kwargs)


def run_mimo_command(
    sandbox: Any,
    command: str,
    *,
    cwd: str,
    envs: dict[str, str],
    timeout: int,
    events_path: str = DEFAULT_EVENTS_PATH,
    max_event_bytes: int = DEFAULT_MAX_EVENT_BYTES,
    max_events: int = DEFAULT_MAX_EVENTS,
) -> tuple[Any, list[dict[str, Any]]]:
    """Run MiMo and return (CommandResult, bounded NDJSON events).

    stdout is redirected to a sandbox file so CubeSandbox's non-streaming
    ``Commands.run`` does not need callbacks. Only the first
    ``max_event_bytes`` of that file are read back and parsed into at most
    ``max_events`` events.
    """
    raw = os.environ.get("MIMO_STREAM_RAW", "").strip().lower() in {
        "1",
        "true",
        "yes",
    }
    quoted = shlex.quote(events_path)
    wrapped = f"rm -f {quoted} && {{ {command}; }} > {quoted}"
    result = run_command(
        sandbox,
        wrapped,
        cwd=cwd,
        envs=envs,
        timeout=timeout,
    )
    read = run_command(
        sandbox,
        f"head -c {int(max_event_bytes)} {quoted} 2>/dev/null || true",
        timeout=60,
    )
    payload = str(getattr(read, "stdout", "") or "")
    collector = JsonlCollector(raw=raw, max_events=max_events, max_bytes=max_event_bytes)
    if payload:
        collector(payload if payload.endswith("\n") else payload + "\n")
    collector.flush()
    events = collector.events or parse_jsonl(payload, max_events=max_events)
    return result, events


def ensure_success(result: Any, action: str) -> None:
    exit_code = getattr(result, "exit_code", None)
    if exit_code not in (None, 0):
        raise SystemExit(
            f"Failed to {action} (exit {exit_code}).\n"
            f"STDOUT:\n{getattr(result, 'stdout', '')}\n"
            f"STDERR:\n{getattr(result, 'stderr', '')}"
        )


def session_id_from_events(events: list[dict[str, Any]]) -> str:
    ids = {
        str(event["sessionID"])
        for event in events
        if isinstance(event.get("sessionID"), str) and event["sessionID"]
    }
    if not ids:
        raise SystemExit("MiMo Code returned no sessionID in its NDJSON stream")
    if len(ids) != 1:
        raise SystemExit(f"MiMo Code returned multiple session IDs: {sorted(ids)}")
    return ids.pop()


def events_contain_text(events: list[dict[str, Any]], text: str) -> bool:
    """Return whether any string field in the bounded event list contains text."""

    def contains(value: Any) -> bool:
        if isinstance(value, str):
            return text in value
        if isinstance(value, dict):
            return any(contains(item) for item in value.values())
        if isinstance(value, list):
            return any(contains(item) for item in value)
        return False

    return any(contains(event) for event in events)


def session_list_contains(text: str, session_id: str) -> bool:
    try:
        payload = json.loads(text)
    except (TypeError, ValueError):
        return False
    sessions = payload.get("sessions", []) if isinstance(payload, dict) else payload
    if not isinstance(sessions, list):
        return False
    return any(
        isinstance(item, dict) and item.get("id") == session_id for item in sessions
    )


def sandbox_identifier(sandbox: Any) -> str:
    return str(getattr(sandbox, "sandbox_id", getattr(sandbox, "id", "unknown")))


def kill_sandbox(sandbox: Any, sandbox_id: str, *, run_failed: bool) -> None:
    """Kill a sandbox without masking a primary error or hiding a cleanup leak."""
    try:
        sandbox.kill()
        print(f"\nSandbox {sandbox_id} killed.")
    except Exception as exc:
        message = (
            f"Failed to kill sandbox {sandbox_id}: {exc}. "
            f"Clean it up manually with sandbox ID {sandbox_id}."
        )
        if run_failed:
            print(f"Warning: {message}", file=sys.stderr)
            return
        raise SystemExit(message) from exc
