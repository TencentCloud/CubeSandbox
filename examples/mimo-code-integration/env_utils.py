# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import argparse
import json
import os
import shlex
from pathlib import Path
from urllib.parse import urlparse

from dotenv import load_dotenv

DEFAULT_MIMOCODE_HOME = "/root/.mimocode"
DEFAULT_WORKSPACE = "/workspace"
DEFAULT_MODEL = "mimo/mimo-v2.5-pro"
MIMO_PLATFORM_BASE_URL = "https://api.xiaomimimo.com/v1"
MIMO_API_KEY_ENV = "MIMO_API_KEY"

RUNTIME_SWITCHES = {
    "MIMOCODE_PURE": "true",
    "MIMOCODE_DISABLE_SHARE": "true",
    "MIMOCODE_DISABLE_AUTOUPDATE": "true",
    "MIMOCODE_DISABLE_MODELS_FETCH": "true",
    "MIMOCODE_DISABLE_LSP_DOWNLOAD": "true",
    "MIMOCODE_DISABLE_EXTERNAL_SKILLS": "true",
    "MIMOCODE_ENABLE_ANALYSIS": "false",
}


def load_local_dotenv() -> None:
    """Load the example's .env without overriding explicit process values."""
    path = Path(__file__).with_name(".env")
    if path.is_file():
        load_dotenv(dotenv_path=path, override=False)


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
        value = int(raw)
    except ValueError as exc:
        raise SystemExit(f"{name} must be an integer, got {raw!r}") from exc
    if value <= 0:
        raise SystemExit(f"{name} must be greater than zero, got {value}")
    return value


def positive_int(raw: str) -> int:
    try:
        value = int(raw)
    except ValueError as exc:
        raise argparse.ArgumentTypeError(f"expected an integer, got {raw!r}") from exc
    if value <= 0:
        raise argparse.ArgumentTypeError("value must be greater than zero")
    return value


def mimocode_home() -> str:
    home = optional("MIMOCODE_HOME", DEFAULT_MIMOCODE_HOME)
    if not os.path.isabs(home):
        raise SystemExit(f"MIMOCODE_HOME must be an absolute path, got {home!r}")
    return home


def mimo_workspace() -> str:
    workspace = optional("MIMO_WORKSPACE", DEFAULT_WORKSPACE)
    if not os.path.isabs(workspace):
        raise SystemExit(f"MIMO_WORKSPACE must be an absolute path, got {workspace!r}")
    return workspace


def mimo_model() -> str:
    model = optional("MIMO_MODEL", DEFAULT_MODEL).strip()
    provider, separator, model_id = model.partition("/")
    if separator != "/" or provider != "mimo" or not model_id:
        raise SystemExit(
            "MIMO_MODEL must use the MiMo Platform form 'mimo/<model-id>', "
            f"got {model!r}"
        )
    return model


def mimo_model_id() -> str:
    return mimo_model().split("/", 1)[1]


def mimo_api_host() -> str:
    host = urlparse(MIMO_PLATFORM_BASE_URL).hostname
    if not host:
        raise RuntimeError("MiMo Platform base URL has no hostname")
    return host


def build_mimo_config() -> str:
    """Return a deterministic MiMo Platform config with no real credential."""
    model = mimo_model()
    model_id = mimo_model_id()
    config = {
        "model": model,
        "small_model": model,
        "default_agent": "build",
        "share": "disabled",
        "autoupdate": False,
        "enabled_providers": ["mimo"],
        "provider": {
            "mimo": {
                "npm": "@ai-sdk/openai-compatible",
                "name": "MiMo",
                "options": {
                    "baseURL": MIMO_PLATFORM_BASE_URL,
                    "headers": {"api-key": f"{{env:{MIMO_API_KEY_ENV}}}"},
                },
                "models": {model_id: {"name": model_id}},
            }
        },
    }
    return json.dumps(config, separators=(",", ":"), sort_keys=True)


def build_mimo_env(*, include_secret: bool = True) -> dict[str, str]:
    """Build the environment passed to MiMo Code inside the sandbox."""
    env = {
        "MIMOCODE_HOME": mimocode_home(),
        "MIMOCODE_CONFIG_CONTENT": build_mimo_config(),
        **RUNTIME_SWITCHES,
    }
    if include_secret:
        env[MIMO_API_KEY_ENV] = required(MIMO_API_KEY_ENV)
    return env


def mimo_inject(secret: str) -> list[dict[str, str]]:
    """Return the CubeEgress inject spec required by MiMo Platform."""
    return [{"header": "api-key", "secret": secret, "format": "${SECRET}"}]


def mimo_command(
    prompt: str,
    *,
    workspace: str | None = None,
    session_id: str | None = None,
    agent: str | None = None,
    dangerous: bool = True,
) -> str:
    """Build a headless MiMo Code invocation with machine-readable output."""
    args = [
        "mimo",
        "run",
        "--format",
        "json",
        "--dir",
        workspace or mimo_workspace(),
        "--model",
        mimo_model(),
    ]
    if session_id:
        args.extend(["--session", session_id])
    if agent:
        args.extend(["--agent", agent])
    if dangerous:
        args.append("--dangerously-skip-permissions")
    args.append(prompt)
    return shlex.join(args)


def session_list_command(workspace: str | None = None) -> str:
    directory = workspace or mimo_workspace()
    return f"cd {shlex.quote(directory)} && mimo session list --format json"


def shell_join(*parts: str) -> str:
    return " && ".join(part for part in parts if part)
