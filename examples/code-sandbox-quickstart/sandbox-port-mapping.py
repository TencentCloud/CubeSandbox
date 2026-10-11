# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Host port-mapping access: create sandboxes A and B, run an HTTP service
inside B on a template-exposed port, then curl it from sandbox A using the
host port mapping allocated on B's node (nodeIP:hostPort) — no CubeProxy,
no Host-header routing; CubeVS eBPF DNATs the traffic into B's MicroVM.

Traffic path:

    client ──► nodeIP:hostPort ── CubeVS from_world (node NIC ingress) DNAT ──► B

How it works:
    The port-mapping DNAT lives in the `from_world` TC program on the
    primary NIC *ingress* (CubeNet/src/nodenic.bpf.c). When A and B are on
    the same node, A's packets to nodeIP:hostPort are SNATed in `from_cube`
    and looped back into the NIC ingress in software
    (`bpf_redirect(..., BPF_F_INGRESS)`), where from_world DNATs them into
    B's tap — no NIC/switch hairpinning required. Off-node clients reach
    the same ingress path over the wire, so the mapping works cluster-wide.

Exposed ports come from the TEMPLATE (`cubemastercli tpl create-from-image
--expose-port ...`); cubemaster `cubelet_conf.enable_exposed_port` must be
true (shipped default). The host port is discovered via CubeMaster's info API:

    GET /cube/sandbox/info?sandbox_id=<B>&container_port=<port>
    → data[0].exposed_port_endpoint = "<hostIP>:<hostPort>"  (mode host_port)

CubeMaster returns the tap_ip endpoint (sandboxIP:containerPort) when the
caller is on the sandbox's own node; this script forces host_port mode with
the X-Cube-Caller-Host-IP header because that is the endpoint under test.

Required env (see .env.example):
    CUBE_TEMPLATE_ID     template used for both sandboxes
    CUBE_API_URL         Cube API address as seen from THIS machine

Optional env:
    CUBE_SERVICE_PORT    container port the test service listens on; must be
                         one of the template's exposed ports (default: 8080)
    CUBEMASTER_URL       CubeMaster HTTP address (default: CUBE_API_URL host
                         with port 8089)
"""

from __future__ import annotations

import json
import os
import sys
import time
import urllib.request
from urllib.parse import urlparse

from e2b_code_interpreter import Sandbox
from e2b.sandbox.commands.command_handle import CommandExitException
from env_utils import ensure_dev_sidecar, load_local_dotenv

load_local_dotenv()
ensure_dev_sidecar()

TEMPLATE_ID = os.environ["CUBE_TEMPLATE_ID"]
SERVICE_PORT = int(os.environ.get("CUBE_SERVICE_PORT", "8080"))
SERVICE_BODY = "hello-from-B-portmapping"
QUERY_TIMEOUT_S = 30
CURL_TIMEOUT_S = 30

CONNREFUSED_HINT = """\
[outer] Connection refused from sandbox A.
[outer] The host-port DNAT runs in the from_world TC program on the primary
[outer] NIC ingress (CubeNet/src/nodenic.bpf.c). Check, in order:
[outer]   1. the container port is in the template's --expose-port list;
[outer]   2. cubemaster cubelet_conf.enable_exposed_port: true;
[outer]   3. sandbox A's egress policy allows the node IP.
"""


def cubemaster_url() -> str:
    override = os.environ.get("CUBEMASTER_URL", "").strip()
    if override:
        return override.rstrip("/")
    outer = os.environ.get("CUBE_API_URL") or os.environ.get("E2B_API_URL", "")
    host = urlparse(outer).hostname or "127.0.0.1"
    return f"http://{host}:8089"


def query_exposed_endpoint(master: str, sandbox_id: str, container_port: int) -> str:
    """Return '<hostIP>:<hostPort>' for the sandbox's exposed container port."""
    url = (
        f"{master}/cube/sandbox/info?sandbox_id={sandbox_id}"
        f"&container_port={container_port}"
    )
    req = urllib.request.Request(
        url,
        # Force host_port mode: without a caller host IP that differs from the
        # sandbox's node, single-node clusters answer with the tap-ip endpoint.
        headers={"X-Cube-Caller-Host-IP": "198.51.100.254"},
    )
    with urllib.request.urlopen(req, timeout=10) as resp:
        payload = json.load(resp)
    if payload.get("ret", {}).get("ret_code") != 200:
        raise RuntimeError(f"info API error: {payload.get('ret')}")
    data = payload.get("data") or []
    if not data:
        raise RuntimeError("info API returned no sandbox data")
    item = data[0]
    mode = item.get("exposed_port_mode", "")
    endpoint = item.get("exposed_port_endpoint", "")
    if not endpoint:
        raise RuntimeError(
            "no exposed_port_endpoint yet (port mapping not programmed?)"
        )
    if mode != "host_port":
        raise RuntimeError(f"unexpected exposed_port_mode {mode!r}, want host_port")
    return endpoint


def main() -> None:
    master = cubemaster_url()
    print(f"[outer] cubemaster: {master}")
    print(f"[outer] service container port: {SERVICE_PORT} "
          "(must be in the template's exposed ports)")

    sandbox_b = Sandbox.create(template=TEMPLATE_ID, timeout=-1)
    sandbox_a = Sandbox.create(template=TEMPLATE_ID, timeout=-1)
    try:
        b_id = sandbox_b.sandbox_id
        print(f"[outer] sandbox A: {sandbox_a.sandbox_id}")
        print(f"[outer] sandbox B: {b_id}")

        # Serve the marker file on the exposed port inside B. setsid + nohup
        # + full stdio detachment keeps the server alive after the envd
        # command session ends (background=True ties the process to it).
        sandbox_b.commands.run(
            f"mkdir -p /tmp/www && echo '{SERVICE_BODY}' > /tmp/www/index.html"
        )
        sandbox_b.commands.run(
            f"setsid nohup python3 -m http.server {SERVICE_PORT} "
            "--directory /tmp/www >/tmp/http.log 2>&1 </dev/null &"
        )

        # Discover the node-level host port mapping for B's service.
        endpoint = None
        deadline = time.monotonic() + QUERY_TIMEOUT_S
        while True:
            try:
                endpoint = query_exposed_endpoint(master, b_id, SERVICE_PORT)
                break
            except Exception as exc:
                if time.monotonic() > deadline:
                    sys.exit(f"[outer] exposed endpoint query failed: {exc}")
                time.sleep(1)
        print(f"[outer] B exposed endpoint (host port mapping): {endpoint}")

        # From A, curl the node IP + host port straight into B.
        curl_cmd = f"curl -sS --fail --max-time 10 http://{endpoint}/"
        print(f"[inner:A] {curl_cmd}")
        deadline = time.monotonic() + CURL_TIMEOUT_S
        while True:
            try:
                result = sandbox_a.commands.run(curl_cmd, timeout=15)
            except CommandExitException as exc:
                # curl(7) ECONNREFUSED on a resolved endpoint means the DNAT
                # path didn't trigger; print the checklist instead of retrying.
                if exc.exit_code == 7:
                    sys.stdout.write(CONNREFUSED_HINT)
                    sys.exit(2)
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
        print("[outer] OK: sandbox A reached B via the node host-port mapping")
    finally:
        sandbox_a.kill()
        sandbox_b.kill()


if __name__ == "__main__":
    main()
