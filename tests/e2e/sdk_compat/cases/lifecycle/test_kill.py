# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import pytest

from adapters.api_adapter import ApiClient
from framework.capabilities import LIFECYCLE
from framework.lifecycle import assert_connect_fails, is_terminal_failure, sandbox_listed

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.lifecycle,
    pytest.mark.p1,
    pytest.mark.requires_capability(LIFECYCLE),
]


def test_kill_prevents_reconnect(sdk_sandbox, sdk_backend, sdk_e2e_config):
    sandbox_id = sdk_sandbox.sandbox_id

    sdk_sandbox.kill()

    failure = assert_connect_fails(sandbox_id, sdk_backend, sdk_e2e_config)
    assert failure

    api = ApiClient(sdk_e2e_config)
    try:
        assert api.get_sandbox(sandbox_id) == {}
    finally:
        api.close()


def test_kill_removes_sandbox_from_listing(sdk_sandbox, sdk_backend, sdk_e2e_config):
    sandbox_id = sdk_sandbox.sandbox_id

    sdk_sandbox.kill()

    listed = sandbox_listed(sandbox_id, sdk_backend, sdk_e2e_config)
    if listed is None:
        pytest.skip(f"backend {sdk_backend!r} does not expose a reliable sandbox list API")
    assert listed is False


def test_kill_is_idempotent_or_reports_terminal_state(
    sdk_sandbox,
    sdk_backend,
    sdk_e2e_config,
):
    sandbox_id = sdk_sandbox.sandbox_id
    sdk_sandbox.kill()

    try:
        sdk_sandbox.kill()
    except Exception as exc:  # noqa: BLE001 - backend-specific terminal response
        assert is_terminal_failure(exc), (
            "second kill must expose a terminal-state response, "
            f"got {type(exc).__name__}: {exc}"
        )

    failure = assert_connect_fails(sandbox_id, sdk_backend, sdk_e2e_config)
    assert failure
