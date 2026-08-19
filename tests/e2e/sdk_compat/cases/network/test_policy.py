# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os

import pytest

from framework.assertions import assert_command_ok
from framework.capabilities import (
    NETWORK_ALLOW_DENY,
    NETWORK_ALWAYS_DENIED,
    NETWORK_PUBLIC_ACCESS,
)
from framework.network_probe import (
    ALTERNATE_TCP_TARGET_IP,
    TCP_TARGET_IP,
    TCP_TARGET_PORT,
    TRAFFIC_ACCESS_TOKEN_HEADERS,
    assert_forbidden,
    assert_public_response,
    assert_tcp_blocked,
    assert_tcp_reachable,
    get_public_url,
    public_url,
    tcp_probe_command,
    wait_for_public_response,
)

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.network,
    pytest.mark.p1,
    pytest.mark.requires_internet,
]


@pytest.mark.requires_capability(NETWORK_ALLOW_DENY)
@pytest.mark.sandbox_create_options(
    network={
        "allow_out": [TCP_TARGET_IP],
        "deny_out": ["0.0.0.0/0"],
    },
)
def test_allow_out_can_punch_through_deny_all(sdk_sandbox, sdk_e2e_config):
    result = sdk_sandbox.run_command(
        tcp_probe_command(timeout=sdk_e2e_config.network_probe_timeout),
        timeout=sdk_e2e_config.command_timeout,
    )

    assert_command_ok(result)
    assert result.stdout.strip() == "OK", (
        f"allow_out did not permit {TCP_TARGET_IP}:{TCP_TARGET_PORT}; "
        f"stdout={result.stdout!r} stderr={result.stderr!r}"
    )


@pytest.mark.requires_capability(NETWORK_ALLOW_DENY)
@pytest.mark.sandbox_create_options(
    network={
        "deny_out": ["0.0.0.0/0"],
    },
)
def test_deny_out_blocks_public_tcp(sdk_sandbox, sdk_e2e_config):
    result = sdk_sandbox.run_command(
        tcp_probe_command(timeout=sdk_e2e_config.network_probe_timeout),
        timeout=sdk_e2e_config.command_timeout,
    )

    assert_tcp_blocked(result, TCP_TARGET_IP)


@pytest.mark.requires_capability(NETWORK_PUBLIC_ACCESS)
@pytest.mark.sandbox_create_options(allow_internet_access=False)
def test_allow_internet_access_false_blocks_public_tcp(sdk_sandbox, sdk_e2e_config):
    result = sdk_sandbox.run_command(
        tcp_probe_command(timeout=sdk_e2e_config.network_probe_timeout),
        timeout=sdk_e2e_config.command_timeout,
    )

    assert_tcp_blocked(result, TCP_TARGET_IP)


@pytest.mark.requires_capability(NETWORK_ALLOW_DENY)
@pytest.mark.requires_capability(NETWORK_PUBLIC_ACCESS)
@pytest.mark.sandbox_create_options(
    allow_internet_access=False,
    network={"allow_out": [TCP_TARGET_IP]},
)
def test_allow_out_works_when_public_access_is_disabled(sdk_sandbox, sdk_e2e_config):
    allowed = sdk_sandbox.run_command(
        tcp_probe_command(TCP_TARGET_IP, timeout=sdk_e2e_config.network_probe_timeout),
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_tcp_reachable(allowed, TCP_TARGET_IP)

    blocked = sdk_sandbox.run_command(
        tcp_probe_command(
            ALTERNATE_TCP_TARGET_IP,
            timeout=sdk_e2e_config.network_probe_timeout,
        ),
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_tcp_blocked(blocked, ALTERNATE_TCP_TARGET_IP)


@pytest.mark.requires_capability(NETWORK_ALLOW_DENY)
@pytest.mark.requires_capability(NETWORK_PUBLIC_ACCESS)
@pytest.mark.sandbox_create_options(
    allow_internet_access=True,
    network={"deny_out": [TCP_TARGET_IP]},
)
def test_deny_out_blocks_selected_target_but_preserves_public_access(
    sdk_sandbox,
    sdk_e2e_config,
):
    blocked = sdk_sandbox.run_command(
        tcp_probe_command(TCP_TARGET_IP, timeout=sdk_e2e_config.network_probe_timeout),
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_tcp_blocked(blocked, TCP_TARGET_IP)

    allowed = sdk_sandbox.run_command(
        tcp_probe_command(
            ALTERNATE_TCP_TARGET_IP,
            timeout=sdk_e2e_config.network_probe_timeout,
        ),
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_tcp_reachable(allowed, ALTERNATE_TCP_TARGET_IP)


@pytest.mark.requires_capability(NETWORK_PUBLIC_ACCESS)
@pytest.mark.sandbox_create_options(allow_internet_access=False)
def test_no_internet_still_allows_internal_commands(sdk_sandbox, sdk_e2e_config):
    result = sdk_sandbox.run_command(
        "printf isolated-execution-ok",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    assert result.stdout == "isolated-execution-ok"


@pytest.mark.requires_capability(NETWORK_PUBLIC_ACCESS)
def test_default_public_access_allows_unauthenticated_requests(
    sdk_sandbox,
):
    token = sdk_sandbox.traffic_access_token()
    assert token is None, "default public access should not issue a traffic token"

    response = wait_for_public_response(public_url(sdk_sandbox))
    assert_public_response(response)


@pytest.mark.requires_capability(NETWORK_PUBLIC_ACCESS)
@pytest.mark.sandbox_create_options(network={"allow_public_traffic": False})
def test_restricted_public_access_requires_traffic_token(
    sdk_sandbox,
):
    token = sdk_sandbox.traffic_access_token()
    assert token, "restricted public access should issue a traffic token"

    url = public_url(sdk_sandbox)

    assert_forbidden(get_public_url(url), "missing traffic token")
    for header_name in TRAFFIC_ACCESS_TOKEN_HEADERS:
        assert_forbidden(
            get_public_url(url, headers={header_name: "invalid-token"}),
            f"invalid traffic token in {header_name}",
        )

    for header_name in TRAFFIC_ACCESS_TOKEN_HEADERS:
        response = wait_for_public_response(
            url,
            headers={header_name: token},
        )
        assert_public_response(response)


# Representative addresses from CubeVS built-in deny CIDRs when public egress
# is enabled (link-local + RFC1918; see docs/guide/network-policy.md).
ALWAYS_DENIED_TARGETS = (
    os.environ.get("SDK_E2E_ALWAYS_DENIED_LINK_LOCAL_IP", "169.254.1.1"),
    os.environ.get("SDK_E2E_ALWAYS_DENIED_PRIVATE_IP", "10.255.255.1"),
)
ALWAYS_DENIED_PORT = int(os.environ.get("SDK_E2E_ALWAYS_DENIED_PORT", "80"))


@pytest.mark.requires_capability(NETWORK_ALWAYS_DENIED)
@pytest.mark.sandbox_create_options(allow_internet_access=True)
def test_always_denied_blocks_link_local_and_private_by_default(
    sdk_sandbox,
    sdk_e2e_config,
):
    """Smoke: with public egress on, representative private/link-local targets fail.

    This is not a strong proof that CubeVS always-deny is installed: generic
    addresses like 169.254.1.1 / 10.255.255.1 are usually unreachable even
    without policy. A discriminating check needs a lab-routable private IP
    that would succeed if always-deny were absent (not wired here).
    """
    public_ok = sdk_sandbox.run_command(
        tcp_probe_command(
            TCP_TARGET_IP,
            TCP_TARGET_PORT,
            timeout=sdk_e2e_config.network_probe_timeout,
        ),
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_tcp_reachable(public_ok, TCP_TARGET_IP)

    for target in ALWAYS_DENIED_TARGETS:
        blocked = sdk_sandbox.run_command(
            tcp_probe_command(
                target,
                ALWAYS_DENIED_PORT,
                timeout=sdk_e2e_config.network_probe_timeout,
            ),
            timeout=sdk_e2e_config.command_timeout,
        )
        assert_tcp_blocked(blocked, target, ALWAYS_DENIED_PORT)
