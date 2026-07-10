# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import pytest
from framework.assertions import assert_command_ok

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.p2,
    pytest.mark.requires_internet,
]


def test_dns_resolution_works(sdk_sandbox, sdk_e2e_config):
    """The sandbox can resolve public DNS names."""
    result = sdk_sandbox.run_command(
        "python3 -c \"import socket; print(socket.gethostbyname('example.com'))\"",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    # The resolved IP should be a dotted-quad IPv4 address.
    ip = result.stdout.strip()
    parts = ip.split(".")
    assert len(parts) == 4, f"expected IPv4 address, got {ip!r}"
    assert all(p.isdigit() for p in parts), f"expected numeric octets, got {ip!r}"


def test_http_egress_works(sdk_sandbox, sdk_e2e_config):
    """The sandbox can make outbound HTTP requests to the public internet."""
    result = sdk_sandbox.run_command(
        "python3 -c \""
        "import urllib.request, sys; "
        "resp = urllib.request.urlopen('http://example.com', timeout=10); "
        "print(resp.status)"
        "\"",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    assert result.stdout.strip() == "200"


def test_https_egress_works(sdk_sandbox, sdk_e2e_config):
    """The sandbox can make outbound HTTPS requests."""
    result = sdk_sandbox.run_command(
        "python3 -c \""
        "import urllib.request; "
        "resp = urllib.request.urlopen('https://example.com', timeout=10); "
        "print(resp.status)"
        "\"",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    assert result.stdout.strip() == "200"
