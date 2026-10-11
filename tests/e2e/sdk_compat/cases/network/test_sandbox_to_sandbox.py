# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Sandbox-to-sandbox data-plane access (CubeSandbox only).

Pytest counterparts of the quickstart demos
``examples/code-sandbox-quickstart/sandbox-curl-sandbox.py`` and
``sandbox-port-mapping.py``:

- CubeProxy Host routing: sandbox A curls an HTTP service in sandbox B
  through CubeProxy, pinning ``<port>-<sandboxID>.<domain>`` to the proxy
  node IP with ``curl --resolve`` (no wildcard DNS inside the sandbox).
- Node host-port mapping: B is created from a module-provisioned template
  exposing SERVICE_PORT, CubeVS DNATs ``nodeIP:hostPort`` into B, and A curls
  the endpoint reported by CubeMaster's info API. When A and B land on the
  same node the traffic is SNATed in ``from_cube`` and looped back into the
  primary NIC ingress (hairpin); cross-node it traverses the wire. Either
  placement satisfies the same assertion, so no scheduling constraint is
  needed.
- Egress policy is not bypassed by hairpin: a deny-all sandbox A is rejected
  when curling ``nodeIP:hostPort``.

All cases are scheduler-placement independent and need no public internet.
"""

from __future__ import annotations

import os
import re
import shlex
import time
from dataclasses import replace
from urllib.parse import urlparse

import pytest
import requests

from adapters import create_adapter_with_capacity_retry
from framework.assertions import assert_command_ok
from framework.build_throttle import template_build_slot
from framework.capabilities import (
    NETWORK_SANDBOX_TO_SANDBOX,
    capabilities_for_backend,
)
from framework.cleanup import safe_kill
from framework.config import SdkE2EConfig
from framework.parallel import scale_timeout_for_xdist

SERVICE_PORT = int(os.environ.get("SDK_E2E_S2S_SERVICE_PORT", "8080"))
SERVICE_BODY = "hello-from-sandbox-b"
DEFAULT_TEMPLATE_IMAGE = (
    "cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest"
)
# Keep envd + Jupyter exposed so the image probe and normal SDK data-plane
# paths remain usable on sandboxes created from this template.
TEMPLATE_EXPOSED_PORTS = [49999, 49983, SERVICE_PORT]
TEMPLATE_READY_TIMEOUT = 300
CURL_DEADLINE_SECONDS = 30
ENDPOINT_QUERY_TIMEOUT_SECONDS = 60
# TEST-NET-2 placeholder: forces CubeMaster's info API to answer with the
# host_port endpoint instead of the same-node tap_ip endpoint.
CALLER_HOST_IP_OVERRIDE = "198.51.100.254"

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.network,
    pytest.mark.p1,
]


@pytest.fixture(autouse=True)
def _require_sandbox_to_sandbox(sdk_backend: str) -> None:
    """Gate every test here, including ones that create adapters directly
    (the marker-based gate in the sdk_sandbox fixture does not run for them).
    """
    if NETWORK_SANDBOX_TO_SANDBOX not in capabilities_for_backend(sdk_backend):
        pytest.skip(
            f"backend {sdk_backend!r} does not support capability "
            f"{NETWORK_SANDBOX_TO_SANDBOX!r}"
        )


def _template_image() -> str:
    return (
        os.environ.get("SDK_E2E_S2S_TEMPLATE_IMAGE")
        or os.environ.get("CUBE_TEMPLATE_E2E_IMAGE")
        or DEFAULT_TEMPLATE_IMAGE
    )


def _wait_for_template_ready(template_id: str, config, timeout: int | None = None):
    from cubesandbox import Template

    # Same-image builds serialize on CubeMaster's per-artifactID lock; widen
    # the budget for parallel (xdist) runs like test_mask_request_host does.
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
def s2s_expose_template_id(pytestconfig: pytest.Config):
    """Build a template that exposes SERVICE_PORT for host-port mapping."""
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
        # template builds stay bounded regardless of the xdist worker count.
        with template_build_slot(label="sandbox_to_sandbox"):
            job = Template.build(
                image=_template_image(),
                writable_layer_size=os.environ.get(
                    "CUBE_TEMPLATE_E2E_WRITABLE_LAYER_SIZE", "1G"
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


def _create_sandbox(
    sdk_backend: str,
    sdk_e2e_config: SdkE2EConfig,
    request: pytest.FixtureRequest,
    *,
    template_id: str | None = None,
    create_options: dict | None = None,
):
    node_config = replace(
        sdk_e2e_config, cube_template_id=template_id or sdk_e2e_config.cube_template_id
    )
    if not node_config.cube_template_id:
        pytest.skip("CUBE_TEMPLATE_ID or --cube-template-id is required for SDK E2E")
    return create_adapter_with_capacity_retry(
        sdk_backend,
        node_config,
        metadata={
            "test_suite": "sdk_compat",
            "test_backend": sdk_backend,
            "test_case": request.node.name,
        },
        create_options=create_options,
    )


def _first_non_loopback_ipv4() -> str:
    # UDP "connect" picks the egress interface without sending traffic.
    # Correct only when the test runner is co-located with the CubeProxy node;
    # off-node runners must set CUBE_PROXY_NODE_IP explicitly.
    import socket

    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as s:
        s.connect(("192.0.2.1", 80))  # TEST-NET-1, never actually sent
        return s.getsockname()[0]


def _proxy_node_ip(config: SdkE2EConfig) -> str:
    """CubeProxy address as seen from INSIDE a sandbox."""
    # A loopback CUBE_PROXY_NODE_IP is meaningful only for host-side clients
    # (e.g. test_mask_request_host); inside a sandbox it points at the sandbox
    # itself, so treat it as unset and fall through to derivation.
    if config.cube_proxy_node_ip and config.cube_proxy_node_ip not in (
        "127.0.0.1",
        "localhost",
        "::1",
    ):
        return config.cube_proxy_node_ip
    host = urlparse(config.cube_api_url).hostname or ""
    if host and host not in ("127.0.0.1", "localhost", "::1"):
        return host
    # Same fallback as the quickstart scripts: a loopback API URL means the
    # runner is on the node itself, so its egress IP is the proxy address.
    try:
        return _first_non_loopback_ipv4()
    except OSError:
        pytest.skip(
            "CUBE_PROXY_NODE_IP is required: CUBE_API_URL is loopback and the "
            "node IP could not be derived (127.0.0.1 inside a sandbox points "
            "at the sandbox itself)"
        )


def _cubemaster_url(config: SdkE2EConfig) -> str:
    override = os.environ.get("CUBEMASTER_URL", "").strip()
    if override:
        return override.rstrip("/")
    host = urlparse(config.cube_api_url).hostname or "127.0.0.1"
    return f"http://{host}:8089"


def _start_marker_http_server(sandbox, config: SdkE2EConfig) -> None:
    result = sandbox.run_command(
        f"mkdir -p /tmp/s2s_www && echo '{SERVICE_BODY}' > /tmp/s2s_www/index.html",
        timeout=config.command_timeout,
    )
    assert_command_ok(result)
    # Detach stdio so the shell returns while the server keeps running until
    # the sandbox is destroyed (same pattern as test_mask_request_host).
    result = sandbox.run_command(
        f"nohup python3 -m http.server {SERVICE_PORT} --directory /tmp/s2s_www "
        ">/tmp/s2s_www.log 2>&1 </dev/null &",
        timeout=config.command_timeout,
    )
    assert_command_ok(result)


def _curl_probe(
    sandbox,
    config: SdkE2EConfig,
    url: str,
    *,
    resolve: tuple[str, int, str] | None = None,
    headers: tuple[str, ...] = (),
) -> tuple[int, str, str]:
    """Run curl inside the sandbox; returns (curl exit code, HTTP code, body).

    Never fails the command itself: the curl exit code is captured in stdout
    so callers can assert on refused/timeout outcomes. Do not pass secrets in
    ``headers`` to assertions that echo the command line.
    """
    parts = [
        "curl", "-sS", "--max-time", "10",
        "-o", "/tmp/s2s_body", "-w", "%{http_code}",
    ]
    if resolve is not None:
        host, port, ip = resolve
        parts += ["--resolve", f"{host}:{port}:{ip}"]
    for header in headers:
        parts += ["-H", header]
    parts.append(url)
    command = (
        f"out=$({' '.join(shlex.quote(p) for p in parts)} 2>/tmp/s2s_err); rc=$?; "
        "printf 'rc=%s code=%s\\n' \"$rc\" \"$out\"; cat /tmp/s2s_body; "
        "cat /tmp/s2s_err >&2"
    )
    result = sandbox.run_command(command, timeout=config.command_timeout)
    assert_command_ok(result)
    match = re.match(r"rc=(\d+) code=(\d*)\n", result.stdout)
    assert match, f"unexpected probe output: {result.stdout!r}"
    return int(match.group(1)), match.group(2), result.stdout[match.end():]


def _curl_until_ready(
    sandbox,
    config: SdkE2EConfig,
    url: str,
    *,
    expected_codes: tuple[str, ...] = ("200",),
    **kwargs,
) -> tuple[int, str, str]:
    """Retry while B's service is still coming up.

    Readiness is rc==0 AND an expected HTTP status: CubeProxy answers 502
    while B's server is still binding, so a bare rc==0 would race the
    service startup.
    """
    deadline = time.monotonic() + CURL_DEADLINE_SECONDS
    while True:
        probe = _curl_probe(sandbox, config, url, **kwargs)
        if probe[0] == 0 and probe[1] in expected_codes:
            return probe
        if time.monotonic() > deadline:
            raise AssertionError(
                f"curl to {url} did not yield {'/'.join(expected_codes)} "
                f"within {CURL_DEADLINE_SECONDS}s; "
                f"last rc={probe[0]} code={probe[1]!r} body={probe[2][:120]!r}"
            )
        time.sleep(1)


def _wait_host_port_endpoint(
    master: str, sandbox_id: str, container_port: int
) -> str:
    """Poll CubeMaster's info API for the ``hostIP:hostPort`` endpoint."""
    deadline = time.monotonic() + ENDPOINT_QUERY_TIMEOUT_SECONDS
    last = "not queried"
    while time.monotonic() < deadline:
        try:
            resp = requests.get(
                f"{master}/cube/sandbox/info",
                params={"sandbox_id": sandbox_id, "container_port": container_port},
                headers={"X-Cube-Caller-Host-IP": CALLER_HOST_IP_OVERRIDE},
                timeout=10,
            )
            payload = resp.json()
            data = payload.get("data") or []
            if data:
                item = data[0]
                endpoint = item.get("exposed_port_endpoint", "")
                mode = item.get("exposed_port_mode", "")
                if endpoint and mode == "host_port":
                    return endpoint
                last = f"mode={mode!r} endpoint={endpoint!r}"
            else:
                last = f"empty data: {payload!r}"
        except (requests.RequestException, ValueError) as exc:
            last = f"{type(exc).__name__}: {exc}"
        time.sleep(1)
    raise AssertionError(
        f"host-port endpoint for sandbox {sandbox_id} port {container_port} "
        f"was not published within {ENDPOINT_QUERY_TIMEOUT_SECONDS}s: {last}"
    )


def test_cubeproxy_host_routing_between_sandboxes(
    sdk_sandbox, sdk_backend, sdk_e2e_config, request
):
    """A reaches B's HTTP service through CubeProxy Host-header routing."""
    proxy_ip = _proxy_node_ip(sdk_e2e_config)
    sandbox_b = _create_sandbox(sdk_backend, sdk_e2e_config, request)
    try:
        _start_marker_http_server(sandbox_b, sdk_e2e_config)

        host = sandbox_b.get_host(SERVICE_PORT)
        url = f"http://{host}:{sdk_e2e_config.cube_proxy_port_http}/"
        rc, code, body = _curl_until_ready(
            sdk_sandbox,
            sdk_e2e_config,
            url,
            resolve=(host, sdk_e2e_config.cube_proxy_port_http, proxy_ip),
        )
        assert code == "200", (
            f"CubeProxy should route {host} to B's service; rc={rc} code={code!r} "
            f"body={body[:120]!r}"
        )
        assert SERVICE_BODY in body, (
            f"response should come from B's service; body={body[:120]!r}"
        )
    finally:
        safe_kill(sandbox_b, sdk_e2e_config)


def test_cubeproxy_host_routing_requires_token_when_public_access_restricted(
    sdk_sandbox, sdk_backend, sdk_e2e_config, request
):
    """B with allow_public_traffic=False: no token gets 403, B's token gets 200."""
    proxy_ip = _proxy_node_ip(sdk_e2e_config)
    sandbox_b = _create_sandbox(
        sdk_backend,
        sdk_e2e_config,
        request,
        create_options={"network": {"allow_public_traffic": False}},
    )
    try:
        token = sandbox_b.traffic_access_token()
        assert token, "restricted public access should issue a traffic token"
        _start_marker_http_server(sandbox_b, sdk_e2e_config)

        host = sandbox_b.get_host(SERVICE_PORT)
        url = f"http://{host}:{sdk_e2e_config.cube_proxy_port_http}/"
        resolve = (host, sdk_e2e_config.cube_proxy_port_http, proxy_ip)

        # Retry until the proxy's token gate answers 403 (a 502 would mean
        # B's service is still starting and says nothing about auth).
        rc, code, _ = _curl_until_ready(
            sdk_sandbox, sdk_e2e_config, url, resolve=resolve, expected_codes=("403",)
        )
        assert code == "403", (
            f"request without a traffic token should be rejected with 403; "
            f"rc={rc} code={code!r}"
        )

        rc, code, body = _curl_until_ready(
            sdk_sandbox,
            sdk_e2e_config,
            url,
            resolve=resolve,
            headers=(f"e2b-traffic-access-token: {token}",),
        )
        assert rc == 0 and code == "200" and SERVICE_BODY in body, (
            f"request with B's traffic token should succeed; "
            f"rc={rc} code={code!r} body={body[:120]!r}"
        )
    finally:
        safe_kill(sandbox_b, sdk_e2e_config)


def test_host_port_mapping_between_sandboxes(
    sdk_backend, sdk_e2e_config, s2s_expose_template_id, request
):
    """A reaches B's service via the node-level hostIP:hostPort DNAT."""
    if not s2s_expose_template_id:
        pytest.skip("host-port mapping cases require a CubeSandbox template build")
    sandbox_b = _create_sandbox(
        sdk_backend, sdk_e2e_config, request, template_id=s2s_expose_template_id
    )
    try:
        _start_marker_http_server(sandbox_b, sdk_e2e_config)
        endpoint = _wait_host_port_endpoint(
            _cubemaster_url(sdk_e2e_config), sandbox_b.sandbox_id, SERVICE_PORT
        )
        node_ip = endpoint.split(":")[0]

        # The node IP may fall inside the built-in always-denied private
        # ranges; an explicit allow_out keeps the case environment-independent.
        sandbox_a = _create_sandbox(
            sdk_backend,
            sdk_e2e_config,
            request,
            create_options={"network": {"allow_out": [node_ip]}},
        )
        try:
            rc, code, body = _curl_until_ready(
                sandbox_a, sdk_e2e_config, f"http://{endpoint}/"
            )
            assert code == "200" and SERVICE_BODY in body, (
                f"A should reach B via {endpoint}; "
                f"rc={rc} code={code!r} body={body[:120]!r}"
            )
        finally:
            safe_kill(sandbox_a, sdk_e2e_config)
    finally:
        safe_kill(sandbox_b, sdk_e2e_config)


def test_host_port_mapping_respects_egress_policy(
    sdk_backend, sdk_e2e_config, s2s_expose_template_id, request
):
    """Hairpin traffic is not exempt from egress policy: deny-all A is rejected."""
    if not s2s_expose_template_id:
        pytest.skip("host-port mapping cases require a CubeSandbox template build")
    sandbox_b = _create_sandbox(
        sdk_backend, sdk_e2e_config, request, template_id=s2s_expose_template_id
    )
    try:
        _start_marker_http_server(sandbox_b, sdk_e2e_config)
        endpoint = _wait_host_port_endpoint(
            _cubemaster_url(sdk_e2e_config), sandbox_b.sandbox_id, SERVICE_PORT
        )

        sandbox_a = _create_sandbox(
            sdk_backend,
            sdk_e2e_config,
            request,
            create_options={"network": {"deny_out": ["0.0.0.0/0"]}},
        )
        try:
            rc, code, _ = _curl_probe(
                sandbox_a, sdk_e2e_config, f"http://{endpoint}/"
            )
            assert rc != 0, (
                f"deny-all egress policy should reject the nodeIP:hostPort "
                f"(hairpin) connection; rc={rc} code={code!r}"
            )
        finally:
            safe_kill(sandbox_a, sdk_e2e_config)
    finally:
        safe_kill(sandbox_b, sdk_e2e_config)
