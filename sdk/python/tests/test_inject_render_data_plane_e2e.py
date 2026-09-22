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

Requirements:

- ``CUBE_TEMPLATE_ID`` or pytest ``--cube-template-id``
- ``--run-e2e`` or ``CUBE_E2E=1``
"""

from __future__ import annotations

import os

import pytest

from cubesandbox import Action, Config, Inject, Match, Rule, Sandbox

pytestmark = pytest.mark.e2e

# The listener runs inside the sandbox, so the egress rule matches on a host
# the sandbox can reach itself.
ECHO_HOST = "127.0.0.1"
ECHO_PORT = 18099
ECHO_PATH = "/echo"

# Stands in for a real credential; the assertions only care that the two
# placeholders are treated the same way the data plane treats them.
SECRET = "s3cr3t-token"

# A format that repeats the placeholder twice — the shape that exposed the
# divergence between preview and data plane.
REPEATED_FORMAT = "Basic ${SECRET}:${SECRET}"

# Listener + requester run as one script so there is no second sandbox hop.
# It prints the Authorization header the listener actually observed.
GUEST_SCRIPT = f"""
import http.server
import threading
import urllib.request

observed = {{}}

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        observed["authorization"] = self.headers.get("Authorization", "")
        body = observed["authorization"].encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass

server = http.server.HTTPServer(("{ECHO_HOST}", {ECHO_PORT}), Handler)
threading.Thread(target=server.serve_forever, daemon=True).start()

# Route the request through the sandbox's own egress so CubeEgress applies
# the inject rule rather than the loopback shortcut.
try:
    urllib.request.urlopen(
        "http://{ECHO_HOST}:{ECHO_PORT}{ECHO_PATH}", timeout=10
    ).read()
except Exception as exc:
    print(f"request failed: {{exc}}")

server.shutdown()
print(observed.get("authorization", ""))
"""


def _skip_unless_e2e(pytestconfig: pytest.Config) -> str:
    if not pytestconfig.getoption("--run-e2e") and os.environ.get("CUBE_E2E") != "1":
        pytest.skip("use --run-e2e or set CUBE_E2E=1")
    template_id = (
        pytestconfig.getoption("--cube-template-id")
        or os.environ.get("CUBE_TEMPLATE_ID")
    )
    if not template_id:
        pytest.skip("set CUBE_TEMPLATE_ID or --cube-template-id")
    return template_id


def test_inject_render_matches_what_the_data_plane_sends(
    pytestconfig: pytest.Config,
) -> None:
    """The SDK preview must equal the header the upstream really receives.

    Before the fix this failed loudly: the preview said
    ``Basic s3cr3t-token:s3cr3t-token`` while the listener observed
    ``Basic s3cr3t-token:${SECRET}``.
    """
    template_id = _skip_unless_e2e(pytestconfig)
    config = Config(api_url=os.environ.get("CUBE_API_URL", "http://127.0.0.1:3000"))

    rule = Rule(
        name="inject-repeated-placeholder",
        match=Match(host=ECHO_HOST, port=ECHO_PORT),
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

    with Sandbox.create(
        template=template_id,
        timeout=180,
        allow_internet_access=False,
        network={"rules": [rule]},
        config=config,
    ) as sandbox:
        result = sandbox.run_code(GUEST_SCRIPT)
        observed = (result.text or "").strip().splitlines()
        observed_header = observed[-1] if observed else ""

    preview = Inject(
        header="Authorization",
        secret=SECRET,
        format=REPEATED_FORMAT,
    ).render()

    assert observed_header, (
        "the guest listener did not report an Authorization header; "
        f"run_code output was: {result.text!r}"
    )
    assert observed_header == preview, (
        "SDK preview disagrees with the data plane: the operator is shown a "
        "credential the upstream never receives"
    )
