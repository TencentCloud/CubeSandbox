# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Small, dependency-free helpers shared by the Gemini CLI examples."""

from __future__ import annotations

import os
import shlex
from pathlib import Path
from typing import Any


def load_dotenv(path: str = ".env") -> None:
    """Load simple KEY=VALUE lines without overriding exported variables."""
    dotenv = Path(path)
    if not dotenv.is_file():
        return
    for line in dotenv.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        key = key.strip()
        value = value.strip().strip('"').strip("'")
        if key:
            os.environ.setdefault(key, value)


def required(name: str) -> str:
    value = os.environ.get(name, "").strip()
    if not value:
        raise SystemExit(f"Missing required environment variable: {name}")
    return value


def int_env(name: str, default: int) -> int:
    value = os.environ.get(name, "").strip()
    return int(value) if value else default


def shell_join(*commands: str) -> str:
    return " && ".join(command for command in commands if command)


def gemini_command(prompt: str, *, model: str | None, approve_all: bool) -> str:
    """Build a non-interactive Gemini CLI command with safely quoted values."""
    command = ["gemini", "--prompt", prompt]
    if model:
        command.extend(["--model", model])
    if approve_all:
        command.append("--yolo")
    return " ".join(shlex.quote(item) for item in command)


def run_command(sandbox: Any, command: str, **kwargs: Any) -> Any:
    """Run a command while accommodating SDKs that call env vars env or envs."""
    try:
        return sandbox.commands.run(command, **kwargs)
    except TypeError as exc:
        if "envs" not in kwargs or "envs" not in str(exc):
            raise
        retry = dict(kwargs)
        retry["env"] = retry.pop("envs")
        return sandbox.commands.run(command, **retry)


def ensure_success(result: Any, action: str) -> None:
    exit_code = getattr(result, "exit_code", None)
    if exit_code not in (None, 0):
        raise SystemExit(
            f"Failed to {action} (exit {exit_code}).\n"
            f"STDOUT:\n{getattr(result, 'stdout', '')}\n"
            f"STDERR:\n{getattr(result, 'stderr', '')}"
        )


def sandbox_id(sandbox: Any) -> str:
    return str(getattr(sandbox, "sandbox_id", getattr(sandbox, "id", "unknown")))
