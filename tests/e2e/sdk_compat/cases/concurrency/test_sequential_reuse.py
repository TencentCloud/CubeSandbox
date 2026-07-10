# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import pytest
from framework.assertions import assert_command_ok
from framework.capabilities import COMMANDS

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.p2,
    pytest.mark.requires_capability(COMMANDS),
]


def test_rapid_sequential_commands(sdk_sandbox, sdk_e2e_config):
    for i in range(5):
        result = sdk_sandbox.run_command(
            f"echo iter_{i}",
            timeout=sdk_e2e_config.command_timeout,
        )
        assert_command_ok(result)
        assert "iter_" in result.stdout
