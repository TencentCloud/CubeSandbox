# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

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


def test_command_runs_as_root_by_default(sdk_sandbox, sdk_e2e_config):
    result = sdk_sandbox.run_command(
        "id -un", timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    assert result.stdout.strip() == "root"


def test_command_runs_as_explicit_root(sdk_sandbox, sdk_e2e_config):
    result = sdk_sandbox.run_command(
        "id -un", user="root", timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    assert result.stdout.strip() == "root"


def test_command_runs_as_nobody(sdk_sandbox, sdk_e2e_config):
    result = sdk_sandbox.run_command(
        "id -un", user="nobody", timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    assert result.stdout.strip() == "nobody"


def test_command_user_isolation_cannot_write_root_file(
    sdk_sandbox, sdk_e2e_config,
):
    root_path = "/tmp/sdk-compat-user-isolation-root.txt"
    sdk_sandbox.run_command(
        f"echo root-data > {root_path} && chmod 600 {root_path}",
        user="root", timeout=sdk_e2e_config.command_timeout,
    )

    result = sdk_sandbox.run_command(
        f"echo nobody-data > {root_path}",
        user="nobody", timeout=sdk_e2e_config.command_timeout,
    )
    combined = (result.stdout + result.stderr).lower()
    assert "denied" in combined or "permission" in combined, (
        f"Expected permission error, got stdout={result.stdout!r} stderr={result.stderr!r}"
    )


def test_command_user_isolation_reads_own_file(
    sdk_sandbox, sdk_e2e_config,
):
    nobody_path = "/tmp/sdk-compat-user-isolation-nobody.txt"
    sdk_sandbox.run_command(
        f"echo nobody-data > {nobody_path}",
        user="nobody", timeout=sdk_e2e_config.command_timeout,
    )

    result = sdk_sandbox.run_command(
        f"cat {nobody_path}",
        user="nobody", timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    assert result.stdout.strip() == "nobody-data"


def test_command_user_default_env_vars_present(
    sdk_sandbox, sdk_e2e_config,
):
    for user_name in ("root", "nobody"):
        result = sdk_sandbox.run_command(
            "echo HOME=$HOME USER=$USER",
            user=user_name, timeout=sdk_e2e_config.command_timeout,
        )
        assert_command_ok(result)
        assert user_name in result.stdout, (
            f"Expected {user_name!r} in env for {user_name}, got: {result.stdout!r}"
        )
