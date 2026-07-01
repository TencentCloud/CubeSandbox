# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os
import shlex
from pathlib import Path
from urllib.parse import urlparse

from dotenv import load_dotenv

DEFAULT_PI_DIR = "/root/.pi/agent"
DEFAULT_SESSION_DIR = f"{DEFAULT_PI_DIR}/sessions"
DEFAULT_WORKSPACE = "/workspace"

PROVIDER_KEY_ENV = {
    "anthropic": "ANTHROPIC_API_KEY",
    "openai": "OPENAI_API_KEY",
    "google": "GEMINI_API_KEY",
    "deepseek": "DEEPSEEK_API_KEY",
    "openrouter": "OPENROUTER_API_KEY",
}

PROVIDER_KEY_ALIASES = {
    "anthropic": ("ANTHROPIC_AUTH_TOKEN",),
}

PROVIDER_DEFAULT_HOST = {
    "anthropic": "api.anthropic.com",
    "openai": "api.openai.com",
    "google": "generativelanguage.googleapis.com",
    "deepseek": "api.deepseek.com",
    "openrouter": "openrouter.ai",
}

SECRET_ENV_NAMES = (
    "ANTHROPIC_API_KEY",
    "ANTHROPIC_AUTH_TOKEN",
    "OPENAI_API_KEY",
    "GEMINI_API_KEY",
    "DEEPSEEK_API_KEY",
    "OPENROUTER_API_KEY",
)

PASSTHROUGH_ENV_NAMES = (
    "ANTHROPIC_BASE_URL",
    "ANTHROPIC_MODEL",
    "PI_CACHE_RETENTION",
    "HTTP_PROXY",
    "HTTPS_PROXY",
    "NO_PROXY",
)


def load_local_dotenv() -> None:
    """Best-effort load of a nearby .env file without overriding real env vars."""
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


def pi_provider() -> str:
    return optional("PI_PROVIDER", "anthropic")


def pi_model() -> str:
    if pi_provider() == "anthropic":
        return (
            os.environ.get("PI_MODEL")
            or os.environ.get("ANTHROPIC_MODEL")
            or "claude-sonnet-4-6"
        )
    return optional("PI_MODEL", "claude-sonnet-4-6")


def pi_workspace() -> str:
    return optional("PI_WORKSPACE", DEFAULT_WORKSPACE)


def provider_key_name(provider: str | None = None) -> str:
    provider_name = provider or pi_provider()
    names = provider_key_candidates(provider_name)
    for name in names:
        if os.environ.get(name):
            return name
    return names[0]


def require_provider_key(provider: str | None = None) -> str:
    provider_name = provider or pi_provider()
    names = provider_key_candidates(provider_name)
    for name in names:
        value = os.environ.get(name)
        if value:
            return value
    raise SystemExit(
        "Missing required environment variable: one of " + ", ".join(names)
    )


def provider_key_candidates(provider: str) -> tuple[str, ...]:
    default_name = PROVIDER_KEY_ENV.get(provider, f"{provider.upper()}_API_KEY")
    return (default_name, *PROVIDER_KEY_ALIASES.get(provider, ()))


def build_pi_env(include_secrets: bool = True) -> dict[str, str]:
    """Build the env map passed to the Pi command inside the sandbox.

    Set ``include_secrets=False`` for the CubeEgress vault flavor: the real
    provider key rides the wire via egress injection, so it must never enter
    the sandbox environment.
    """
    env = {
        "PI_CODING_AGENT_DIR": optional("PI_CODING_AGENT_DIR", DEFAULT_PI_DIR),
        "PI_CODING_AGENT_SESSION_DIR": optional(
            "PI_CODING_AGENT_SESSION_DIR", DEFAULT_SESSION_DIR
        ),
        "PI_SKIP_VERSION_CHECK": optional("PI_SKIP_VERSION_CHECK", "1"),
        "PI_TELEMETRY": optional("PI_TELEMETRY", "0"),
    }
    for name in PASSTHROUGH_ENV_NAMES:
        value = os.environ.get(name)
        if value:
            env[name] = value
    if include_secrets:
        for name in SECRET_ENV_NAMES:
            value = os.environ.get(name)
            if value:
                env[name] = value
    return env


def pi_llm_host(provider: str | None = None) -> str:
    """Resolve the LLM API host that Pi must reach.

    Precedence: explicit ``PI_LLM_HOST`` > host parsed from ``ANTHROPIC_BASE_URL``
    (for Anthropic-compatible endpoints) > the provider default.
    """
    provider_name = provider or pi_provider()
    explicit = os.environ.get("PI_LLM_HOST")
    if explicit:
        return _host_from_url(explicit)
    if provider_name == "anthropic":
        base_url = os.environ.get("ANTHROPIC_BASE_URL")
        if base_url:
            host = _host_from_url(base_url)
            if host:
                return host
    return PROVIDER_DEFAULT_HOST.get(provider_name, "")


def _host_from_url(value: str) -> str:
    candidate = value.strip()
    if not candidate:
        return ""
    if "://" not in candidate:
        candidate = f"https://{candidate}"
    return urlparse(candidate).hostname or ""


def provider_inject(provider: str, secret: str) -> list[dict[str, str]]:
    """Build CubeEgress credential-injection headers for a provider.

    Anthropic uses ``x-api-key`` plus a required API version header; everything
    else uses an ``Authorization: Bearer`` header. Static values are carried as
    ``secret`` with the default ``${SECRET}`` format so they satisfy CubeEgress
    validation (which requires a non-empty ``secret`` on every inject entry).
    """
    if provider == "anthropic":
        return [
            {"header": "x-api-key", "secret": secret, "format": "${SECRET}"},
            {"header": "anthropic-version", "secret": "2023-06-01"},
        ]
    return [{"header": "Authorization", "secret": secret, "format": "Bearer ${SECRET}"}]


def pi_command(prompt: str, *, mode: str = "json", name: str | None = None) -> str:
    """Build a headless (non-interactive) Pi invocation.

    ``--print`` makes Pi process the prompt and exit instead of launching the
    interactive TUI (which would hang over the E2B exec channel). ``--mode json``
    streams machine-readable JSONL events. ``--approve`` is a boolean flag that
    trusts project-local files (AGENTS.md/CLAUDE.md) for this run; the prompt is
    passed as the trailing positional message.
    """
    args = ["pi", "--print"]
    if mode:
        args.extend(["--mode", mode])
    provider = pi_provider()
    model = pi_model()
    thinking = optional("PI_THINKING")
    if provider:
        args.extend(["--provider", provider])
    if model:
        args.extend(["--model", model])
    if thinking:
        args.extend(["--thinking", thinking])
    if name:
        args.extend(["--name", name])
    args.append("--approve")
    args.append(prompt)
    return " ".join(shlex.quote(arg) for arg in args)


def shell_join(*parts: str) -> str:
    return " && ".join(part for part in parts if part)
