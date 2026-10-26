#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Realistic mask_request_host demo:
#   - One shared HTTP app that routes by Host (www / blog)
#   - Two sandboxes, each masking inbound Host to a different site
#   - Same public-URL access pattern; responses differ by sandbox config

import json
import os
import time

import requests
from env_utils import load_local_dotenv

from cubesandbox import Sandbox

load_local_dotenv()

PORT = 3000
SITES = (
    {
        "name": "www",
        "mask_request_host": "www.example.com",
        "expected_site": "marketing",
    },
    {
        "name": "blog",
        "mask_request_host": "blog.example.com",
        "expected_site": "blog",
    },
)

# Same server binary in both sandboxes. It looks at the Host CubeProxy
# forwarded after mask_request_host expansion and returns a site-specific page.
SERVER = r"""
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

SITES = {
    "www.example.com": {
        "site": "marketing",
        "title": "Example Corp",
        "message": "Welcome to www.example.com",
    },
    "blog.example.com": {
        "site": "blog",
        "title": "Example Blog",
        "message": "Latest posts on blog.example.com",
    },
}

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        host = (self.headers.get("Host") or "").split(":", 1)[0].lower()
        page = SITES.get(host)
        if page is None:
            body = json.dumps({
                "error": "unknown host",
                "host": self.headers.get("Host"),
                "x_forwarded_host": self.headers.get("X-Forwarded-Host"),
            }).encode()
            self.send_response(404)
        else:
            body = json.dumps({
                **page,
                "host": self.headers.get("Host"),
                "x_forwarded_host": self.headers.get("X-Forwarded-Host"),
                "path": self.path,
            }).encode()
            self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_args):
        pass

ThreadingHTTPServer(("0.0.0.0", 3000), Handler).serve_forever()
"""


def start_server(sandbox: Sandbox) -> None:
    sandbox.files.write("/tmp/host_router.py", SERVER)
    # Cube's current Python SDK waits for commands to exit and does not yet
    # implement E2B's background=True handle. Detach all stdio so the shell
    # returns while the server keeps running until the sandbox is destroyed.
    sandbox.commands.run(
        "nohup python3 /tmp/host_router.py "
        ">/tmp/host_router.log 2>&1 </dev/null &"
    )


def wait_for_json(url: str, *, attempts: int = 20) -> dict:
    last_error: Exception | None = None
    for _ in range(attempts):
        try:
            response = requests.get(url, timeout=5)
            if response.ok:
                return response.json()
            last_error = RuntimeError(
                f"HTTP {response.status_code}: {response.text[:200]!r}"
            )
        except (requests.RequestException, ValueError) as exc:
            last_error = exc
        time.sleep(1)
    raise RuntimeError(f"service at {url} did not become ready: {last_error!r}")


template = os.environ["CUBE_TEMPLATE_ID"]
sandboxes: list[Sandbox] = []

try:
    for site in SITES:
        sandbox = Sandbox.create(
            template=template,
            network={"mask_request_host": site["mask_request_host"]},
        )
        sandboxes.append(sandbox)
        start_server(sandbox)
        print(
            f"[{site['name']}] sandbox={sandbox.sandbox_id} "
            f"mask_request_host={site['mask_request_host']!r}"
        )

    print()
    for site, sandbox in zip(SITES, sandboxes):
        public_host = sandbox.get_host(PORT)
        url = f"http://{public_host}/"
        data = wait_for_json(url)

        print(f"=== request via {public_host} ===")
        print(json.dumps(data, indent=2))
        print()

        assert data.get("site") == site["expected_site"], (
            f"expected site={site['expected_site']!r}, got {data!r}"
        )
        assert data.get("host") == site["mask_request_host"], (
            f"upstream Host should be {site['mask_request_host']!r}, got {data!r}"
        )
        assert data.get("x_forwarded_host") == public_host, (
            f"X-Forwarded-Host should preserve public host {public_host!r}, got {data!r}"
        )

    print(
        "mask_request_host is working: same server code, two sandboxes, "
        "two Hosts, two responses"
    )
finally:
    for sandbox in sandboxes:
        try:
            sandbox.kill()
        except Exception:
            pass
