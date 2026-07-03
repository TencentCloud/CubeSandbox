#!/usr/bin/env python3
"""rust_web_service.py — Build and serve a Rust HTTP microservice inside CubeSandbox.

Demonstrates the "BYO Image" pattern with a Rust web service: build an axum
HTTP server from the pre-warmed ``/opt/rust-demo`` project, launch it, and
access it from outside the sandbox via CubeProxy.

Flow
----
1. Rebuild the axum demo project inside the sandbox.
2. Start the server in the background.
3. Access it via ``sandbox.get_host(8080)`` through CubeProxy.
4. Verify the JSON health-check response.

Usage
-----
    cp .env.example .env   # fill in E2B_API_URL, E2B_API_KEY, CUBE_TEMPLATE_ID
    pip install -r requirements.txt
    python rust_web_service.py
"""

from __future__ import annotations

import argparse
import os
import sys
import time
from pathlib import Path

import requests
from dotenv import load_dotenv
from e2b import Sandbox

load_dotenv(dotenv_path=Path(__file__).with_name(".env"), override=False)

# When no CA cert is configured, fall back to verify=False with a visible warning.
# Prefer setting SSL_CERT_FILE in your environment for proper validation.
_SSL_VERIFY = os.environ.get("SSL_CERT_FILE", None)
if _SSL_VERIFY is None:
    print("[ssl] SSL_CERT_FILE not set — using verify=False for sandbox endpoints", file=sys.stderr)
    print("[ssl] Set SSL_CERT_FILE to your Cube CA bundle for proper validation", file=sys.stderr)

DEMO_DIR = "/opt/rust-demo"


def wait_for_http(url: str, timeout: float = 30, interval: float = 0.5) -> bool:
    """Poll *url* until it returns HTTP 2xx, or *timeout* seconds elapse."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            resp = requests.get(url, verify=_SSL_VERIFY if _SSL_VERIFY else False, timeout=5)
            if resp.status_code < 400:
                return True
        except Exception:
            pass
        time.sleep(interval)
    return False


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Build and serve a Rust axum HTTP service in CubeSandbox."
    )
    parser.add_argument(
        "--template",
        default=None,
        help="Cube template ID (default: $CUBE_TEMPLATE_ID).",
    )
    parser.add_argument(
        "--port",
        type=int,
        default=8080,
        help="Port the HTTP server listens on (default: 8080).",
    )
    parser.add_argument(
        "--timeout",
        type=int,
        default=300,
        help="Build timeout in seconds.",
    )
    args = parser.parse_args()

    template_id = args.template or os.environ.get("CUBE_TEMPLATE_ID")
    if not template_id:
        print("Error: set CUBE_TEMPLATE_ID in .env or pass --template", file=sys.stderr)
        sys.exit(1)

    serve_port = args.port

    print(f"Template:  {template_id}")
    print(f"Port:      {serve_port}")
    print()

    with Sandbox.create(template=template_id) as sandbox:
        sid = sandbox.sandbox_id
        print(f"Sandbox:   {sid}")
        print()

        # 1. Rebuild the pre-warmed demo project
        print("[1/4] Build axum demo server")
        r = sandbox.commands.run(
            f"cd {DEMO_DIR} && cargo build --release",
            timeout=args.timeout,
        )
        if r.exit_code != 0:
            print(f"Build FAILED:\n{r.stderr[-2000:]}", file=sys.stderr)
            sys.exit(1)
        print("      Build OK")
        print()

        # 2. Start the server in the background (nohup + & sends it to the
        #    background inside the sandbox; the shell exits immediately)
        print("[2/4] Start server in background")
        sandbox.commands.run(
            f"cd {DEMO_DIR} && nohup ./target/release/rust-demo > /tmp/server.log 2>&1 &",
            timeout=5,
        )

        # 3. Wait for the server to be ready
        print("[3/4] Wait for server to accept connections...")
        proxy_url = f"https://{sandbox.get_host(serve_port)}/"
        print(f"      Proxy URL: {proxy_url}")

        if not wait_for_http(proxy_url):
            # Try to read the server log for diagnostics
            try:
                log = sandbox.files.read("/tmp/server.log")
                print(f"Server log:\n{log}", file=sys.stderr)
            except Exception:
                pass
            print("ERROR: Server did not become ready in time.", file=sys.stderr)
            sys.exit(1)
        print("      Server is ready!")
        print()

        # 4. Access the service
        print("[4/4] Call the health endpoint")
        resp = requests.get(proxy_url, verify=_SSL_VERIFY if _SSL_VERIFY else False, timeout=10)
        print(f"      HTTP {resp.status_code}")
        print()
        print("─" * 50)
        try:
            data = resp.json()
            for key, value in data.items():
                print(f"  {key}: {value}")
        except Exception:
            print(resp.text)
        print("─" * 50)

        # Read server log
        try:
            log = sandbox.files.read("/tmp/server.log")
            if log.strip():
                print(f"\n[server log]\n{log.strip()[-500:]}")
        except Exception:
            pass


if __name__ == "__main__":
    main()
