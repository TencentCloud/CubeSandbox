# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""
network_l7_custom_port_echo.py — L7 rule demo across the four port quadrants.

One sandbox, five rules, six probes:

    1. default http  — rule without port (scheme="http" only): intercepts :80
    2. default https — rule without port (scheme="https" only): intercepts :443
    3. default both  — rule with NEITHER port NOR scheme: one rule intercepts
                       both :80 and :443 (the classic {80/http, 443/https} set)
    4. custom http   — rule with port=18080, scheme="http"
    5. custom https  — rule with port=1012,  scheme="https"

How it works:
    Each L7 rule opens the L3 path for the (host, port) tuples it matches and
    tells CubeEgress which traffic to intercept. The HTTP legs assert the
    injected marker header inside the echoed response body. Leg 4's target is
    a local echo server started by this script (self-contained); leg 1 uses
    httpbingo.org because binding :80 on the host is impractical. Leg 3
    (postman-echo.com) proves a bare host-only rule fans out to the whole
    default {80/http, 443/https} set — both probes go through the SAME rule.
    Rule evaluation is first-match-wins, so each leg deliberately uses its own
    host: same-host rules would shadow each other and blur attribution.

    The HTTPS legs (2, 3b, 5) need an upstream certificate the CubeEgress proxy
    can verify — it runs ``proxy_ssl_verify on`` against the system CA bundle,
    so a local self-signed echo would be rejected by design. They therefore use
    public endpoints: httpbingo.org:443 and postman-echo.com:443 (echo →
    marker asserted) plus tls-v1-2.badssl.com:1012 (non-standard port, no
    header echo → 200 + non-empty body asserted).

Prerequisites:
    - CUBE_TEMPLATE_ID, with the template built so the sandbox trusts the
      cluster interception CA (same requirement as cube-test-network.py).
    - The cluster can reach httpbingo.org / tls-v1-2.badssl.com (override via
      the env vars below if your environment needs different endpoints).

Env:
    CUBE_TEMPLATE_ID            (required) template to create the sandbox from
    EXAMPLE_L7_TARGET_HOST      (optional) bridge IP for the local echo leg
    EXAMPLE_L7_HTTP_PORT        (optional) local echo port, default 18080
    EXAMPLE_L7_DEFAULT_HOST     (optional) host for legs 1-2, default httpbingo.org
    EXAMPLE_L7_BOTH_HOST        (optional) host for leg 3, default postman-echo.com
                                (must differ from EXAMPLE_L7_DEFAULT_HOST —
                                first-match-wins would otherwise shadow rules)
    EXAMPLE_L7_CUSTOM_HTTPS_URL (optional) URL for leg 4, default
                                https://tls-v1-2.badssl.com:1012/

Constraints (SDK- and server-enforced):
    - port requires scheme; set both or neither (SDK raises ValueError).
      scheme alone stays on the classic {80, 443} set and only qualifies
      whether HTTP or HTTPS traffic matches (legs 1-2 use exactly that).
    - host must be a domain or a single IP — subnet CIDRs are rejected.
    - Scheme must be consistent per (host, port) across rules; the server
      rejects a conflicting policy outright.
    - At most 8 distinct (port, scheme) tuples per host.
    - host/scheme matching is case-insensitive.
"""

import json
import os
import subprocess
import sys
import threading
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse

from cubesandbox import Action, Inject, Match, Rule, Sandbox
from env_utils import load_local_dotenv

load_local_dotenv()

template_id = os.environ["CUBE_TEMPLATE_ID"]

HTTP_PORT = int(os.environ.get("EXAMPLE_L7_HTTP_PORT", "18080"))
DEFAULT_HOST = os.environ.get("EXAMPLE_L7_DEFAULT_HOST", "httpbingo.org")
BOTH_HOST = os.environ.get("EXAMPLE_L7_BOTH_HOST", "postman-echo.com")
assert BOTH_HOST != DEFAULT_HOST, (
    "EXAMPLE_L7_BOTH_HOST must differ from EXAMPLE_L7_DEFAULT_HOST "
    "(first-match-wins would shadow the rules)"
)
CUSTOM_HTTPS_URL = os.environ.get(
    "EXAMPLE_L7_CUSTOM_HTTPS_URL", "https://tls-v1-2.badssl.com:1012/"
)
custom_https = urlparse(CUSTOM_HTTPS_URL)
assert custom_https.scheme == "https" and custom_https.hostname and custom_https.port, (
    f"EXAMPLE_L7_CUSTOM_HTTPS_URL must be an https URL with an explicit port: {CUSTOM_HTTPS_URL!r}"
)
assert custom_https.port != 443, "custom https leg requires a non-standard port"

MARKER = f"cube-l7-echo-{uuid.uuid4().hex[:12]}"


class HeaderEchoHandler(BaseHTTPRequestHandler):
    def do_GET(self) -> None:  # noqa: N802
        payload = json.dumps({"path": self.path, "headers": dict(self.headers)}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *_args: object) -> None:
        pass


def detect_target_host() -> str:
    """Pick a host bridge IP reachable from sandboxes (never the node IP)."""
    override = os.environ.get("EXAMPLE_L7_TARGET_HOST")
    if override:
        return override
    try:
        out = subprocess.check_output(["ip", "-4", "-o", "addr", "show", "up"], text=True)
        for line in out.splitlines():
            parts = line.split()
            if len(parts) >= 4 and parts[1].startswith(("docker", "br-")):
                return parts[3].split("/")[0]
    except Exception:
        pass
    print(
        "WARNING: no UP docker*/br-* bridge found; falling back to 172.17.0.1, "
        "which is unreachable from sandboxes when docker0 is down. If the "
        "custom-http leg fails, set EXAMPLE_L7_TARGET_HOST to the bridge IP of "
        "the network your sandboxes attach to (see `ip -4 addr`).",
        file=sys.stderr,
    )
    return "172.17.0.1"  # docker0 default; override via EXAMPLE_L7_TARGET_HOST if down


target_host = detect_target_host()

server = ThreadingHTTPServer(("0.0.0.0", HTTP_PORT), HeaderEchoHandler)
thread = threading.Thread(target=server.serve_forever, daemon=True)
thread.start()
print(f"echo server on 0.0.0.0:{HTTP_PORT}; custom-http leg targets {target_host}:{HTTP_PORT}")

inject = [Inject(header="X-Cube-L7-Demo", secret=MARKER)]
rules = [
    # 1+2. Default set via scheme-only rules (no port): :80 http / :443 https.
    Rule(
        name="default-http",
        match=Match(host=DEFAULT_HOST, scheme="http"),
        action=Action(allow=True, inject=inject),
    ),
    Rule(
        name="default-https",
        match=Match(host=DEFAULT_HOST, scheme="https"),
        action=Action(allow=True, inject=inject),
    ),
    # 3. Neither port nor scheme: one rule fans out to {80/http, 443/https}.
    Rule(
        name="default-both",
        match=Match(host=BOTH_HOST),
        action=Action(allow=True, inject=inject),
    ),
    # 4. Custom HTTP port against the local echo server.
    Rule(
        name="custom-http",
        match=Match(host=target_host, port=HTTP_PORT, scheme="http"),
        action=Action(allow=True, inject=inject),
    ),
    # 5. Custom HTTPS port against a real publicly-signed upstream.
    Rule(
        name="custom-https",
        match=Match(host=custom_https.hostname, port=custom_https.port, scheme="https"),
        action=Action(allow=True, audit="metadata"),
    ),
]

results = []

try:
    with Sandbox.create(
        template=template_id,
        timeout=120,
        allow_internet_access=False,
        network={"rules": rules},
    ) as sandbox:

        def probe_marker(label: str, url: str) -> None:
            # httpbingo wraps every echoed header value in a single-element
            # JSON array; a plain substring grep still matches the marker.
            r = sandbox.commands.run(f"curl -sS --max-time 20 '{url}'", timeout=30)
            out = r.stdout.strip()
            ok = MARKER in out
            snippet = out if len(out) <= 300 else out[:300] + "..."
            print(f"{label}: {'OK' if ok else 'FAIL'} — {snippet or r.stderr.strip()}")
            results.append(ok)

        def probe_status(label: str, url: str) -> None:
            r = sandbox.commands.run(
                f"curl -sS --max-time 20 '{url}' -o /dev/null -w 'code=%{{http_code}} len=%{{size_download}}'",
                timeout=30,
            )
            out = r.stdout.strip()
            ok = "code=200" in out and "len=0" not in out
            print(f"{label}: {'OK' if ok else 'FAIL'} — {out or r.stderr.strip()}")
            results.append(ok)

        probe_marker(f"1 default http  :{80:<5}", f"http://{DEFAULT_HOST}/headers")
        probe_marker(f"2 default https :{443:<5}", f"https://{DEFAULT_HOST}/headers")
        probe_marker(f"3a default both :{80:<5}", f"http://{BOTH_HOST}/headers")
        probe_marker(f"3b default both :{443:<5}", f"https://{BOTH_HOST}/headers")
        probe_marker(f"4 custom http   :{HTTP_PORT:<5}", f"http://{target_host}:{HTTP_PORT}/headers")
        probe_status(f"5 custom https  :{custom_https.port:<5}", CUSTOM_HTTPS_URL)
finally:
    server.shutdown()
    server.server_close()
    thread.join(timeout=5)

passed = sum(results)
print(f"summary: {passed}/{len(results)} legs passed (marker={MARKER})")
