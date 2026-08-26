#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""
network_dynamic_update.py — Change a running sandbox's egress policy in place.

Use case:
    An agent starts locked down and needs to be granted a destination partway
    through a task, or has to lose one the moment a step finishes. Recreating
    the sandbox would throw away its filesystem and process state, so the
    policy has to change under a live VM.

How it works:
    sandbox.update_network() replaces the whole egress policy. It takes one
    object carrying `allow_internet_access` alongside the rest, matching E2B's
    update_network; creation keeps the flag as a separate argument because E2B's
    create does too. Like creation it is a replacement rather than a patch — a
    key you leave out is cleared.

    What creation cannot do is re-evaluate connections that are already open. An
    update does: a connection the new policy no longer permits is reset instead
    of running until it closes on its own. That is what makes revocation take
    effect rather than merely apply to future connections.

This script walks four scenarios:
    1. IP allow list — grant one, confirm another stays blocked, revoke.
    2. A live connection carried across a revoking update, which gets reset.
    3. Domain allow list — grant one, then switch the policy to another.
    4. L7 rules — start intercepting a host mid-run, with no restart.

Run:
    cp .env.example .env   # fill in values
    pip install -r requirements.txt
    python network_dynamic_update.py
"""

import os
import sys

from cubesandbox import Sandbox

from env_utils import load_local_dotenv

load_local_dotenv()

TEMPLATE_ID = os.environ.get("CUBE_TEMPLATE_ID")
if not TEMPLATE_ID:
    sys.exit("CUBE_TEMPLATE_ID is required")

# Two reachable IPs, so "granted" and "still blocked" can be told apart in the
# same sandbox. Override if these are not routable for you.
GRANTED_IP = os.environ.get("GRANTED_IP", "8.8.8.8")
OTHER_IP = os.environ.get("OTHER_IP", "1.1.1.1")
PROBE_PORT = int(os.environ.get("PROBE_PORT", "53"))

# Two stable HTTPS hosts for the domain scenario. Domain filtering works off the
# TLS SNI on 443, so the probe has to be a real request rather than a bare
# connect to a resolved address.
GRANTED_DOMAIN = os.environ.get("GRANTED_DOMAIN", "example.com")
OTHER_DOMAIN = os.environ.get("OTHER_DOMAIN", "example.org")

# An echo host for the L7 scenario: it reflects request headers back, which is
# how an injected header becomes visible from inside the sandbox.
L7_HOST = os.environ.get("L7_HOST", "httpbun.com")
L7_HEADER = "X-Cube-Injected"

HOLDER_PATH = "/tmp/hold_flow.py"

# Holds one connection open and reports how it ends. It never writes to the
# socket: the peer speaks some protocol and arbitrary bytes would make it hang
# up, which is indistinguishable from the policy reset we want to observe. TCP
# keepalive gives us packets on the flow — the datapath only re-evaluates a flow
# when the guest sends — without touching the byte stream.
HOLDER_SCRIPT = """
import socket, sys

host, port, window = sys.argv[1], int(sys.argv[2]), float(sys.argv[3])
s = socket.socket()
s.settimeout(5)
try:
    s.connect((host, port))
except Exception as exc:
    print("CONNECT_FAILED:%s" % exc, flush=True)
    sys.exit(0)

s.setsockopt(socket.SOL_SOCKET, socket.SO_KEEPALIVE, 1)
for opt, value in (("TCP_KEEPIDLE", 1), ("TCP_KEEPINTVL", 1), ("TCP_KEEPCNT", 4)):
    if hasattr(socket, opt):
        s.setsockopt(socket.IPPROTO_TCP, getattr(socket, opt), value)
print("CONNECTED", flush=True)

s.settimeout(window)
try:
    print("EOF" if s.recv(1) == b"" else "DATA", flush=True)
except socket.timeout:
    print("ALIVE", flush=True)
except ConnectionResetError:
    print("RESET", flush=True)
except OSError as exc:
    print("ERROR:%s" % type(exc).__name__, flush=True)
"""


def tcp_reachable(sandbox: Sandbox, ip: str) -> bool:
    """Whether a fresh TCP connection to ip:PROBE_PORT succeeds."""
    result = sandbox.commands.run(
        f"timeout 5 bash -c '</dev/tcp/{ip}/{PROBE_PORT}' "
        f"&& echo REACHABLE || echo BLOCKED",
        timeout=20,
    )
    return "REACHABLE" in result.stdout


def https_status(sandbox: Sandbox, domain: str) -> str:
    """HTTP status from an HTTPS request; curl reports 000 when it never connected."""
    result = sandbox.commands.run(
        f"curl -sS --max-time 8 -o /dev/null -w '%{{http_code}}\\n' https://{domain}/ || true",
        timeout=30,
    )
    status = result.stdout.strip().splitlines()[-1] if result.stdout.strip() else "000"
    return "BLOCKED" if status == "000" else status


def l7_probe(sandbox: Sandbox, path: str) -> str:
    """Status plus whether the injected header came back, for one HTTPS request.

    Uses curl -k on purpose. While a host is intercepted, CubeEgress terminates
    the TLS session with its own certificate, so verification would fail unless
    the guest image trusts the interception CA. Skipping verification keeps the
    example about the policy rather than about CA distribution.
    """
    result = sandbox.commands.run(
        f"curl -sSk --max-time 10 -w '\\nSTATUS:%{{http_code}}\\n' https://{L7_HOST}{path} "
        f"|| echo 'STATUS:000'",
        timeout=30,
    )
    out = result.stdout
    status = "000"
    for line in out.splitlines():
        if line.startswith("STATUS:"):
            status = line.split(":", 1)[1].strip()
    injected = L7_HEADER.lower() in out.lower()
    if status == "000":
        return "no connection"
    return f"HTTP {status}" + (f", {L7_HEADER} injected" if injected else "")


def hold_across_update(sandbox: Sandbox, host: str, port: int, revoke) -> str:
    """Open a connection, run revoke() while it is live, and report its fate.

    Returns RESET when the datapath tore the flow down, ALIVE when the update
    left it running, or EOF/DATA/ERROR when the peer acted on its own — in which
    case this run simply cannot tell the two apart.
    """
    sandbox.files.write(HOLDER_PATH, HOLDER_SCRIPT)
    sandbox.commands.run(
        f"nohup python3 {HOLDER_PATH} {host} {port} 10 >/tmp/hold.out 2>&1 & echo started",
        timeout=20,
    )
    ready = sandbox.commands.run(
        f"for i in $(seq 10); do grep -q . /tmp/hold.out && break; sleep 1; done; "
        f"head -1 /tmp/hold.out",
        timeout=30,
    )
    if "CONNECTED" not in ready.stdout:
        return f"NOT_ESTABLISHED({ready.stdout.strip()})"

    revoke()

    outcome = sandbox.commands.run(
        "for i in $(seq 15); do test $(wc -l </tmp/hold.out) -ge 2 && break; sleep 1; done; "
        "sed -n 2p /tmp/hold.out",
        timeout=40,
    )
    return outcome.stdout.strip() or "PENDING"


def main() -> None:
    # Start fully locked down: no allow list, no public egress.
    with Sandbox.create(
        template=TEMPLATE_ID,
        allow_internet_access=False,
        timeout=600,
    ) as sandbox:
        print(f"sandbox {sandbox.sandbox_id} created with no egress")
        print(f"  {GRANTED_IP} reachable: {tcp_reachable(sandbox, GRANTED_IP)}   (expect False)")

        # --- 1. IP allow list -------------------------------------------------
        sandbox.update_network(
            network={"allow_out": [GRANTED_IP], "allow_internet_access": False}
        )
        print(f"\ngranted {GRANTED_IP}")
        print(f"  {GRANTED_IP} reachable: {tcp_reachable(sandbox, GRANTED_IP)}   (expect True)")
        print(f"  {OTHER_IP} reachable: {tcp_reachable(sandbox, OTHER_IP)}   (expect False)")

        # --- 2. A live connection carried across a revoking update ------------
        # Held on 443: the holder stays silent, and a TLS server waiting for a
        # ClientHello tolerates that far longer than a DNS server tolerates an
        # idle TCP/53 connection. The policy allows the whole IP, so the port
        # makes no difference to what is being shown.
        def revoke_everything():
            sandbox.update_network(network={"allow_internet_access": False})
            print("\nrevoked every destination while a connection was open")

        outcome = hold_across_update(sandbox, GRANTED_IP, 443, revoke_everything)
        print(f"  held connection ended as: {outcome}   (expect RESET)")
        print(f"  {GRANTED_IP} reachable: {tcp_reachable(sandbox, GRANTED_IP)}   (expect False)")

        # --- 3. Domain allow list ---------------------------------------------
        # Domains need the default-deny that allow_internet_access=False gives,
        # otherwise everything is reachable anyway and the allow list is moot.
        # The resolver's own address is allowed automatically, so DNS keeps
        # working without being named here.
        sandbox.update_network(
            network={"allow_out": [GRANTED_DOMAIN], "allow_internet_access": False}
        )
        print(f"\ngranted domain {GRANTED_DOMAIN}")
        print(f"  https://{GRANTED_DOMAIN} -> {https_status(sandbox, GRANTED_DOMAIN)}   (expect 200)")
        print(f"  https://{OTHER_DOMAIN} -> {https_status(sandbox, OTHER_DOMAIN)}   (expect BLOCKED)")

        sandbox.update_network(
            network={"allow_out": [OTHER_DOMAIN], "allow_internet_access": False}
        )
        print(f"\nswitched the allow list to {OTHER_DOMAIN}")
        print(f"  https://{OTHER_DOMAIN} -> {https_status(sandbox, OTHER_DOMAIN)}   (expect 200)")

        # --- 4. L7 rules ------------------------------------------------------
        # Start intercepting a host mid-run. A rule's host is allowed implicitly,
        # and once any rule exists for it the host is L7 default-deny: only what
        # a rule matches gets through, everything else is refused by the proxy.
        sandbox.update_network(
            network={
                "allow_out": [L7_HOST],
                "allow_internet_access": False,
                "rules": [
                    {
                        "name": "inject_on_headers",
                        "match": {"scheme": "https", "sni": L7_HOST, "host": L7_HOST,
                                  "path": "/headers"},
                        "action": {"allow": True,
                                   "inject": [{"header": L7_HEADER, "secret": "demo-token"}]},
                    },
                ],
            }
        )
        print(f"\nstarted intercepting {L7_HOST} with an L7 rule on /headers")
        print(f"  GET /headers -> {l7_probe(sandbox, '/headers')}   (expect HTTP 200, header injected)")
        print(f"  GET /get     -> {l7_probe(sandbox, '/get')}   (expect HTTP 403, no rule matches)")

        print("\ndynamic network update ok")


if __name__ == "__main__":
    main()
