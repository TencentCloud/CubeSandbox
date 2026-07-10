# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import pytest
from framework.capabilities import PROXY_URL

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.p2,
    pytest.mark.requires_capability(PROXY_URL),
    pytest.mark.requires_cubeproxy,
]


def test_sandbox_reachable_via_proxy(sdk_sandbox, sdk_e2e_config):
    """Sandbox commands and filesystem work through CubeProxy routing.

    This implicitly validates that the proxy hostname resolves and routes
    to the correct sandbox. Every adapter method exercises the proxy path,
    so a successful round-trip proves the proxy layer is healthy.
    """
    sdk_sandbox.write_file("/tmp/sdk-compat-proxy.txt", "proxy-ok")
    assert sdk_sandbox.read_file("/tmp/sdk-compat-proxy.txt") == "proxy-ok"


def test_proxy_preserves_command_isolation(sdk_sandbox, sdk_e2e_config):
    """Commands executed through the proxy see the sandbox's own filesystem,
    not a shared or stale view."""
    sdk_sandbox.run_command(
        "echo proxy-unique > /tmp/sdk-compat-proxy-cmd.txt",
        timeout=sdk_e2e_config.command_timeout,
    )
    # Filesystem API reads the same sandbox through the same proxy.
    assert sdk_sandbox.read_file("/tmp/sdk-compat-proxy-cmd.txt").strip() == "proxy-unique"


def test_run_code_works_through_proxy(sdk_sandbox, sdk_e2e_config):
    """The Jupyter /execute endpoint is reachable through CubeProxy."""
    result = sdk_sandbox.run_code("2 ** 10", timeout=sdk_e2e_config.run_code_timeout)
    assert result.error is None
    assert result.text == "1024"
