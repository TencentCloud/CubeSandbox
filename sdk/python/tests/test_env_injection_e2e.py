# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""E2E: Sandbox.create(env_vars=...) 注入的变量可被 commands.run 使用."""

from __future__ import annotations

import os
import uuid

import pytest

from cubesandbox import Config, Sandbox


pytestmark = pytest.mark.e2e


def _option(pytestconfig: pytest.Config, option: str, env: str, default: str | None = None) -> str | None:
    return pytestconfig.getoption(option) or os.environ.get(env) or default


def _require_e2e(pytestconfig: pytest.Config) -> None:
    if not pytestconfig.getoption("--run-e2e") and os.environ.get("CUBE_E2E") != "1":
        pytest.skip("use --run-e2e or set CUBE_E2E=1 to run live CubeAPI e2e tests")


def _config(pytestconfig: pytest.Config) -> Config:
    return Config(
        api_url=_option(pytestconfig, "--cube-api-url", "CUBE_API_URL", "http://127.0.0.1:3000"),
        template_id=_option(pytestconfig, "--cube-template-id", "CUBE_TEMPLATE_ID"),
        proxy_node_ip=os.environ.get("CUBE_PROXY_NODE_IP", "127.0.0.1"),
        timeout=600,
    )


def test_create_env_vars_available_in_commands_run(pytestconfig: pytest.Config) -> None:
    _require_e2e(pytestconfig)
    config = _config(pytestconfig)
    if not config.template_id:
        pytest.skip("set CUBE_TEMPLATE_ID or pass --cube-template-id")

    env_key = f"CUBE_PY_CREATE_ENV_{uuid.uuid4().hex[:8].upper()}"
    env_value = f"injected-{uuid.uuid4().hex[:8]}"
    sb = Sandbox.create(env_vars={env_key: env_value, "MY_APP_TOKEN": "token-abc-123"}, config=config)

    try:
        # commands.run 内部使用 bash -l -c, 与 SDK 语义一致
        got = sb.commands.run(f"printenv {env_key}")
        assert got.exit_code == 0, got.stderr
        assert env_value in got.stdout

        token = sb.commands.run("printenv MY_APP_TOKEN")
        assert token.exit_code == 0, token.stderr
        assert "token-abc-123" in token.stdout
    finally:
        sb.kill()
