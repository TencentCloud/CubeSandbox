# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""E2E coverage for network.maskRequestHost.

Covers two rewrite shapes used by the quickstart example:
  - with ${PORT}: ``localhost:${PORT}`` → ``localhost:<container_port>``
  - without ${PORT}: fixed hostname such as ``www.example.com``

Also checks the no-mask baseline and create-time rejection of invalid values.

These cases need a container port that is neither envd (49983, mask-exempt)
nor Jupyter (49999, already occupied). The shared CUBE_TEMPLATE_ID often only
exposes 49983/49999, so this module provisions a dedicated template with
SERVICE_PORT exposed, then runs sandbox traffic through CubeProxy.
"""

from __future__ import annotations

import json
import os
import time

import httpx
import pytest

from adapters import create_adapter
from framework.assertions import assert_command_ok
from framework.capabilities import NETWORK_MASK_REQUEST_HOST
from framework.cleanup import safe_kill
from framework.build_throttle import template_build_slot
from framework.config import SdkE2EConfig
from framework.parallel import scale_timeout_for_xdist

SERVICE_PORT = 8765
# With ${PORT}: expands to the requested sandbox container port.
MASK_REQUEST_HOST_WITH_PORT = "localhost:${PORT}"
EXPECTED_UPSTREAM_HOST_WITH_PORT = f"localhost:{SERVICE_PORT}"
# Without ${PORT}: fixed authority, matching Host-based virtual hosting
# (same shape as examples/code-sandbox-quickstart/mask-request-host.py).
MASK_REQUEST_HOST_HOSTNAME = "www.example.com"

# Keep envd + Jupyter exposed so the image probe and normal SDK data-plane
# paths remain usable on sandboxes created from this template.
TEMPLATE_EXPOSED_PORTS = [49999, 49983, SERVICE_PORT]
DEFAULT_TEMPLATE_IMAGE = (
    "cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest"
)
DEFAULT_WRITABLE_LAYER_SIZE = "1G"
TEMPLATE_READY_TIMEOUT = 300

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


def _template_image() -> str:
    return (
        os.environ.get("SDK_E2E_MASK_HOST_TEMPLATE_IMAGE")
        or os.environ.get("CUBE_TEMPLATE_E2E_IMAGE")
        or DEFAULT_TEMPLATE_IMAGE
    )


def _wait_for_template_ready(template_id: str, config, timeout: int | None = None):
    from cubesandbox import Template

    # Widen the serial-run budget for parallel (xdist) runs: same-image builds
    # serialize on CubeMaster's per-artifactID lock, so the last worker's build
    # can queue well past TEMPLATE_READY_TIMEOUT on a loaded-but-healthy node.
    if timeout is None:
        timeout = scale_timeout_for_xdist(TEMPLATE_READY_TIMEOUT)
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            info = Template.get(template_id, config=config)
            if info.status in ("READY", "FAILED"):
                return info
        except Exception:
            pass
        time.sleep(2)
    pytest.fail(f"template {template_id} did not reach READY within {timeout}s")


def _delete_template(template_id: str, config) -> None:
    from cubesandbox import Template
    from cubesandbox._exceptions import ApiError, TemplateNotFoundError

    deadline = time.time() + 180
    while time.time() < deadline:
        try:
            Template.delete(template_id, config=config)
            return
        except TemplateNotFoundError:
            return
        except ApiError as exc:
            if "attempt is already in progress" in str(exc):
                time.sleep(5)
                continue
            raise


@pytest.fixture(scope="module")
def mask_request_host_template_id(pytestconfig: pytest.Config):
    """Build a template that exposes SERVICE_PORT for CubeProxy port mapping."""
    from cubesandbox import Config, Template

    base = SdkE2EConfig.from_env(
        backends=pytestconfig.getoption("--sdk-e2e-backends"),
        cube_api_url=pytestconfig.getoption("--cube-api-url"),
        cube_template_id=pytestconfig.getoption("--cube-template-id"),
    )
    if "cubesandbox" not in base.backends:
        yield None
        return

    sdk_config = Config(api_url=base.cube_api_url)
    created_id: str | None = None
    try:
        # Hold a build slot across build + READY wait so concurrent in-flight
        # template builds stay bounded regardless of the xdist worker count;
        # this keeps the outcome identical to a serial ``-n1`` run.
        with template_build_slot(label="mask_request_host"):
            job = Template.build(
                image=_template_image(),
                writable_layer_size=os.environ.get(
                    "CUBE_TEMPLATE_E2E_WRITABLE_LAYER_SIZE",
                    DEFAULT_WRITABLE_LAYER_SIZE,
                ),
                exposed_ports=TEMPLATE_EXPOSED_PORTS,
                probe_port=49999,
                config=sdk_config,
            )
            assert job.template_id.startswith("tpl-"), job.template_id
            created_id = job.template_id
            info = _wait_for_template_ready(created_id, sdk_config)
            assert info.status == "READY", (
                f"template {created_id} finished with status={info.status!r}"
            )
        yield created_id
    finally:
        if created_id is not None:
            try:
                _delete_template(created_id, sdk_config)
            except Exception:
                pass


@pytest.fixture(autouse=True)
def _apply_mask_request_host_template(
    request: pytest.FixtureRequest,
    sdk_backend: str,
    mask_request_host_template_id: str | None,
):
    """Route sdk_sandbox creates onto the module template via marker.

    Do not override session-scoped ``sdk_e2e_config`` (preflight depends on it).
    ``sandbox_template_id`` is the framework's supported per-test template override.
    """
    if sdk_backend != "cubesandbox":
        return
    if not mask_request_host_template_id:
        pytest.skip(
            "maskRequestHost proxy cases require provisioning a CubeSandbox "
            "template that exposes SERVICE_PORT"
        )
    request.node.add_marker(
        pytest.mark.sandbox_template_id(mask_request_host_template_id)
    )


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
    network={"mask_request_host": MASK_REQUEST_HOST_WITH_PORT},
)
def test_mask_request_host_with_port_placeholder(sdk_sandbox, sdk_e2e_config):
    """maskRequestHost with ${PORT} expands to localhost:<container_port>."""
    _start_host_echo_server(sdk_sandbox, sdk_e2e_config)

    public_host = sdk_sandbox.get_host(SERVICE_PORT)
    data = _public_get_json(public_host, sdk_e2e_config)

    assert data.get("host") == EXPECTED_UPSTREAM_HOST_WITH_PORT, (
        f"upstream Host should be rewritten to {EXPECTED_UPSTREAM_HOST_WITH_PORT!r}; "
        f"got={data!r} public_host={public_host!r}"
    )
    assert data.get("x_forwarded_host") == public_host, (
        f"X-Forwarded-Host should preserve the public Host {public_host!r}; "
        f"got={data!r}"
    )


@pytest.mark.sandbox_create_options(
    network={"mask_request_host": MASK_REQUEST_HOST_HOSTNAME},
)
def test_mask_request_host_without_port_uses_fixed_hostname(
    sdk_sandbox, sdk_e2e_config
):
    """maskRequestHost without ${PORT} forwards a fixed hostname (vhost-style)."""
    _start_host_echo_server(sdk_sandbox, sdk_e2e_config)

    public_host = sdk_sandbox.get_host(SERVICE_PORT)
    data = _public_get_json(public_host, sdk_e2e_config)

    assert data.get("host") == MASK_REQUEST_HOST_HOSTNAME, (
        f"upstream Host should be rewritten to {MASK_REQUEST_HOST_HOSTNAME!r}; "
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
        pytest.skip("a CubeSandbox template is required for SDK E2E create")

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
