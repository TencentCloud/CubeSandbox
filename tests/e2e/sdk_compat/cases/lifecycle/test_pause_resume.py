# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import pytest

from framework.assertions import assert_code_ok, assert_command_ok
from framework.capabilities import PAUSE_RESUME, RUN_CODE
from framework.lifecycle import (
    wait_until_data_plane_ready,
    wait_until_paused,
    wait_until_running,
)

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.lifecycle,
    pytest.mark.p1,
    pytest.mark.requires_capability(PAUSE_RESUME),
]


def _pause_and_resume(sdk_sandbox, sdk_e2e_config):
    sdk_sandbox.pause(timeout=sdk_e2e_config.default_timeout)
    wait_until_paused(sdk_sandbox, timeout=sdk_e2e_config.default_timeout)
    resumed = sdk_sandbox.resume_or_connect(timeout=sdk_e2e_config.default_timeout)
    wait_until_running(resumed, timeout=sdk_e2e_config.default_timeout)
    wait_until_data_plane_ready(
        resumed,
        timeout=sdk_e2e_config.default_timeout,
        command_timeout=sdk_e2e_config.command_timeout,
    )
    return resumed


def test_pause_sets_state_paused(sdk_sandbox, sdk_e2e_config):
    sdk_sandbox.pause(timeout=sdk_e2e_config.default_timeout)
    state = wait_until_paused(sdk_sandbox, timeout=sdk_e2e_config.default_timeout)
    assert state == "paused"


def test_pause_and_connect_resume_preserves_files(sdk_sandbox, sdk_e2e_config):
    sdk_sandbox.write_file("/tmp/sdk-compat-pause.txt", "before-pause")

    resumed = _pause_and_resume(sdk_sandbox, sdk_e2e_config)
    try:
        assert resumed.sandbox_id == sdk_sandbox.sandbox_id
        assert resumed.read_file("/tmp/sdk-compat-pause.txt") == "before-pause"
    finally:
        resumed.close()


def test_pause_and_connect_resume_allows_commands(sdk_sandbox, sdk_e2e_config):
    resumed = _pause_and_resume(sdk_sandbox, sdk_e2e_config)
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
    resumed = _pause_and_resume(sdk_sandbox, sdk_e2e_config)
    try:
        result = resumed.run_command(
            'printf "%s" "$SDK_COMPAT_PAUSE_ENV"',
            timeout=sdk_e2e_config.command_timeout,
        )
        assert_command_ok(result)
        assert result.stdout == "pause-env"
    finally:
        resumed.close()


def test_resume_accepts_never_timeout(sdk_sandbox, sdk_e2e_config):
    sdk_sandbox.pause(timeout=sdk_e2e_config.default_timeout)

    sdk_sandbox.resume(timeout=-1)
    assert wait_until_running(
        sdk_sandbox,
        timeout=sdk_e2e_config.default_timeout,
    ) == "running"

    wait_until_data_plane_ready(
        sdk_sandbox,
        timeout=sdk_e2e_config.default_timeout,
        command_timeout=sdk_e2e_config.command_timeout,
    )

    result = sdk_sandbox.run_command(
        "printf never-timeout-resume",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    assert result.stdout == "never-timeout-resume"


@pytest.mark.parametrize(
    ("timeout", "stdout"),
    [
        (0, "zero-timeout-resume"),
        (60, "positive-timeout-resume"),
    ],
)
def test_resume_accepts_zero_and_positive_timeout(
    sdk_sandbox,
    sdk_e2e_config,
    timeout,
    stdout,
):
    sdk_sandbox.pause(timeout=sdk_e2e_config.default_timeout)

    sdk_sandbox.resume(timeout=timeout)
    assert wait_until_running(
        sdk_sandbox,
        timeout=sdk_e2e_config.default_timeout,
    ) == "running"

    wait_until_data_plane_ready(
        sdk_sandbox,
        timeout=sdk_e2e_config.default_timeout,
        command_timeout=sdk_e2e_config.command_timeout,
    )

    result = sdk_sandbox.run_command(
        f"printf {stdout}",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    assert result.stdout == stdout


def test_resume_rejects_invalid_negative_timeout(sdk_sandbox, sdk_e2e_config):
    sdk_sandbox.pause(timeout=sdk_e2e_config.default_timeout)

    with pytest.raises(
        Exception,  # noqa: B017 - SDKs wrap API errors differently
    ) as exc_info:
        sdk_sandbox.resume(timeout=-2)

    _assert_invalid_timeout_error(exc_info.value)


def _assert_invalid_timeout_error(exc: Exception) -> None:
    status_code = getattr(exc, "status_code", None)
    response = getattr(exc, "response", None)
    if status_code is None and response is not None:
        status_code = getattr(response, "status_code", None)
    if status_code is None:
        # E2B's SandboxException has no status_code attribute. Instead, its
        # message starts with the HTTP status, for example "400: timeout: ...",
        # so extract that number for the assertion below.
        prefix, separator, _ = str(exc).partition(":")
        if separator and prefix.strip().isdigit():
            status_code = int(prefix)
    assert status_code == 400, (
        f"expected HTTP 400 timeout validation error, got "
        f"{type(exc).__name__}: {exc}"
    )

    message = str(exc).lower()
    assert "timeout" in message


@pytest.mark.requires_capability(RUN_CODE)
@pytest.mark.requires_code_interpreter
def test_pause_and_connect_resume_preserves_run_code_state(sdk_sandbox, sdk_e2e_config):
    first = sdk_sandbox.run_code(
        "sdk_compat_pause_value = 84",
        timeout=sdk_e2e_config.run_code_timeout,
    )
    assert_code_ok(first)

    resumed = _pause_and_resume(sdk_sandbox, sdk_e2e_config)
    try:
        second = resumed.run_code(
            "sdk_compat_pause_value + 1",
            timeout=sdk_e2e_config.run_code_timeout,
        )
        assert_code_ok(second)
        assert second.text == "85"
    finally:
        resumed.close()
