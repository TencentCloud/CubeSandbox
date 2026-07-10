# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import pytest
from framework.assertions import assert_command_ok
from framework.capabilities import METADATA

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.p1,
    pytest.mark.requires_capability(METADATA),
]


def test_env_vars_visible_in_commands(sdk_sandbox, sdk_e2e_config):
    result = sdk_sandbox.run_command(
        'SDK_META=meta_value python3 -c "import os; print(os.environ.get(\'SDK_META\', \'\'))"',
        timeout=sdk_e2e_config.command_timeout,
    )

    assert_command_ok(result)
    assert "meta_value" in result.stdout


def test_metadata_propagation_via_command(sdk_sandbox, sdk_e2e_config):
    path = "/tmp/sdk-meta-test.txt"

    sdk_sandbox.write_file(path, "metadata-value")
    result = sdk_sandbox.run_command(
        f"cat {path}",
        timeout=sdk_e2e_config.command_timeout,
    )

    assert_command_ok(result)
    assert result.stdout == "metadata-value"
