# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Claude Code 示例脚本的共享沙箱命令工具函数。"""

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
    for key in ("command", "path"):
        value = arguments.get(key)
        if value:
            return str(value).replace("\n", " ")[:120]
    return ""


def _render_message(message: object) -> None:
    if not isinstance(message, dict):
        return
    msg_type = message.get("type")
    if msg_type == "assistant":
        content = message.get("content")
        if isinstance(content, str) and content.strip():
            print(content.strip())
        elif isinstance(content, list):
            for block in content:
                if not isinstance(block, dict):
                    continue
                btype = block.get("type")
                if btype == "text":
                    text = str(block.get("text", "")).strip()
                    if text:
                        print(text)
                elif btype == "tool_use":
                    name = block.get("name") or "tool"
                    arguments = (
                        block.get("arguments")
                        if isinstance(block.get("arguments"), dict)
                        else {}
                    )
                    brief = _tool_brief(arguments)
                    print(f"  → [tool] {name}{': ' + brief if brief else ''}")
    elif msg_type == "user":
        content = message.get("content")
        if isinstance(content, list):
            for block in content:
                if isinstance(block, dict) and block.get("type") == "tool_result":
                    if block.get("is_error"):
                        print(f"  ✗ [tool] {block.get('tool_use_id', 'unknown')} failed")
    elif msg_type == "result":
        if message.get("is_error"):
            print(f"  [error] {str(message.get('error', ''))[:300]}")


def _render_jsonl_line(line: str) -> None:
    """将一行 Claude Code JSON 事件渲染为简洁的人类可读输出。"""
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
    _render_message(event)


def jsonl_renderer() -> Callable[[object], None]:
    buffer = {"text": ""}

    def write(chunk: object) -> None:
        text = getattr(chunk, "line", chunk)
        buffer["text"] += text if isinstance(text, str) else str(text)
        while "\n" in buffer["text"]:
            line, buffer["text"] = buffer["text"].split("\n", 1)
            _render_jsonl_line(line)

    return write


def run_command(
    sandbox: Any,
    command: str,
    *,
    cwd: str | None = None,
    envs: dict[str, str] | None = None,
    timeout: int | float | None = None,
    stream: bool = False,
    user: str = "root",
):
    kwargs = {"cwd": cwd, "timeout": timeout, "user": user}
    kwargs = {key: value for key, value in kwargs.items() if value is not None}
    if envs:
        kwargs["envs"] = envs
    if stream:
        raw = os.environ.get("CLAUDE_STREAM_RAW", "").strip().lower() in ("1", "true", "yes")
        kwargs["on_stdout"] = stream_writer(sys.stdout) if raw else jsonl_renderer()
        kwargs["on_stderr"] = stream_writer(sys.stderr)

    try:
        return sandbox.commands.run(command, **kwargs)
    except TypeError as exc:
        if "envs" not in kwargs or "envs" not in str(exc):
            raise
        kwargs["env"] = kwargs.pop("envs")
        return sandbox.commands.run(command, **kwargs)


def ensure_success(result, action: str) -> None:
    exit_code = getattr(result, "exit_code", None)
    if exit_code not in (None, 0):
        stdout = getattr(result, "stdout", "")
        stderr = getattr(result, "stderr", "")
        raise SystemExit(
            f"Failed to {action} (exit {exit_code}).\nSTDOUT:\n{stdout}\nSTDERR:\n{stderr}"
        )


def sandbox_identifier(sandbox: Any) -> str:
    return getattr(sandbox, "sandbox_id", getattr(sandbox, "id", "unknown"))
