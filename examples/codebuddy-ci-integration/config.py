#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Configuration and command construction for the CodeBuddy CI example."""

from __future__ import annotations

import os
import shlex
from pathlib import Path

from dotenv import load_dotenv

DEFAULT_WORKSPACE = "/workspace"


def load_local_dotenv() -> None:
    """Load the example's .env without overriding a CI runner's real env."""
    path = Path(__file__).with_name(".env")
    if path.is_file():
        load_dotenv(path, override=False)


def required(name: str) -> str:
    value = os.environ.get(name)
    if not value:
        raise SystemExit(f"Missing required environment variable: {name}")
    return value


def positive_int(name: str, default: int) -> int:
    value = os.environ.get(name)
    if not value:
        return default
    try:
        parsed = int(value)
    except ValueError as exc:
        raise SystemExit(f"{name} must be an integer, got {value!r}") from exc
    if parsed <= 0:
        raise SystemExit(f"{name} must be positive, got {parsed}")
    return parsed


def workspace() -> str:
    value = os.environ.get("CODEBUDDY_WORKSPACE", DEFAULT_WORKSPACE)
    if not value.startswith("/"):
        raise SystemExit("CODEBUDDY_WORKSPACE must be an absolute sandbox path")
    return value.rstrip("/") or "/"


def codebuddy_env() -> dict[str, str]:
    """Return the smallest environment map needed by a single agent command."""
    token = required("CODEBUDDY_AUTH_TOKEN")
    env = {
        "CODEBUDDY_AUTH_TOKEN": token,
        "CODEBUDDY_DISABLE_TELEMETRY": "1",
        "CODEBUDDY_CONFIG_DIR": os.environ.get(
            "CODEBUDDY_CONFIG_DIR", "/root/.codebuddy"
        ),
    }
    return env


def codebuddy_command(
    prompt: str,
    *,
    session_id: str,
    max_turns: int,
    resume: bool = False,
) -> str:
    """Build an audited non-interactive CodeBuddy invocation.

    ``--permission-mode bypassPermissions`` is deliberately confined to this
    disposable MicroVM. The accompanying guide requires default-deny egress
    except for the CodeBuddy API endpoint before using this mode in CI.
    """
    if not prompt.strip():
        raise ValueError("prompt must not be empty")
    if not session_id or not session_id[0].isalnum():
        raise ValueError("session_id must start with a letter or number")
    if max_turns <= 0:
        raise ValueError("max_turns must be positive")

    args = [
        "codebuddy",
        "--print",
        "--output-format",
        "json",
        "--permission-mode",
        "bypassPermissions",
        "--session-id",
        session_id,
        "--max-turns",
        str(max_turns),
    ]
    model = os.environ.get("CODEBUDDY_MODEL")
    if model:
        args.extend(["--model", model])
    if resume:
        args.extend(["--resume", session_id])
    args.append(prompt)
    return shlex.join(args)


def shell_join(*parts: str) -> str:
    return " && ".join(part for part in parts if part)
