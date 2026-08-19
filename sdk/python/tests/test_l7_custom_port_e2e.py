# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Live custom-port CubeEgress dataplane tests.

The test starts a host-side HTTP echo target, creates one sandbox with an
HTTP custom-port inject rule and an HTTPS custom-port deny rule, then executes
both requests from inside the sandbox.

Required opt-in:

- ``CUBE_E2E=1`` or pytest ``--run-e2e``
- ``CUBE_L7_E2E_HTTP_TARGET_HOST``: a host address reachable from sandboxes
  that is not CubeVS's node-IP fast path (a Docker bridge gateway is suitable)
- ``CUBE_TEMPLATE_ID`` or pytest ``--cube-template-id``

Optional:

- ``CUBE_L7_E2E_HTTP_BIND_HOST`` (default ``0.0.0.0``)
- ``CUBE_L7_E2E_HTTP_PORT`` (default ``18080``)
- ``CUBE_L7_E2E_HTTPS_URL``
  (default ``https://tls-v1-2.badssl.com:1012/``)
- ``CUBE_L7_E2E_HTTPS_ALLOW_URL``
  (defaults to the same non-standard-port HTTPS URL)
"""

from __future__ import annotations

import base64
import json
import os
import threading
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse

import pytest

from cubesandbox import Action, Config, Inject, Match, Rule, Sandbox

pytestmark = pytest.mark.e2e


class _HeaderEchoHandler(BaseHTTPRequestHandler):
    def do_GET(self) -> None:  # noqa: N802
        payload = json.dumps({"path": self.path, "headers": dict(self.headers)}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, _format: str, *_args: object) -> None:
        return


def _option(pytestconfig: pytest.Config, option: str, env: str) -> str | None:
    return pytestconfig.getoption(option) or os.environ.get(env)


def _require_custom_port_e2e(pytestconfig: pytest.Config) -> tuple[str, str]:
    if not pytestconfig.getoption("--run-e2e") and os.environ.get("CUBE_E2E") != "1":
        pytest.skip("use --run-e2e or set CUBE_E2E=1")
    target_host = os.environ.get("CUBE_L7_E2E_HTTP_TARGET_HOST")
    if not target_host:
        pytest.skip("set CUBE_L7_E2E_HTTP_TARGET_HOST to a sandbox-reachable host address")
    template_id = _option(pytestconfig, "--cube-template-id", "CUBE_TEMPLATE_ID")
    if not template_id:
        pytest.skip("set CUBE_TEMPLATE_ID or --cube-template-id")
    return target_host, template_id


def _python_command(source: str) -> str:
    encoded = base64.b64encode(source.encode()).decode()
    return f"python3 -c \"import base64;exec(base64.b64decode('{encoded}'))\""


def _https_probe_script(url: str) -> str:
    return f"""
import urllib.error
import urllib.request
opener = urllib.request.build_opener(urllib.request.ProxyHandler({{}}))
try:
    with opener.open({url!r}, timeout=30) as response:
        print('STATUS=' + str(response.status))
        print('FINAL_URL=' + response.geturl())
        print('BODY_LEN=' + str(len(response.read())))
except urllib.error.HTTPError as error:
    print('STATUS=' + str(error.code))
"""


def test_l7_custom_http_inject_and_https_deny(pytestconfig: pytest.Config) -> None:
    target_host, template_id = _require_custom_port_e2e(pytestconfig)
    bind_host = os.environ.get("CUBE_L7_E2E_HTTP_BIND_HOST", "0.0.0.0")
    http_port = int(os.environ.get("CUBE_L7_E2E_HTTP_PORT", "18080"))
    https_url = os.environ.get(
        "CUBE_L7_E2E_HTTPS_URL", "https://tls-v1-2.badssl.com:1012/"
    )
    parsed_https = urlparse(https_url)
    assert parsed_https.scheme == "https" and parsed_https.hostname and parsed_https.port

    marker = f"cube-l7-{uuid.uuid4().hex}"
    server = ThreadingHTTPServer((bind_host, http_port), _HeaderEchoHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()

    config = Config(api_url=os.environ.get("CUBE_API_URL", "http://127.0.0.1:3000"))
    rules = [
        Rule(
            name="e2e-custom-http-inject",
            match=Match(host=target_host, port=http_port, scheme="http"),
            action=Action(
                allow=True,
                inject=[Inject(header="X-Cube-L7-E2E", secret=marker)],
            ),
        ),
        Rule(
            name="e2e-custom-https-deny",
            match=Match(host=parsed_https.hostname, port=parsed_https.port, scheme="https"),
            action=Action(allow=False),
        ),
    ]

    try:
        with Sandbox.create(
            template=template_id,
            timeout=120,
            allow_internet_access=False,
            network={"rules": rules},
            config=config,
        ) as sandbox:
            http_url = f"http://{target_host}:{http_port}/headers"
            http_script = f"""
import urllib.request
opener = urllib.request.build_opener(urllib.request.ProxyHandler({{}}))
with opener.open({http_url!r}, timeout=20) as response:
    print('STATUS=' + str(response.status))
    print(response.read().decode())
"""
            http_result = sandbox.commands.run(_python_command(http_script), timeout=30)
            assert http_result.exit_code == 0, http_result.stderr
            assert "STATUS=200" in http_result.stdout
            assert marker in http_result.stdout, (
                "custom HTTP request reached the target without the injected header: "
                + http_result.stdout
            )

            https_result = sandbox.commands.run(
                _python_command(_https_probe_script(https_url)), timeout=40
            )
            assert https_result.exit_code == 0, https_result.stderr
            assert "STATUS=403" in https_result.stdout, (
                "custom HTTPS deny rule was not enforced: " + https_result.stdout
            )
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)


def test_l7_custom_https_allow_reaches_real_upstream(pytestconfig: pytest.Config) -> None:
    _, template_id = _require_custom_port_e2e(pytestconfig)
    https_url = os.environ.get(
        "CUBE_L7_E2E_HTTPS_ALLOW_URL",
        os.environ.get("CUBE_L7_E2E_HTTPS_URL", "https://tls-v1-2.badssl.com:1012/"),
    )
    parsed = urlparse(https_url)
    assert parsed.scheme == "https" and parsed.hostname and parsed.port
    assert parsed.port != 443, "custom HTTPS allow E2E requires a non-standard port"

    config = Config(api_url=os.environ.get("CUBE_API_URL", "http://127.0.0.1:3000"))
    rule = Rule(
        name="e2e-custom-https-allow",
        match=Match(host=parsed.hostname, port=parsed.port, scheme="https"),
        action=Action(allow=True, audit="metadata"),
    )

    with Sandbox.create(
        template=template_id,
        timeout=120,
        allow_internet_access=False,
        network={"rules": [rule]},
        config=config,
    ) as sandbox:
        result = sandbox.commands.run(
            _python_command(_https_probe_script(https_url)), timeout=40
        )
        assert result.exit_code == 0, result.stderr
        assert "STATUS=200" in result.stdout, (
            "custom HTTPS allow did not reach the real upstream: " + result.stdout
        )
        assert "BODY_LEN=0" not in result.stdout, (
            "custom HTTPS upstream returned no response body: " + result.stdout
        )
