# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Configuration and secret-handling helpers for the OpenCode example."""

from __future__ import annotations

import os
from collections.abc import Collection
from dataclasses import dataclass, field
from pathlib import Path
from urllib.parse import urlparse

from dotenv import load_dotenv


DEFAULT_WORKSPACE = "/workspace"
DEFAULT_NODE_CA_BUNDLE = "/etc/ssl/certs/ca-certificates.crt"
DEFAULT_PLACEHOLDER_KEY = "cube-egress-managed-placeholder"


@dataclass(frozen=True)
class ProviderSpec:
    name: str
    key_env: str
    default_host: str
    auth_header: str
    auth_format: str


@dataclass(frozen=True)
class ProviderConfig:
    name: str
    model: str
    host: str
    key_env: str
    auth_header: str
    auth_format: str
    secret: str = field(repr=False)


@dataclass(frozen=True)
class RunConfig:
    api_url: str
    template_id: str
    workspace: str
    sandbox_timeout: int
    exec_timeout: int
    node_ca_bundle: str
    provider: ProviderConfig


PROVIDERS: dict[str, ProviderSpec] = {
    "anthropic": ProviderSpec(
        name="anthropic",
        key_env="ANTHROPIC_API_KEY",
        default_host="api.anthropic.com",
        auth_header="x-api-key",
        auth_format="${SECRET}",
    ),
    "openai": ProviderSpec(
        name="openai",
        key_env="OPENAI_API_KEY",
        default_host="api.openai.com",
        auth_header="Authorization",
        auth_format="Bearer ${SECRET}",
    ),
    "deepseek": ProviderSpec(
        name="deepseek",
        key_env="DEEPSEEK_API_KEY",
        default_host="api.deepseek.com",
        auth_header="Authorization",
        auth_format="Bearer ${SECRET}",
    ),
    "openrouter": ProviderSpec(
        name="openrouter",
        key_env="OPENROUTER_API_KEY",
        default_host="openrouter.ai",
        auth_header="Authorization",
        auth_format="Bearer ${SECRET}",
    ),
}

OPENCODE_PASSTHROUGH_ENV = (
    "OPENCODE_CONFIG",
    "OPENCODE_CONFIG_DIR",
    "OPENCODE_CONFIG_CONTENT",
)


def load_local_dotenv() -> None:
    """Load the nearest example .env without overriding process variables."""
    candidates = (Path(__file__).with_name(".env"), Path.cwd() / ".env")
    seen: set[Path] = set()
    for candidate in candidates:
        resolved = candidate.resolve()
        if resolved in seen:
            continue
        seen.add(resolved)
        if candidate.is_file():
            load_dotenv(dotenv_path=candidate, override=False)
            return


def required(name: str) -> str:
    value = os.environ.get(name)
    if value is None or not value.strip():
        raise SystemExit(f"Missing required environment variable: {name}")
    return value


def optional(name: str, default: str = "") -> str:
    return os.environ.get(name) or default


def int_env(name: str, default: int, *, minimum: int = 1) -> int:
    raw = os.environ.get(name)
    if not raw:
        return default
    try:
        value = int(raw)
    except ValueError as exc:
        raise SystemExit(f"{name} must be an integer, got {raw!r}") from exc
    if value < minimum:
        raise SystemExit(f"{name} must be at least {minimum}, got {value}")
    return value


def provider_name() -> str:
    return optional("OPENCODE_PROVIDER", "anthropic").strip().lower()


def provider_config(*, require_secret: bool = True) -> ProviderConfig:
    name = provider_name()
    try:
        spec = PROVIDERS[name]
    except KeyError as exc:
        supported = ", ".join(sorted(PROVIDERS))
        raise SystemExit(
            f"Unsupported OPENCODE_PROVIDER {name!r}; choose one of: {supported}"
        ) from exc

    model = required("OPENCODE_MODEL").strip()
    if "/" not in model:
        raise SystemExit(
            "OPENCODE_MODEL must use OpenCode's provider/model form, "
            f"got {model!r}"
        )
    model_provider = model.split("/", 1)[0].strip().lower()
    if model_provider != name:
        raise SystemExit(
            f"OPENCODE_MODEL provider {model_provider!r} does not match "
            f"OPENCODE_PROVIDER {name!r}"
        )

    secret = os.environ.get(spec.key_env, "")
    if require_secret and not secret.strip():
        raise SystemExit(f"Missing required environment variable: {spec.key_env}")

    explicit_host = optional("OPENCODE_LLM_HOST")
    host = _host_from_url(explicit_host) if explicit_host else spec.default_host
    if not host:
        raise SystemExit("Could not determine the LLM API host")

    return ProviderConfig(
        name=spec.name,
        model=model,
        host=host,
        key_env=spec.key_env,
        auth_header=spec.auth_header,
        auth_format=spec.auth_format,
        secret=secret,
    )


def run_config(*, require_secret: bool = True) -> RunConfig:
    api_url = required("CUBE_API_URL").strip().rstrip("/")
    workspace = optional("OPENCODE_WORKSPACE", DEFAULT_WORKSPACE).strip()
    if not workspace:
        raise SystemExit("OPENCODE_WORKSPACE must not be empty")
    return RunConfig(
        api_url=api_url,
        template_id=required("CUBE_TEMPLATE_ID").strip(),
        workspace=workspace,
        sandbox_timeout=int_env("OPENCODE_SANDBOX_TIMEOUT", 1800),
        exec_timeout=int_env("OPENCODE_EXEC_TIMEOUT", 900),
        node_ca_bundle=optional(
            "OPENCODE_NODE_CA_BUNDLE", DEFAULT_NODE_CA_BUNDLE
        ).strip(),
        provider=provider_config(require_secret=require_secret),
    )


def build_opencode_env(
    provider: ProviderConfig | None = None,
    *,
    include_secret: bool = True,
) -> dict[str, str]:
    """Build the minimal environment passed to OpenCode inside the VM.

    With ``include_secret=False`` the VM receives only a placeholder. The real
    key is expected to be injected into the outbound request by CubeEgress.
    """
    config = provider or provider_config(require_secret=include_secret)
    env = {
        "OPENCODE_AUTO_SHARE": optional("OPENCODE_AUTO_SHARE", "false"),
        "OPENCODE_DISABLE_AUTOUPDATE": optional(
            "OPENCODE_DISABLE_AUTOUPDATE", "true"
        ),
        "OPENCODE_DISABLE_PRUNE": optional("OPENCODE_DISABLE_PRUNE", "true"),
        "OPENCODE_DISABLE_TERMINAL_TITLE": optional(
            "OPENCODE_DISABLE_TERMINAL_TITLE", "true"
        ),
    }
    for name in OPENCODE_PASSTHROUGH_ENV:
        value = os.environ.get(name)
        if value:
            env[name] = value

    if include_secret:
        if not config.secret:
            raise SystemExit(f"Missing required environment variable: {config.key_env}")
        env[config.key_env] = config.secret
    else:
        env[config.key_env] = optional(
            "OPENCODE_PLACEHOLDER_KEY", DEFAULT_PLACEHOLDER_KEY
        )
        env["NODE_EXTRA_CA_CERTS"] = optional(
            "OPENCODE_NODE_CA_BUNDLE", DEFAULT_NODE_CA_BUNDLE
        )
    return env


def provider_inject(provider: ProviderConfig) -> list[dict[str, str]]:
    """Return CubeEgress Inject constructor arguments for this provider."""
    if not provider.secret:
        raise SystemExit(f"Missing required environment variable: {provider.key_env}")
    return [
        {
            "header": provider.auth_header,
            "secret": provider.secret,
            "format": provider.auth_format,
        }
    ]


def known_secret_values() -> tuple[str, ...]:
    values = {
        value
        for spec in PROVIDERS.values()
        if (value := os.environ.get(spec.key_env)) and value.strip()
    }
    return tuple(sorted(values, key=len, reverse=True))


def redact_secrets(
    text: object,
    secrets: Collection[str] | None = None,
) -> str:
    redacted = "" if text is None else str(text)
    values = secrets if secrets is not None else known_secret_values()
    for secret in sorted({value for value in values if value}, key=len, reverse=True):
        redacted = redacted.replace(secret, "<redacted>")
    return redacted


def _host_from_url(value: str) -> str:
    candidate = value.strip()
    if not candidate:
        return ""
    if "://" not in candidate:
        candidate = f"https://{candidate}"
    parsed = urlparse(candidate)
    return (parsed.hostname or "").lower()
