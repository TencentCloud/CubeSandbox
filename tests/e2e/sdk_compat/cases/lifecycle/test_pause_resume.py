# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import pytest

from framework.assertions import assert_code_ok, assert_command_ok
from framework.capabilities import PAUSE_RESUME, RUN_CODE
from framework.lifecycle import wait_until_paused, wait_until_running

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.lifecycle,
    pytest.mark.p1,
    pytest.mark.requires_capability(PAUSE_RESUME),
]


@pytest.fixture()
def cubesandbox_resume_sandbox(sdk_backend, sdk_sandbox):
    if sdk_backend != "cubesandbox":
        pytest.skip("explicit resume timeout validation is CubeSandbox SDK specific")
    return sdk_sandbox


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


def test_resume_accepts_never_timeout(cubesandbox_resume_sandbox, sdk_e2e_config):
    cubesandbox_resume_sandbox.pause(timeout=sdk_e2e_config.default_timeout)

    cubesandbox_resume_sandbox.resume(timeout=-1)
    assert wait_until_running(
        cubesandbox_resume_sandbox,
        timeout=sdk_e2e_config.default_timeout,
    ) == "running"

    result = cubesandbox_resume_sandbox.run_command(
        "printf never-timeout-resume",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    assert result.stdout == "never-timeout-resume"


def test_resume_rejects_invalid_negative_timeout(cubesandbox_resume_sandbox, sdk_e2e_config):
    from cubesandbox import ApiError

    cubesandbox_resume_sandbox.pause(timeout=sdk_e2e_config.default_timeout)

    with pytest.raises(ApiError) as exc_info:
        cubesandbox_resume_sandbox.resume(timeout=-2)

    assert getattr(exc_info.value, "status_code", None) == 400
    assert "timeout" in str(exc_info.value).lower()


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
