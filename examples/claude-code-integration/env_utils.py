# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Shared helpers for the Claude Code + CubeSandbox demos."""

from __future__ import annotations

import os
from pathlib import Path

from dotenv import load_dotenv


def load_local_dotenv() -> None:
    """Best-effort load of a nearby `.env` file without overriding real env."""
    for path in (Path(__file__).with_name(".env"), Path.cwd() / ".env"):
        if path.is_file():
            load_dotenv(dotenv_path=path, override=False)
            return


def required(name: str) -> str:
    value = os.environ.get(name, "").strip()
    if not value:
        raise SystemExit(
            f"missing required env var {name}; copy env.example -> .env and fill it in"
        )
    return value


def build_agent_env() -> dict[str, str]:
    """Env vars that get forwarded into the sandbox for `claude` invocations.

    The Anthropic API key stays on the operator side by default: it is passed
    to `sandbox.commands.run(..., envs=...)` per-call so it lives in the envd
    exec envelope, not in a persistent /etc/environment file inside the VM.
    Combined with a CubeEgress inject rule (see README §5) you can drop the
    key from this dict entirely and let CubeEgress attach it on the wire.
    """
    envs: dict[str, str] = {}
    for name in (
        "ANTHROPIC_API_KEY",
        "ANTHROPIC_MODEL",
        "ANTHROPIC_BASE_URL",
        "CLAUDE_CODE_USE_BEDROCK",
        "CLAUDE_CODE_USE_VERTEX",
    ):
        value = os.environ.get(name)
        if value:
            envs[name] = value
    return envs
