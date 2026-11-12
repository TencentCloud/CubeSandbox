# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""E2E coverage for the E2B-compatible ``mcp`` create option.

Each sandbox is created from a template that ships ``mcp-gateway`` (alias
``mcp-gateway`` by default, override with ``SDK_E2E_MCP_TEMPLATE_ID``); see
``examples/mcp-gateway/template/Dockerfile``. The whole module is skipped when
that template does not exist. The ``time`` server is used because it
is pre-installed there and needs no internet access.
"""

from __future__ import annotations

import os
from dataclasses import replace

import pytest

from adapters import connect_adapter, create_adapter
from framework.capabilities import MCP_GATEWAY, PAUSE_RESUME, ROLLBACK_CLONE
from framework.cleanup import safe_kill
from framework.mcp_client import McpHttpClient, tool_text

MCP_TEMPLATE_ID = os.environ.get("SDK_E2E_MCP_TEMPLATE_ID", "mcp-gateway")
MCP_CONFIG = {"time": {}}
TIME_TOOL = "get_current_time"

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.mcp,
    pytest.mark.p1,
    pytest.mark.requires_capability(MCP_GATEWAY),
    pytest.mark.sandbox_template_id(MCP_TEMPLATE_ID, optional=True),
    pytest.mark.sandbox_create_options(mcp=MCP_CONFIG),
]


def _assert_time_tool_works(sdk_sandbox, sdk_e2e_config, token: str) -> None:
    with McpHttpClient(sdk_sandbox.mcp_url(), token, sdk_e2e_config) as client:
        info = client.initialize()
        assert info.get("serverInfo", {}).get("name") == "cube-mcp-gateway", info
        names = {tool["name"] for tool in client.list_tools()}
        assert TIME_TOOL in names, f"tools={sorted(names)}"
        result = client.call_tool(TIME_TOOL, {"timezone": "UTC"})
        assert not result.get("isError"), result
        assert "UTC" in tool_text(result), result


def test_mcp_gateway_serves_configured_servers(sdk_sandbox, sdk_e2e_config):
    token = sdk_sandbox.mcp_token()
    assert token, "the SDK should expose the gateway token after create"
    assert sdk_sandbox.mcp_url().endswith(f"50005-{sdk_sandbox.sandbox_id}.{sdk_e2e_config.cube_sandbox_domain}/mcp")
    _assert_time_tool_works(sdk_sandbox, sdk_e2e_config, token)


def test_mcp_gateway_requires_token(sdk_sandbox, sdk_e2e_config):
    payload = {"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": {}}
    for token in (None, "not-the-token"):
        with McpHttpClient(sdk_sandbox.mcp_url(), token, sdk_e2e_config) as client:
            response = client.post_raw(payload)
        assert response.status_code == 401, (
            f"token={token!r} status={response.status_code} body={response.text[:200]!r}"
        )


def test_mcp_token_is_recoverable_after_connect(sdk_sandbox, sdk_backend, sdk_e2e_config):
    token = sdk_sandbox.mcp_token()
    connected = connect_adapter(sdk_backend, sdk_sandbox.sandbox_id, sdk_e2e_config)
    try:
        assert connected.mcp_token() == token
        _assert_time_tool_works(connected, sdk_e2e_config, token)
    finally:
        connected.close()


@pytest.mark.slow
@pytest.mark.requires_capability(PAUSE_RESUME)
def test_mcp_gateway_survives_pause_resume(sdk_sandbox, sdk_e2e_config):
    token = sdk_sandbox.mcp_token()
    _assert_time_tool_works(sdk_sandbox, sdk_e2e_config, token)
    sdk_sandbox.pause(timeout=sdk_e2e_config.create_timeout)
    resumed = sdk_sandbox.resume_or_connect(timeout=sdk_e2e_config.create_timeout)
    try:
        _assert_time_tool_works(resumed, sdk_e2e_config, token)
    finally:
        resumed.close()


@pytest.mark.slow
@pytest.mark.requires_capability(ROLLBACK_CLONE)
def test_mcp_gateway_is_inherited_by_clone(sdk_sandbox, sdk_e2e_config):
    token = sdk_sandbox.mcp_token()
    clones = sdk_sandbox.clone(n=1)
    try:
        assert len(clones) == 1
        clone = clones[0]
        assert clone.sandbox_id != sdk_sandbox.sandbox_id
        assert clone.mcp_token() == token
        _assert_time_tool_works(clone, sdk_e2e_config, token)
    finally:
        for clone in clones:
            safe_kill(clone, sdk_e2e_config)


@pytest.mark.parametrize(
    ("mcp", "message"),
    [
        (["time"], "keyed by MCP server name"),
        ({"time": "yes"}, "configuration must be an object"),
        ({"github/acme": {"runCmd": "x"}}, "github/<owner>/<repo>"),
    ],
)
def test_invalid_mcp_is_rejected_at_create(sdk_backend, sdk_e2e_config, mcp, message):
    adapter = None
    try:
        with pytest.raises(Exception) as exc_info:
            adapter = create_adapter(
                sdk_backend,
                replace(sdk_e2e_config, cube_template_id=MCP_TEMPLATE_ID),
                metadata={"test_suite": "sdk_compat", "test_case": "invalid_mcp"},
                create_options={"mcp": mcp},
            )
        assert message in str(exc_info.value), str(exc_info.value)
    finally:
        if adapter is not None:
            safe_kill(adapter, sdk_e2e_config)


def test_unknown_server_fails_gateway_start(sdk_backend, sdk_e2e_config):
    adapter = None
    try:
        with pytest.raises(Exception) as exc_info:
            adapter = create_adapter(
                sdk_backend,
                replace(sdk_e2e_config, cube_template_id=MCP_TEMPLATE_ID),
                metadata={"test_suite": "sdk_compat", "test_case": "unknown_mcp_server"},
                create_options={"mcp": {"no-such-server": {}}},
            )
        assert "Failed to start MCP gateway" in str(exc_info.value), str(exc_info.value)
        assert "no-such-server" in str(exc_info.value), str(exc_info.value)
    finally:
        if adapter is not None:
            safe_kill(adapter, sdk_e2e_config)
