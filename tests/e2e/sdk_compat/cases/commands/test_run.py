# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import time

import pytest

from framework.assertions import assert_command_ok
from framework.capabilities import COMMANDS

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.commands,
    pytest.mark.p0,
    pytest.mark.requires_capability(COMMANDS),
]


def test_command_stdout_stderr_and_exit_code(sdk_sandbox, sdk_e2e_config):
    result = sdk_sandbox.run_command(
        "printf 'hello-out'; printf 'hello-err' >&2; exit 7",
        timeout=sdk_e2e_config.command_timeout,
    )

    assert result.exit_code == 7
    assert "hello-out" in result.stdout
    assert "hello-err" in result.stderr


def test_command_environment_is_available(sdk_sandbox, sdk_e2e_config):
    result = sdk_sandbox.run_command(
        "SDK_COMPAT_VALUE=ok; export SDK_COMPAT_VALUE; printf '%s\\n' \"$SDK_COMPAT_VALUE\"",
        timeout=sdk_e2e_config.command_timeout,
    )

    assert_command_ok(result)
    assert result.stdout.strip() == "ok"


def test_command_handles_special_characters(sdk_sandbox, sdk_e2e_config):
    text = "!@#$%^&*()_+"
    result = sdk_sandbox.run_command(
        f"printf '%s' '{text}'",
        timeout=sdk_e2e_config.command_timeout,
    )

    assert_command_ok(result)
    assert result.stdout == text


def test_command_handles_multiline_output(sdk_sandbox, sdk_e2e_config):
    result = sdk_sandbox.run_command(
        "printf 'line1\\nline2\\nline3\\n'",
        timeout=sdk_e2e_config.command_timeout,
    )

    assert_command_ok(result)
    assert result.stdout.splitlines() == ["line1", "line2", "line3"]


@pytest.mark.p1
def test_missing_command_returns_127(sdk_sandbox, sdk_e2e_config):
    result = sdk_sandbox.run_command(
        "cube_sdk_compat_missing_binary --version",
        timeout=sdk_e2e_config.command_timeout,
    )

    assert result.exit_code == 127


@pytest.mark.p1
def test_command_timeout_is_enforced(sdk_sandbox):
    started = time.monotonic()
    try:
        result = sdk_sandbox.run_command("sleep 5", timeout=1)
    except Exception as error:  # E2B aborts the client stream at the same deadline.
        result = None
        assert "timeout" in str(error).lower() or "deadline" in str(error).lower()
    elapsed = time.monotonic() - started

    if result is not None:
        assert result.exit_code != 0
    assert 0.7 <= elapsed < 4, f"command timeout took {elapsed:.2f}s"
