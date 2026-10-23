# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Sandbox access to a service listening on the node IP.

Direct TCP/UDP/ICMP is delivered to the host stack with the sandbox address
kept as the source. An HTTP rule for that same IP is intercepted by CubeEgress.

The listener runs in this process, so the test must execute on the sandbox
node (one-click single node). Set SDK_E2E_NODE_IP when the default-route
source is not the node NIC address. The case skips when cube-dev is absent.
"""

from __future__ import annotations

import ipaddress
import json
import os
import re
import socket
import subprocess
import threading
import uuid
from collections.abc import Iterator
from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest
from adapters import create_adapter
from framework.assertions import assert_command_ok
from framework.capabilities import (
    NETWORK_ALLOW_DENY,
    NETWORK_L7_CUSTOM_PORT,
    NETWORK_NODE_LOCAL,
    capabilities_for_backend,
)
from framework.cleanup import safe_kill
from framework.config import SdkE2EConfig
from framework.network_probe import (
    TCP_TARGET_IP,
    assert_tcp_blocked,
    tcp_probe_command,
)

INJECT_HEADER = "X-Cube-Node-Local"
INJECT_SECRET = f"e2e-node-local-{uuid.uuid4().hex[:12]}"

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.network,
    pytest.mark.p1,
    pytest.mark.requires_capability(NETWORK_NODE_LOCAL),
]


def _require(backend: str, *caps: str) -> None:
    missing = [cap for cap in caps if cap not in capabilities_for_backend(backend)]
    if missing:
        pytest.skip(
            f"backend {backend!r} does not support {', '.join(missing)}"
        )


def _node_ipv4() -> str:
    override = os.environ.get("SDK_E2E_NODE_IP", "").strip()
    if override:
        ipaddress.IPv4Address(override)
        return override
    try:
        out = subprocess.check_output(
            ["ip", "-4", "route", "get", "1.1.1.1"], text=True
        )
    except (OSError, subprocess.CalledProcessError) as exc:
        pytest.skip(f"cannot determine node IP; set SDK_E2E_NODE_IP ({exc})")
    match = re.search(r"\bsrc\s+(\S+)", out)
    if not match:
        pytest.skip(f"cannot parse node IP from {out!r}; set SDK_E2E_NODE_IP")
    ipaddress.IPv4Address(match.group(1))
    return match.group(1)


def _cube_dev_network() -> ipaddress.IPv4Network:
    try:
        out = subprocess.check_output(
            ["ip", "-4", "-o", "addr", "show", "dev", "cube-dev"], text=True
        )
    except (OSError, subprocess.CalledProcessError) as exc:
        pytest.skip(
            "cube-dev is not on this host; node-local cases run on the "
            f"sandbox node ({exc})"
        )
    match = re.search(r"\binet\s+(\S+)", out)
    if not match:
        pytest.skip(f"cube-dev has no IPv4 address ({out!r})")
    return ipaddress.ip_interface(match.group(1)).network


class _EchoHandler(BaseHTTPRequestHandler):
    def do_GET(self) -> None:  # noqa: N802
        payload = json.dumps(
            {
                "peer": self.client_address[0],
                "headers": dict(self.headers.items()),
            }
        ).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *_args: object) -> None:
        pass


@contextmanager
def _http_server() -> Iterator[int]:
    try:
        server = ThreadingHTTPServer(("0.0.0.0", 0), _EchoHandler)
    except OSError as exc:
        pytest.skip(f"cannot bind the node-local HTTP listener ({exc})")
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield int(server.server_address[1])
    finally:
        server.shutdown()
        server.server_close()


@contextmanager
def _udp_server() -> Iterator[int]:
    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    try:
        sock.bind(("0.0.0.0", 0))
    except OSError as exc:
        sock.close()
        pytest.skip(f"cannot bind the node-local UDP listener ({exc})")
    stop = threading.Event()

    def _serve() -> None:
        sock.settimeout(0.5)
        while not stop.is_set():
            try:
                data, addr = sock.recvfrom(1024)
            except TimeoutError:
                continue
            except OSError:
                return
            if data:
                sock.sendto(data, addr)

    thread = threading.Thread(target=_serve, daemon=True)
    thread.start()
    try:
        yield int(sock.getsockname()[1])
    finally:
        stop.set()
        sock.close()
        thread.join(timeout=2)


def _http_get(url: str) -> str:
    return (
        "python3 - <<'PY'\n"
        "import urllib.request\n"
        f"url = {url!r}\n"
        "try:\n"
        "    with urllib.request.urlopen(url, timeout=10) as resp:\n"
        "        status = resp.status\n"
        "        body = resp.read().decode()\n"
        "    print('STATUS:%s' % status)\n"
        "    print(body)\n"
        "except Exception as exc:\n"
        "    print('ERROR:%s:%s' % (type(exc).__name__, exc))\n"
        "PY"
    )


def _udp_probe(host: str, port: int, payload: str) -> str:
    return (
        "python3 - <<'PY'\n"
        "import socket\n"
        f"host = {host!r}\n"
        f"port = {port}\n"
        f"payload = {payload!r}.encode()\n"
        "s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)\n"
        "s.settimeout(5)\n"
        "try:\n"
        "    s.sendto(payload, (host, port))\n"
        "    data, _addr = s.recvfrom(1024)\n"
        "    print(data.decode())\n"
        "except Exception as exc:\n"
        "    print('ERROR:%s:%s' % (type(exc).__name__, exc))\n"
        "finally:\n"
        "    s.close()\n"
        "PY"
    )


def _icmp_probe(host: str) -> str:
    """One ICMP echo from inside the sandbox. Guest images do not ship ping."""
    return (
        "python3 - <<'PY'\n"
        "import os\n"
        "import socket\n"
        "import struct\n"
        "import sys\n"
        "import time\n"
        f"host = {host!r}\n"
        "ident = os.getpid() & 0xffff\n"
        "seq = 1\n"
        "payload = b'cube-node-local-icmp'\n"
        "\n"
        "def checksum(data):\n"
        "    if len(data) % 2:\n"
        "        data += b'\\x00'\n"
        "    total = sum(struct.unpack('!%dH' % (len(data) // 2), data))\n"
        "    while total >> 16:\n"
        "        total = (total & 0xffff) + (total >> 16)\n"
        "    return ((~total) & 0xffff) or 0xffff\n"
        "\n"
        "header = struct.pack('!BBHHH', 8, 0, 0, ident, seq)\n"
        "cksum = checksum(header + payload)\n"
        "packet = struct.pack('!BBHHH', 8, 0, cksum, ident, seq) + payload\n"
        "sock = None\n"
        "try:\n"
        "    sock = socket.socket(\n"
        "        socket.AF_INET, socket.SOCK_RAW, socket.IPPROTO_ICMP)\n"
        "    sock.sendto(packet, (host, 0))\n"
        "    deadline = time.monotonic() + 2\n"
        "    while True:\n"
        "        remaining = deadline - time.monotonic()\n"
        "        if remaining <= 0:\n"
        "            raise TimeoutError('no echo reply within 2s')\n"
        "        sock.settimeout(remaining)\n"
        "        try:\n"
        "            data, addr = sock.recvfrom(1500)\n"
        "        except socket.timeout:\n"
        "            raise TimeoutError('no echo reply within 2s')\n"
        "        if not data or data[0] >> 4 != 4:\n"
        "            continue\n"
        "        ihl = (data[0] & 0x0f) * 4\n"
        "        if ihl < 20 or len(data) < ihl + 8:\n"
        "            continue\n"
        "        typ, _code, _cksum, rid, rseq = struct.unpack(\n"
        "            '!BBHHH', data[ihl:ihl + 8])\n"
        "        if typ == 0 and rid == ident and rseq == seq and addr[0] == host:\n"
        "            print('icmp echo reply from %s' % addr[0])\n"
        "            break\n"
        "except Exception as exc:\n"
        "    print('ERROR:%s:%s' % (type(exc).__name__, exc), file=sys.stderr)\n"
        "    sys.exit(1)\n"
        "finally:\n"
        "    if sock is not None:\n"
        "        sock.close()\n"
        "PY"
    )


def _parse_http(result) -> tuple[int, dict]:
    assert_command_ok(result)
    text = result.stdout
    assert not text.startswith("ERROR:"), (
        f"HTTP request failed inside the sandbox; stdout={text!r} "
        f"stderr={result.stderr!r}"
    )
    lines = text.splitlines()
    assert lines and lines[0].startswith("STATUS:"), (
        f"expected STATUS line; stdout={text!r} stderr={result.stderr!r}"
    )
    status = int(lines[0].split(":", 1)[1])
    try:
        body = json.loads("\n".join(lines[1:]))
    except json.JSONDecodeError as exc:
        raise AssertionError(f"echo body was not JSON; stdout={text!r}") from exc
    return status, body


def _node_answers_icmp(ip: str) -> bool:
    try:
        proc = subprocess.run(
            ["ping", "-c", "1", "-W", "1", ip],
            capture_output=True,
            text=True,
            timeout=3,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired):
        return False
    return proc.returncode == 0


@pytest.mark.requires_capability(NETWORK_ALLOW_DENY)
def test_node_local_direct_tcp_udp(
    sdk_backend: str,
    sdk_e2e_config: SdkE2EConfig,
):
    """Plain access reaches the host service and keeps the sandbox source address."""
    _require(sdk_backend, NETWORK_NODE_LOCAL, NETWORK_ALLOW_DENY)
    node_ip = _node_ipv4()
    sandbox_net = _cube_dev_network()
    payload = f"cube-node-local-{uuid.uuid4().hex[:8]}"

    with _http_server() as http_port, _udp_server() as udp_port:
        adapter = None
        try:
            adapter = create_adapter(
                sdk_backend,
                sdk_e2e_config,
                metadata={
                    "test_suite": "sdk_compat",
                    "test_backend": sdk_backend,
                    "test_case": "node_local_direct",
                },
                create_options={
                    "allow_internet_access": False,
                    "network": {
                        "allow_out": [node_ip],
                        "deny_out": ["0.0.0.0/0"],
                    },
                },
            )
            result = adapter.run_command(
                _http_get(f"http://{node_ip}:{http_port}/"),
                timeout=sdk_e2e_config.command_timeout,
            )
            status, body = _parse_http(result)
            assert status == 200, body
            peer = ipaddress.ip_address(body["peer"])
            assert peer in sandbox_net and str(peer) != node_ip, (
                f"direct peer {peer} should be a sandbox address in {sandbox_net}, "
                f"not the node IP {node_ip}; body={body!r}"
            )

            udp = adapter.run_command(
                _udp_probe(node_ip, udp_port, payload),
                timeout=sdk_e2e_config.command_timeout,
            )
            assert_command_ok(udp)
            assert udp.stdout.strip() == payload, (
                f"UDP echo mismatch; stdout={udp.stdout!r} stderr={udp.stderr!r}"
            )

            blocked = adapter.run_command(
                tcp_probe_command(
                    TCP_TARGET_IP, timeout=sdk_e2e_config.network_probe_timeout
                ),
                timeout=sdk_e2e_config.command_timeout,
            )
            assert_tcp_blocked(blocked, TCP_TARGET_IP)
        finally:
            if adapter is not None:
                safe_kill(adapter, sdk_e2e_config)


@pytest.mark.requires_capability(NETWORK_ALLOW_DENY)
def test_node_local_direct_icmp(
    sdk_backend: str,
    sdk_e2e_config: SdkE2EConfig,
):
    """ICMP echo to the node IP is delivered locally. Skips if the node drops echo."""
    _require(sdk_backend, NETWORK_NODE_LOCAL, NETWORK_ALLOW_DENY)
    node_ip = _node_ipv4()
    _cube_dev_network()
    if not _node_answers_icmp(node_ip):
        pytest.skip(
            f"node does not answer ICMP echo on {node_ip}; "
            "enable echo replies to cover the ICMP path"
        )

    adapter = None
    try:
        adapter = create_adapter(
            sdk_backend,
            sdk_e2e_config,
            metadata={
                "test_suite": "sdk_compat",
                "test_backend": sdk_backend,
                "test_case": "node_local_icmp",
            },
            create_options={
                "allow_internet_access": False,
                "network": {
                    "allow_out": [node_ip],
                    "deny_out": ["0.0.0.0/0"],
                },
            },
        )
        result = adapter.run_command(
            _icmp_probe(node_ip),
            timeout=sdk_e2e_config.command_timeout,
        )
        assert_command_ok(result)
    finally:
        if adapter is not None:
            safe_kill(adapter, sdk_e2e_config)


@pytest.mark.requires_capability(NETWORK_L7_CUSTOM_PORT)
def test_node_local_http_via_cubeegress(
    sdk_backend: str,
    sdk_e2e_config: SdkE2EConfig,
):
    """An HTTP rule on the node IP is marked and injected by CubeEgress."""
    _require(sdk_backend, NETWORK_NODE_LOCAL, NETWORK_L7_CUSTOM_PORT)
    node_ip = _node_ipv4()
    _cube_dev_network()

    with _http_server() as http_port:
        adapter = None
        try:
            adapter = create_adapter(
                sdk_backend,
                sdk_e2e_config,
                metadata={
                    "test_suite": "sdk_compat",
                    "test_backend": sdk_backend,
                    "test_case": "node_local_l7_http",
                },
                create_options={
                    "allow_internet_access": False,
                    "network": {
                        "rules": [
                            {
                                "name": "node-local-http",
                                "match": {
                                    "host": node_ip,
                                    "port": http_port,
                                    "scheme": "http",
                                },
                                "action": {
                                    "allow": True,
                                    "inject": [
                                        {
                                            "header": INJECT_HEADER,
                                            "secret": INJECT_SECRET,
                                        }
                                    ],
                                },
                            }
                        ],
                    },
                },
            )
            result = adapter.run_command(
                _http_get(f"http://{node_ip}:{http_port}/"),
                timeout=sdk_e2e_config.command_timeout,
            )
            status, body = _parse_http(result)
            assert status == 200, body
            headers = {str(k).lower(): str(v) for k, v in body["headers"].items()}
            assert headers.get(INJECT_HEADER.lower()) == INJECT_SECRET, (
                f"CubeEgress did not inject {INJECT_HEADER}; body={body!r}"
            )
        finally:
            if adapter is not None:
                safe_kill(adapter, sdk_e2e_config)
