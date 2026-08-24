# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import pytest
import requests

from adapters.api_adapter import ApiClient
from framework.assertions import assert_command_ok
from framework.capabilities import LIFECYCLE

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.lifecycle,
    pytest.mark.p1,
    pytest.mark.requires_capability(LIFECYCLE),
]


def test_refresh_omitted_duration_keeps_sandbox_usable(sdk_sandbox, sdk_e2e_config):
    api = ApiClient(sdk_e2e_config)
    try:
        api.refresh_sandbox(sdk_sandbox.sandbox_id)
    finally:
        api.close()

    result = sdk_sandbox.run_command(
        "printf refreshed",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    assert result.stdout == "refreshed"


@pytest.mark.parametrize(
    ("duration", "stdout"),
    [
        (-1, "refresh-never-timeout"),
        (60, "refresh-positive-timeout"),
        (7200, "refresh-long-timeout"),
    ],
)
def test_refresh_accepts_valid_duration_values(
    sdk_sandbox,
    sdk_e2e_config,
    duration,
    stdout,
):
    api = ApiClient(sdk_e2e_config)
    try:
        api.refresh_sandbox(sdk_sandbox.sandbox_id, duration=duration)
    finally:
        api.close()

    result = sdk_sandbox.run_command(
        f"printf {stdout}",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    assert result.stdout == stdout


@pytest.mark.parametrize("duration", [0, -2])
def test_refresh_rejects_invalid_duration_values(
    sdk_sandbox,
    sdk_e2e_config,
    duration,
):
    api = ApiClient(sdk_e2e_config)
    try:
        with pytest.raises(requests.HTTPError) as exc_info:
            api.refresh_sandbox(sdk_sandbox.sandbox_id, duration=duration)
    finally:
        api.close()

    response = exc_info.value.response
    assert response is not None
    assert response.status_code == 400
    assert "duration" in response.text.lower()
