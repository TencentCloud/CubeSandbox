# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os
import shlex
from pathlib import Path
from urllib.parse import urlparse, urlunparse

from dotenv import load_dotenv

# Default CodeBuddy state directory (overridable via CODEBUDDY_CONFIG_DIR). Must
# match the value baked into the Dockerfile so pause/resume snapshots land on
# the expected path. Lives under /workspace so the SDK-allowed exec user
# (``user``) can write to it without the home-dir write hole that
# ``/home/codebuddy/.codebuddy`` would otherwise open.
DEFAULT_CODEBUDDY_HOME = "/workspace/.codebuddy"
DEFAULT_WORKSPACE = "/workspace"

# API key / base URL for each provider. The "io" map covers the international
# site (codebuddy.ai); "internal" is the China site (copilot.tencent.com).
INTERNET_ENVIRONMENTS = ("io", "internal", "ioa")

# Provider key env vars and the upstream host CodeBuddy Code calls when the
# matching CODEBUDDY_API_KEY is set. The provider is selected by
# CODEBUDDY_INTERNET_ENVIRONMENT (see env_utils.internet_environment()).
PROVIDER_KEY_ENV = {
    "anthropic": "ANTHROPIC_API_KEY",
    "openai": "OPENAI_API_KEY",
    "deepseek": "DEEPSEEK_API_KEY",
    "google": "GEMINI_API_KEY",
    "codebuddy_io": "CODEBUDDY_API_KEY",
}

PROVIDER_DEFAULT_HOST = {
    "anthropic": "api.anthropic.com",
    "openai": "api.openai.com",
    "deepseek": "api.deepseek.com",
    "google": "generativelanguage.googleapis.com",
    "codebuddy_io": "api.codebuddy.ai",
}

# Default model when the user only sets CODEBUDDY_MODEL to nothing; Anthropic is
# the most common provider paired with CodeBuddy Code today, so we ship a sane
# default there. Other providers require an explicit model (model IDs change
# often and there is no safe cross-provider default).
PROVIDER_DEFAULT_MODEL = {
    "anthropic": "claude-sonnet-4-6",
}

# Variables that are forwarded verbatim into the in-sandbox exec env. The list
# is intentionally narrow: only env vars that affect CodeBuddy's request shape
# or operator-controlled behavior. API keys are forwarded separately (see
# build_codebuddy_env / include_secrets=False) so a CI host with several
# providers never leaks all of them into one sandbox.
PASSTHROUGH_ENV_NAMES = (
    "ANTHROPIC_BASE_URL",
    "ANTHROPIC_MODEL",
    "CODEBUDDY_BASE_URL",
    "CODEBUDDY_MODEL",
    "CODEBUDDY_SMALL_FAST_MODEL",
    "CODEBUDDY_BIG_SLOW_MODEL",
    "CODEBUDDY_CODE_SUBAGENT_MODEL",
    "CODEBUDDY_INTERNET_ENVIRONMENT",
    "MAX_THINKING_TOKENS",
    "HTTP_PROXY",
    "HTTPS_PROXY",
    "NO_PROXY",
    "CODEBUDDY_CUSTOM_HEADERS",
)


def load_local_dotenv() -> None:
    """Best-effort load of a nearby .env file without overriding real env vars."""
    for path in (
        Path(__file__).with_name(".env"),
        Path.cwd() / ".env",
    ):
        if path.is_file():
            load_dotenv(dotenv_path=path, override=False)
            return


def required(name: str) -> str:
    value = os.environ.get(name)
    if not value:
        raise SystemExit(f"Missing required environment variable: {name}")
    return value


def cube_required(cube_name: str, legacy_name: str) -> str:
    """Resolve a CUBE_* config key with an E2B_* legacy fallback.

    ``cube_name`` is the canonical env-var name (``CUBE_API_URL``, ``CUBE_API_KEY``);
    ``legacy_name`` is the older alias (``E2B_API_URL``, ``E2B_API_KEY``). The
    function checks the canonical name first and falls back to the legacy name,
    so existing deployments that only set ``E2B_*`` continue to work without changes.
    """
    value = os.environ.get(cube_name) or os.environ.get(legacy_name)
    if not value:
        raise SystemExit(
            f"Missing required environment variable: set either {cube_name} "
            f"(preferred) or {legacy_name} (legacy alias) in your .env"
        )
    return value


def optional(name: str, default: str = "") -> str:
    """Return ``os.environ[name]`` if the key exists, else ``default``.

    An explicitly-empty value (``CODEBUDDY_FOO=""``) is propagated verbatim —
    we only fall back when the variable is genuinely unset. This avoids the
    ``os.environ.get(key) or default`` foot-gun where empty strings silently
    flip to the default and mask bad .env input.
    """
    if name not in os.environ:
        return default
    return os.environ[name]


def int_env(name: str, default: int) -> int:
    raw = os.environ.get(name)
    if raw is None or raw == "":
        return default
    try:
        return int(raw)
    except ValueError as exc:
        raise SystemExit(f"{name} must be an integer, got {raw!r}") from exc


def _env_positive_int(name: str, default: int) -> int:
    """``int_env`` + ``positive_int``: rejects zero/negative env-var values too.

    argparse evaluates ``default=`` before ``type=``, so a bare
    ``default=int_env(...)`` lets a malformed env var (e.g.
    ``CODEBUDDY_SANDBOX_TIMEOUT=0``) bypass the ``type=positive_int`` check.
    Use this helper for timeout defaults that share the positive-integer
    constraint with their CLI flag.
    """
    raw = os.environ.get(name)
    if raw is None or raw == "":
        return default
    try:
        parsed = int(raw)
    except ValueError as exc:
        raise SystemExit(f"{name} must be an integer, got {raw!r}") from exc
    if parsed <= 0:
        raise SystemExit(f"{name} must be a positive integer, got {parsed}")
    return parsed


def codebuddy_home() -> str:
    return optional("CODEBUDDY_CONFIG_DIR", DEFAULT_CODEBUDDY_HOME)


def codebuddy_workspace() -> str:
    return optional("CODEBUDDY_WORKSPACE", DEFAULT_WORKSPACE)


def internet_environment() -> str:
    """Normalize CODEBUDDY_INTERNET_ENVIRONMENT to one of the supported values.

    ``io`` (international), ``internal`` (China), ``ioa`` (Tencent enterprise).
    We lowercase + strip so accidental whitespace / case from a ``.env`` file
    does not push CodeBuddy into the wrong auth flow.
    """
    value = optional("CODEBUDDY_INTERNET_ENVIRONMENT", "io").strip().lower()
    if value not in INTERNET_ENVIRONMENTS:
        raise SystemExit(
            f"CODEBUDDY_INTERNET_ENVIRONMENT must be one of "
            f"{', '.join(INTERNET_ENVIRONMENTS)}, got {value!r}"
        )
    return value


def provider() -> str:
    """Pick a logical provider for key injection / allow-list resolution.

    The CodeBuddy upstream itself keys off CODEBUDDY_INTERNET_ENVIRONMENT plus
    CODEBUDDY_API_KEY / CODEBUDDY_BASE_URL; this helper just translates that
    trio into a single string so the egress allow-list (network_policy.py) can
    pick the right host and the right auth-header shape.

    **Heuristic caveat:** host detection uses substring matching (``"anthropic" in
    host``, etc.). A custom gateway hostname like ``anthropic-proxy.attacker.io``
    would match incorrectly. Set ``CODEBUDDY_PROVIDER`` explicitly to bypass the
    heuristic whenever a non-standard upstream is in use.
    """
    explicit = os.environ.get("CODEBUDDY_PROVIDER")
    if explicit:
        return explicit.strip().lower()
    env = internet_environment()
    if env == "io":
        return "codebuddy_io"
    base_url = os.environ.get("CODEBUDDY_BASE_URL") or ""
    host = _host_from_url(base_url)
    if "anthropic" in host:
        return "anthropic"
    if "openai" in host:
        return "openai"
    if "deepseek" in host:
        return "deepseek"
    if "googleapis" in host:
        return "google"
    return "codebuddy_io"


def codebuddy_model() -> str:
    """Resolve the model CodeBuddy should run against.

    Precedence: explicit ``CODEBUDDY_MODEL`` > provider-specific env
    (``ANTHROPIC_MODEL``, ...) > provider default (Anthropic only).
    """
    provider_name = provider()
    explicit = os.environ.get("CODEBUDDY_MODEL")
    if not explicit and provider_name == "anthropic":
        explicit = os.environ.get("ANTHROPIC_MODEL")
    if explicit:
        return explicit
    default = PROVIDER_DEFAULT_MODEL.get(provider_name)
    if default:
        return default
    raise SystemExit(
        f"No default model for provider {provider_name!r}. Set CODEBUDDY_MODEL in your "
        ".env (model IDs are provider-specific; there is no safe cross-provider default)."
    )


def provider_key_name() -> str:
    """Return the env-var name we expect to hold the active provider's key."""
    provider_name = provider()
    candidates = provider_key_candidates(provider_name)
    for name in candidates:
        if os.environ.get(name):
            return name
    return candidates[0]


def require_provider_key() -> str:
    """Resolve the active provider key, raising if none is set.

    Unlike pi-agent we only need one key — CodeBuddy itself only ever talks to
    one upstream at a time — but we still scan multiple env-var names so
    users who copy-paste their Anthropic env still work without editing.
    """
    provider_name = provider()
    candidates = provider_key_candidates(provider_name)
    for name in candidates:
        value = os.environ.get(name)
        if value:
            return value
    raise SystemExit(
        "Missing required environment variable: one of " + ", ".join(candidates)
    )


def provider_key_candidates(provider_name: str) -> tuple[str, ...]:
    provider_name = provider_name.strip().lower()
    default_name = PROVIDER_KEY_ENV.get(
        provider_name, f"{provider_name.upper()}_API_KEY"
    )
    # CodeBuddy also accepts CODEBUDDY_AUTH_TOKEN as a platform auth token; the
    # user usually wants the API key path, but we fall back to it so the same
    # .env file works for both.
    candidates = [default_name, "CODEBUDDY_API_KEY", "CODEBUDDY_AUTH_TOKEN"]
    if provider_name == "codebuddy_io":
        # When the user keeps the international CodeBuddy site as the default
        # but points CODEBUDDY_BASE_URL at a custom upstream, fall back to the
        # matching provider key so they do not have to set CODEBUDDY_PROVIDER.
        base_url = os.environ.get("CODEBUDDY_BASE_URL") or ""
        host = _host_from_url(base_url)
        if "anthropic" in host:
            candidates.append("ANTHROPIC_API_KEY")
        elif "openai" in host:
            candidates.append("OPENAI_API_KEY")
        elif "deepseek" in host:
            candidates.append("DEEPSEEK_API_KEY")
        elif "googleapis" in host:
            candidates.append("GEMINI_API_KEY")
    return tuple(candidates)


def llm_host() -> str:
    """Resolve the LLM API host that CodeBuddy must reach.

    Precedence: explicit ``CODEBUDDY_LLM_HOST`` > host parsed from
    ``CODEBUDDY_BASE_URL`` / ``ANTHROPIC_BASE_URL`` > provider default.
    """
    provider_name = provider()
    explicit = os.environ.get("CODEBUDDY_LLM_HOST")
    if explicit:
        return _host_from_url(explicit)
    for env_name in ("CODEBUDDY_BASE_URL", "ANTHROPIC_BASE_URL"):
        base_url = os.environ.get(env_name)
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


# Proxy env vars whose URLs may carry embedded credentials (e.g.
# ``http://user:pass@corp-proxy:8080``). CodeBuddy's LLM agent runs inside the
# VM and would otherwise see those credentials in its own environment — strip
# them before forwarding. The other LLM-host resolution paths here never see a
# URL with userinfo (we extract only the hostname), so this only matters for the
# HTTP_PROXY passthrough below.
PROXY_URL_ENV_NAMES = frozenset({"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"})


def strip_url_userinfo(value: str) -> str:
    """Remove the ``user:password@`` segment from a URL.

    Accepts both full URLs (``http://u:p@h:8080``) and bare hoststrings
    (``u:p@h:8080``); the latter is normalized with an ``https://`` prefix
    first so ``urlparse`` can split authority from path. Strings that do not
    parse cleanly are returned unchanged rather than masked, since the
    downstream consumer is the Linux env inside the sandbox, which is the
    same place any malformed proxy URL would already fail.
    """
    candidate = value.strip()
    if not candidate or "@" not in candidate:
        return value
    if "://" not in candidate:
        candidate = f"https://{candidate}"
    parsed = urlparse(candidate)
    if not parsed.hostname:
        return value
    if parsed.username is None and parsed.password is None:
        # ``@`` present but only as a non-credential delimiter — leave alone.
        return value
    netloc = parsed.hostname
    if parsed.port is not None:
        netloc = f"{netloc}:{parsed.port}"
    return urlunparse(parsed._replace(netloc=netloc))


def provider_inject(provider_name: str, secret: str) -> list[dict[str, str]]:
    """CubeEgress credential-injection specs for a provider's auth header(s).

    Each dict maps directly to a ``cubesandbox.Inject(header=..., secret=...,
    format=...)``. CubeEgress attaches these headers to matched outbound
    requests, so the real key rides the wire and never enters the sandbox VM.
    Anthropic uses ``x-api-key`` plus the required API-version header; every
    other provider uses ``Authorization: Bearer``.
    """
    if provider_name.strip().lower() == "anthropic":
        return [
            {"header": "x-api-key", "secret": secret, "format": "${SECRET}"},
            {"header": "anthropic-version", "secret": "2023-06-01", "format": "${SECRET}"},
        ]
    return [{"header": "Authorization", "secret": secret, "format": "Bearer ${SECRET}"}]


def build_codebuddy_env(include_secrets: bool = True) -> dict[str, str]:
    """Build the env map passed to the CodeBuddy command inside the sandbox.

    Set ``include_secrets=False`` for the CubeEgress vault flavor: the real
    provider key rides the wire via egress injection, so it must never enter
    the sandbox environment.
    """
    env = {
        "CODEBUDDY_CONFIG_DIR": codebuddy_home(),
        "CODEBUDDY_INTERNET_ENVIRONMENT": internet_environment(),
        "DISABLE_TELEMETRY": optional("DISABLE_TELEMETRY", "1"),
        "DISABLE_ERROR_REPORTING": optional("DISABLE_ERROR_REPORTING", "1"),
        "DISABLE_AUTOUPDATER": optional("DISABLE_AUTOUPDATER", "1"),
        "DISABLE_FEEDBACK_COMMAND": optional("DISABLE_FEEDBACK_COMMAND", "1"),
    }
    for name in PASSTHROUGH_ENV_NAMES:
        value = os.environ.get(name)
        if value:
            # Proxy URLs may carry host credentials (http://user:pass@proxy:...)
            # which would otherwise leak to the LLM agent inside the VM. Strip
            # the userinfo segment but keep the host:port so the agent can still
            # route through the proxy.
            if name in PROXY_URL_ENV_NAMES:
                value = strip_url_userinfo(value)
            env[name] = value
    if include_secrets:
        # Forward ONLY the first (highest-priority) provider key that is actually
        # set, never every candidate — a host with several provider keys (e.g. a
        # CI matrix) must not leak all of them into the sandbox. The fallback
        # chain in provider_key_candidates covers multi-name aliases (e.g.
        # ANTHROPIC_API_KEY vs CODEBUDDY_API_KEY) for the same logical provider,
        # but once one name matches we stop, so a dual-key host only forwards the
        # one the operator actually set for this invocation.
        for name in provider_key_candidates(provider()):
            value = os.environ.get(name)
            if value:
                env[name] = value
                break
    return env


def codebuddy_command(
    prompt: str,
    *,
    dangerously_skip_permissions: bool = True,
    resume: str | None = None,
    continue_session: bool = False,
    session_id: str | None = None,
    model: str | None = None,
) -> str:
    """Build a headless (non-interactive) CodeBuddy invocation.

    ``-p`` makes CodeBuddy process the prompt and exit instead of launching the
    interactive TUI (which would hang over the E2B exec channel). ``-y``
    (a.k.a. ``--dangerously-skip-permissions``) trusts every tool call for this
    run — the alternative is a permission prompt that cannot be answered over
    the non-interactive exec channel. Pass ``dangerously_skip_permissions=False``
    only if you have wired up a safe sandbox-specific tool allow-list via
    settings.json.

    Session flags:
      * ``-c`` (continue) re-uses the most recent session.
      * ``-r <id>`` / ``--resume <id>`` re-uses a specific session.
      * ``--session-id <uuid>`` pins the session id for the new run.

    The prompt is the trailing positional argument.
    """
    args = ["codebuddy", "-p"]
    if dangerously_skip_permissions:
        args.append("-y")
    if continue_session:
        args.append("-c")
    if resume:
        args.extend(["--resume", resume])
    if session_id:
        args.extend(["--session-id", session_id])
    if model:
        args.extend(["--model", model])
    args.append(prompt)
    return " ".join(shlex.quote(arg) for arg in args)


def shell_join(*parts: str) -> str:
    return " && ".join(part for part in parts if part)
