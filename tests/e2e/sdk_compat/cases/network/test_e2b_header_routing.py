# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""E2E coverage for CubeProxy E2B header routing (fixed-address access).

When the Host header carries no ``<port>-<sandbox-id>.<domain>`` routing
attributes, CubeProxy falls back to the official E2B routing headers so the
SDKs can target a fixed gateway address (plain IP:port, E2B_SANDBOX_URL)
without wildcard DNS. This module exercises that fallback against envd:

  - ``E2b-Sandbox-Id`` (required) + omitted ``E2b-Sandbox-Port`` routes to
    the envd port 49983 by default
  - a request with neither Host routing attributes nor E2b headers is
    rejected with 400
  - an unknown sandbox id resolves no route and is rejected with 404

Requires CUBE_PROXY_NODE_IP (the test dials CubeProxy by IP). Skips when it
is unset — deployments reached through wildcard DNS keep covering the
Host-based path in test_mask_request_host.py instead.
"""

from __future__ import annotations

import httpx
import pytest

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.network,
    pytest.mark.p1,
]


def test_e2b_header_routing_envd_via_fixed_address(
    sdk_sandbox,
    sdk_e2e_config,
):
    """Bare proxy IP + E2b-Sandbox-Id header reaches envd; without routing
    attributes the same request is rejected."""
    if not sdk_e2e_config.cube_proxy_node_ip:
        pytest.skip(
            "CUBE_PROXY_NODE_IP is required for fixed-address header routing "
            "through CubeProxy"
        )
    url = (
        f"http://{sdk_e2e_config.cube_proxy_node_ip}:"
        f"{sdk_e2e_config.cube_proxy_port_http}/health"
    )
    with httpx.Client(
        timeout=sdk_e2e_config.network_probe_timeout
    ) as client:
        # E2b-Sandbox-Port omitted: must default to the envd port 49983.
        routed = client.get(url, headers={"E2b-Sandbox-Id": sdk_sandbox.sandbox_id})
        assert routed.status_code == 204, (
            f"envd /health via E2B header routing should return 204; "
            f"got {routed.status_code}: {routed.text[:200]!r}"
        )

        rejected = client.get(url)
        assert rejected.status_code == 400, (
            f"request without Host routing attributes or E2b headers should "
            f"be rejected with 400; got {rejected.status_code}: "
            f"{rejected.text[:200]!r}"
        )

        unknown = client.get(url, headers={"E2b-Sandbox-Id": "0" * 32})
        assert unknown.status_code == 404, (
            f"unknown sandbox id should resolve no route (404); got "
            f"{unknown.status_code}: {unknown.text[:200]!r}"
        )
