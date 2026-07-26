# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os
import time

import pytest
import requests

from framework.assertions import assert_command_ok
from framework.capabilities import NETWORK_ALLOW_DENY, NETWORK_PUBLIC_ACCESS

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

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.network,
    pytest.mark.p1,
    pytest.mark.requires_internet,
]


def _tcp_probe_command(
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


def _assert_tcp_reachable(result, target: str) -> None:
    assert_command_ok(result)
    assert result.stdout.strip() == "OK", (
        f"TCP target {target}:{TCP_TARGET_PORT} should be reachable; "
        f"stdout={result.stdout!r} stderr={result.stderr!r}"
    )


def _assert_tcp_blocked(result, target: str) -> None:
    assert_command_ok(result)
    output = result.stdout.strip()
    assert output.startswith("FAIL:"), (
        f"TCP target {target}:{TCP_TARGET_PORT} should be blocked; "
        f"stdout={result.stdout!r} stderr={result.stderr!r}"
    )


def _public_url(sdk_sandbox) -> str:
    host = sdk_sandbox.get_host(PUBLIC_ACCESS_PORT).rstrip("/")
    path = PUBLIC_ACCESS_PATH if PUBLIC_ACCESS_PATH.startswith("/") else f"/{PUBLIC_ACCESS_PATH}"
    # CubeSandbox returns a host, while some E2B SDK versions expose get_url()
    # semantics through the adapter and may already include a scheme.
    base_url = host if host.startswith(("http://", "https://")) else f"http://{host}"
    return f"{base_url}{path}"


def _get_public_url(url: str, *, headers: dict[str, str] | None = None) -> requests.Response:
    return requests.get(url, headers=headers, timeout=PUBLIC_HTTP_TIMEOUT)


def _public_response_matches(response: requests.Response) -> bool:
    return (
        response.status_code == PUBLIC_ACCESS_EXPECTED_STATUS
        and response.text == PUBLIC_ACCESS_EXPECTED_BODY
    )


def _wait_for_public_response(
    url: str,
    *,
    headers: dict[str, str] | None = None,
) -> requests.Response:
    deadline = time.monotonic() + PUBLIC_HTTP_READY_TIMEOUT
    interval = 1.0
    last_observation = "not requested"
    while time.monotonic() < deadline:
        try:
            response = _get_public_url(url, headers=headers)
            if _public_response_matches(response):
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


def _assert_public_response(response: requests.Response) -> None:
    assert response.status_code == PUBLIC_ACCESS_EXPECTED_STATUS, (
        f"expected HTTP {PUBLIC_ACCESS_EXPECTED_STATUS}; "
        f"status={response.status_code} body={response.text[:120]!r}"
    )
    assert response.text == PUBLIC_ACCESS_EXPECTED_BODY, (
        f"expected public response body {PUBLIC_ACCESS_EXPECTED_BODY!r}; "
        f"status={response.status_code} body={response.text[:120]!r}"
    )


def _assert_forbidden(response: requests.Response, scenario: str) -> None:
    # The restrict-public-access contract documents HTTP 403 for missing or
    # invalid traffic access tokens.
    assert response.status_code == 403, (
        f"{scenario} should be rejected with HTTP 403; "
        f"status={response.status_code} body={response.text[:120]!r}"
    )


@pytest.mark.requires_capability(NETWORK_ALLOW_DENY)
@pytest.mark.sandbox_create_options(
    network={
        "allow_out": [TCP_TARGET_IP],
        "deny_out": ["0.0.0.0/0"],
    },
)
def test_allow_out_can_punch_through_deny_all(sdk_sandbox, sdk_e2e_config):
    result = sdk_sandbox.run_command(
        _tcp_probe_command(timeout=sdk_e2e_config.network_probe_timeout),
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
        _tcp_probe_command(timeout=sdk_e2e_config.network_probe_timeout),
        timeout=sdk_e2e_config.command_timeout,
    )

    _assert_tcp_blocked(result, TCP_TARGET_IP)


@pytest.mark.requires_capability(NETWORK_PUBLIC_ACCESS)
@pytest.mark.sandbox_create_options(allow_internet_access=False)
def test_allow_internet_access_false_blocks_public_tcp(sdk_sandbox, sdk_e2e_config):
    result = sdk_sandbox.run_command(
        _tcp_probe_command(timeout=sdk_e2e_config.network_probe_timeout),
        timeout=sdk_e2e_config.command_timeout,
    )

    _assert_tcp_blocked(result, TCP_TARGET_IP)


@pytest.mark.requires_capability(NETWORK_ALLOW_DENY)
@pytest.mark.requires_capability(NETWORK_PUBLIC_ACCESS)
@pytest.mark.sandbox_create_options(
    allow_internet_access=False,
    network={"allow_out": [TCP_TARGET_IP]},
)
def test_allow_out_works_when_public_access_is_disabled(sdk_sandbox, sdk_e2e_config):
    allowed = sdk_sandbox.run_command(
        _tcp_probe_command(TCP_TARGET_IP, timeout=sdk_e2e_config.network_probe_timeout),
        timeout=sdk_e2e_config.command_timeout,
    )
    _assert_tcp_reachable(allowed, TCP_TARGET_IP)

    blocked = sdk_sandbox.run_command(
        _tcp_probe_command(
            ALTERNATE_TCP_TARGET_IP,
            timeout=sdk_e2e_config.network_probe_timeout,
        ),
        timeout=sdk_e2e_config.command_timeout,
    )
    _assert_tcp_blocked(blocked, ALTERNATE_TCP_TARGET_IP)


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
        _tcp_probe_command(TCP_TARGET_IP, timeout=sdk_e2e_config.network_probe_timeout),
        timeout=sdk_e2e_config.command_timeout,
    )
    _assert_tcp_blocked(blocked, TCP_TARGET_IP)

    allowed = sdk_sandbox.run_command(
        _tcp_probe_command(
            ALTERNATE_TCP_TARGET_IP,
            timeout=sdk_e2e_config.network_probe_timeout,
        ),
        timeout=sdk_e2e_config.command_timeout,
    )
    _assert_tcp_reachable(allowed, ALTERNATE_TCP_TARGET_IP)


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

    response = _wait_for_public_response(_public_url(sdk_sandbox))
    _assert_public_response(response)


@pytest.mark.requires_capability(NETWORK_PUBLIC_ACCESS)
@pytest.mark.sandbox_create_options(network={"allow_public_traffic": False})
def test_restricted_public_access_requires_traffic_token(
    sdk_sandbox,
):
    token = sdk_sandbox.traffic_access_token()
    assert token, "restricted public access should issue a traffic token"

    url = _public_url(sdk_sandbox)

    _assert_forbidden(_get_public_url(url), "missing traffic token")
    for header_name in TRAFFIC_ACCESS_TOKEN_HEADERS:
        _assert_forbidden(
            _get_public_url(url, headers={header_name: "invalid-token"}),
            f"invalid traffic token in {header_name}",
        )

    for header_name in TRAFFIC_ACCESS_TOKEN_HEADERS:
        response = _wait_for_public_response(
            url,
            headers={header_name: token},
        )
        _assert_public_response(response)
