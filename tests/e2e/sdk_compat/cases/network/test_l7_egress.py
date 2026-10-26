# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""CubeEgress L7 rules: inject, first-match, deny, TLS MITM CA, SNI/host."""

from __future__ import annotations

import json
import os
import re

import pytest
from framework.assertions import assert_command_ok
from framework.capabilities import NETWORK_L7_EGRESS
from framework.l7_egress import (
    L7_ATTEMPTS,
    L7_HTTP_TIMEOUT,
    http_json_command,
    l7_command_timeout,
)

HTTPBUN_HOST = os.environ.get("SDK_E2E_L7_ECHO_HOST", "httpbun.com")
OTHER_HOST = os.environ.get("SDK_E2E_L7_OTHER_HOST", "example.com")
INJECT_HEADER = os.environ.get("SDK_E2E_L7_INJECT_HEADER", "X-Cube-E2E-Inject")
INJECT_SECRET = os.environ.get(
    "SDK_E2E_L7_INJECT_SECRET", "e2e-inject-secret-not-a-real-key"
)
# CubeEgress MITM CA CN. The authoritative value (one-click prepare and the
# k8s chart, deploy/.../cube-egress-prepare.sh + chart values caCommonName) is
# "CubeSandbox Egress MITM CA". Override with SDK_E2E_L7_MITM_CA_CN if a
# deployment customized it. A regex fallback in the test also tolerates
# "Cube Sandbox Egress"-style spacing variants.
MITM_CA_CN = os.environ.get("SDK_E2E_L7_MITM_CA_CN", "CubeSandbox Egress MITM CA")
pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.network,
    pytest.mark.p1,
    pytest.mark.requires_internet,
    pytest.mark.requires_capability(NETWORK_L7_EGRESS),
]


def _https_rule(
    name: str,
    *,
    host: str,
    allow: bool,
    path: str | None = None,
    method: list[str] | None = None,
    inject: list[dict] | None = None,
) -> dict:
    match: dict = {"scheme": "https", "sni": host, "host": host}
    if path is not None:
        match["path"] = path
    if method is not None:
        match["method"] = method
    action: dict = {"allow": allow}
    if inject is not None:
        action["inject"] = inject
    return {"name": name, "match": match, "action": action}


def _parse_status_and_body(result) -> tuple[int | None, str]:
    assert_command_ok(result)
    text = result.stdout
    assert not text.startswith("ERROR:"), (
        f"HTTPS request failed inside sandbox; stdout={text!r} stderr={result.stderr!r}"
    )
    lines = text.splitlines()
    assert lines and lines[0].startswith("STATUS:"), (
        f"expected STATUS: line; stdout={text!r} stderr={result.stderr!r}"
    )
    status = int(lines[0].split(":", 1)[1])
    return status, "\n".join(lines[1:])


def _headers_from_httpbun(body: str, *, status: int | None = None) -> dict[str, str]:
    try:
        data = json.loads(body)
    except json.JSONDecodeError as exc:
        raise AssertionError(
            f"httpbun response was not JSON; status={status} body={body[:200]!r}: {exc}"
        ) from exc
    raw = data.get("headers") or data
    if not isinstance(raw, dict):
        raise AssertionError(
            f"unexpected httpbun body; status={status} body={body[:200]!r}"
        )
    return {str(k): str(v) for k, v in raw.items()}


def _header_ci(headers: dict[str, str], name: str) -> str | None:
    want = name.lower()
    for key, value in headers.items():
        if key.lower() == want:
            return value
    return None


def _tls_issuer_command(
    host: str, timeout: int = 15, attempts: int = L7_ATTEMPTS
) -> str:
    return (
        "python3 - <<'PY'\n"
        "import shutil, socket, ssl, subprocess, tempfile, time\n"
        f"host = {host!r}\n"
        f"timeout = {timeout!r}\n"
        f"attempts = {attempts!r}\n"
        "if shutil.which('openssl') is None:\n"
        "    print('ISSUER:ERROR:openssl_not_found')\n"
        "    raise SystemExit(0)\n"
        "ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)\n"
        "ctx.check_hostname = False\n"
        "ctx.verify_mode = ssl.CERT_NONE\n"
        "der = None\n"
        "last = None\n"
        # Retry transient connect/handshake failures (timeout / temp DNS) with
        # backoff; the public echo endpoint can be slow or rate-limit.
        "for attempt in range(attempts):\n"
        "    try:\n"
        "        with socket.create_connection((host, 443), timeout=timeout) as sock:\n"
        "            with ctx.wrap_socket(sock, server_hostname=host) as ssock:\n"
        "                der = ssock.getpeercert(binary_form=True)\n"
        "        break\n"
        "    except Exception as exc:\n"
        "        last = exc\n"
        "        if attempt + 1 < attempts:\n"
        "            time.sleep(min(2 ** attempt, 5))\n"
        "if not der:\n"
        "    print(f'ISSUER:ERROR:connect_failed:{type(last).__name__}:{last}')\n"
        "    raise SystemExit(0)\n"
        "try:\n"
        "    with tempfile.NamedTemporaryFile() as tmp:\n"
        "        tmp.write(der)\n"
        "        tmp.flush()\n"
        "        out = subprocess.check_output(\n"
        "            ['openssl', 'x509', '-inform', 'DER', '-in', tmp.name, '-noout', '-issuer'],\n"
        "            text=True,\n"
        "        )\n"
        "except Exception as exc:\n"
        "    print(f'ISSUER:ERROR:openssl_failed:{type(exc).__name__}:{exc}')\n"
        "    raise SystemExit(0)\n"
        "print('ISSUER:' + out.strip())\n"
        "PY"
    )


@pytest.mark.sandbox_create_options(
    allow_internet_access=False,
    network={
        "rules": [
            _https_rule(
                "inject_httpbun_headers",
                host=HTTPBUN_HOST,
                allow=True,
                path="/headers",
                method=["GET"],
                inject=[
                    {
                        "header": INJECT_HEADER,
                        "secret": INJECT_SECRET,
                    }
                ],
            ),
        ],
    },
)
def test_l7_credential_inject_visible_on_httpbun(sdk_sandbox, sdk_e2e_config):
    """CubeEgress injects header; httpbun echoes it (secret never in VM env)."""
    env_dump = sdk_sandbox.run_command(
        "env",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(env_dump)
    assert INJECT_SECRET not in env_dump.stdout, (
        "inject secret must not appear in the sandbox environment"
    )

    result = sdk_sandbox.run_command(
        http_json_command(
            f"https://{HTTPBUN_HOST}/headers",
            timeout=L7_HTTP_TIMEOUT,
        ),
        timeout=l7_command_timeout(sdk_e2e_config.command_timeout),
    )
    status, body = _parse_status_and_body(result)
    assert status == 200, (
        f"expected 200 from httpbun; status={status} body={body[:200]!r}"
    )
    headers = _headers_from_httpbun(body, status=status)
    assert _header_ci(headers, INJECT_HEADER) == INJECT_SECRET, (
        f"injected header {INJECT_HEADER!r} missing or wrong; headers={headers!r}"
    )


@pytest.mark.sandbox_create_options(
    allow_internet_access=False,
    network={
        "rules": [
            # First match wins: allow /headers despite a later deny for the same path.
            _https_rule(
                "allow_headers_first",
                host=HTTPBUN_HOST,
                allow=True,
                path="/headers",
                method=["GET"],
            ),
            _https_rule(
                "deny_headers_second",
                host=HTTPBUN_HOST,
                allow=False,
                path="/headers",
                method=["GET"],
            ),
            # First match wins: deny /get despite a later allow.
            _https_rule(
                "deny_get_first",
                host=HTTPBUN_HOST,
                allow=False,
                path="/get",
                method=["GET"],
            ),
            _https_rule(
                "allow_get_second",
                host=HTTPBUN_HOST,
                allow=True,
                path="/get",
                method=["GET"],
            ),
        ],
    },
)
def test_l7_first_match_wins(sdk_sandbox, sdk_e2e_config):
    """CubeEgress walks rules in order; first match decides allow/deny."""
    allowed = sdk_sandbox.run_command(
        http_json_command(
            f"https://{HTTPBUN_HOST}/headers",
            timeout=L7_HTTP_TIMEOUT,
        ),
        timeout=l7_command_timeout(sdk_e2e_config.command_timeout),
    )
    status, body = _parse_status_and_body(allowed)
    assert status == 200, (
        f"/headers should hit the first allow rule; status={status} body={body[:200]!r}"
    )

    denied = sdk_sandbox.run_command(
        http_json_command(
            f"https://{HTTPBUN_HOST}/get",
            timeout=L7_HTTP_TIMEOUT,
        ),
        timeout=l7_command_timeout(sdk_e2e_config.command_timeout),
    )
    status, body = _parse_status_and_body(denied)
    assert status == 403, (
        f"/get should hit the first deny rule; status={status} body={body[:200]!r}"
    )


@pytest.mark.sandbox_create_options(
    allow_internet_access=False,
    network={
        "rules": [
            _https_rule(
                "allow_headers_only",
                host=HTTPBUN_HOST,
                allow=True,
                path="/headers",
                method=["GET"],
            ),
            _https_rule(
                "deny_anything_explicit",
                host=HTTPBUN_HOST,
                allow=False,
                path="/anything",
                method=["GET"],
            ),
        ],
    },
)
def test_l7_deny_and_unmatched_are_blocked(sdk_sandbox, sdk_e2e_config):
    """Explicit deny and no-rule-match both return HTTP 403."""
    ok = sdk_sandbox.run_command(
        http_json_command(
            f"https://{HTTPBUN_HOST}/headers",
            timeout=L7_HTTP_TIMEOUT,
        ),
        timeout=l7_command_timeout(sdk_e2e_config.command_timeout),
    )
    status, _ = _parse_status_and_body(ok)
    assert status == 200

    explicit_deny = sdk_sandbox.run_command(
        http_json_command(
            f"https://{HTTPBUN_HOST}/anything",
            timeout=L7_HTTP_TIMEOUT,
        ),
        timeout=l7_command_timeout(sdk_e2e_config.command_timeout),
    )
    status, body = _parse_status_and_body(explicit_deny)
    assert status == 403, (
        f"explicit deny should return 403; status={status} body={body[:200]!r}"
    )

    unmatched = sdk_sandbox.run_command(
        http_json_command(
            f"https://{HTTPBUN_HOST}/get",
            timeout=L7_HTTP_TIMEOUT,
        ),
        timeout=l7_command_timeout(sdk_e2e_config.command_timeout),
    )
    status, body = _parse_status_and_body(unmatched)
    assert status == 403, (
        f"unmatched path should return 403 under L7 default-deny; "
        f"status={status} body={body[:200]!r}"
    )


@pytest.mark.sandbox_create_options(
    allow_internet_access=False,
    network={
        "rules": [
            # No path/method: this probe only does a TLS handshake with SNI
            # (no HTTP request). CubeEgress MITMs at SNI/TLS before L7 HTTP
            # match fields apply. Requires ``openssl`` in the guest image.
            _https_rule(
                "mitm_httpbun",
                host=HTTPBUN_HOST,
                allow=True,
            ),
        ],
    },
)
def test_l7_tls_mitm_issuer_is_cube_egress_ca(sdk_sandbox, sdk_e2e_config):
    """Peer cert issuer is CubeEgress CA after SNI-layer MITM (needs openssl)."""
    result = sdk_sandbox.run_command(
        _tls_issuer_command(HTTPBUN_HOST, timeout=L7_HTTP_TIMEOUT),
        timeout=l7_command_timeout(sdk_e2e_config.command_timeout),
    )
    assert_command_ok(result)
    line = result.stdout.strip()
    assert line.startswith("ISSUER:"), (
        f"expected ISSUER: line; stdout={result.stdout!r} stderr={result.stderr!r}"
    )
    issuer = line[len("ISSUER:") :]
    assert not issuer.startswith("ERROR:"), (
        f"could not read peer certificate issuer inside sandbox "
        f"(openssl missing or failed?): {issuer!r}; "
        f"stdout={result.stdout!r} stderr={result.stderr!r}"
    )
    # Accept the configured CN, or any CubeEgress-style CA subject.
    mitm_ok = MITM_CA_CN in issuer or bool(
        re.search(r"Cube\s*Sandbox\s+Egress", issuer, re.IGNORECASE)
    )
    assert mitm_ok, (
        f"peer certificate issuer should be CubeEgress MITM CA "
        f"(want substring {MITM_CA_CN!r}); got {issuer!r}"
    )


@pytest.mark.sandbox_create_options(
    allow_internet_access=False,
    network={
        "rules": [
            _https_rule(
                "allow_httpbun_sni_host",
                host=HTTPBUN_HOST,
                allow=True,
                path="/headers",
                method=["GET"],
            ),
            _https_rule(
                "deny_other_host",
                host=OTHER_HOST,
                allow=False,
                method=["GET"],
            ),
        ],
    },
)
def test_l7_sni_host_match_allow_and_deny(sdk_sandbox, sdk_e2e_config):
    """match.sni/host allow one destination and deny another."""
    allowed = sdk_sandbox.run_command(
        http_json_command(
            f"https://{HTTPBUN_HOST}/headers",
            timeout=L7_HTTP_TIMEOUT,
        ),
        timeout=l7_command_timeout(sdk_e2e_config.command_timeout),
    )
    status, body = _parse_status_and_body(allowed)
    assert status == 200, (
        f"{HTTPBUN_HOST} should match allow rule; status={status} body={body[:200]!r}"
    )

    denied = sdk_sandbox.run_command(
        http_json_command(
            f"https://{OTHER_HOST}/",
            timeout=L7_HTTP_TIMEOUT,
        ),
        timeout=l7_command_timeout(sdk_e2e_config.command_timeout),
    )
    status, body = _parse_status_and_body(denied)
    assert status == 403, (
        f"{OTHER_HOST} should match deny rule; status={status} body={body[:200]!r}"
    )
