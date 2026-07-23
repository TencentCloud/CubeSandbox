# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# 注意：本文件与 pi-agent-integration/env_utils.py 和 qclaw-sandbox/env_utils.py
# 共享多个基础工具函数（load_local_dotenv、required、optional、int_env、
# shell_join、_host_from_url）。如需修复这些函数，请同步更新所有副本。

from __future__ import annotations

import os
import shlex
from pathlib import Path
from urllib.parse import urlparse

from dotenv import load_dotenv

DEFAULT_CLAUDE_DIR = "/root/.claude"
DEFAULT_WORKSPACE = "/workspace"

PASSTHROUGH_ENV_NAMES = (
    "ANTHROPIC_BASE_URL",
    "ANTHROPIC_MODEL",
    "HTTP_PROXY",
    "HTTPS_PROXY",
    "NO_PROXY",
)


def load_local_dotenv() -> None:
    candidate_paths = [
        Path(__file__).with_name(".env"),
        Path.cwd() / ".env",
    ]
    seen_paths: set[Path] = set()
    for path in candidate_paths:
        resolved_path = path.resolve()
        if resolved_path in seen_paths:
            continue
        seen_paths.add(resolved_path)
        if path.is_file():
            load_dotenv(dotenv_path=path, override=False)
            return


def required(name: str) -> str:
    value = os.environ.get(name)
    if not value:
        raise SystemExit(f"Missing required environment variable: {name}")
    return value


def optional(name: str, default: str = "") -> str:
    return os.environ.get(name) or default


def int_env(name: str, default: int) -> int:
    raw = os.environ.get(name)
    if not raw:
        return default
    try:
        return int(raw)
    except ValueError as exc:
        raise SystemExit(f"{name} must be an integer, got {raw!r}") from exc


def claude_workspace() -> str:
    return optional("CLAUDE_CODE_WORKSPACE", DEFAULT_WORKSPACE)


def require_anthropic_key() -> str:
    key = os.environ.get("ANTHROPIC_API_KEY") or os.environ.get("ANTHROPIC_AUTH_TOKEN")
    if not key:
        raise SystemExit("Missing required environment variable: ANTHROPIC_API_KEY")
    return key


def build_claude_env(include_secrets: bool = True) -> dict[str, str]:
    # CLAUDE_CODE_DISABLE_TELEMETRY 和 CLAUDE_CODE_SKIP_UPDATE_CHECK
    # 已在 Dockerfile 中通过 ENV 设置，此处不需要重复声明。
    env: dict[str, str] = {}
    for name in PASSTHROUGH_ENV_NAMES:
        value = os.environ.get(name)
        if value:
            env[name] = value
    if include_secrets:
        key = os.environ.get("ANTHROPIC_API_KEY") or os.environ.get("ANTHROPIC_AUTH_TOKEN")
        if key:
            env["ANTHROPIC_API_KEY"] = key
    return env


def claude_llm_host() -> str:
    explicit = os.environ.get("CLAUDE_LLM_HOST")
    if explicit:
        return _host_from_url(explicit)
    base_url = os.environ.get("ANTHROPIC_BASE_URL")
    if base_url:
        host = _host_from_url(base_url)
        if host:
            return host
    return "api.anthropic.com"


def _host_from_url(value: str) -> str:
    candidate = value.strip()
    if not candidate:
        return ""
    if "://" not in candidate:
        candidate = f"https://{candidate}"
    return urlparse(candidate).hostname or ""


def claude_command(
    prompt: str, *, output_format: str = "json", approve: bool = True
) -> str:
    args = ["claude", "--print"]
    if output_format:
        args.extend(["--output-format", output_format])
    if approve:
        args.append("--approve")
    args.append(prompt)
    return " ".join(shlex.quote(arg) for arg in args)


def shell_join(*parts: str) -> str:
    return " && ".join(part for part in parts if part)
