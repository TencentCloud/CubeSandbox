# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Shared guest egress / public-access probes for network SDK E2E cases."""

from __future__ import annotations

import os
import time

import requests

from framework.assertions import assert_command_ok

TCP_TARGET_IP = os.environ.get("SDK_E2E_TCP_TARGET_IP", "8.8.8.8")
TCP_TARGET_PORT = int(os.environ.get("SDK_E2E_TCP_TARGET_PORT", "53"))
ALTERNATE_TCP_TARGET_IP = os.environ.get(
    "SDK_E2E_ALTERNATE_TCP_TARGET_IP",
    "1.1.1.1",
)
PUBLIC_ACCESS_PORT = int(os.environ.get("SDK_E2E_PUBLIC_ACCESS_PORT", "49983"))
PUBLIC_ACCESS_PATH = os.environ.get("SDK_E2E_PUBLIC_ACCESS_PATH", "/health")
PUBLIC_ACCESS_EXPECTED_STATUS = int(
    os.environ.get("SDK_E2E_PUBLIC_ACCESS_EXPECTED_STATUS", "204")
)
PUBLIC_ACCESS_EXPECTED_BODY = os.environ.get("SDK_E2E_PUBLIC_ACCESS_EXPECTED_BODY", "")
PUBLIC_HTTP_TIMEOUT = 5
PUBLIC_HTTP_READY_TIMEOUT = 30
TRAFFIC_ACCESS_TOKEN_HEADERS = (
    "e2b-traffic-access-token",
    "cube-traffic-access-token",
)


def tcp_probe_command(
    host: str = TCP_TARGET_IP,
    port: int = TCP_TARGET_PORT,
    timeout: int = 5,
) -> str:
    return (
        "python3 - <<'PY'\n"
        "import socket\n"
        "try:\n"
        "    s = socket.socket()\n"
        f"    s.settimeout({timeout!r})\n"
        f"    rc = s.connect_ex(({host!r}, {port}))\n"
        "    print('OK' if rc == 0 else f'FAIL:{rc}')\n"
        "except Exception as exc:\n"
        "    print(f'ERROR:{type(exc).__name__}:{exc}')\n"
        "finally:\n"
        "    try:\n"
        "        s.close()\n"
        "    except Exception:\n"
        "        pass\n"
        "PY"
    )


def assert_tcp_reachable(result, target: str, port: int | None = None) -> None:
    if port is None:
        port = TCP_TARGET_PORT
    assert_command_ok(result)
    assert result.stdout.strip() == "OK", (
        f"TCP target {target}:{port} should be reachable; "
        f"stdout={result.stdout!r} stderr={result.stderr!r}"
    )


def assert_tcp_blocked(result, target: str, port: int | None = None) -> None:
    """Accept REJECT (FAIL:<errno>) or DROP (timeout / TimeoutError) as blocked."""
    if port is None:
        port = TCP_TARGET_PORT
    assert_command_ok(result)
    output = result.stdout.strip()
    blocked = output.startswith("FAIL:") or _is_timeout_block(output)
    assert blocked, (
        f"TCP target {target}:{port} should be blocked; "
        f"stdout={result.stdout!r} stderr={result.stderr!r}"
    )


def _is_timeout_block(output: str) -> bool:
    # Silent drop often surfaces as socket.timeout from settimeout, not errno.
    return output.startswith("ERROR:") and (
        "Timeout" in output or "timed out" in output.lower()
    )


def public_url(sdk_sandbox) -> str:
    host = sdk_sandbox.get_host(PUBLIC_ACCESS_PORT).rstrip("/")
    path = PUBLIC_ACCESS_PATH if PUBLIC_ACCESS_PATH.startswith("/") else f"/{PUBLIC_ACCESS_PATH}"
    # CubeSandbox returns a host, while some E2B SDK versions expose get_url()
    # semantics through the adapter and may already include a scheme.
    base_url = host if host.startswith(("http://", "https://")) else f"http://{host}"
    return f"{base_url}{path}"


def get_public_url(url: str, *, headers: dict[str, str] | None = None) -> requests.Response:
    return requests.get(url, headers=headers, timeout=PUBLIC_HTTP_TIMEOUT)


def public_response_matches(response: requests.Response) -> bool:
    return (
        response.status_code == PUBLIC_ACCESS_EXPECTED_STATUS
        and response.text == PUBLIC_ACCESS_EXPECTED_BODY
    )


def wait_for_public_response(
    url: str,
    *,
    headers: dict[str, str] | None = None,
) -> requests.Response:
    deadline = time.monotonic() + PUBLIC_HTTP_READY_TIMEOUT
    interval = 1.0
    last_observation = "not requested"
    while time.monotonic() < deadline:
        try:
            response = get_public_url(url, headers=headers)
            if public_response_matches(response):
                return response
            last_observation = (
                f"status={response.status_code} body={response.text[:120]!r}"
            )
        except requests.RequestException as exc:
            last_observation = f"{type(exc).__name__}: {exc}"
        time.sleep(min(interval, max(0.0, deadline - time.monotonic())))
        interval = min(interval * 2, 8.0)
    raise AssertionError(
        "public URL did not return expected response "
        f"status={PUBLIC_ACCESS_EXPECTED_STATUS} "
        f"body={PUBLIC_ACCESS_EXPECTED_BODY!r}: {last_observation}"
    )


def assert_public_response(response: requests.Response) -> None:
    assert response.status_code == PUBLIC_ACCESS_EXPECTED_STATUS, (
        f"expected HTTP {PUBLIC_ACCESS_EXPECTED_STATUS}; "
        f"status={response.status_code} body={response.text[:120]!r}"
    )
    assert response.text == PUBLIC_ACCESS_EXPECTED_BODY, (
        f"expected public response body {PUBLIC_ACCESS_EXPECTED_BODY!r}; "
        f"status={response.status_code} body={response.text[:120]!r}"
    )


def assert_forbidden(response: requests.Response, scenario: str) -> None:
    # The restrict-public-access contract documents HTTP 403 for missing or
    # invalid traffic access tokens.
    assert response.status_code == 403, (
        f"{scenario} should be rejected with HTTP 403; "
        f"status={response.status_code} body={response.text[:120]!r}"
    )
