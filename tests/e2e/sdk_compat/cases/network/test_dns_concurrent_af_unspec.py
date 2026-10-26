# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Minimal reproduction for issue #1406.

Concurrent A/AAAA (``AF_UNSPEC``) resolution inside a sandbox intermittently
fails with ``EAI_AGAIN`` ("Temporary failure in name resolution"), while
A-only and AAAA-only lookups are reliable and glibc ``RES_OPTIONS=single-request``
(which serializes the paired queries) makes ``AF_UNSPEC`` reliable again.

This embeds the issue's original ``getaddrinfo`` loop verbatim (only its output
is made machine-parseable) and turns it into a regression gate:

* ``AF_INET`` establishes that DNS + IPv4 egress work at all (environment sanity).
* ``AF_UNSPEC`` + ``single-request`` is the control: the paired queries are
  serialized, so this is expected to be fully reliable.
* ``AF_UNSPEC`` (default) is the subject: glibc sends the A and AAAA queries
  concurrently on the same UDP source port. On an affected build this is where
  the sandbox UDP/TAP data path drops one of the pair.

If the subject is materially less reliable than the (serialized) control while
both baselines are healthy, #1406 is reproduced and the test FAILS. On a fixed
build all three are reliable and the test passes.
"""

from __future__ import annotations

import json
import math
import os
import shlex
from collections import Counter

import pytest
from framework.assertions import assert_command_ok
from framework.capabilities import NETWORK_DNS_ALLOW

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.network,
    pytest.mark.p2,
    pytest.mark.slow,
    pytest.mark.requires_internet,
    pytest.mark.requires_capability(NETWORK_DNS_ALLOW),
]

# A public hostname with both A and AAAA records, matching the issue report.
REPRO_HOST = os.environ.get("SDK_E2E_DNS_REPRO_HOST", "www.example.com")
REPRO_PORT = int(os.environ.get("SDK_E2E_DNS_REPRO_PORT", "443"))
# Use enough attempts for the success-rate comparison to remain meaningful.
REPRO_ATTEMPTS = int(os.environ.get("SDK_E2E_DNS_REPRO_ATTEMPTS", "30"))
if REPRO_ATTEMPTS < 10:
    raise ValueError("SDK_E2E_DNS_REPRO_ATTEMPTS must be at least 10")
# Amplify the failure: 1s timeout and no glibc-level retry, so a single dropped
# query surfaces as one lost attempt instead of being masked by a resolver retry.
BASE_RES_OPTIONS = os.environ.get(
    "SDK_E2E_DNS_REPRO_RES_OPTIONS", "timeout:1 attempts:1"
)

# The issue's original script, with its ``print(...)`` replaced by a stable
# ``RESULT:<json>`` line so the harness can parse it deterministically.
_REPRO_SCRIPT = (
    "import json, os, socket\n"
    "family = getattr(socket, os.environ['FAMILY'])\n"
    "attempts = int(os.environ.get('ATTEMPTS', '30'))\n"
    "host = os.environ.get('HOST', 'www.example.com')\n"
    "port = int(os.environ.get('PORT', '443'))\n"
    "success = 0\n"
    "dual_stack = 0\n"
    "errors = {}\n"
    "for _ in range(attempts):\n"
    "    try:\n"
    "        addresses = socket.getaddrinfo(host, port, family, socket.SOCK_STREAM)\n"
    "        success += 1\n"
    "        families = {address[0] for address in addresses}\n"
    "        dual_stack += socket.AF_INET in families and socket.AF_INET6 in families\n"
    "    except OSError as exc:\n"
    "        error = str(getattr(exc, 'errno', None)) + ':' + str(exc)\n"
    "        errors[error] = errors.get(error, 0) + 1\n"
    "print('RESULT:' + json.dumps({\n"
    "    'family': os.environ['FAMILY'],\n"
    "    'attempts': attempts,\n"
    "    'success': success,\n"
    "    'dual_stack': dual_stack,\n"
    "    'errors': errors,\n"
    "}))\n"
)


def _repro_command(family: str, res_options: str, attempts: int) -> str:
    """Build the in-sandbox command running the repro script for one family."""
    env_prefix = (
        f"FAMILY={family} "
        f"ATTEMPTS={attempts} "
        f"HOST={shlex.quote(REPRO_HOST)} "
        f"PORT={REPRO_PORT} "
        f"RES_OPTIONS={shlex.quote(res_options)}"
    )
    return f"{env_prefix} python3 - <<'PY'\n{_REPRO_SCRIPT}PY"


def _run_repro(
    sdk_sandbox,
    sdk_e2e_config,
    family: str,
    res_options: str,
    attempts: int = REPRO_ATTEMPTS,
) -> dict:
    """Run one family/RES_OPTIONS combination and return the parsed RESULT dict."""
    # The serialized control may wait on A and AAAA across several nameservers.
    command_timeout = max(sdk_e2e_config.command_timeout, attempts * 6 + 30)
    result = sdk_sandbox.run_command(
        _repro_command(family, res_options, attempts),
        timeout=command_timeout,
    )
    assert_command_ok(result)
    line = next(
        (
            ln.strip()[len("RESULT:") :]
            for ln in result.stdout.splitlines()
            if ln.strip().startswith("RESULT:")
        ),
        None,
    )
    assert line is not None, (
        f"repro script produced no RESULT line for FAMILY={family}; "
        f"stdout={result.stdout!r} stderr={result.stderr!r}"
    )
    return json.loads(line)


@pytest.mark.sandbox_create_options(allow_internet_access=True)
def test_concurrent_af_unspec_resolution_matches_single_request(
    sdk_sandbox,
    sdk_e2e_config,
):
    """AF_UNSPEC must be as reliable as single-stack / serialized resolution."""
    option_names = {option.split(":", 1)[0] for option in BASE_RES_OPTIONS.split()}
    assert not option_names.intersection({"single-request", "single-request-reopen"}), (
        "SDK_E2E_DNS_REPRO_RES_OPTIONS must not serialize A/AAAA queries"
    )
    tolerance = max(3, math.ceil(REPRO_ATTEMPTS * 0.15))

    v4 = _run_repro(sdk_sandbox, sdk_e2e_config, "AF_INET", BASE_RES_OPTIONS)
    if v4["success"] < REPRO_ATTEMPTS - tolerance:
        pytest.skip(f"IPv4 DNS/egress unreliable in this environment; v4={v4!r}")

    control = {"success": 0, "dual_stack": 0, "errors": Counter()}
    subject = {"success": 0, "dual_stack": 0, "errors": Counter()}
    batch_sizes = [REPRO_ATTEMPTS // 3] * 3
    for index in range(REPRO_ATTEMPTS % 3):
        batch_sizes[index] += 1

    for index, attempts in enumerate(batch_sizes):
        order = ("control", "subject") if index % 2 == 0 else ("subject", "control")
        for kind in order:
            res_options = (
                f"single-request {BASE_RES_OPTIONS}"
                if kind == "control"
                else BASE_RES_OPTIONS
            )
            result = _run_repro(
                sdk_sandbox,
                sdk_e2e_config,
                "AF_UNSPEC",
                res_options,
                attempts,
            )
            aggregate = control if kind == "control" else subject
            aggregate["success"] += result["success"]
            aggregate["dual_stack"] += result["dual_stack"]
            aggregate["errors"].update(result["errors"])

    control["errors"] = dict(control["errors"])
    subject["errors"] = dict(subject["errors"])
    context = f"v4={v4!r} control(single-request)={control!r} subject={subject!r}"

    if control["dual_stack"] < REPRO_ATTEMPTS - tolerance:
        pytest.skip(
            "serialized AF_UNSPEC did not reliably return both address families; "
            f"the DNS environment cannot exercise this regression; {context}"
        )

    assert subject["dual_stack"] >= control["dual_stack"] - tolerance, (
        "concurrent AF_UNSPEC resolution returned both address families "
        "significantly less reliably than serialized resolution, consistent with "
        "issue #1406. Workaround: RES_OPTIONS=single-request. "
        f"tolerance={tolerance} {context}"
    )
