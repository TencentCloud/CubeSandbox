# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Catch-all L7 rules: every DNS name, only ports 80 and 443.

``host="*"`` / ``sni="*"`` lets a sandbox reach HTTP and HTTPS on names it
resolves, and sends that traffic through CubeEgress. A port other than 80/443
stays closed. An address that was never looked up in DNS stays closed. A more
specific rule still wins. Credential injection on ``"*"`` is rejected at create
time.
"""

from __future__ import annotations

import os

import pytest
from adapters import create_adapter
from framework.assertions import assert_command_ok
from framework.capabilities import (
    NETWORK_DNS_ALLOW,
    NETWORK_L7_EGRESS,
    capabilities_for_backend,
)
from framework.cleanup import safe_kill
from framework.config import SdkE2EConfig
from framework.l7_egress import (
    L7_HTTP_TIMEOUT,
    http_json_command,
    l7_command_timeout,
)

HTTP_HOST = os.environ.get("SDK_E2E_L7_ECHO_HOST", "httpbun.com")
OTHER_HOST = os.environ.get("SDK_E2E_CATCH_ALL_OTHER_HOST", "example.com")
# github.com accepts TCP/22 and TCP/443, so a failed :22 connect is the policy
# and not a host that simply has nothing listening.
EXTRA_HOST = os.environ.get("SDK_E2E_CATCH_ALL_EXTRA_HOST", "github.com")
EXTRA_PORT = int(os.environ.get("SDK_E2E_CATCH_ALL_EXTRA_PORT", "22"))
# Public address that answers on 443. Probed before any DNS lookup so a
# learned allow cannot make the connect succeed.
DIRECT_IP = os.environ.get("SDK_E2E_CATCH_ALL_DIRECT_IP", "1.1.1.1")

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.network,
    pytest.mark.p1,
    pytest.mark.requires_internet,
    pytest.mark.requires_capability(NETWORK_L7_EGRESS),
    pytest.mark.requires_capability(NETWORK_DNS_ALLOW),
]

CATCH_ALL_RULES = [
    {
        "name": "audit_all_http",
        "match": {"scheme": "http", "host": "*"},
        "action": {"allow": True, "audit": "metadata"},
    },
    {
        "name": "audit_all_https",
        "match": {"scheme": "https", "sni": "*"},
        "action": {"allow": True, "audit": "metadata"},
    },
]


def _skip_without_capability(backend: str) -> None:
    caps = capabilities_for_backend(backend)
    for capability in (NETWORK_L7_EGRESS, NETWORK_DNS_ALLOW):
        if capability not in caps:
            pytest.skip(f"backend {backend!r} does not support {capability}")


def _probe_command(host: str, port: int, timeout: int, *, resolve: bool) -> str:
    """TCP-probe *host*:*port*. When *resolve* is set, look the name up first.

    DNS lookups are retried. The TCP result is not: OK versus FAIL is the
    policy verdict.
    """
    return (
        "python3 - <<'PY'\n"
        "import socket, time\n"
        f"host = {host!r}\n"
        f"port = {port!r}\n"
        f"timeout = {timeout!r}\n"
        f"resolve = {resolve!r}\n"
        "target = host\n"
        "if resolve:\n"
        "    infos = None\n"
        "    last = None\n"
        "    for attempt in range(3):\n"
        "        try:\n"
        "            infos = socket.getaddrinfo(host, port, socket.AF_INET, socket.SOCK_STREAM)\n"
        "            break\n"
        "        except Exception as exc:\n"
        "            last = exc\n"
        "            if attempt + 1 < 3:\n"
        "                time.sleep(min(2 ** attempt, 5))\n"
        "    if infos is None:\n"
        "        print(f'RESOLVE:ERROR:{type(last).__name__}:{last}')\n"
        "        raise SystemExit(0)\n"
        "    addrs = []\n"
        "    for family, _, _, _, sockaddr in infos:\n"
        "        if family == socket.AF_INET:\n"
        "            addrs.append(sockaddr[0])\n"
        "    addrs = list(dict.fromkeys(addrs))\n"
        "    print('RESOLVED:' + ','.join(addrs))\n"
        "    if not addrs:\n"
        "        print('PROBE:ERROR:no_ipv4')\n"
        "        raise SystemExit(0)\n"
        "    target = addrs[0]\n"
        "s = socket.socket()\n"
        "try:\n"
        "    s.settimeout(timeout)\n"
        "    rc = s.connect_ex((target, port))\n"
        "    print('PROBE:OK' if rc == 0 else f'PROBE:FAIL:{rc}')\n"
        "except Exception as exc:\n"
        "    print(f'PROBE:ERROR:{type(exc).__name__}:{exc}')\n"
        "finally:\n"
        "    s.close()\n"
        "PY"
    )


def _lines(result) -> list[str]:
    assert_command_ok(result)
    return [line.strip() for line in result.stdout.splitlines() if line.strip()]


def _assert_reachable(result, label: str) -> None:
    lines = _lines(result)
    resolve_err = next((line for line in lines if line.startswith("RESOLVE:ERROR:")), "")
    assert not resolve_err, (
        f"{label} DNS failed inside the sandbox; "
        f"stdout={result.stdout!r} stderr={result.stderr!r}"
    )
    probe = next((line for line in lines if line.startswith("PROBE:")), "")
    assert probe == "PROBE:OK", (
        f"{label} should be reachable; stdout={result.stdout!r} stderr={result.stderr!r}"
    )


def _assert_blocked(result, label: str) -> None:
    lines = _lines(result)
    resolve_err = next((line for line in lines if line.startswith("RESOLVE:ERROR:")), "")
    assert not resolve_err, (
        f"{label} DNS failed inside the sandbox; "
        f"stdout={result.stdout!r} stderr={result.stderr!r}"
    )
    probe = next((line for line in lines if line.startswith("PROBE:")), "")
    blocked = probe.startswith("PROBE:FAIL:") or (
        probe.startswith("PROBE:ERROR:")
        and ("timeout" in probe.lower() or "timed out" in probe.lower())
    )
    assert blocked, (
        f"{label} should be blocked; stdout={result.stdout!r} stderr={result.stderr!r}"
    )


def _http_status(sdk_sandbox, sdk_e2e_config, url: str) -> int:
    result = sdk_sandbox.run_command(
        http_json_command(url, timeout=L7_HTTP_TIMEOUT),
        timeout=l7_command_timeout(sdk_e2e_config.command_timeout),
    )
    assert_command_ok(result)
    text = result.stdout
    assert not text.startswith("ERROR:"), (
        f"{url} failed inside the sandbox; stdout={text!r} stderr={result.stderr!r}"
    )
    line = text.splitlines()[0]
    assert line.startswith("STATUS:"), (
        f"expected STATUS from {url}; stdout={text[:200]!r} stderr={result.stderr!r}"
    )
    return int(line.split(":", 1)[1])


@pytest.mark.sandbox_create_options(
    allow_internet_access=False,
    network={"rules": CATCH_ALL_RULES},
)
def test_catch_all_allows_only_http_and_https(sdk_sandbox, sdk_e2e_config):
    """Learned names open 80 and 443. Other ports and a raw IP stay closed."""
    direct = sdk_sandbox.run_command(
        _probe_command(DIRECT_IP, 443, sdk_e2e_config.network_probe_timeout, resolve=False),
        timeout=sdk_e2e_config.command_timeout,
    )
    _assert_blocked(direct, f"{DIRECT_IP}:443 without DNS")

    assert _http_status(sdk_sandbox, sdk_e2e_config, f"https://{HTTP_HOST}/get") == 200
    http_port = sdk_sandbox.run_command(
        _probe_command(HTTP_HOST, 80, sdk_e2e_config.network_probe_timeout, resolve=True),
        timeout=sdk_e2e_config.command_timeout,
    )
    _assert_reachable(http_port, f"{HTTP_HOST}:80")

    https = sdk_sandbox.run_command(
        _probe_command(EXTRA_HOST, 443, sdk_e2e_config.network_probe_timeout, resolve=True),
        timeout=sdk_e2e_config.command_timeout,
    )
    _assert_reachable(https, f"{EXTRA_HOST}:443")

    other = sdk_sandbox.run_command(
        _probe_command(EXTRA_HOST, EXTRA_PORT, sdk_e2e_config.network_probe_timeout, resolve=True),
        timeout=sdk_e2e_config.command_timeout,
    )
    _assert_blocked(other, f"{EXTRA_HOST}:{EXTRA_PORT}")


@pytest.mark.sandbox_create_options(
    allow_internet_access=False,
    network={
        "rules": [
            {
                "name": "deny_echo",
                "match": {"scheme": "https", "sni": HTTP_HOST, "host": HTTP_HOST},
                "action": {"allow": False},
            },
            *CATCH_ALL_RULES,
        ],
    },
)
def test_catch_all_specific_rule_wins(sdk_sandbox, sdk_e2e_config):
    """A rule for one name is decided before the catch-all."""
    denied = _http_status(sdk_sandbox, sdk_e2e_config, f"https://{HTTP_HOST}/get")
    assert denied == 403, f"{HTTP_HOST} should hit the specific deny rule; status={denied}"

    allowed = _http_status(sdk_sandbox, sdk_e2e_config, f"https://{OTHER_HOST}/")
    assert allowed == 200, f"{OTHER_HOST} should hit the catch-all allow; status={allowed}"


def test_catch_all_rejects_credential_injection(sdk_backend: str, sdk_e2e_config: SdkE2EConfig):
    """A catch-all rule cannot carry credential injection."""
    _skip_without_capability(sdk_backend)
    adapter = None
    try:
        with pytest.raises(Exception) as exc_info:
            adapter = create_adapter(
                sdk_backend,
                sdk_e2e_config,
                metadata={
                    "test_suite": "sdk_compat",
                    "test_backend": sdk_backend,
                    "test_case": "l7_catch_all_inject",
                },
                create_options={
                    "allow_internet_access": False,
                    "network": {
                        "rules": [
                            {
                                "name": "leak",
                                "match": {"scheme": "https", "sni": "*"},
                                "action": {
                                    "allow": True,
                                    "inject": [
                                        {"header": "Authorization", "secret": "not-a-real-secret"}
                                    ],
                                },
                            }
                        ]
                    },
                },
            )
        assert "cannot be combined with action.inject" in str(exc_info.value), (
            f"create should reject catch-all injection; got={exc_info.value!r}"
        )
    finally:
        if adapter is not None:
            safe_kill(adapter, sdk_e2e_config)
