# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Sandbox-in-sandbox: create sandboxes A and B, then from inside sandbox A
use the cubesandbox SDK to connect to sandbox B and run a command there.

Data flow:

    local script (e2b SDK, same as cmd.py)
      ├── creates sandbox B  ─────────────────────────────┐
      └── creates sandbox A                               │
            └── pip install cubesandbox                   │
            └── runs /tmp/run_in_b.py                     │
                  └── Sandbox.connect(B) ──► commands.run ┘

Sandbox A reaches the control plane (CubeAPI) and data plane (CubeProxy)
using the node IP — 127.0.0.1 inside the sandbox would mean the sandbox
itself, so the API address from .env is rewritten to a reachable address.

Required env (see .env.example):
    CUBE_TEMPLATE_ID     template used for both sandboxes
    CUBE_API_URL         Cube API address as seen from THIS machine

Optional env:
    CUBE_API_KEY         forwarded into sandbox A if set
    CUBE_INNER_API_URL   Cube API address as seen from INSIDE the sandbox
                         (default: CUBE_API_URL with a loopback host replaced
                         by this node's first non-loopback IPv4 address)
    CUBE_PROXY_NODE_IP   CubeProxy node IP for the data plane
                         (default: host part of the inner API URL)
    CUBE_PIP_INDEX_URL   pip index used inside sandbox A (e.g. an internal
                         mirror when the sandbox has no public internet)

Note: pip runs INSIDE sandbox A, so the guest's DNS must be able to resolve
the pip index host. Guest DNS comes from the template's `--dns` setting or
the cubelet `default_dns_servers` fallback — if the default public resolver
is unreachable in your lab, rebuild the template with `--dns <resolver>`.
"""

from __future__ import annotations

import os
import socket
import sys
from urllib.parse import urlparse

from e2b_code_interpreter import Sandbox
from env_utils import ensure_dev_sidecar, load_local_dotenv

load_local_dotenv()
ensure_dev_sidecar()

TEMPLATE_ID = os.environ["CUBE_TEMPLATE_ID"]

PIP_INSTALL_TIMEOUT_S = 180
INNER_RUN_TIMEOUT_S = 60

# Runs INSIDE sandbox A. Connects to sandbox B and runs a command there.
INNER_SCRIPT = r"""\
import os
import sys

from cubesandbox import Sandbox

target = os.environ["TARGET_SANDBOX_ID"]
print(f"[inner] connecting to sandbox B: {target}")
sandbox_b = Sandbox.connect(target)
result = sandbox_b.commands.run("echo hello-from-B; hostname")
print(f"[inner] exit_code={result.exit_code}")
sys.stdout.write(result.stdout)
sys.stderr.write(result.stderr)
sys.exit(result.exit_code)
"""


def _first_non_loopback_ipv4() -> str:
    # UDP "connect" picks the egress interface without sending traffic.
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as s:
        s.connect(("192.0.2.1", 80))  # TEST-NET-1, never actually sent
        return s.getsockname()[0]


def inner_api_url() -> str:
    override = os.environ.get("CUBE_INNER_API_URL", "").strip()
    if override:
        return override.rstrip("/")
    outer = os.environ.get("CUBE_API_URL") or os.environ.get("E2B_API_URL")
    if not outer:
        sys.exit("CUBE_INNER_API_URL or CUBE_API_URL/E2B_API_URL is required")
    parsed = urlparse(outer)
    host = parsed.hostname or ""
    if host in ("127.0.0.1", "localhost", "::1"):
        host = _first_non_loopback_ipv4()
    netloc = f"{host}:{parsed.port}" if parsed.port else host
    return f"{parsed.scheme or 'http'}://{netloc}"


def inner_proxy_node_ip(api_url: str) -> str:
    return os.environ.get("CUBE_PROXY_NODE_IP", "").strip() or (
        urlparse(api_url).hostname or ""
    )


def pip_install_cmd() -> str:
    index = os.environ.get("CUBE_PIP_INDEX_URL", "").strip()
    extra = f" --index-url {index}" if index else ""
    return f"python3 -m pip install --quiet{extra} cubesandbox"


def main() -> None:
    api_url = inner_api_url()
    proxy_ip = inner_proxy_node_ip(api_url)
    print(f"[outer] inner CUBE_API_URL={api_url} CUBE_PROXY_NODE_IP={proxy_ip}")

    sandbox_b = Sandbox.create(template=TEMPLATE_ID, timeout=-1)
    sandbox_a = Sandbox.create(template=TEMPLATE_ID, timeout=-1)
    try:
        b_id = sandbox_b.sandbox_id
        print(f"[outer] sandbox A: {sandbox_a.sandbox_id}")
        print(f"[outer] sandbox B: {b_id}")

        print("[outer] installing cubesandbox SDK inside sandbox A ...")
        install = sandbox_a.commands.run(pip_install_cmd(), timeout=PIP_INSTALL_TIMEOUT_S)
        if install.exit_code != 0:
            sys.exit(
                "[outer] pip install failed inside sandbox A:\n"
                f"{install.stdout}{install.stderr}\n"
                "hint: set CUBE_PIP_INDEX_URL to a reachable mirror if the "
                "sandbox has no public internet access"
            )

        sandbox_a.files.write("/tmp/run_in_b.py", INNER_SCRIPT)

        print("[outer] running inner script inside sandbox A ...")
        envs = {
            "TARGET_SANDBOX_ID": b_id,
            "CUBE_API_URL": api_url,
            "CUBE_PROXY_NODE_IP": proxy_ip,
        }
        if os.environ.get("CUBE_API_KEY"):
            envs["CUBE_API_KEY"] = os.environ["CUBE_API_KEY"]

        result = sandbox_a.commands.run(
            "python3 /tmp/run_in_b.py", envs=envs, timeout=INNER_RUN_TIMEOUT_S
        )
        print(f"[outer] inner exit_code={result.exit_code}")
        sys.stdout.write(result.stdout)
        sys.stderr.write(result.stderr)
        if result.exit_code != 0:
            sys.exit(result.exit_code)
    finally:
        sandbox_a.kill()
        sandbox_b.kill()


if __name__ == "__main__":
    main()
