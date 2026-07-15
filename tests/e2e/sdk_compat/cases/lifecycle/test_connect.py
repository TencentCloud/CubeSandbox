# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import pytest

from adapters import connect_adapter
from framework.assertions import assert_command_ok
from framework.capabilities import LIFECYCLE

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.lifecycle,
    pytest.mark.p1,
    pytest.mark.requires_capability(LIFECYCLE),
]


def test_connect_existing_sandbox_preserves_id(sdk_sandbox, sdk_backend, sdk_e2e_config):
    sandbox_id = sdk_sandbox.sandbox_id
    sdk_sandbox.write_file("/tmp/sdk-compat-connect.txt", "connect-marker")

    connected = connect_adapter(sdk_backend, sandbox_id, sdk_e2e_config)
    try:
        assert connected.sandbox_id == sandbox_id
        assert connected.read_file("/tmp/sdk-compat-connect.txt") == "connect-marker"
    finally:
        connected.close()


def test_connect_existing_sandbox_allows_commands(sdk_sandbox, sdk_backend, sdk_e2e_config):
    sandbox_id = sdk_sandbox.sandbox_id

    connected = connect_adapter(sdk_backend, sandbox_id, sdk_e2e_config)
    try:
        result = connected.run_command(
            "printf connected",
            timeout=sdk_e2e_config.command_timeout,
        )
        assert_command_ok(result)
        assert result.stdout == "connected"
    finally:
        connected.close()
