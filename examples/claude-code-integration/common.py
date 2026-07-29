# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Small, dependency-free helpers shared by the Claude Code examples.

Kept SDK-agnostic so the same helpers work with both the e2b-compatible
SDK and the native ``cubesandbox`` SDK.
"""

from __future__ import annotations

import json
import os
import shlex
import sys
from collections.abc import Callable
from pathlib import Path
from typing import Any
from urllib.parse import urlparse

# ---------------------------------------------------------------------------
# dotenv (no extra dependency — matches gemini-cli pattern)
# ---------------------------------------------------------------------------

def load_dotenv(path: str = ".env") -> None:
    """Load simple KEY=VALUE lines without overriding exported variables."""
    dotenv = Path(path)
    if not dotenv.is_file():
        return
    for line in dotenv.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        key = key.strip()
        value = value.strip().strip('"').strip("'")
        if key:
            os.environ.setdefault(key, value)


# ---------------------------------------------------------------------------
# env helpers
# ---------------------------------------------------------------------------

DEFAULT_WORKSPACE = "/workspace"

PROVIDER_DEFAULT_HOST = "api.anthropic.com"
PROVIDER_KEY_NAME = "ANTHROPIC_API_KEY"


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


def cc_model() -> str:
    return optional("CC_MODEL", "claude-sonnet-4-6")


def cc_workspace() -> str:
    return optional("CC_WORKSPACE", DEFAULT_WORKSPACE)


def require_api_key() -> str:
    # ANTHROPIC_AUTH_TOKEN is used by third-party providers (e.g. DeepSeek)
    # and by Claude Code OAuth; ANTHROPIC_API_KEY is the standard variable.
    for name in (PROVIDER_KEY_NAME, "ANTHROPIC_AUTH_TOKEN"):
        value = os.environ.get(name)
        if value:
            return value
    raise SystemExit(
        f"Missing required environment variable: {PROVIDER_KEY_NAME} or ANTHROPIC_AUTH_TOKEN"
    )


def build_cc_env(include_secrets: bool = True) -> dict[str, str]:
    env: dict[str, str] = {}
    for name in (
        "ANTHROPIC_BASE_URL",
        "HTTP_PROXY",
        "HTTPS_PROXY",
        "NO_PROXY",
    ):
        value = os.environ.get(name)
        if value:
            env[name] = value
    if include_secrets:
        for name in (PROVIDER_KEY_NAME, "ANTHROPIC_AUTH_TOKEN"):
            value = os.environ.get(name)
            if value:
                env[name] = value
    return env


def cc_llm_host() -> str:
    explicit = os.environ.get("CC_LLM_HOST")
    if explicit:
        # CC_LLM_HOST is documented as a hostname (e.g. "api.anthropic.com"),
        # not a URL — return it directly.
        return explicit.strip()
    base_url = os.environ.get("ANTHROPIC_BASE_URL")
    if base_url:
        host = _host_from_url(base_url)
        if host:
            return host
    return PROVIDER_DEFAULT_HOST


def _host_from_url(value: str) -> str:
    candidate = value.strip()
    if not candidate:
        return ""
    if "://" not in candidate:
        candidate = f"https://{candidate}"
    return urlparse(candidate).hostname or ""


def cc_command(
    prompt: str,
    *,
    model: str | None = None,
    effort: str | None = None,
    permission_mode: str | None = None,
    output_format: str = "json",
    dangerously_skip_permissions: bool = False,
) -> str:
    """Build a headless (non-interactive) ``claude -p`` invocation.

    When ``dangerously_skip_permissions=True`` the command must run as a
    non-root user (Claude Code refuses this flag for root).  Combine with
    ``run_command(..., user=\"user\")``.
    """
    args = ["claude", "-p", shlex.quote(prompt), "--output-format", output_format]

    if output_format == "stream-json":
        args.append("--verbose")

    if model:
        args.extend(["--model", shlex.quote(model)])
    if effort:
        args.extend(["--effort", shlex.quote(effort)])
    if permission_mode:
        args.extend(["--permission-mode", shlex.quote(permission_mode)])
    if dangerously_skip_permissions:
        args.append("--dangerously-skip-permissions")

    return " ".join(args)


def shell_join(*parts: str) -> str:
    return " && ".join(part for part in parts if part)


# ---------------------------------------------------------------------------
# sandbox command helpers
# ---------------------------------------------------------------------------

def stream_writer(stream) -> Callable[[object], None]:
    def write(chunk: object) -> None:
        text = getattr(chunk, "line", chunk)
        stream.write(str(text))
        stream.flush()
    return write


def _tool_brief(arguments: dict) -> str:
    for key in ("command", "file_path", "path", "pattern", "query", "url"):
        value = arguments.get(key)
        if value:
            return str(value).replace("\n", " ")[:120]
    return ""


def _render_json_result(obj: dict) -> None:
    result_text = obj.get("result", "")
    if result_text and isinstance(result_text, str):
        print(result_text.strip())

    if obj.get("is_error"):
        print(f"\n[error] {obj.get('result', 'unknown error')}")

    usage = obj.get("usage", {})
    if usage:
        cost = obj.get("total_cost_usd", 0)
        tokens_in = usage.get("input_tokens", 0)
        tokens_out = usage.get("output_tokens", 0)
        print(f"\n--- cost: ${cost:.4f} | tokens: {tokens_in} in / {tokens_out} out ---")


def _render_stream_json_line(line: str) -> None:
    line = line.rstrip("\r\n")
    if not line.strip():
        return

    try:
        event = json.loads(line)
    except (ValueError, TypeError):
        print(line)
        return

    if not isinstance(event, dict):
        return

    etype = event.get("type", "")

    if etype == "assistant":
        message = event.get("message", {})
        if isinstance(message, dict):
            content = message.get("content", "")
            if isinstance(content, str) and content.strip():
                print(content.strip())
            elif isinstance(content, list):
                for block in content:
                    if not isinstance(block, dict):
                        continue
                    btype = block.get("type", "")
                    if btype == "text":
                        text = str(block.get("text", "")).strip()
                        if text:
                            print(text)
                    elif btype == "tool_use":
                        name = block.get("name", "tool")
                        inp = block.get("input", {})
                        brief = _tool_brief(inp) if isinstance(inp, dict) else ""
                        print(f"  -> [tool] {name}{': ' + brief if brief else ''}")

    elif etype == "user":
        message = event.get("message", {})
        if isinstance(message, dict):
            content = message.get("content", "")
            if isinstance(content, list):
                for block in content:
                    if isinstance(block, dict) and block.get("type") == "tool_result":
                        is_error = block.get("is_error", False)
                        prefix = "  X" if is_error else "  OK"
                        tool_id = block.get("tool_use_id", "")[:20]
                        print(f"{prefix} [tool_result] {tool_id}")

    elif etype == "result":
        _render_json_result(event)

    elif etype == "system":
        subtype = event.get("subtype", "")
        if subtype == "init":
            model = event.get("model", "?")
            version = event.get("claude_code_version", "?")
            print(f"[init] model={model}, claude_code={version}")


def json_render_writer() -> Callable[[object], None]:
    buffer: dict[str, str] = {"text": ""}

    def write(chunk: object) -> None:
        text = getattr(chunk, "line", chunk)
        buffer["text"] += text if isinstance(text, str) else str(text)
        while "\n" in buffer["text"]:
            line, buffer["text"] = buffer["text"].split("\n", 1)
            _render_stream_json_line(line)

    return write


def run_command(
    sandbox: Any,
    command: str,
    *,
    cwd: str | None = None,
    envs: dict[str, str] | None = None,
    timeout: int | float | None = None,
    stream: bool = False,
    user: str = "root",
):
    kwargs = {"cwd": cwd, "timeout": timeout, "user": user}
    kwargs = {key: value for key, value in kwargs.items() if value is not None}
    if envs:
        kwargs["envs"] = envs
    if stream:
        raw = os.environ.get("CC_STREAM_RAW", "").strip().lower() in ("1", "true", "yes")
        kwargs["on_stdout"] = stream_writer(sys.stdout) if raw else json_render_writer()
        kwargs["on_stderr"] = stream_writer(sys.stderr)

    try:
        return sandbox.commands.run(command, **kwargs)
    except TypeError as exc:
        if "envs" not in kwargs or "envs" not in str(exc):
            raise
        kwargs["env"] = kwargs.pop("envs")
        return sandbox.commands.run(command, **kwargs)
    except Exception as exc:
        # e2b SDK raises CommandExitException (a CommandResult subclass) when
        # the sandbox command exits non-zero.  Claude Code writes errors to
        # stdout as JSON, so the exception's stdout field carries the
        # diagnostic.  Return the exception object itself — it has .stdout,
        # .stderr, .exit_code, and .error.
        cls = type(exc).__name__
        if cls == "CommandExitException" or "CommandExit" in cls:
            return exc
        raise


def ensure_success(result, action: str) -> None:
    exit_code = getattr(result, "exit_code", None)
    if exit_code not in (None, 0):
        stdout = getattr(result, "stdout", "")
        stderr = getattr(result, "stderr", "")
        raise SystemExit(
            f"Failed to {action} (exit {exit_code}).\nSTDOUT:\n{stdout}\nSTDERR:\n{stderr}"
        )


def sandbox_identifier(sandbox: Any) -> str:
    return getattr(sandbox, "sandbox_id", getattr(sandbox, "id", "unknown"))
