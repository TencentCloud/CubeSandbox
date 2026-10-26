# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""
gre_tunnel_gateway.py - Validate route-aware egress through a GRE gateway.

Host prerequisites:
    1. cube-router is enabled on the Cube node.
    2. A GRE tunnel already exists on the Cube node.
    3. Host routes or policy routes send selected sandbox egress through that
       GRE tunnel.
    4. The GRE remote node forwards and NATs traffic to its own egress network.

The script only creates a sandbox and generates traffic. GRE device creation,
remote-node forwarding, and host policy routing are intentionally left outside
the example because they are environment-specific.
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
gre_tunnel_name = env("GRE_TUNNEL_NAME", "natgre")
gre_remote_tunnel_ip = require_env("GRE_REMOTE_TUNNEL_IP")
internet_target = env("GRE_INTERNET_TARGET_IP", "1.1.1.1")
internet_target_tcp_port = int(env("GRE_INTERNET_TARGET_TCP_PORT", "53"))
gre_underlay_remote_ip = optional_env("GRE_UNDERLAY_REMOTE_IP")

allow_out = [cidr32(gre_remote_tunnel_ip), cidr32(internet_target)]

print("Host-side checks to run before this example:")
print(f"  ip addr show {shlex.quote(gre_tunnel_name)}")
print(f"  ip route get {shlex.quote(gre_remote_tunnel_ip)}")
print(f"  ip route get {shlex.quote(internet_target)}")
print(f"  tcpdump -ni cube-router 'host {gre_remote_tunnel_ip} or host {internet_target}'")
print(f"  tcpdump -ni {gre_tunnel_name} 'host {gre_remote_tunnel_ip} or host {internet_target}'")
if gre_underlay_remote_ip:
    print(f"  tcpdump -ni <underlay-nic> 'proto gre or host {gre_underlay_remote_ip}'")

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
        "GRE remote tunnel endpoint traffic generator",
        udp_probe_command(gre_remote_tunnel_ip),
    )
    run_check(
        sandbox,
        "internet target TCP reachability through GRE remote gateway",
        tcp_probe_command(internet_target, internet_target_tcp_port),
    )

print("\nExpected packet path:")
print(f"  sandbox -> cube-router -> {gre_tunnel_name} -> GRE remote -> {internet_target}")
