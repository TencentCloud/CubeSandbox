# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import time
from typing import Any

import requests

from framework.config import SdkE2EConfig


def probe_platform_lifecycle(config: SdkE2EConfig) -> tuple[bool, str, dict[str, Any]]:
    """Best-effort readiness probe for platform-managed lifecycle.

    Returns ``(ready, reason, details)``. When the proxy admin endpoint is not
    reachable from the test runner, ``ready`` stays ``True`` and the caller
    should rely on per-test skip-on-timeout behavior.
    """

    details: dict[str, Any] = {}
    if not config.platform_lifecycle_enabled:
        return False, "SDK_E2E_PLATFORM_LIFECYCLE is disabled", details

    if not config.cube_proxy_node_ip:
        return (
            True,
            "CUBE_PROXY_NODE_IP is unset; platform lifecycle readiness cannot be probed",
            details,
        )

    url = (
        f"http://{config.cube_proxy_node_ip}:{config.cube_proxy_admin_port}"
        "/admin/healthz"
    )
    details["admin_healthz_url"] = url
    try:
        response = requests.get(url, timeout=5)
        details["status_code"] = response.status_code
        response.raise_for_status()
        payload = response.json()
        details["payload"] = payload
    except Exception as exc:  # noqa: BLE001 - probe should stay best-effort
        return (
            True,
            f"platform lifecycle admin probe unreachable ({exc}); tests may skip on timeout",
            details,
        )

    heartbeat_ms = payload.get("heartbeat_last_pushed_ms")
    details["heartbeat_last_pushed_ms"] = heartbeat_ms
    if heartbeat_ms is None:
        return (
            False,
            "cube-proxy admin healthz did not report heartbeat_last_pushed_ms",
            details,
        )

    age_ms = int(time.time() * 1000) - int(heartbeat_ms)
    details["heartbeat_age_ms"] = age_ms
    if age_ms > 120_000:
        return (
            False,
            f"stale cube-proxy heartbeat ({age_ms}ms old); lifecycle manager may be offline",
            details,
        )

    return True, "platform lifecycle probe passed", details
