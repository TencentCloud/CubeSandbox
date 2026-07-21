# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os
import shlex
from pathlib import Path
from urllib.parse import urlparse

from dotenv import load_dotenv

DEFAULT_CLAUDE_DIR = "/root/.claude"
DEFAULT_SESSION_DIR = f"{DEFAULT_CLAUDE_DIR}/sessions"
DEFAULT_WORKSPACE = "/workspace"

# Claude Code uses Anthropic's API exclusively.
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

PROVIDER_DEFAULT_MODEL = {
    "anthropic": "claude-sonnet-4-6",
}

PASSTHROUGH_ENV_NAMES = (
    "ANTHROPIC_API_KEY",
    "ANTHROPIC_AUTH_TOKEN",
    "ANTHROPIC_BASE_URL",
    "ANTHROPIC_MODEL",
    "HTTP_PROXY",
    "HTTPS_PROXY",
    "NO_PROXY",
    "CLAUDE_CODE_GLOBAL_CONFIG",
    "CLAUDE_CODE_PROJECT_CONFIG",
    "CLAUDE_CODE_ALLOWED_TOOLS",
    "CLAUDE_CODE_BETA_FLAGS",
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


def claude_provider() -> str:
    return optional("CLAUDE_PROVIDER", "anthropic").strip().lower()


def claude_model() -> str:
    provider = claude_provider()
    explicit = os.environ.get("CLAUDE_MODEL")
    if not explicit and provider == "anthropic":
        explicit = os.environ.get("ANTHROPIC_MODEL")
    if explicit:
        return explicit
    default = PROVIDER_DEFAULT_MODEL.get(provider)
    if default:
        return default
    raise SystemExit(
        f"No default model for provider {provider!r}. Set CLAUDE_MODEL in your .env "
        "(model IDs are provider-specific; there is no safe cross-provider default)."
    )


def claude_workspace() -> str:
    return optional("CLAUDE_WORKSPACE", DEFAULT_WORKSPACE)


def provider_key_name(provider: str | None = None) -> str:
    provider_name = provider or claude_provider()
    names = provider_key_candidates(provider_name)
    for name in names:
        if os.environ.get(name):
            return name
    return names[0]


def require_provider_key(provider: str | None = None) -> str:
    provider_name = provider or claude_provider()
    names = provider_key_candidates(provider_name)
    for name in names:
        value = os.environ.get(name)
        if value:
            return value
    raise SystemExit(
        "Missing required environment variable: one of " + ", ".join(names)
    )


def provider_key_candidates(provider: str) -> tuple[str, ...]:
    provider = provider.strip().lower()
    default_name = PROVIDER_KEY_ENV.get(provider, f"{provider.upper()}_API_KEY")
    return (default_name, *PROVIDER_KEY_ALIASES.get(provider, ()))


def build_claude_env(include_secrets: bool = True) -> dict[str, str]:
    """Build the env map passed to the Claude Code command inside the sandbox.

    Set ``include_secrets=False`` for the CubeEgress vault flavor: the real
    provider key rides the wire via egress injection, so it must never enter
    the sandbox environment.
    """
    env = {
        "CLAUDE_CODE_DIR": optional("CLAUDE_CODE_DIR", DEFAULT_CLAUDE_DIR),
        "CLAUDE_CODE_SESSION_DIR": optional(
            "CLAUDE_CODE_SESSION_DIR", DEFAULT_SESSION_DIR
        ),
        "CLAUDE_CODE_TELEMETRY": optional("CLAUDE_CODE_TELEMETRY", "0"),
    }
    for name in PASSTHROUGH_ENV_NAMES:
        value = os.environ.get(name)
        if value:
            env[name] = value
    if include_secrets:
        # Forward ONLY the active provider's key(s), never every known secret —
        # a host with several provider keys (e.g. a CI matrix) must not leak all
        # of them into the sandbox.
        for name in provider_key_candidates(claude_provider()):
            value = os.environ.get(name)
            if value:
                env[name] = value
    return env


def claude_llm_host(provider: str | None = None) -> str:
    """Resolve the LLM API host that Claude Code must reach.

    Precedence: explicit ``CLAUDE_LLM_HOST`` > host parsed from
    ``ANTHROPIC_BASE_URL`` > the provider default.
    """
    provider_name = (provider or claude_provider()).strip().lower()
    explicit = os.environ.get("CLAUDE_LLM_HOST")
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
    """CubeEgress credential-injection specs for a provider's auth header(s).

    Each dict maps directly to a ``cubesandbox.Inject(header=..., secret=...,
    format=...)``. CubeEgress attaches these headers to matched outbound
    requests, so the real key rides the wire and never enters the sandbox VM.
    ``format`` defaults to ``${SECRET}`` (raw secret); bearer schemes use
    ``Bearer ${SECRET}``. Anthropic uses ``x-api-key`` plus the required
    API-version header; every other provider uses ``Authorization: Bearer``.
    """
    if provider.strip().lower() == "anthropic":
        return [
            {"header": "x-api-key", "secret": secret, "format": "${SECRET}"},
            {"header": "anthropic-version", "secret": "2023-06-01", "format": "${SECRET}"},
        ]
    return [{"header": "Authorization", "secret": secret, "format": "Bearer ${SECRET}"}]


def claude_command(
    prompt: str,
    *,
    name: str | None = None,
    model: str | None = None,
    permission_mode: str = "acceptEdits",
) -> str:
    """Build a headless (non-interactive) Claude Code invocation.

    ``-p`` (``--print``) makes Claude Code process the prompt and exit instead
    of launching the interactive TUI (which would hang over the E2B exec
    channel). Output is plain text written to stdout.

    ``permission_mode`` controls how Claude Code handles tool permissions.
    Inside an isolated sandbox ``acceptEdits`` (default) auto-approves file
    edits. Pass ``"default"`` for interactive approval of everything.
    """
    args = ["claude", "-p"]
    if name:
        args.extend(["--name", name])
    model = model or claude_model()
    if model:
        args.extend(["--model", model])
    if permission_mode:
        args.extend(["--permission-mode", permission_mode])
    args.append(prompt)
    return " ".join(shlex.quote(arg) for arg in args)


def shell_join(*parts: str) -> str:
    return " && ".join(part for part in parts if part)