# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import pytest
from framework.assertions import assert_command_ok
from framework.capabilities import SET_TIMEOUT

pytestmark = [pytest.mark.e2e, pytest.mark.sdk_compat, pytest.mark.p0]


@pytest.mark.smoke
def test_create_returns_usable_sandbox(sdk_sandbox, sdk_e2e_config):
    info = sdk_sandbox.info()

    assert sdk_sandbox.sandbox_id
    assert info.sandbox_id == sdk_sandbox.sandbox_id

    result = sdk_sandbox.run_command(
        "printf 'sdk-compat:%s' \"$USER\"",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    assert result.stdout.startswith("sdk-compat:")


def test_info_is_stable_for_created_sandbox(sdk_sandbox):
    first = sdk_sandbox.info()
    second = sdk_sandbox.info()

    assert first.sandbox_id == sdk_sandbox.sandbox_id
    assert second.sandbox_id == sdk_sandbox.sandbox_id


@pytest.mark.p1
def test_kill_destroys_sandbox(sdk_sandbox, sdk_e2e_config):
    sdk_sandbox.kill()

    # After kill() the sandbox should be gone, so info() must raise.
    raised = False
    try:
        sdk_sandbox.info()
    except Exception:
        raised = True
    assert raised, "expected info() to raise after kill()"


@pytest.mark.p1
@pytest.mark.requires_capability(SET_TIMEOUT)
def test_set_timeout_updates_sandbox(sdk_sandbox, sdk_e2e_config):
    sdk_sandbox.set_timeout(600)

    # set_timeout must not error and must not move the sandbox into a terminal state.
    info = sdk_sandbox.info()
    assert info.sandbox_id == sdk_sandbox.sandbox_id
    if info.state is not None:
        assert info.state.lower() not in {"killed", "stopped", "dead", "expired"}
