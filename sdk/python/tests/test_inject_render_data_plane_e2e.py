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

Requirements:

- ``--run-e2e`` or ``CUBE_E2E=1``
- ``CUBE_TEMPLATE_ID`` or pytest ``--cube-template-id``
- ``CUBE_L7_E2E_HTTP_TARGET_HOST`` (environment variable): a host address
  reachable from sandboxes
  that is not CubeVS's node-IP fast path (a Docker bridge gateway is
  suitable). It must answer ``GET /`` with a body echoing the request's
  ``Authorization`` header — httpbin's ``/headers`` or any equivalent.
"""

from __future__ import annotations

import os

import pytest

from cubesandbox import Action, Config, Inject, Match, Rule, Sandbox

pytestmark = pytest.mark.e2e

TARGET_HOST_ENV = "CUBE_L7_E2E_HTTP_TARGET_HOST"
TARGET_PORT = 80
TARGET_PATH = "/"

# Stands in for a real credential; the assertions only care that the two
# placeholders are treated the same way the data plane treats them.
SECRET = "s3cr3t-token"

# A format that repeats the placeholder twice — the shape that exposed the
# divergence between preview and data plane.
REPEATED_FORMAT = "Basic ${SECRET}:${SECRET}"


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
            "suitable) and that echoes the request's Authorization header back "
            "in its body."
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

    rule = Rule(
        name="inject-repeated-placeholder",
        match=Match(host=target_host, port=TARGET_PORT, scheme="http"),
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
    # code: the whole point is the *value* the upstream saw.
    guest_script = f"""
import json
import urllib.request

resp = urllib.request.urlopen(
    "http://{target_host}:{TARGET_PORT}{TARGET_PATH}", timeout=20
)
body = resp.read().decode("utf-8", "replace")
try:
    headers = json.loads(body).get("headers", {{}})
except ValueError:
    headers = {{}}
print(headers.get("Authorization", ""))
"""

    with Sandbox.create(
        template=template_id,
        timeout=180,
        allow_internet_access=False,
        network={"rules": [rule]},
        config=config,
    ) as sandbox:
        result = sandbox.run_code(guest_script)
        # `Execution.text` is the value of the cell's final *expression*, and the
        # script ends with `print(...)` — a statement — so there is no main
        # result and `text` is None. Read what the guest actually printed.
        stdout = list(getattr(result.logs, "stdout", None) or [])
        observed_header = next(
            (line.strip() for line in reversed(stdout) if line.strip()), ""
        )

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
