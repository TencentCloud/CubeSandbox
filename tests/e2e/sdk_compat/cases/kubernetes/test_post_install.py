# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os
import time
import uuid
from urllib.parse import urlsplit

import pytest
import requests
from framework.assertions import assert_command_ok
from framework.capabilities import NETWORK_ALLOW_DENY, NETWORK_PUBLIC_ACCESS
from framework.network_probe import TRAFFIC_ACCESS_TOKEN_HEADERS

pytestmark = [pytest.mark.e2e, pytest.mark.sdk_compat, pytest.mark.k8s_post_install]


def test_kubernetes_preflight(k8s_environment):
    assert k8s_environment["scheduler_nodes"]


def http_probe(url: str, expected: str | None = None) -> str:
    # ProxyHandler({}) prevents inherited HTTP_PROXY from bypassing the path
    # under test. TLS certificate verification remains enabled for HTTPS.
    return (
        "python3 - <<'PY'\nimport urllib.request\n"
        "client = urllib.request.build_opener(urllib.request.ProxyHandler({}))\n"
        f"with client.open({url!r}, timeout=15) as r:\n"
        "    assert r.status == 200, r.status\n"
        "    body = r.read(1048576).decode()\n"
        + (f"    assert body == {expected!r}, body\n" if expected is not None else "")
        + "print('HTTP_OK')\nPY"
    )


@pytest.mark.k8s_service
@pytest.mark.requires_capability(NETWORK_ALLOW_DENY)
@pytest.mark.parametrize("address", ["ip", "fqdn"])
def test_kubernetes_service(sdk_sandbox, k8s_service, sdk_e2e_config, address):
    # Use the guest resolver as deployed; /etc/resolv.conf may be read-only.
    host = k8s_service[address]
    if ":" in host:
        host = f"[{host}]"
    result = sdk_sandbox.run_command(
        http_probe(f"http://{host}:{k8s_service['port']}/", k8s_service["body"]),
        timeout=max(30, sdk_e2e_config.command_timeout),
    )
    assert_command_ok(result)
    assert "HTTP_OK" in result.stdout


@pytest.mark.requires_internet
def test_public_dns_and_https(sdk_sandbox, sdk_e2e_config):
    url = os.environ.get("SDK_E2E_K8S_PUBLIC_URL", "https://example.com/")
    assert url.startswith("https://"), "SDK_E2E_K8S_PUBLIC_URL must use HTTPS"
    result = sdk_sandbox.run_command(
        http_probe(url), timeout=max(30, sdk_e2e_config.command_timeout)
    )
    assert_command_ok(result)
    assert "HTTP_OK" in result.stdout


@pytest.mark.requires_capability(NETWORK_PUBLIC_ACCESS)
def test_cubeproxy_custom_port(sdk_sandbox, sdk_e2e_config):
    # The selected template must declare this port in exposedPorts. Starting
    # our own server avoids depending on a pre-installed health endpoint.
    port = int(os.environ.get("SDK_E2E_K8S_CUSTOM_PORT", "8088"))
    assert 1 <= port <= 65535
    body = "cube-k8s-" + uuid.uuid4().hex
    sdk_sandbox.write_file(
        "/tmp/cube-k8s-http.py",
        (
            "from http.server import BaseHTTPRequestHandler, HTTPServer\n"
            "class Handler(BaseHTTPRequestHandler):\n"
            "    def do_GET(self):\n"
            f"        body = {body!r}.encode()\n"
            "        self.send_response(200)\n"
            "        self.send_header('Content-Length', str(len(body)))\n"
            "        self.end_headers()\n"
            "        self.wfile.write(body)\n"
            f"HTTPServer(('0.0.0.0', {port}), Handler).serve_forever()\n"
        ),
    )
    result = sdk_sandbox.run_command(
        "nohup python3 /tmp/cube-k8s-http.py </dev/null >/tmp/cube-k8s-http.log 2>&1 & echo started"
    )
    assert_command_ok(result)
    host = sdk_sandbox.get_host(port)
    virtual_url = host if host.startswith(("http://", "https://")) else "http://" + host
    headers = {"Host": urlsplit(virtual_url).netloc}
    token = sdk_sandbox.traffic_access_token()
    if token:
        headers.update({name: token for name in TRAFFIC_ACCESS_TOKEN_HEADERS})
    # Match the SDK's direct-proxy route when configured; retain virtual Host
    # so this still exercises CubeProxy's actual sandbox routing.
    if sdk_e2e_config.cube_proxy_node_ip:
        proxy = sdk_e2e_config.cube_proxy_node_ip
        if ":" in proxy:
            proxy = f"[{proxy}]"
        url = f"http://{proxy}:{sdk_e2e_config.cube_proxy_port_http}/"
    else:
        url = virtual_url
    deadline = time.monotonic() + 30
    last = "not requested"
    with requests.Session() as client:
        client.trust_env = False
        while time.monotonic() < deadline:
            try:
                response = client.get(
                    url, headers=headers, timeout=5, allow_redirects=False
                )
                if response.status_code == 200 and response.text == body:
                    return
                last = f"HTTP {response.status_code}: {response.text[:120]}"
            except requests.RequestException as exc:
                last = str(exc)
            time.sleep(1)
    pytest.fail(
        f"CubeProxy custom port {port} did not return this sandbox's marker: {last}"
    )
