# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os
import shlex
import warnings
from pathlib import Path
from urllib.parse import urlparse

from dotenv import load_dotenv

DEFAULT_CODEBUDDY_HOME = "/root/.codebuddy"
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

# Only Anthropic ships a default model; other providers must set CODEBUDDY_MODEL
# explicitly (model IDs are provider-specific and change often), so we fail
# loudly instead of sending a Claude model name to, say, OpenAI.
PROVIDER_DEFAULT_MODEL = {
    "anthropic": "claude-sonnet-4-6",
}

PASSTHROUGH_ENV_NAMES = (
    "ANTHROPIC_BASE_URL",
    "ANTHROPIC_MODEL",
    "HTTP_PROXY",
    "HTTPS_PROXY",
    "NO_PROXY",
)
PROXY_ENV_NAMES = ("HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY")


def load_local_dotenv() -> None:
    """Best-effort load of a nearby .env file without overriding real env vars."""
    candidate_paths = [Path(__file__).with_name(".env")]

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
    """Return the configured non-empty value, otherwise ``default``."""
    return os.environ.get(name) or default


def int_env(name: str, default: int) -> int:
    raw = os.environ.get(name)
    if not raw:
        return default
    try:
        return int(raw)
    except ValueError as exc:
        raise SystemExit(f"{name} must be an integer, got {raw!r}") from exc


def codebuddy_provider() -> str:
    # Normalize case so every downstream comparison and dict lookup
    # (provider_inject, PROVIDER_DEFAULT_HOST, key candidates, ...) is
    # case-insensitive; "Anthropic" and "anthropic" must behave the same.
    return optional("CODEBUDDY_PROVIDER", "anthropic").strip().lower()


def codebuddy_model() -> str:
    provider = codebuddy_provider()
    explicit = os.environ.get("CODEBUDDY_MODEL")
    if not explicit and provider == "anthropic":
        explicit = os.environ.get("ANTHROPIC_MODEL")
    if explicit:
        return explicit
    default = PROVIDER_DEFAULT_MODEL.get(provider)
    if default:
        return default
    raise SystemExit(
        f"No default model for provider {provider!r}. Set CODEBUDDY_MODEL in your .env "
        "(model IDs are provider-specific; there is no safe cross-provider default)."
    )


def codebuddy_workspace() -> str:
    return optional("CODEBUDDY_WORKSPACE", DEFAULT_WORKSPACE)


def codebuddy_permission_mode() -> str:
    return optional("CODEBUDDY_PERMISSION_MODE", "acceptEdits")


def provider_key_name(provider: str | None = None) -> str:
    provider_name = provider or codebuddy_provider()
    names = provider_key_candidates(provider_name)
    for name in names:
        if os.environ.get(name):
            return name
    return names[0]


def require_provider_key(provider: str | None = None) -> str:
    provider_name = provider or codebuddy_provider()
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


def build_codebuddy_env(include_secrets: bool = True) -> dict[str, str]:
    """Build the env map passed to the CodeBuddy command inside the sandbox.

    Set ``include_secrets=False`` for the CubeEgress vault flavor: the real
    provider key rides the wire via egress injection, so it must never enter
    the sandbox environment.
    """
    if include_secrets and any(os.environ.get(name) for name in ("HTTP_PROXY", "HTTPS_PROXY")):
        warnings.warn(
            "Direct-key mode forwards proxy settings; the proxy can observe LLM traffic and credentials.",
            RuntimeWarning,
            stacklevel=2,
        )
    env = {
        "CODEBUDDY_HOME": optional("CODEBUDDY_HOME", DEFAULT_CODEBUDDY_HOME),
        "CODEBUDDY_TELEMETRY": optional("CODEBUDDY_TELEMETRY", "0"),
    }
    for name in PASSTHROUGH_ENV_NAMES:
        if not include_secrets and name in PROXY_ENV_NAMES:
            continue
        value = os.environ.get(name)
        if value:
            env[name] = value
    if include_secrets:
        # Forward ONLY the active provider's key(s), never every known secret —
        # a host with several provider keys (e.g. a CI matrix) must not leak all
        # of them into the sandbox.
        for name in provider_key_candidates(codebuddy_provider()):
            value = os.environ.get(name)
            if value:
                env[name] = value
    return env


def codebuddy_llm_host(provider: str | None = None) -> str:
    """Resolve the LLM API host that CodeBuddy must reach.

    Precedence: explicit ``CODEBUDDY_LLM_HOST`` > host parsed from ``ANTHROPIC_BASE_URL``
    (for Anthropic-compatible endpoints) > the provider default.
    """
    provider_name = (provider or codebuddy_provider()).strip().lower()
    explicit = os.environ.get("CODEBUDDY_LLM_HOST")
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


def codebuddy_command(
    prompt: str, *, output_format: str = "stream-json", name: str | None = None
) -> str:
    """Build a headless (non-interactive) CodeBuddy invocation.

    ``-p`` (``--print``) makes CodeBuddy process the prompt and exit instead of
    launching the interactive TUI (which would hang over the E2B exec channel).
    ``--output-format stream-json`` streams machine-readable JSONL events.
    ``--permission-mode acceptEdits`` allows CodeBuddy to edit files without
    prompting — handy in an isolated sandbox. The prompt is passed as the
    positional argument after ``-p``.
    """
    args = ["codebuddy", "-p"]
    if output_format:
        args.extend(["--output-format", output_format])
    model = codebuddy_model()
    if model:
        args.extend(["--model", model])
    perm_mode = codebuddy_permission_mode()
    if perm_mode:
        args.extend(["--permission-mode", perm_mode])
    if name:
        args.extend(["--name", name])
    args.append(prompt)
    return " ".join(shlex.quote(arg) for arg in args)


def shell_join(*parts: str) -> str:
    return " && ".join(part for part in parts if part)
