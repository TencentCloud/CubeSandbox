# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Sandbox-to-sandbox service access: create sandboxes A and B, start an HTTP
service inside B, then curl that service from inside sandbox A.

Traffic path (no SDK involved on the request path):

    sandbox A ── curl ──► CubeProxy (node IP :80, Host header routing)
                              └──► envd / service inside sandbox B

CubeProxy routes by the ``<port>-<sandboxID>.<domain>`` Host header, so the
curl in A only needs the proxy node IP plus a Host header — no DNS for
*.cube.app is required inside the sandbox.

Required env (see .env.example):
    CUBE_TEMPLATE_ID     template used for both sandboxes
    CUBE_API_URL         Cube API address as seen from THIS machine

Optional env:
    CUBE_INNER_API_URL   used only to derive the default proxy node IP
    CUBE_PROXY_NODE_IP   CubeProxy node IP (default: this node's first
                         non-loopback IPv4 when the API URL is loopback; that
                         derivation is only correct when this script runs on
                         the proxy node itself)
    CUBE_PROXY_PORT_HTTP CubeProxy HTTP port (default: 80)
"""

from __future__ import annotations

import os
import socket
import sys
import time
from urllib.parse import urlparse

from e2b_code_interpreter import Sandbox
from e2b.sandbox.commands.command_handle import CommandExitException
from env_utils import _traffic_access_headers, ensure_dev_sidecar, load_local_dotenv

load_local_dotenv()
ensure_dev_sidecar()

TEMPLATE_ID = os.environ["CUBE_TEMPLATE_ID"]

SERVICE_PORT = 8080
SERVICE_BODY = "hello-from-B-service"
CURL_TIMEOUT_S = 30


def _first_non_loopback_ipv4() -> str:
    # UDP "connect" picks the egress interface without sending traffic.
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as s:
        s.connect(("192.0.2.1", 80))  # TEST-NET-1, never actually sent
        return s.getsockname()[0]


def proxy_node_ip() -> str:
    override = os.environ.get("CUBE_PROXY_NODE_IP", "").strip()
    if override:
        return override
    outer = os.environ.get("CUBE_INNER_API_URL") or os.environ.get(
        "CUBE_API_URL"
    ) or os.environ.get("E2B_API_URL", "")
    host = urlparse(outer).hostname or ""
    if host in ("", "127.0.0.1", "localhost", "::1"):
        host = _first_non_loopback_ipv4()
    return host


def proxy_port() -> int:
    return int(os.environ.get("CUBE_PROXY_PORT_HTTP", "80"))


def main() -> None:
    proxy_ip = proxy_node_ip()
    port = proxy_port()
    print(f"[outer] CubeProxy endpoint from sandbox A: {proxy_ip}:{port}")

    sandbox_b = Sandbox.create(template=TEMPLATE_ID, timeout=-1)
    sandbox_a = Sandbox.create(template=TEMPLATE_ID, timeout=-1)
    try:
        b_id = sandbox_b.sandbox_id
        print(f"[outer] sandbox A: {sandbox_a.sandbox_id}")
        print(f"[outer] sandbox B: {b_id}")

        # Start an HTTP service inside B serving a marker file. setsid +
        # nohup + full stdio detachment keeps the server alive after the
        # envd command session ends (background=True ties the process to it).
        sandbox_b.commands.run(
            f"mkdir -p /tmp/www && echo '{SERVICE_BODY}' > /tmp/www/index.html"
        )
        sandbox_b.commands.run(
            f"setsid nohup python3 -m http.server {SERVICE_PORT} "
            "--directory /tmp/www >/tmp/http.log 2>&1 </dev/null &"
        )

        # Host header CubeProxy routes on: <port>-<sandboxID>.<domain>
        host = sandbox_b.get_host(SERVICE_PORT)
        print(f"[outer] service host: {host}")

        # If B was created with allow_public_traffic=False, CubeProxy requires
        # the traffic access token header.
        extra_headers = ""
        for key, value in _traffic_access_headers(sandbox_b).items():
            extra_headers += f" -H '{key}: {value}'"

        # --resolve pins the *.cube.app name to the proxy node IP, so no DNS
        # for cube.app is needed inside sandbox A.
        curl_cmd = (
            f"curl -sS --fail --max-time 10 "
            f"--resolve {host}:{port}:{proxy_ip}{extra_headers} "
            f"http://{host}:{port}/"
        )
        print(f"[inner:A] {curl_cmd}")

        # Retry while the http.server in B is still coming up. commands.run
        # raises CommandExitException on a non-zero exit, so transient curl
        # failures (connection refused, CubeProxy 502) land in the except arm.
        deadline = time.monotonic() + CURL_TIMEOUT_S
        while True:
            try:
                result = sandbox_a.commands.run(curl_cmd, timeout=15)
            except CommandExitException as exc:
                if time.monotonic() > deadline:
                    print(f"[outer] curl failed:\n{exc.stdout}{exc.stderr}")
                    sys.exit(1)
                time.sleep(1)
                continue
            if result.exit_code == 0:
                break
            if time.monotonic() > deadline:
                print(f"[outer] curl failed:\n{result.stdout}{result.stderr}")
                sys.exit(1)
            time.sleep(1)

        print(f"[outer] curl exit_code={result.exit_code}")
        sys.stdout.write(result.stdout)
        sys.stderr.write(result.stderr)
        if SERVICE_BODY not in result.stdout:
            sys.exit(f"[outer] unexpected response body: {result.stdout!r}")
        print("[outer] OK: sandbox A reached the service in sandbox B")
    finally:
        sandbox_a.kill()
        sandbox_b.kill()


if __name__ == "__main__":
    main()
