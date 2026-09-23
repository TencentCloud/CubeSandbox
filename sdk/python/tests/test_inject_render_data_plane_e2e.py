# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
E2E: an inject ``format`` that repeats ``${SECRET}`` must preview the same
header value the data plane actually sends.

``CubeEgress/lua/access_phase.lua`` substitutes only the *first* ``${SECRET}``
(``string.gsub(fmt, "%${SECRET}", escaped, 1)`` — note the trailing ``1``).
The Python and Go SDK preview helpers replaced *every* occurrence, so an
operator previewing ``Basic ${SECRET}:${SECRET}`` was shown a fully
substituted credential while the sandbox's upstream received a header that
still contained a literal ``${SECRET}`` — a broken credential plus a leaked
placeholder.

The request deliberately targets a host *outside* the sandbox. Egress rules
are enforced by TPROXY on the sandbox's egress path; loopback traffic inside
the guest is delivered locally and never reaches the access phase, so an
in-sandbox listener would observe no injected header at all and the test could
not distinguish "the fix works" from "nothing ran". The sibling L7 e2e makes
the same requirement.

The echo target is served in-process — the same ``_HeaderEchoHandler`` the
sibling L7 e2e uses — so this test carries no dependency on an external
service answering ``GET /`` with a JSON body, and the port it binds is the
port the rule matches.

Requirements:

- ``--run-e2e`` or ``CUBE_E2E=1``
- ``CUBE_TEMPLATE_ID`` or pytest ``--cube-template-id``
- ``CUBE_L7_E2E_HTTP_TARGET_HOST`` (environment variable): a host address
  reachable from sandboxes that is not CubeVS's node-IP fast path (a Docker
  bridge gateway is suitable).
"""

from __future__ import annotations

import json
import os
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

from cubesandbox import Action, Config, Inject, Match, Rule, Sandbox

pytestmark = pytest.mark.e2e

TARGET_HOST_ENV = "CUBE_L7_E2E_HTTP_TARGET_HOST"
BIND_HOST_ENV = "CUBE_L7_E2E_HTTP_BIND_HOST"
BIND_PORT_ENV = "CUBE_L7_E2E_HTTP_PORT"
# Deliberately not 80: binding a privileged port would need root, and the
# sibling L7 e2e already reserves 18080.
DEFAULT_BIND_PORT = 18081

# Stands in for a real credential; the assertions only care that the two
# placeholders are treated the same way the data plane treats them.
SECRET = "s3cr3t-token"

# A format that repeats the placeholder twice — the shape that exposed the
# divergence between preview and data plane.
REPEATED_FORMAT = "Basic ${SECRET}:${SECRET}"


class _HeaderEchoHandler(BaseHTTPRequestHandler):
    """Answer ``GET /`` with the request's headers as JSON."""

    def do_GET(self) -> None:  # noqa: N802
        payload = json.dumps({"path": self.path, "headers": dict(self.headers)}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, _format: str, *_args: object) -> None:
        return


def _skip_unless_e2e(pytestconfig: pytest.Config) -> tuple[str, str]:
    if not pytestconfig.getoption("--run-e2e") and os.environ.get("CUBE_E2E") != "1":
        pytest.skip("use --run-e2e or set CUBE_E2E=1")
    template_id = (
        pytestconfig.getoption("--cube-template-id")
        or os.environ.get("CUBE_TEMPLATE_ID")
    )
    target_host = os.environ.get(TARGET_HOST_ENV)
    if not template_id:
        pytest.skip("set CUBE_TEMPLATE_ID or --cube-template-id")
    if not target_host:
        pytest.skip(
            f"set {TARGET_HOST_ENV}: a host address reachable from sandboxes that "
            "is not CubeVS's node-IP fast path (a Docker bridge gateway is "
            "suitable). The echo server is started in-process."
        )
    return template_id, target_host


def test_inject_render_matches_what_the_data_plane_sends(
    pytestconfig: pytest.Config,
) -> None:
    """The SDK preview must equal the header the upstream really receives.

    Before the fix this failed loudly: the preview said
    ``Basic s3cr3t-token:s3cr3t-token`` while the upstream received
    ``Basic s3cr3t-token:${SECRET}``.
    """
    template_id, target_host = _skip_unless_e2e(pytestconfig)
    config = Config(api_url=os.environ.get("CUBE_API_URL", "http://127.0.0.1:3000"))

    bind_host = os.environ.get(BIND_HOST_ENV, "0.0.0.0")
    bind_port = int(os.environ.get(BIND_PORT_ENV, str(DEFAULT_BIND_PORT)))
    server = ThreadingHTTPServer((bind_host, bind_port), _HeaderEchoHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()

    rule = Rule(
        name="inject-repeated-placeholder",
        match=Match(host=target_host, port=bind_port, scheme="http"),
        action=Action(
            allow=True,
            inject=[
                Inject(
                    header="Authorization",
                    secret=SECRET,
                    format=REPEATED_FORMAT,
                )
            ],
        ),
    )

    # Read the header back out of the echo body rather than trusting a status
    # code: the whole point is the *value* the upstream saw. The opener bypasses
    # any proxy the guest inherits from its environment, so a proxy cannot be
    # mistaken for "nothing was injected".
    target_url = f"http://{target_host}:{bind_port}/"
    guest_script = f"""
import json
import urllib.error
import urllib.request

opener = urllib.request.build_opener(urllib.request.ProxyHandler({{}}))
try:
    with opener.open({target_url!r}, timeout=20) as response:
        body = response.read().decode("utf-8", "replace")
except urllib.error.HTTPError as error:
    body = error.read().decode("utf-8", "replace")
except OSError as error:
    print("PROBE_ERROR=" + repr(error))
    raise SystemExit(0)
try:
    headers = json.loads(body).get("headers", {{}})
except ValueError:
    print("PROBE_ERROR=non-JSON body: " + body[:200])
    raise SystemExit(0)
print(headers.get("Authorization", ""))
"""

    try:
        with Sandbox.create(
            template=template_id,
            timeout=180,
            allow_internet_access=False,
            network={"rules": [rule]},
            config=config,
        ) as sandbox:
            result = sandbox.run_code(guest_script)
            # `Execution.text` is the value of the cell's final *expression*, and
            # the script ends with `print(...)` — a statement — so there is no
            # main result and `text` is None. Read what the guest printed.
            stdout = list(getattr(result.logs, "stdout", None) or [])
            # A probe-side failure is not a fix regression: report it as itself so
            # the operator debugs the URL instead of TPROXY.
            probe_error = next(
                (line.strip() for line in stdout if line.startswith("PROBE_ERROR=")),
                "",
            )
            observed_header = next(
                (
                    line.strip()
                    for line in reversed(stdout)
                    if line.strip() and not line.startswith("PROBE_ERROR=")
                ),
                "",
            )
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=10)

    assert not probe_error, f"the guest could not read the echo target: {probe_error}"

    preview = Inject(
        header="Authorization",
        secret=SECRET,
        format=REPEATED_FORMAT,
    ).render()

    assert observed_header, (
        "the echo target did not report an Authorization header, so nothing "
        "was injected. Check that the request left the sandbox (an in-guest "
        "loopback target bypasses CubeEgress) and that the target echoes "
        f"request headers back. stdout={stdout!r}"
    )
    assert observed_header == preview, (
        "SDK preview disagrees with the data plane: the operator is shown a "
        "credential the upstream never receives"
    )
