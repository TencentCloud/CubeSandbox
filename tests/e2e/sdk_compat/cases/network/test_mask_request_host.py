# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import json
import time

import httpx
import pytest

from adapters import create_adapter
from framework.assertions import assert_command_ok
from framework.capabilities import NETWORK_MASK_REQUEST_HOST
from framework.cleanup import safe_kill

SERVICE_PORT = 8765
MASK_REQUEST_HOST = "localhost:${PORT}"
EXPECTED_UPSTREAM_HOST = f"localhost:{SERVICE_PORT}"

HOST_ECHO_SERVER = f"""\
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        body = json.dumps({{
            "host": self.headers.get("Host"),
            "x_forwarded_host": self.headers.get("X-Forwarded-Host"),
        }}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_args):
        pass

ThreadingHTTPServer(("0.0.0.0", {SERVICE_PORT}), Handler).serve_forever()
"""

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.network,
    pytest.mark.p1,
    pytest.mark.requires_capability(NETWORK_MASK_REQUEST_HOST),
]


def _start_host_echo_server(sdk_sandbox, sdk_e2e_config) -> None:
    sdk_sandbox.write_file("/tmp/host_echo.py", HOST_ECHO_SERVER)
    # Cube's current Python SDK waits for commands to exit and does not yet
    # implement E2B's background=True handle. Detach stdio so the shell returns
    # while the server keeps running until the sandbox is destroyed.
    result = sdk_sandbox.run_command(
        "nohup python3 /tmp/host_echo.py >/tmp/host_echo.log 2>&1 </dev/null &",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)


def _public_get_json(
    public_host: str,
    sdk_e2e_config,
    *,
    attempts: int = 20,
    sleep_seconds: float = 1.0,
) -> dict:
    """GET the host-echo service through CubeProxy / public sandbox URL.

    When ``CUBE_PROXY_NODE_IP`` is set, TCP connects to that IP:port while the
    Host header keeps the virtual sandbox hostname (same as SDK transport).
    """
    timeout = sdk_e2e_config.network_probe_timeout
    last_error: Exception | None = None

    for _ in range(attempts):
        try:
            if sdk_e2e_config.cube_proxy_node_ip:
                url = (
                    f"http://{sdk_e2e_config.cube_proxy_node_ip}:"
                    f"{sdk_e2e_config.cube_proxy_port_http}/"
                )
                headers = {"Host": public_host}
            else:
                url = f"http://{public_host}/"
                headers = {}

            with httpx.Client(timeout=timeout, follow_redirects=True) as client:
                response = client.get(url, headers=headers)
            if response.is_success:
                return response.json()
            last_error = RuntimeError(
                f"HTTP {response.status_code}: {response.text[:200]!r}"
            )
        except (httpx.HTTPError, json.JSONDecodeError, ValueError) as exc:
            last_error = exc
        time.sleep(sleep_seconds)

    raise AssertionError(
        f"sandbox HTTP service on {public_host!r} did not become ready; "
        f"last_error={last_error!r}"
    )


@pytest.mark.sandbox_create_options(
    network={"mask_request_host": MASK_REQUEST_HOST},
)
def test_mask_request_host_rewrites_upstream_host(sdk_sandbox, sdk_e2e_config):
    _start_host_echo_server(sdk_sandbox, sdk_e2e_config)

    public_host = sdk_sandbox.get_host(SERVICE_PORT)
    data = _public_get_json(public_host, sdk_e2e_config)

    assert data.get("host") == EXPECTED_UPSTREAM_HOST, (
        f"upstream Host should be rewritten to {EXPECTED_UPSTREAM_HOST!r}; "
        f"got={data!r} public_host={public_host!r}"
    )
    assert data.get("x_forwarded_host") == public_host, (
        f"X-Forwarded-Host should preserve the public Host {public_host!r}; "
        f"got={data!r}"
    )


def test_without_mask_request_host_keeps_public_host(sdk_sandbox, sdk_e2e_config):
    _start_host_echo_server(sdk_sandbox, sdk_e2e_config)

    public_host = sdk_sandbox.get_host(SERVICE_PORT)
    data = _public_get_json(public_host, sdk_e2e_config)

    assert data.get("host") == public_host, (
        f"without maskRequestHost, upstream Host should stay {public_host!r}; "
        f"got={data!r}"
    )
    assert data.get("x_forwarded_host") in (None, ""), (
        f"without maskRequestHost, X-Forwarded-Host should be unset; got={data!r}"
    )


def test_invalid_mask_request_host_is_rejected_at_create(
    sdk_backend,
    sdk_e2e_config,
):
    if not sdk_e2e_config.cube_template_id:
        pytest.skip("CUBE_TEMPLATE_ID or --cube-template-id is required for SDK E2E")

    adapter = None
    try:
        with pytest.raises(Exception) as exc_info:
            adapter = create_adapter(
                sdk_backend,
                sdk_e2e_config,
                metadata={
                    "test_suite": "sdk_compat",
                    "test_backend": sdk_backend,
                    "test_case": "invalid_mask_request_host",
                },
                create_options={
                    "network": {"mask_request_host": "https://evil.example"},
                },
            )

        message = str(exc_info.value)
        assert (
            "maskRequestHost" in message
            or "mask_request_host" in message
            or "invalid" in message.lower()
        ), (
            f"create failure should mention maskRequestHost validation; got={message!r}"
        )
    finally:
        if adapter is not None:
            safe_kill(adapter, sdk_e2e_config)
