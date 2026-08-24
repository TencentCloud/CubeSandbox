# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os
from datetime import datetime

import pytest
import requests

from framework.auth import auth_headers
from framework.assertions import assert_command_ok
from framework.capabilities import LIFECYCLE

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.lifecycle,
    pytest.mark.p1,
    pytest.mark.requires_capability(LIFECYCLE),
]

E2B_NEVER_TIMEOUT_END_AT = "9999-12-31T23:59:59Z"
E2B_USER_AGENTS = (
    "e2b-python-sdk/2.34.0",
    "e2b-js-sdk/2.28.0",
    "e2b-code-interpreter/2.4.1",
)


def test_set_timeout_accepts_never_timeout_via_sdk(sdk_sandbox, sdk_e2e_config):
    sdk_sandbox.set_timeout(-1)

    result = sdk_sandbox.run_command(
        "printf never-timeout-set",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    assert result.stdout == "never-timeout-set"


def test_never_timeout_end_at_is_compatible_with_supported_e2b_user_agents(
    sdk_backend,
    sdk_sandbox,
    sdk_e2e_config,
):
    sdk_sandbox.set_timeout(-1)
    detail_path = f"/sandboxes/{sdk_sandbox.sandbox_id}"

    for user_agent in E2B_USER_AGENTS:
        detail = _get_json(
            sdk_e2e_config,
            sdk_backend,
            detail_path,
            user_agent,
        )
        _assert_e2b_never_timeout_end_at(detail, user_agent)

        listed = _get_json(
            sdk_e2e_config,
            sdk_backend,
            "/sandboxes",
            user_agent,
        )
        _assert_e2b_never_timeout_end_at(
            _find_sandbox(listed, sdk_sandbox.sandbox_id),
            user_agent,
        )

    native_user_agent = "cubesandbox-sdk-compat-e2e/1.0"
    native_detail = _get_json(
        sdk_e2e_config,
        sdk_backend,
        detail_path,
        native_user_agent,
    )
    assert "endAt" not in native_detail, (
        "Cube-native clients must retain the omitted-endAt representation for "
        f"never-timeout sandboxes, got {native_detail.get('endAt')!r}"
    )


def test_set_timeout_accepts_positive_timeout_via_sdk(sdk_sandbox, sdk_e2e_config):
    sdk_sandbox.set_timeout(60)

    result = sdk_sandbox.run_command(
        "printf positive-timeout-set",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    assert result.stdout == "positive-timeout-set"


def test_set_timeout_accepts_zero_timeout_via_sdk(sdk_sandbox):
    sdk_sandbox.set_timeout(0)


def test_set_timeout_rejects_invalid_negative_via_sdk(sdk_sandbox, sdk_e2e_config):
    with pytest.raises(
        Exception,  # noqa: B017 - SDKs wrap API errors differently
    ) as exc_info:
        sdk_sandbox.set_timeout(-2)

    _assert_invalid_timeout_error(exc_info.value)

    result = sdk_sandbox.run_command(
        "printf still-running",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    assert result.stdout == "still-running"


def _assert_invalid_timeout_error(exc: Exception) -> None:
    status_code = getattr(exc, "status_code", None)
    response = getattr(exc, "response", None)
    if status_code is None and response is not None:
        status_code = getattr(response, "status_code", None)
    if status_code is not None:
        assert status_code == 400

    message = str(exc).lower()
    assert "timeout" in message


def _request_headers(sdk_backend: str, user_agent: str) -> dict[str, str]:
    headers = auth_headers()
    if sdk_backend == "e2b":
        api_key = os.environ.get("E2B_API_KEY", "").strip()
        if api_key:
            headers["X-API-Key"] = api_key
    headers["User-Agent"] = user_agent
    return headers


def _get_json(
    sdk_e2e_config,
    sdk_backend: str,
    path: str,
    user_agent: str,
):
    response = requests.get(
        f"{sdk_e2e_config.cube_api_url}{path}",
        headers=_request_headers(sdk_backend, user_agent),
        timeout=sdk_e2e_config.api_timeout,
    )
    assert response.status_code == 200, (
        f"GET {path} with User-Agent {user_agent!r} returned "
        f"HTTP {response.status_code}: {response.text}"
    )
    return response.json()


def _assert_e2b_never_timeout_end_at(payload: dict, user_agent: str) -> None:
    end_at = payload.get("endAt")
    assert end_at == E2B_NEVER_TIMEOUT_END_AT, (
        f"E2B User-Agent {user_agent!r} must receive the never-timeout "
        f"endAt sentinel, got {end_at!r}"
    )
    datetime.fromisoformat(end_at.replace("Z", "+00:00"))


def _find_sandbox(payload: list[dict], sandbox_id: str) -> dict:
    for sandbox in payload:
        if (
            sandbox.get("sandboxID") == sandbox_id
            or sandbox.get("sandbox_id") == sandbox_id
        ):
            return sandbox
    raise AssertionError(f"sandbox {sandbox_id!r} was not present in GET /sandboxes")
