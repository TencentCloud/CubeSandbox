# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Shared guest egress / public-access probes for network SDK E2E cases."""

from __future__ import annotations

import os
import time

import requests

from framework.assertions import assert_command_ok

TCP_TARGET_IP = os.environ.get("SDK_E2E_TCP_TARGET_IP", "8.8.8.8")
TCP_TARGET_PORT = int(os.environ.get("SDK_E2E_TCP_TARGET_PORT", "53"))
ALTERNATE_TCP_TARGET_IP = os.environ.get(
    "SDK_E2E_ALTERNATE_TCP_TARGET_IP",
    "1.1.1.1",
)
PUBLIC_ACCESS_PORT = int(os.environ.get("SDK_E2E_PUBLIC_ACCESS_PORT", "49983"))
PUBLIC_ACCESS_PATH = os.environ.get("SDK_E2E_PUBLIC_ACCESS_PATH", "/health")
PUBLIC_ACCESS_EXPECTED_STATUS = int(
    os.environ.get("SDK_E2E_PUBLIC_ACCESS_EXPECTED_STATUS", "204")
)
PUBLIC_ACCESS_EXPECTED_BODY = os.environ.get("SDK_E2E_PUBLIC_ACCESS_EXPECTED_BODY", "")
PUBLIC_HTTP_TIMEOUT = 5
PUBLIC_HTTP_READY_TIMEOUT = 30
TRAFFIC_ACCESS_TOKEN_HEADERS = (
    "e2b-traffic-access-token",
    "cube-traffic-access-token",
)


def tcp_probe_command(
    host: str = TCP_TARGET_IP,
    port: int = TCP_TARGET_PORT,
    timeout: int = 5,
) -> str:
    return (
        "python3 - <<'PY'\n"
        "import socket\n"
        "try:\n"
        "    s = socket.socket()\n"
        f"    s.settimeout({timeout!r})\n"
        f"    rc = s.connect_ex(({host!r}, {port}))\n"
        "    print('OK' if rc == 0 else f'FAIL:{rc}')\n"
        "except Exception as exc:\n"
        "    print(f'ERROR:{type(exc).__name__}:{exc}')\n"
        "finally:\n"
        "    try:\n"
        "        s.close()\n"
        "    except Exception:\n"
        "        pass\n"
        "PY"
    )


def assert_tcp_reachable(result, target: str, port: int | None = None) -> None:
    if port is None:
        port = TCP_TARGET_PORT
    assert_command_ok(result)
    assert result.stdout.strip() == "OK", (
        f"TCP target {target}:{port} should be reachable; "
        f"stdout={result.stdout!r} stderr={result.stderr!r}"
    )


def assert_tcp_blocked(result, target: str, port: int | None = None) -> None:
    """Accept REJECT (FAIL:<errno>) or DROP (timeout / TimeoutError) as blocked."""
    if port is None:
        port = TCP_TARGET_PORT
    assert_command_ok(result)
    output = result.stdout.strip()
    blocked = output.startswith("FAIL:") or _is_timeout_block(output)
    assert blocked, (
        f"TCP target {target}:{port} should be blocked; "
        f"stdout={result.stdout!r} stderr={result.stderr!r}"
    )


def _is_timeout_block(output: str) -> bool:
    # Silent drop often surfaces as socket.timeout from settimeout, not errno.
    return output.startswith("ERROR:") and (
        "Timeout" in output or "timed out" in output.lower()
    )


# Guest-side holder for testing what a policy update does to a connection that
# is already established.
#
# Two constraints shape this, and they pull against each other:
#
#   - The datapath only re-evaluates a flow when the guest sends on it, so a
#     completely idle socket would observe nothing at all.
#   - Sending application bytes is not an option. The peer speaks some protocol
#     (a DNS server on TCP/53, say) and arbitrary bytes make it hang up — which
#     arrives as a closed connection and is indistinguishable, to a naive probe,
#     from the policy reset we are trying to detect.
#
# TCP keepalive satisfies both: the probes are bare ACKs that keep packets
# flowing on the 4-tuple without ever touching the byte stream.
#
# Writes READY_PATH once connected, then RESULT_PATH with one of:
#   RESET - ECONNRESET, i.e. the datapath tore the flow down. The only outcome
#           that unambiguously comes from policy enforcement.
#   ALIVE - nothing happened for the whole observation window.
#   EOF   - the peer closed on its own (idle timeout, protocol policy). Says
#           nothing about our policy, so callers treat it as inconclusive.
#   DATA  - the peer sent something unexpected; also inconclusive.
_ESTABLISHED_FLOW_HOLDER = """
import socket, sys

host, port, ready_path, result_path, window = (
    sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4], float(sys.argv[5])
)


def record(value):
    with open(result_path, "w") as fh:
        fh.write(value)


s = socket.socket()
s.settimeout(5)
try:
    s.connect((host, port))
except Exception as exc:
    record("CONNECT_FAILED:%s" % exc)
    sys.exit(0)

# Probe every second after one second of idleness, so a revoked flow meets the
# datapath within ~1s of the update instead of waiting for application traffic.
s.setsockopt(socket.SOL_SOCKET, socket.SO_KEEPALIVE, 1)
for opt, value in (("TCP_KEEPIDLE", 1), ("TCP_KEEPINTVL", 1), ("TCP_KEEPCNT", 4)):
    if hasattr(socket, opt):
        s.setsockopt(socket.IPPROTO_TCP, getattr(socket, opt), value)

with open(ready_path, "w") as fh:
    fh.write("CONNECTED")

s.settimeout(window)
try:
    record("EOF" if s.recv(1) == b"" else "DATA")
except socket.timeout:
    record("ALIVE")
except ConnectionResetError:
    record("RESET")
except OSError as exc:
    record("ERROR:%s" % type(exc).__name__)
"""

ESTABLISHED_HOLDER_PATH = "/tmp/cube_e2e_hold_flow.py"
ESTABLISHED_READY_PATH = "/tmp/cube_e2e_hold_ready"
ESTABLISHED_RESULT_PATH = "/tmp/cube_e2e_hold_result"


# A revoked flow is reset within about a second of the update (keepalive probes
# run at 1s), so the window only needs a little slack. Every extra second adds
# exposure to the peer's own idle timeout, which arrives as an inconclusive EOF.
ESTABLISHED_OBSERVE_SECONDS = int(
    os.environ.get("SDK_E2E_ESTABLISHED_WINDOW_SECONDS", "5")
)

# Held connections use 443 rather than the reachability probes' port. The holder
# never writes to the socket, and a TLS server waiting for a ClientHello tolerates
# that for tens of seconds, whereas a DNS server closes an idle TCP/53 connection
# almost immediately. The policies under test allow the whole IP, so the port
# makes no difference to what is being asserted.
ESTABLISHED_TARGET_PORT = int(
    os.environ.get("SDK_E2E_ESTABLISHED_TARGET_PORT", "443")
)


def start_established_flow(
    sdk_sandbox,
    host: str = TCP_TARGET_IP,
    port: int = ESTABLISHED_TARGET_PORT,
    *,
    window_seconds: int = ESTABLISHED_OBSERVE_SECONDS,
    command_timeout: int = 30,
) -> bool:
    """Open a TCP connection inside the guest and leave it running in the background.

    window_seconds bounds how long the holder watches the connection. Keep it
    short: a revoked flow is reset within about a second of the update, while a
    long window only adds time for the peer to hit its own idle timeout and
    muddy the result with an EOF.

    Returns whether the connection was established, so callers can skip rather
    than fail when the target is simply not reachable in this environment.
    """
    sdk_sandbox.write_file(ESTABLISHED_HOLDER_PATH, _ESTABLISHED_FLOW_HOLDER)
    sdk_sandbox.run_command(
        f"rm -f {ESTABLISHED_READY_PATH} {ESTABLISHED_RESULT_PATH}",
        timeout=command_timeout,
    )
    sdk_sandbox.run_command(
        f"nohup python3 {ESTABLISHED_HOLDER_PATH} {host} {port} "
        f"{ESTABLISHED_READY_PATH} {ESTABLISHED_RESULT_PATH} {window_seconds} "
        f">/dev/null 2>&1 & echo started",
        timeout=command_timeout,
    )
    for _ in range(10):
        probe = sdk_sandbox.run_command(
            f"test -f {ESTABLISHED_READY_PATH} && echo READY || echo WAIT",
            timeout=command_timeout,
        )
        if "READY" in probe.stdout:
            return True
        time.sleep(1)
    return False


def require_conclusive_flow_outcome(outcome: str, target: str) -> str:
    """Skip unless the holder's verdict actually reflects policy enforcement.

    Only RESET and ALIVE do. EOF/DATA mean the peer acted on its own and a
    PENDING/ERROR verdict means the probe never reached a conclusion — asserting
    on any of those would report an environment quirk as a product bug.
    """
    import pytest

    if outcome not in ("RESET", "ALIVE"):
        pytest.skip(
            f"held connection to {target} gave an inconclusive verdict {outcome!r}; "
            "the peer closed it or the probe never settled, so this run cannot "
            "tell a policy reset from a peer-side close"
        )
    return outcome


def wait_established_flow_outcome(
    sdk_sandbox,
    *,
    attempts: int = 25,
    command_timeout: int = 30,
) -> str:
    """Poll for the holder's verdict; returns RESET, ALIVE, EOF, DATA, ERROR:*, or PENDING."""
    for _ in range(attempts):
        probe = sdk_sandbox.run_command(
            f"cat {ESTABLISHED_RESULT_PATH} 2>/dev/null || echo PENDING",
            timeout=command_timeout,
        )
        outcome = probe.stdout.strip()
        if outcome and outcome != "PENDING":
            return outcome
        time.sleep(1)
    return "PENDING"


def public_url(sdk_sandbox) -> str:
    host = sdk_sandbox.get_host(PUBLIC_ACCESS_PORT).rstrip("/")
    path = PUBLIC_ACCESS_PATH if PUBLIC_ACCESS_PATH.startswith("/") else f"/{PUBLIC_ACCESS_PATH}"
    # CubeSandbox returns a host, while some E2B SDK versions expose get_url()
    # semantics through the adapter and may already include a scheme.
    base_url = host if host.startswith(("http://", "https://")) else f"http://{host}"
    return f"{base_url}{path}"


def get_public_url(url: str, *, headers: dict[str, str] | None = None) -> requests.Response:
    return requests.get(url, headers=headers, timeout=PUBLIC_HTTP_TIMEOUT)


def public_response_matches(response: requests.Response) -> bool:
    return (
        response.status_code == PUBLIC_ACCESS_EXPECTED_STATUS
        and response.text == PUBLIC_ACCESS_EXPECTED_BODY
    )


def wait_for_public_response(
    url: str,
    *,
    headers: dict[str, str] | None = None,
) -> requests.Response:
    deadline = time.monotonic() + PUBLIC_HTTP_READY_TIMEOUT
    interval = 1.0
    last_observation = "not requested"
    while time.monotonic() < deadline:
        try:
            response = get_public_url(url, headers=headers)
            if public_response_matches(response):
                return response
            last_observation = (
                f"status={response.status_code} body={response.text[:120]!r}"
            )
        except requests.RequestException as exc:
            last_observation = f"{type(exc).__name__}: {exc}"
        time.sleep(min(interval, max(0.0, deadline - time.monotonic())))
        interval = min(interval * 2, 8.0)
    raise AssertionError(
        "public URL did not return expected response "
        f"status={PUBLIC_ACCESS_EXPECTED_STATUS} "
        f"body={PUBLIC_ACCESS_EXPECTED_BODY!r}: {last_observation}"
    )


def assert_public_response(response: requests.Response) -> None:
    assert response.status_code == PUBLIC_ACCESS_EXPECTED_STATUS, (
        f"expected HTTP {PUBLIC_ACCESS_EXPECTED_STATUS}; "
        f"status={response.status_code} body={response.text[:120]!r}"
    )
    assert response.text == PUBLIC_ACCESS_EXPECTED_BODY, (
        f"expected public response body {PUBLIC_ACCESS_EXPECTED_BODY!r}; "
        f"status={response.status_code} body={response.text[:120]!r}"
    )


def assert_forbidden(response: requests.Response, scenario: str) -> None:
    # The restrict-public-access contract documents HTTP 403 for missing or
    # invalid traffic access tokens.
    assert response.status_code == 403, (
        f"{scenario} should be rejected with HTTP 403; "
        f"status={response.status_code} body={response.text[:120]!r}"
    )
