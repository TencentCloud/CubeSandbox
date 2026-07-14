# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os
import shlex
import warnings
from pathlib import Path
from urllib.parse import urlparse

from dotenv import load_dotenv

DEFAULT_OPENCODE_DIR = "/root/.opencode"
DEFAULT_WORKSPACE = "/workspace"

PROVIDER_KEY_ENV = {
    "anthropic": "ANTHROPIC_API_KEY",
    "openai": "OPENAI_API_KEY",
    "google": "GEMINI_API_KEY",
}

PROVIDER_DEFAULT_HOST = {
    "anthropic": "api.anthropic.com",
    "openai": "api.openai.com",
    "google": "generativelanguage.googleapis.com",
}

# Only Anthropic ships a default model; other providers must set OPENCODE_MODEL
# explicitly (model IDs are provider-specific and change often), so we fail
# loudly instead of sending a Claude model name to, say, OpenAI.
PROVIDER_DEFAULT_MODEL = {
    "anthropic": "claude-sonnet-4-6",
}

PASSTHROUGH_ENV_NAMES = (
    "ANTHROPIC_BASE_URL",
    "HTTP_PROXY",
    "HTTPS_PROXY",
    "NO_PROXY",
)


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
    return os.environ.get(name) or default


def int_env(name: str, default: int) -> int:
    raw = os.environ.get(name)
    if not raw:
        return default
    try:
        return int(raw)
    except ValueError as exc:
        raise SystemExit(f"{name} must be an integer, got {raw!r}") from exc


def opencode_provider() -> str:
    # Normalize case so every downstream comparison and dict lookup
    # (provider_inject, PROVIDER_DEFAULT_HOST, key candidates, ...) is
    # case-insensitive; "Anthropic" and "anthropic" must behave the same.
    return optional("OPENCODE_PROVIDER", "anthropic").strip().lower()


def opencode_model() -> str:
    provider = opencode_provider()
    explicit = os.environ.get("OPENCODE_MODEL")
    if explicit:
        return explicit
    default = PROVIDER_DEFAULT_MODEL.get(provider)
    if default:
        return default
    raise SystemExit(
        f"No default model for provider {provider!r}. Set OPENCODE_MODEL in your .env "
        "(model IDs are provider-specific; there is no safe cross-provider default)."
    )


def opencode_workspace() -> str:
    return optional("OPENCODE_WORKSPACE", DEFAULT_WORKSPACE)


def provider_key_name(provider: str | None = None) -> str:
    provider_name = provider or opencode_provider()
    names = provider_key_candidates(provider_name)
    for name in names:
        if os.environ.get(name):
            return name
    return names[0]


def require_provider_key(provider: str | None = None) -> str:
    provider_name = provider or opencode_provider()
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
    return (default_name,)


def build_opencode_env(include_secrets: bool = True) -> dict[str, str]:
    """Build the env map passed to the OpenCode command inside the sandbox.

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
        "OPENCODE_DIR": optional("OPENCODE_DIR", DEFAULT_OPENCODE_DIR),
        "OPENCODE_TELEMETRY": optional("OPENCODE_TELEMETRY", "0"),
    }
    for name in PASSTHROUGH_ENV_NAMES:
        value = os.environ.get(name)
        if value:
            env[name] = value
    if include_secrets:
        # Forward ONLY the active provider's key(s), never every known secret —
        # a host with several provider keys (e.g. a CI matrix) must not leak all
        # of them into the sandbox.
        for name in provider_key_candidates(opencode_provider()):
            value = os.environ.get(name)
            if value:
                env[name] = value
    return env


def opencode_llm_host(provider: str | None = None) -> str:
    """Resolve the LLM API host that OpenCode must reach.

    Precedence: explicit ``OPENCODE_LLM_HOST`` > host parsed from
    ``ANTHROPIC_BASE_URL`` for Anthropic > the provider default.
    """
    provider_name = (provider or opencode_provider()).strip().lower()
    explicit = os.environ.get("OPENCODE_LLM_HOST")
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


def opencode_run_command(
    prompt: str, *, provider: str | None = None, model: str | None = None
) -> str:
    """Build a headless (non-interactive) OpenCode invocation.

    ``opencode run "prompt"`` makes OpenCode process the prompt and exit
    instead of launching an interactive session. The prompt is passed as the
    positional argument to the ``run`` subcommand.
    """
    args = ["opencode", "run"]
    p = provider or opencode_provider()
    m = model or opencode_model()
    if p:
        args.extend(["--provider", p])
    if m:
        args.extend(["--model", m])
    args.append(prompt)
    return " ".join(shlex.quote(arg) for arg in args)


def opencode_serve_command(
    *, hostname: str = "0.0.0.0", port: int = 4096,
    provider: str | None = None, model: str | None = None,
) -> str:
    """Build an OpenCode serve invocation for SDK-based integration.

    ``opencode serve --hostname 0.0.0.0 --port 4096`` starts an HTTP server
    that the ``@opencode-ai/sdk`` can connect to for programmatic control.
    """
    args = ["opencode", "serve"]
    args.extend(["--hostname", hostname])
    args.extend(["--port", str(port)])
    p = provider or opencode_provider()
    m = model or opencode_model()
    if p:
        args.extend(["--provider", p])
    if m:
        args.extend(["--model", m])
    return " ".join(shlex.quote(arg) for arg in args)


def shell_join(*parts: str) -> str:
    return " && ".join(part for part in parts if part)
