# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import pytest

from framework.assertions import assert_code_ok, assert_command_ok
from framework.capabilities import PAUSE_RESUME, RUN_CODE
from framework.lifecycle import wait_until_paused

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.lifecycle,
    pytest.mark.p1,
    pytest.mark.requires_capability(PAUSE_RESUME),
]


def test_pause_sets_state_paused(sdk_sandbox, sdk_e2e_config):
    sdk_sandbox.pause(timeout=sdk_e2e_config.default_timeout)
    state = wait_until_paused(sdk_sandbox, timeout=sdk_e2e_config.default_timeout)
    assert state == "paused"


def test_pause_and_connect_resume_preserves_files(sdk_sandbox, sdk_e2e_config):
    sdk_sandbox.write_file("/tmp/sdk-compat-pause.txt", "before-pause")

    sdk_sandbox.pause(timeout=sdk_e2e_config.default_timeout)
    resumed = sdk_sandbox.resume_or_connect(timeout=sdk_e2e_config.default_timeout)
    try:
        assert resumed.sandbox_id == sdk_sandbox.sandbox_id
        assert resumed.read_file("/tmp/sdk-compat-pause.txt") == "before-pause"
    finally:
        resumed.close()


def test_pause_and_connect_resume_allows_commands(sdk_sandbox, sdk_e2e_config):
    sdk_sandbox.pause(timeout=sdk_e2e_config.default_timeout)
    resumed = sdk_sandbox.resume_or_connect(timeout=sdk_e2e_config.default_timeout)
    try:
        result = resumed.run_command(
            "printf resumed",
            timeout=sdk_e2e_config.command_timeout,
        )
        assert_command_ok(result)
        assert result.stdout == "resumed"
    finally:
        resumed.close()


@pytest.mark.sandbox_create_options(env_vars={"SDK_COMPAT_PAUSE_ENV": "pause-env"})
def test_pause_and_connect_resume_preserves_env_vars(sdk_sandbox, sdk_e2e_config):
    sdk_sandbox.pause(timeout=sdk_e2e_config.default_timeout)
    resumed = sdk_sandbox.resume_or_connect(timeout=sdk_e2e_config.default_timeout)
    try:
        result = resumed.run_command(
            'printf "%s" "$SDK_COMPAT_PAUSE_ENV"',
            timeout=sdk_e2e_config.command_timeout,
        )
        assert_command_ok(result)
        assert result.stdout == "pause-env"
    finally:
        resumed.close()


@pytest.mark.requires_capability(RUN_CODE)
@pytest.mark.requires_code_interpreter
def test_pause_and_connect_resume_preserves_run_code_state(sdk_sandbox, sdk_e2e_config):
    first = sdk_sandbox.run_code(
        "sdk_compat_pause_value = 84",
        timeout=sdk_e2e_config.run_code_timeout,
    )
    assert_code_ok(first)

    sdk_sandbox.pause(timeout=sdk_e2e_config.default_timeout)
    resumed = sdk_sandbox.resume_or_connect(timeout=sdk_e2e_config.default_timeout)
    try:
        second = resumed.run_code(
            "sdk_compat_pause_value + 1",
            timeout=sdk_e2e_config.run_code_timeout,
        )
        assert_code_ok(second)
        assert second.text == "85"
    finally:
        resumed.close()
