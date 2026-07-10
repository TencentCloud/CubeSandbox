# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import pytest
from framework.assertions import assert_command_ok
from framework.capabilities import PAUSE_RESUME

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.p1,
    pytest.mark.requires_capability(PAUSE_RESUME),
]


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
