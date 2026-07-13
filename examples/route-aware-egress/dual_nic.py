# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""
dual_nic.py - Validate route-aware egress on a host with two NICs.

Host prerequisites:
    1. cube-router is enabled on the Cube node.
    2. PUBLIC_TARGET_IP follows the host default route, usually the primary NIC.
    3. SECONDARY_NIC_TARGET_IP has a host route through the secondary NIC.

The script only creates a sandbox and generates traffic. Host routing and packet
captures must be prepared outside the sandbox.
"""

import os
import shlex

from e2b_code_interpreter import Sandbox

from env_utils import load_local_dotenv


def require_env(name: str) -> str:
    value = os.environ.get(name, "").strip()
    if not value or value.startswith("<"):
        raise SystemExit(f"missing required environment variable: {name}")
    return value


def env(name: str, default: str) -> str:
    return os.environ.get(name, default).strip() or default


def optional_env(name: str) -> str:
    value = os.environ.get(name, "").strip()
    if not value or value.startswith("<"):
        return ""
    return value


def cidr32(ip_or_cidr: str) -> str:
    return ip_or_cidr if "/" in ip_or_cidr else f"{ip_or_cidr}/32"


def run_check(sandbox: Sandbox, label: str, command: str) -> None:
    print(f"\n== {label} ==")
    print(f"$ {command}")
    result = sandbox.commands.run(command)
    stdout = result.stdout.strip()
    stderr = result.stderr.strip()
    if stdout:
        print(stdout)
    if stderr:
        print(stderr)


def udp_probe_command(target: str, port: int = 33434) -> str:
    code = (
        "import socket; "
        f"target={target!r}; port={port}; "
        "sock=socket.socket(socket.AF_INET, socket.SOCK_DGRAM); "
        "sock.sendto(b'cube-route-aware-egress', (target, port)); "
        "print(f'udp probe sent to {target}:{port}')"
    )
    return f"python3 -c {shlex.quote(code)}"


def tcp_probe_command(target: str, port: int) -> str:
    code = (
        "import socket; "
        f"target={target!r}; port={port}; "
        "sock=socket.create_connection((target, port), timeout=5); "
        "sock.close(); "
        "print(f'tcp connect ok: {target}:{port}')"
    )
    return f"python3 -c {shlex.quote(code)}"


load_local_dotenv()

template_id = require_env("CUBE_TEMPLATE_ID")
public_target = env("PUBLIC_TARGET_IP", "1.1.1.1")
public_target_tcp_port = int(env("PUBLIC_TARGET_TCP_PORT", "53"))
secondary_target = require_env("SECONDARY_NIC_TARGET_IP")
secondary_target_tcp_port = optional_env("SECONDARY_NIC_TARGET_TCP_PORT")
primary_nic = env("PRIMARY_NIC_NAME", "eth0")
secondary_nic = env("SECONDARY_NIC_NAME", "eth1")

allow_out = [cidr32(public_target), cidr32(secondary_target)]

print("Host-side checks to run before this example:")
print(f"  ip route get {shlex.quote(public_target)}")
print(f"  ip route get {shlex.quote(secondary_target)}")
print(f"  tcpdump -ni cube-router 'host {public_target} or host {secondary_target}'")
print(f"  tcpdump -ni {primary_nic} 'host {public_target} or host {secondary_target}'")
print(f"  tcpdump -ni {secondary_nic} 'host {public_target} or host {secondary_target}'")

with Sandbox.create(
    template=template_id,
    allow_internet_access=False,
    network={"allow_out": allow_out},
) as sandbox:
    info = sandbox.get_info()
    print(f"\nsandbox id: {info.sandbox_id}")
    print(f"allow_out: {allow_out}")

    run_check(
        sandbox,
        f"public target TCP reachability via host default route ({primary_nic})",
        tcp_probe_command(public_target, public_target_tcp_port),
    )
    run_check(
        sandbox,
        f"internal target traffic generator via secondary NIC ({secondary_nic})",
        udp_probe_command(secondary_target),
    )
    if secondary_target_tcp_port:
        run_check(
            sandbox,
            f"internal target TCP reachability via secondary NIC ({secondary_nic})",
            tcp_probe_command(secondary_target, int(secondary_target_tcp_port)),
        )

print("\nExpected packet path:")
print(f"  sandbox -> cube-router -> {primary_nic} -> {public_target}")
print(f"  sandbox -> cube-router -> {secondary_nic} -> {secondary_target}")
