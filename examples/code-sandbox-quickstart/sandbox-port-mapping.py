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
true (shipped default). The host port is discovered with the CubeSandbox SDK:

    ep = sandbox_b.get_exposed_port_endpoint(8080)
    ep.endpoint  # "<hostIP>:<hostPort>" (mode "host_port")

A caller on the sandbox's own node gets the tap_ip endpoint
(sandboxIP:containerPort) by default; this script passes
caller_host_ip="198.51.100.254" to force host_port mode because that is the
endpoint under test.

Required env (see .env.example):
    CUBE_TEMPLATE_ID     template used for both sandboxes
    CUBE_API_URL         Cube API address as seen from THIS machine

Optional env:
    CUBE_SERVICE_PORT    container port the test service listens on; must be
                         one of the template's exposed ports (default: 8080)
"""

from __future__ import annotations

import os
import sys
import time

from e2b_code_interpreter import Sandbox
from e2b.sandbox.commands.command_handle import CommandExitException
from cubesandbox import Sandbox as CubeSandbox
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


def query_exposed_endpoint(sandbox_b: CubeSandbox) -> str:
    """Return '<hostIP>:<hostPort>' for the sandbox's exposed container port."""
    ep = sandbox_b.get_exposed_port_endpoint(
        SERVICE_PORT,
        # Force host_port mode: without a caller host IP that differs from the
        # sandbox's node, single-node clusters answer with the tap-ip endpoint.
        caller_host_ip="198.51.100.254",
    )
    if ep.mode != "host_port" or not ep.endpoint:
        raise RuntimeError(
            f"unexpected exposed-port answer: mode={ep.mode!r} endpoint={ep.endpoint!r}"
        )
    return ep.endpoint


def main() -> None:
    api_url = (os.environ.get("CUBE_API_URL") or os.environ.get("E2B_API_URL", "")).rstrip("/")
    print(f"[outer] cube-api: {api_url}")
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

        # Discover the node-level host port mapping for B's service via the
        # CubeSandbox SDK (the e2b-compatible SDK has no equivalent).
        cube_b = CubeSandbox.connect(b_id)
        endpoint = None
        deadline = time.monotonic() + QUERY_TIMEOUT_S
        while True:
            try:
                endpoint = query_exposed_endpoint(cube_b)
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
                # curl(7) ECONNREFUSED is also the startup race (the service
                # in B has not bound yet), so only treat it as a datapath
                # failure once the full retry window has elapsed.
                if time.monotonic() > deadline:
                    if exc.exit_code == 7:
                        sys.stdout.write(CONNREFUSED_HINT)
                        sys.exit(2)
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
