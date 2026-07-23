# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# 注意：本文件与 pi-agent-integration/env_utils.py 和 claude-code-sandbox/env_utils.py
# 共享多个基础工具函数（load_local_dotenv、required、optional、int_env、
# shell_join、_host_from_url）。如需修复这些函数，请同步更新所有副本。

from __future__ import annotations

import os
from pathlib import Path
from urllib.parse import urlparse

from dotenv import load_dotenv

PROVIDER_KEY_ENV = {
    "anthropic": "ANTHROPIC_API_KEY",
    "deepseek": "DEEPSEEK_API_KEY",
    "openai": "OPENAI_API_KEY",
    "google": "GEMINI_API_KEY",
}

PROVIDER_DEFAULT_MODEL = {
    "anthropic": "anthropic/claude-sonnet-4-6",
    "deepseek": "deepseek/deepseek-v4-flash",
}

PROVIDER_DEFAULT_HOST = {
    "anthropic": "api.anthropic.com",
    "deepseek": "api.deepseek.com",
    "openai": "api.openai.com",
}

DEFAULT_WORKSPACE = "/workspace"

PASSTHROUGH_ENV_NAMES = (
    "ANTHROPIC_BASE_URL",
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


def qclaw_provider() -> str:
    return optional("QCLAW_PROVIDER", "deepseek").strip().lower()


def qclaw_model() -> str:
    explicit = os.environ.get("QCLAW_MODEL")
    if explicit:
        return explicit
    provider = qclaw_provider()
    default = PROVIDER_DEFAULT_MODEL.get(provider)
    if default:
        return default
    raise SystemExit(
        f"No default model for provider {provider!r}. Set QCLAW_MODEL in your .env."
    )


def qclaw_workspace() -> str:
    return optional("QCLAW_WORKSPACE", DEFAULT_WORKSPACE)


def provider_key_name(provider: str | None = None) -> str:
    provider_name = provider or qclaw_provider()
    return PROVIDER_KEY_ENV.get(provider_name, f"{provider_name.upper()}_API_KEY")


def require_provider_key(provider: str | None = None) -> str:
    provider_name = provider or qclaw_provider()
    key_name = PROVIDER_KEY_ENV.get(provider_name)
    if not key_name:
        raise SystemExit(f"Unknown provider {provider_name!r}")
    value = os.environ.get(key_name)
    if not value:
        raise SystemExit(f"Missing required environment variable: {key_name}")
    return value


def build_qclaw_env(include_secrets: bool = True) -> dict[str, str]:
    env = {}
    for name in PASSTHROUGH_ENV_NAMES:
        value = os.environ.get(name)
        if value:
            env[name] = value
    if include_secrets:
        key_name = provider_key_name()
        value = os.environ.get(key_name)
        if value:
            env[key_name] = value
    return env


def qclaw_llm_host(provider: str | None = None) -> str:
    provider_name = (provider or qclaw_provider()).strip().lower()
    explicit = os.environ.get("QCLAW_LLM_HOST")
    if explicit:
        return _host_from_url(explicit)
    return PROVIDER_DEFAULT_HOST.get(provider_name, "")


def _host_from_url(value: str) -> str:
    candidate = value.strip()
    if not candidate:
        return ""
    if "://" not in candidate:
        candidate = f"https://{candidate}"
    return urlparse(candidate).hostname or ""


def shell_join(*parts: str) -> str:
    return " && ".join(part for part in parts if part)
