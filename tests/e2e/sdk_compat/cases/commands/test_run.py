# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import pytest
from framework.assertions import assert_command_ok
from framework.capabilities import COMMANDS

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
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
        "SDK_COMPAT_VALUE=ok python3 - <<'PY'\n"
        "import os\n"
        "print(os.environ['SDK_COMPAT_VALUE'])\n"
        "PY",
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
def test_command_with_cwd(sdk_sandbox, sdk_e2e_config):
    sdk_sandbox.make_dir("/tmp/sdk-compat-cwd-test")

    result = sdk_sandbox.run_command(
        "cd /tmp/sdk-compat-cwd-test && pwd",
        timeout=sdk_e2e_config.command_timeout,
    )

    assert_command_ok(result)
    assert "/tmp/sdk-compat-cwd-test" in result.stdout


@pytest.mark.p1
def test_command_large_output(sdk_sandbox, sdk_e2e_config):
    result = sdk_sandbox.run_command(
        "seq 1 1000",
        timeout=sdk_e2e_config.command_timeout,
    )

    assert_command_ok(result)
    assert len(result.stdout.strip().splitlines()) == 1000


@pytest.mark.p2
def test_command_env_from_create(sdk_sandbox, sdk_e2e_config):
    result = sdk_sandbox.run_command(
        "echo $SDK_COMPAT_TEST_ENV",
        timeout=sdk_e2e_config.command_timeout,
    )

    assert_command_ok(result)
    assert result.stdout.strip() == ""
