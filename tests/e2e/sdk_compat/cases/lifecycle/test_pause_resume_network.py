# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Pause/resume must restore create-time network policy (egress + public access)."""

from __future__ import annotations

import pytest

from framework.capabilities import (
    NETWORK_ALLOW_DENY,
    NETWORK_PUBLIC_ACCESS,
    PAUSE_RESUME,
)
from framework.lifecycle import (
    wait_until_data_plane_ready,
    wait_until_paused,
    wait_until_running,
)
from framework.network_probe import (
    ALTERNATE_TCP_TARGET_IP,
    TCP_TARGET_IP,
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
    pytest.mark.lifecycle,
    pytest.mark.network,
    pytest.mark.p1,
    pytest.mark.requires_internet,
    pytest.mark.requires_capability(PAUSE_RESUME),
]


def _pause_and_resume(sdk_sandbox, sdk_e2e_config):
    sdk_sandbox.pause(timeout=sdk_e2e_config.default_timeout)
    wait_until_paused(sdk_sandbox, timeout=sdk_e2e_config.default_timeout)
    resumed = sdk_sandbox.resume_or_connect(timeout=sdk_e2e_config.default_timeout)
    wait_until_running(resumed, timeout=sdk_e2e_config.default_timeout)
    wait_until_data_plane_ready(
        resumed,
        timeout=sdk_e2e_config.default_timeout,
        command_timeout=sdk_e2e_config.command_timeout,
    )
    return resumed


@pytest.mark.requires_capability(NETWORK_PUBLIC_ACCESS)
@pytest.mark.sandbox_create_options(allow_internet_access=False)
def test_pause_resume_keeps_allow_internet_access_false(sdk_sandbox, sdk_e2e_config):
    before = sdk_sandbox.run_command(
        tcp_probe_command(timeout=sdk_e2e_config.network_probe_timeout),
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_tcp_blocked(before, TCP_TARGET_IP)

    resumed = _pause_and_resume(sdk_sandbox, sdk_e2e_config)
    try:
        after = resumed.run_command(
            tcp_probe_command(timeout=sdk_e2e_config.network_probe_timeout),
            timeout=sdk_e2e_config.command_timeout,
        )
        assert_tcp_blocked(after, TCP_TARGET_IP)
    finally:
        resumed.close()


@pytest.mark.requires_capability(NETWORK_ALLOW_DENY)
@pytest.mark.sandbox_create_options(
    network={
        "deny_out": ["0.0.0.0/0"],
    },
)
def test_pause_resume_keeps_deny_out_all(sdk_sandbox, sdk_e2e_config):
    before = sdk_sandbox.run_command(
        tcp_probe_command(timeout=sdk_e2e_config.network_probe_timeout),
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_tcp_blocked(before, TCP_TARGET_IP)

    resumed = _pause_and_resume(sdk_sandbox, sdk_e2e_config)
    try:
        after = resumed.run_command(
            tcp_probe_command(timeout=sdk_e2e_config.network_probe_timeout),
            timeout=sdk_e2e_config.command_timeout,
        )
        assert_tcp_blocked(after, TCP_TARGET_IP)
    finally:
        resumed.close()


@pytest.mark.requires_capability(NETWORK_ALLOW_DENY)
@pytest.mark.requires_capability(NETWORK_PUBLIC_ACCESS)
@pytest.mark.sandbox_create_options(
    allow_internet_access=False,
    network={"allow_out": [TCP_TARGET_IP]},
)
def test_pause_resume_keeps_allow_out_allowlist(sdk_sandbox, sdk_e2e_config):
    allowed = sdk_sandbox.run_command(
        tcp_probe_command(TCP_TARGET_IP, timeout=sdk_e2e_config.network_probe_timeout),
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_tcp_reachable(allowed, TCP_TARGET_IP)

    resumed = _pause_and_resume(sdk_sandbox, sdk_e2e_config)
    try:
        still_allowed = resumed.run_command(
            tcp_probe_command(
                TCP_TARGET_IP,
                timeout=sdk_e2e_config.network_probe_timeout,
            ),
            timeout=sdk_e2e_config.command_timeout,
        )
        assert_tcp_reachable(still_allowed, TCP_TARGET_IP)

        still_blocked = resumed.run_command(
            tcp_probe_command(
                ALTERNATE_TCP_TARGET_IP,
                timeout=sdk_e2e_config.network_probe_timeout,
            ),
            timeout=sdk_e2e_config.command_timeout,
        )
        assert_tcp_blocked(still_blocked, ALTERNATE_TCP_TARGET_IP)
    finally:
        resumed.close()


@pytest.mark.requires_capability(NETWORK_PUBLIC_ACCESS)
@pytest.mark.requires_cubeproxy
@pytest.mark.sandbox_create_options(network={"allow_public_traffic": False})
def test_pause_resume_keeps_restricted_public_access_token(sdk_sandbox, sdk_e2e_config):
    token = sdk_sandbox.traffic_access_token()
    assert token, "restricted public access should issue a traffic token"
    url = public_url(sdk_sandbox)

    assert_forbidden(get_public_url(url), "missing traffic token before pause")
    before = wait_for_public_response(
        url,
        headers={TRAFFIC_ACCESS_TOKEN_HEADERS[0]: token},
    )
    assert_public_response(before)

    # Restricted public access also gates envd behind the traffic token, so do
    # not use the command-based data-plane probe from `_pause_and_resume`.
    sdk_sandbox.pause(timeout=sdk_e2e_config.default_timeout)
    wait_until_paused(sdk_sandbox, timeout=sdk_e2e_config.default_timeout)
    resumed = sdk_sandbox.resume_or_connect(timeout=sdk_e2e_config.default_timeout)
    wait_until_running(resumed, timeout=sdk_e2e_config.default_timeout)
    try:
        # Host/proxy path may change after resume; rebuild the public URL from
        # the resumed adapter so CubeProxy HostIP rewrite is covered.
        resumed_url = public_url(resumed)
        resumed_token = resumed.traffic_access_token() or token

        assert_forbidden(
            get_public_url(resumed_url),
            "missing traffic token after resume",
        )
        for header_name in TRAFFIC_ACCESS_TOKEN_HEADERS:
            assert_forbidden(
                get_public_url(
                    resumed_url,
                    headers={header_name: "invalid-token"},
                ),
                f"invalid traffic token in {header_name} after resume",
            )

        for header_name in TRAFFIC_ACCESS_TOKEN_HEADERS:
            response = wait_for_public_response(
                resumed_url,
                headers={header_name: resumed_token},
            )
            assert_public_response(response)
    finally:
        resumed.close()
