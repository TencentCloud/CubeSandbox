# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os
import shlex
import sys
from pathlib import Path
from urllib.parse import urlparse

from dotenv import load_dotenv

DEFAULT_BASE_URL = "https://tokenhub.tencentmaas.com/v1"
DEFAULT_MODEL = "hy3"
DEFAULT_WORKSPACE = "/workspace"
DEFAULT_STATE_DIR = "/root/.local/share/opencode"
DIRECT_MODE_WARNING = (
    "[security] Direct mode injects HY3_API_KEY into the OpenCode process "
    "while egress is open. Use network_policy.py for shared or production clusters."
)


def load_local_dotenv() -> None:
    """Load the nearest example .env without overriding real environment values."""
    candidates = (Path(__file__).with_name(".env"), Path.cwd() / ".env")
    seen: set[Path] = set()
    for path in candidates:
        resolved = path.resolve()
        if resolved in seen:
            continue
        seen.add(resolved)
        if path.is_file():
            load_dotenv(dotenv_path=path, override=False)
            return


def required(name: str) -> str:
    value = os.environ.get(name, "").strip()
    if not value:
        raise SystemExit(f"Missing required environment variable: {name}")
    return value


def optional(name: str, default: str = "") -> str:
    return os.environ.get(name, "").strip() or default


def int_env(name: str, default: int) -> int:
    raw = os.environ.get(name, "").strip()
    if not raw:
        return default
    try:
        value = int(raw)
    except ValueError as exc:
        raise SystemExit(f"{name} must be an integer, got {raw!r}") from exc
    if value <= 0:
        raise SystemExit(f"{name} must be greater than zero")
    return value


def warn_direct_mode() -> None:
    print(DIRECT_MODE_WARNING, file=sys.stderr)


def require_hy3_model() -> str:
    model = optional("HY3_MODEL", DEFAULT_MODEL)
    if model != DEFAULT_MODEL:
        raise SystemExit("This example requires HY3_MODEL=hy3")
    return model


def hy3_base_url() -> str:
    value = optional("HY3_BASE_URL", DEFAULT_BASE_URL).rstrip("/")
    parsed = urlparse(value)
    if parsed.scheme != "https" or not parsed.hostname:
        raise SystemExit("HY3_BASE_URL must be a complete HTTPS URL")
    if parsed.username or parsed.password or parsed.query or parsed.fragment:
        raise SystemExit(
            "HY3_BASE_URL must not contain credentials, query, or fragment"
        )
    if parsed.path.rstrip("/") != "/v1":
        raise SystemExit("HY3_BASE_URL path must be exactly /v1")
    return value


def hy3_host(validated_base_url: str) -> str:
    """Extract the host from a value already returned by hy3_base_url()."""
    host = urlparse(validated_base_url).hostname
    if not host:
        raise SystemExit("Could not determine the host from HY3_BASE_URL")
    return host


def opencode_workspace() -> str:
    return optional("OPENCODE_WORKSPACE", DEFAULT_WORKSPACE)


def build_opencode_env(*, include_secret: bool) -> dict[str, str]:
    """Build the minimal environment passed to OpenCode inside the sandbox."""
    env = {
        "HY3_BASE_URL": hy3_base_url(),
        "HY3_MODEL": require_hy3_model(),
        "OPENCODE_DISABLE_AUTOUPDATE": "1",
        "OPENCODE_DISABLE_SHARE": "1",
        "OPENCODE_DISABLE_MODELS_FETCH": "1",
        "OPENCODE_DISABLE_DEFAULT_PLUGINS": "1",
        "OPENCODE_DISABLE_LSP_DOWNLOAD": "1",
    }
    if include_secret:
        env["HY3_API_KEY"] = required("HY3_API_KEY")
    return env


def opencode_command(
    prompt: str,
    *,
    session_id: str | None = None,
    output_format: str = "json",
    auto_approve: bool = True,
) -> str:
    """Build a deterministic, headless OpenCode invocation."""
    if output_format not in {"default", "json"}:
        raise ValueError(f"Unsupported OpenCode output format: {output_format}")
    args = [
        "opencode",
        "run",
        "--pure",
        "--format",
        output_format,
        "--model",
        "tokenhub/hy3",
    ]
    if auto_approve:
        args.append("--auto")
    if session_id:
        args.extend(["--session", session_id])
    args.append(prompt)
    return " ".join(shlex.quote(arg) for arg in args)


def shell_join(*parts: str) -> str:
    return " && ".join(part for part in parts if part)
