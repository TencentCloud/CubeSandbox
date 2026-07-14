# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os
import shlex
from pathlib import Path
from urllib.parse import urlparse

from dotenv import load_dotenv

DEFAULT_WORKSPACE = "/workspace"

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


def claude_code_model() -> str:
    """Resolve the model Claude Code should use.

    Precedence: explicit ``CLAUDE_CODE_MODEL`` > default ``claude-sonnet-4-6``.
    """
    explicit = os.environ.get("CLAUDE_CODE_MODEL")
    if explicit:
        return explicit
    return "claude-sonnet-4-6"


def claude_code_workspace() -> str:
    return optional("CLAUDE_CODE_WORKSPACE", DEFAULT_WORKSPACE)


def require_anthropic_key() -> str:
    """Return the Anthropic API key, failing if absent."""
    value = os.environ.get("ANTHROPIC_API_KEY")
    if not value:
        raise SystemExit("Missing required environment variable: ANTHROPIC_API_KEY")
    return value


def build_claude_code_env(include_secrets: bool = True) -> dict[str, str]:
    """Build the env map passed to the Claude Code command inside the sandbox.

    Set ``include_secrets=False`` for the CubeEgress vault flavor: the real
    Anthropic key rides the wire via egress injection, so it must never enter
    the sandbox environment.
    """
    env: dict[str, str] = {}
    for name in PASSTHROUGH_ENV_NAMES:
        value = os.environ.get(name)
        if value:
            env[name] = value
    if include_secrets:
        # Forward the Anthropic key so Claude Code can authenticate with the
        # LLM backend. Under the vault flavor the key is a placeholder — the
        # real one is injected on the wire by CubeEgress.
        anthropic_key = os.environ.get("ANTHROPIC_API_KEY")
        if anthropic_key:
            env["ANTHROPIC_API_KEY"] = anthropic_key
    return env


def claude_code_llm_host() -> str:
    """Resolve the LLM API host that Claude Code must reach.

    Precedence: explicit ``CLAUDE_CODE_LLM_HOST`` > host parsed from
    ``ANTHROPIC_BASE_URL`` > default ``api.anthropic.com``.
    """
    explicit = os.environ.get("CLAUDE_CODE_LLM_HOST")
    if explicit:
        return _host_from_url(explicit)
    base_url = os.environ.get("ANTHROPIC_BASE_URL")
    if base_url:
        host = _host_from_url(base_url)
        if host:
            return host
    return "api.anthropic.com"


def anthropic_inject(secret: str) -> list[dict[str, str]]:
    """CubeEgress credential-injection specs for Anthropic's auth headers.

    Each dict maps directly to a ``cubesandbox.Inject(header=..., secret=...,
    format=...)``. CubeEgress attaches these headers to matched outbound
    requests, so the real key rides the wire and never enters the sandbox VM.
    Anthropic uses ``x-api-key`` plus the required API-version header.
    """
    return [
        {"header": "x-api-key", "secret": secret, "format": "${SECRET}"},
        {"header": "anthropic-version", "secret": "2023-06-01", "format": "${SECRET}"},
    ]


def claude_command(
    prompt: str,
    *,
    output_format: str = "stream-json",
    model: str | None = None,
    accept_edits: bool = True,
) -> str:
    """Build a headless (non-interactive) Claude Code invocation.

    ``-p`` (``--print``) makes Claude Code process the prompt and exit instead
    of launching the interactive TUI (which would hang over the E2B exec
    channel). ``--output-format stream-json`` streams machine-readable JSONL
    events. ``accept_edits`` toggles ``--accept-edits``, which auto-approves
    file edits in an isolated sandbox — pass ``accept_edits=False`` in
    high-security workflows. The prompt is passed as the positional argument
    to ``-p``.
    """
    args = ["claude", "-p"]
    if output_format:
        args.extend(["--output-format", output_format])
    resolved_model = model or claude_code_model()
    if resolved_model:
        args.extend(["--model", resolved_model])
    if accept_edits:
        args.append("--accept-edits")
    args.append(prompt)
    return " ".join(shlex.quote(arg) for arg in args)


def shell_join(*parts: str) -> str:
    return " && ".join(part for part in parts if part)


def _host_from_url(value: str) -> str:
    candidate = value.strip()
    if not candidate:
        return ""
    if "://" not in candidate:
        candidate = f"https://{candidate}"
    return urlparse(candidate).hostname or ""
