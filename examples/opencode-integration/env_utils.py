# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os
import shlex
from pathlib import Path
from urllib.parse import urlparse, urlunparse

from dotenv import load_dotenv

# Default OpenCode config directory (overridable via OPENCODE_CONFIG_DIR).
# OpenCode separates config (this) from data (``$OPENCODE_DATA_DIR``); see
# ``opencode_data_dir()`` below for the data side (auth, sessions, storage,
# logs). Both must match the ``ARG`` values baked into the Dockerfile so
# pause/resume snapshots land on the expected paths. Both live under
# ``/workspace`` so the SDK-allowed exec user (``user``) can write to them
# without the home-dir write hole that ``/home/opencode/.config/opencode``
# would otherwise open.
DEFAULT_OPENCODE_CONFIG_DIR = "/workspace/.opencode/config"
DEFAULT_OPENCODE_DATA_DIR = "/workspace/.opencode/data"

# Default workspace inside the sandbox. OpenCode is invoked with ``cd
# $WORKSPACE`` before each run so file edits and tool output land where the
# snapshot will capture them.
DEFAULT_WORKSPACE = "/workspace"

# OpenCode ships with built-in provider presets (anthropic, openai,
# google, azure, bedrock, ...). The CLI keys off the well-known provider
# environment variables (ANTHROPIC_API_KEY, OPENAI_API_KEY, ...); we mirror
# that map here so the egress allow-list (network_policy.py) can pick the
# right upstream host and the right auth-header shape.
PROVIDER_KEY_ENV = {
    "anthropic": "ANTHROPIC_API_KEY",
    "openai": "OPENAI_API_KEY",
    "google": "GOOGLE_API_KEY",
    "google-vertex": "GOOGLE_VERTEX_API_KEY",
    "azure": "AZURE_API_KEY",
    "bedrock": "AWS_BEDROCK_API_KEY",
    "deepseek": "DEEPSEEK_API_KEY",
    "groq": "GROQ_API_KEY",
    "mistral": "MISTRAL_API_KEY",
    "openrouter": "OPENROUTER_API_KEY",
}

PROVIDER_DEFAULT_HOST = {
    "anthropic": "api.anthropic.com",
    "openai": "api.openai.com",
    "google": "generativelanguage.googleapis.com",
    "google-vertex": "aiplatform.googleapis.com",
    "azure": "openai.azure.com",
    "bedrock": "bedrock-runtime.us-east-1.amazonaws.com",
    "deepseek": "api.deepseek.com",
    "groq": "api.groq.com",
    "mistral": "api.mistral.ai",
    "openrouter": "openrouter.ai",
}

# Default model when the user does not set OPENCODE_MODEL. Anthropic is the
# most common provider paired with OpenCode today, so we ship a sane default
# there. Other providers require an explicit model (model IDs change often
# and there is no safe cross-provider default).
PROVIDER_DEFAULT_MODEL = {
    "anthropic": "claude-sonnet-4-6",
}

# Variables that are forwarded verbatim into the in-sandbox exec env. The list
# is intentionally narrow: only env vars that affect OpenCode's request shape
# or operator-controlled behavior. API keys are forwarded separately (see
# build_opencode_env / include_secrets=False) so a CI host with several
# providers never leaks all of them into one sandbox.
PASSTHROUGH_ENV_NAMES = (
    "ANTHROPIC_BASE_URL",
    "ANTHROPIC_MODEL",
    "OPENAI_BASE_URL",
    "OPENAI_MODEL",
    "GOOGLE_BASE_URL",
    "OPENCODE_BASE_URL",
    "OPENCODE_MODEL",
    "OPENCODE_SMALL_MODEL",
    "OPENCODE_PROVIDER",
    "HTTP_PROXY",
    "HTTPS_PROXY",
    "NO_PROXY",
    "OPENCODE_CUSTOM_HEADERS",
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

    An explicitly-empty value (``OPENCODE_FOO=""``) is propagated verbatim —
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
    ``OPENCODE_SANDBOX_TIMEOUT=0``) bypass the ``type=positive_int`` check.
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


def opencode_home() -> str:
    return optional("OPENCODE_CONFIG_DIR", DEFAULT_OPENCODE_CONFIG_DIR)


def opencode_data_dir() -> str:
    """Return the OpenCode data directory (sessions, auth.json, storage, logs).

    OpenCode separates config (``$OPENCODE_CONFIG_DIR``/``$XDG_CONFIG_HOME``)
    from data (``$OPENCODE_DATA_DIR``/``$XDG_DATA_HOME``). The data dir is
    where session storage and provider auth land; capturing it in the
    snapshot is what makes pause/resume actually preserve the conversation.
    Defaults to ``/workspace/.opencode/data`` so the data dir follows the
    workspace into the snapshot, matching the Dockerfile's ``OPENCODE_DATA_DIR``
    ``ARG``.
    """
    return optional("OPENCODE_DATA_DIR", DEFAULT_OPENCODE_DATA_DIR)


def opencode_workspace() -> str:
    return optional("OPENCODE_WORKSPACE", DEFAULT_WORKSPACE)


def provider() -> str:
    """Pick a logical provider for key injection / allow-list resolution.

    OpenCode itself keys off the upstream env vars (``ANTHROPIC_API_KEY``,
    ``OPENAI_API_KEY``, ...); this helper just translates that into a single
    string so the egress allow-list (network_policy.py) can pick the right
    host and the right auth-header shape.

    **Heuristic caveat:** host detection uses substring matching on the
    hostname only (``"anthropic" in host``, etc.). A custom gateway hostname
    like ``anthropic-proxy.attacker.io`` would match to ``anthropic``.
    Set ``OPENCODE_PROVIDER`` explicitly to bypass the heuristic whenever a
    non-standard upstream is in use. URLs whose Anthropic compatibility
    lives only in the path (e.g. ``https://api.deepseek.com/anthropic``)
    are intentionally not auto-detected — they require an explicit
    ``OPENCODE_PROVIDER=anthropic``.
    """
    explicit = os.environ.get("OPENCODE_PROVIDER")
    if explicit:
        return explicit.strip().lower()
    # Prefer the active API key when one is set — that is the strongest
    # signal of which upstream OpenCode will actually call.
    for name, env_var in PROVIDER_KEY_ENV.items():
        if os.environ.get(env_var):
            return name
    # BASE_URL substring heuristic. ``anthropic`` is checked first because
    # many providers (DeepSeek, Moonshot, etc.) ship Anthropic-compatible
    # gateways under their own hostname — the user usually wants the
    # anthropic credential path, not a per-provider key.
    base_url = os.environ.get("OPENCODE_BASE_URL") or ""
    host = _host_from_url(base_url)
    if "anthropic" in host:
        return "anthropic"
    if "openai" in host:
        return "openai"
    if "deepseek" in host:
        return "deepseek"
    if "googleapis" in host:
        return "google"
    if "openrouter" in host:
        return "openrouter"
    if "groq" in host:
        return "groq"
    if "mistral" in host:
        return "mistral"
    # Default to anthropic when no env signal is present; ``opencode_model``
    # will require OPENCODE_MODEL unless the provider has a known default.
    return "anthropic"


def opencode_model() -> str:
    """Resolve the model OpenCode should run against.

    Precedence: explicit ``OPENCODE_MODEL`` > provider-specific env
    (``ANTHROPIC_MODEL``, ``OPENAI_MODEL``, ...) > provider default
    (Anthropic only).
    """
    provider_name = provider()
    explicit = os.environ.get("OPENCODE_MODEL")
    if not explicit and provider_name == "anthropic":
        explicit = os.environ.get("ANTHROPIC_MODEL")
    if explicit:
        return explicit
    default = PROVIDER_DEFAULT_MODEL.get(provider_name)
    if default:
        return default
    raise SystemExit(
        f"No default model for provider {provider_name!r}. Set OPENCODE_MODEL in your "
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

    OpenCode itself only talks to one upstream at a time, so we only need one
    key — but we still scan multiple env-var names so users who copy-paste
    their Anthropic env still work without editing.
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
    return (default_name,)


def llm_host() -> str:
    """Resolve the LLM API host that OpenCode must reach.

    Precedence: explicit ``OPENCODE_LLM_HOST`` > host parsed from
    ``OPENCODE_BASE_URL`` / ``ANTHROPIC_BASE_URL`` / ``OPENAI_BASE_URL`` >
    provider default.
    """
    provider_name = provider()
    explicit = os.environ.get("OPENCODE_LLM_HOST")
    if explicit:
        return _host_from_url(explicit)
    for env_name in (
        "OPENCODE_BASE_URL",
        "ANTHROPIC_BASE_URL",
        "OPENAI_BASE_URL",
        "GOOGLE_BASE_URL",
    ):
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
# ``http://user:pass@corp-proxy:8080``). OpenCode's LLM agent runs inside the
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


def build_opencode_env(include_secrets: bool = True) -> dict[str, str]:
    """Build the env map passed to the OpenCode command inside the sandbox.

    Set ``include_secrets=False`` for the CubeEgress vault flavor: the real
    provider key rides the wire via egress injection, so it must never enter
    the sandbox environment.
    """
    env = {
        "OPENCODE_CONFIG_DIR": opencode_home(),
        "OPENCODE_DATA_DIR": opencode_data_dir(),
        "XDG_CONFIG_HOME": "/workspace",
        "XDG_DATA_HOME": "/workspace",
        "DISABLE_TELEMETRY": optional("DISABLE_TELEMETRY", "1"),
        "DISABLE_ERROR_REPORTING": optional("DISABLE_ERROR_REPORTING", "1"),
        "OPENCODE_DISABLE_AUTOUPDATE": optional("OPENCODE_DISABLE_AUTOUPDATE", "1"),
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
        # Forward ONLY the provider key that matches the active provider — a
        # host with several provider keys (e.g. a CI matrix) must not leak all
        # of them into the sandbox.
        key_name = provider_key_name()
        value = os.environ.get(key_name)
        if value:
            env[key_name] = value
    return env


def opencode_command(
    prompt: str,
    *,
    dangerously_skip_permissions: bool = True,
    resume: str | None = None,
    continue_session: bool = False,
    session_id: str | None = None,
    model: str | None = None,
) -> str:
    """Build a headless (non-interactive) OpenCode invocation.

    ``opencode run`` makes OpenCode process the prompt and exit instead of
    launching the interactive TUI (which would hang over the E2B exec
    channel). By default OpenCode prompts for permission on every tool
    call; in non-interactive mode that prompt cannot be answered, so the
    run hangs. Pass ``--dangerously-skip-permissions`` (added in OpenCode
    1.17.x) to auto-approve every tool call for the duration of the run.
    Equivalent to ``OPENCODE_PERMISSION='{"*":"allow"}'`` in the env. Pass
    ``dangerously_skip_permissions=False`` only if you have wired up a
    safe sandbox-specific tool allow-list via ``opencode.json``.

    Session flags:
      * ``-c`` (continue) re-uses the most recent session.
      * ``-s <id>`` re-uses a specific session id.
      * ``--session-id <uuid>`` pins the session id for the new run.

    The prompt is the trailing positional argument.
    """
    args = ["opencode", "run"]
    if dangerously_skip_permissions:
        args.append("--dangerously-skip-permissions")
    if continue_session:
        args.append("-c")
    if resume:
        args.extend(["-s", resume])
    if session_id:
        args.extend(["--session-id", session_id])
    if model:
        args.extend(["-m", model])
    args.append(prompt)
    return " ".join(shlex.quote(arg) for arg in args)


def shell_join(*parts: str) -> str:
    return " && ".join(part for part in parts if part)
