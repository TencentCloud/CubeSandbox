# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Domain allow_out + DNS A learning (exact and leading ``*.`` wildcard)."""

from __future__ import annotations

import os

import pytest

from framework.assertions import assert_command_ok
from framework.capabilities import NETWORK_DNS_ALLOW

ALLOWED_DOMAIN = os.environ.get("SDK_E2E_DNS_ALLOW_DOMAIN", "example.com")
ALLOWED_WILDCARD = os.environ.get("SDK_E2E_DNS_ALLOW_WILDCARD", "*.example.com")
WILDCARD_SUBDOMAIN = os.environ.get(
    "SDK_E2E_DNS_ALLOW_WILDCARD_SUBDOMAIN",
    "www.example.com",
)
BLOCKED_DOMAIN = os.environ.get("SDK_E2E_DNS_ALLOW_BLOCKED_DOMAIN", "one.one.one.one")
DOMAIN_TCP_PORT = int(os.environ.get("SDK_E2E_DNS_ALLOW_TCP_PORT", "443"))

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.network,
    pytest.mark.p1,
    pytest.mark.requires_internet,
    pytest.mark.requires_capability(NETWORK_DNS_ALLOW),
]


def _resolve_and_probe_command(host: str, port: int, timeout: int, attempts: int = 3) -> str:
    """Resolve *host* (retrying transient DNS failures), print addrs, then
    TCP-probe the first IPv4 address.

    Only the resolution step is retried (temporary resolver failure / rate
    limit); the TCP probe verdict (OK / FAIL) is a policy outcome and is never
    retried, so a blocked domain still reports PROBE:FAIL immediately.
    """
    return (
        "python3 - <<'PY'\n"
        "import socket, time\n"
        f"host = {host!r}\n"
        f"port = {port!r}\n"
        f"timeout = {timeout!r}\n"
        f"attempts = {attempts!r}\n"
        "infos = None\n"
        "last = None\n"
        "for attempt in range(attempts):\n"
        "    try:\n"
        "        infos = socket.getaddrinfo(host, port, type=socket.SOCK_STREAM)\n"
        "        break\n"
        "    except Exception as exc:\n"
        "        last = exc\n"
        "        if attempt + 1 < attempts:\n"
        "            time.sleep(min(2 ** attempt, 5))\n"
        "if infos is None:\n"
        "    print(f'RESOLVE:ERROR:{type(last).__name__}:{last}')\n"
        "    raise SystemExit(0)\n"
        "addrs = []\n"
        "for family, _, _, _, sockaddr in infos:\n"
        "    if family not in (socket.AF_INET,):\n"
        "        continue\n"
        "    addrs.append(sockaddr[0])\n"
        "print('RESOLVED:' + ','.join(dict.fromkeys(addrs)))\n"
        "if not addrs:\n"
        "    print('PROBE:ERROR:no_ipv4')\n"
        "    raise SystemExit(0)\n"
        "target = addrs[0]\n"
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


def _assert_domain_reachable(result, host: str) -> None:
    assert_command_ok(result)
    lines = [line.strip() for line in result.stdout.splitlines() if line.strip()]
    resolve_err = next((line for line in lines if line.startswith("RESOLVE:ERROR:")), "")
    assert not resolve_err, (
        f"{host} DNS resolve failed inside sandbox (not a policy verdict); "
        f"stdout={result.stdout!r} stderr={result.stderr!r}"
    )
    resolved = next((line for line in lines if line.startswith("RESOLVED:")), "")
    probe = next((line for line in lines if line.startswith("PROBE:")), "")
    assert resolved.startswith("RESOLVED:") and resolved != "RESOLVED:", (
        f"{host} should resolve via DNS learning path; "
        f"stdout={result.stdout!r} stderr={result.stderr!r}"
    )
    assert probe == "PROBE:OK", (
        f"{host}:{DOMAIN_TCP_PORT} should be reachable after DNS A learning; "
        f"stdout={result.stdout!r} stderr={result.stderr!r}"
    )


def _assert_domain_blocked_after_resolve(result, host: str) -> None:
    """DNS must succeed so we can judge TCP deny; then connect to that IP fails.

    Domain allow-list mode still forwards DNS for unmatched QNAMEs; only the
    later TCP connect is denied. A resolve failure is an environment/DNS issue,
    not a TCP-policy verdict.
    """
    assert_command_ok(result)
    lines = [line.strip() for line in result.stdout.splitlines() if line.strip()]
    resolve_err = next((line for line in lines if line.startswith("RESOLVE:ERROR:")), "")
    assert not resolve_err, (
        f"{host} DNS resolve failed inside sandbox (cannot judge TCP policy); "
        f"stdout={result.stdout!r} stderr={result.stderr!r}"
    )
    probe = next((line for line in lines if line.startswith("PROBE:")), "")
    blocked = probe.startswith("PROBE:FAIL:") or (
        probe.startswith("PROBE:ERROR:")
        and ("Timeout" in probe or "timed out" in probe.lower())
    )
    assert blocked, (
        f"{host}:{DOMAIN_TCP_PORT} should be blocked (no learned allow); "
        f"stdout={result.stdout!r} stderr={result.stderr!r}"
    )


@pytest.mark.sandbox_create_options(
    allow_internet_access=False,
    network={
        "allow_out": [ALLOWED_DOMAIN],
        "deny_out": ["0.0.0.0/0"],
    },
)
def test_domain_allow_out_learns_dns_and_blocks_others(sdk_sandbox, sdk_e2e_config):
    """Exact domain allow_out + DNS learning; other destinations blocked."""
    allowed = sdk_sandbox.run_command(
        _resolve_and_probe_command(
            ALLOWED_DOMAIN,
            DOMAIN_TCP_PORT,
            sdk_e2e_config.network_probe_timeout,
        ),
        timeout=sdk_e2e_config.command_timeout,
    )
    _assert_domain_reachable(allowed, ALLOWED_DOMAIN)

    blocked = sdk_sandbox.run_command(
        _resolve_and_probe_command(
            BLOCKED_DOMAIN,
            DOMAIN_TCP_PORT,
            sdk_e2e_config.network_probe_timeout,
        ),
        timeout=sdk_e2e_config.command_timeout,
    )
    _assert_domain_blocked_after_resolve(blocked, BLOCKED_DOMAIN)


@pytest.mark.sandbox_create_options(
    allow_internet_access=False,
    network={
        "allow_out": [ALLOWED_WILDCARD],
        "deny_out": ["0.0.0.0/0"],
    },
)
def test_wildcard_domain_allow_matches_subdomain_not_apex(sdk_sandbox, sdk_e2e_config):
    """``*.example.com`` matches subdomain; apex does not (probe apex first).

    Apex is probed before the subdomain so a shared A-record cannot be learned
    via the subdomain and then make a later apex TCP probe succeed by IP.
    """
    # Apex resolve + TCP to the learned-or-resolved IP must fail: ``*.`` does
    # not match the apex QNAME, so no allow entry is installed for that IP.
    apex = sdk_sandbox.run_command(
        _resolve_and_probe_command(
            ALLOWED_DOMAIN,
            DOMAIN_TCP_PORT,
            sdk_e2e_config.network_probe_timeout,
        ),
        timeout=sdk_e2e_config.command_timeout,
    )
    _assert_domain_blocked_after_resolve(apex, ALLOWED_DOMAIN)

    subdomain = sdk_sandbox.run_command(
        _resolve_and_probe_command(
            WILDCARD_SUBDOMAIN,
            DOMAIN_TCP_PORT,
            sdk_e2e_config.network_probe_timeout,
        ),
        timeout=sdk_e2e_config.command_timeout,
    )
    _assert_domain_reachable(subdomain, WILDCARD_SUBDOMAIN)

    other = sdk_sandbox.run_command(
        _resolve_and_probe_command(
            BLOCKED_DOMAIN,
            DOMAIN_TCP_PORT,
            sdk_e2e_config.network_probe_timeout,
        ),
        timeout=sdk_e2e_config.command_timeout,
    )
    _assert_domain_blocked_after_resolve(other, BLOCKED_DOMAIN)
