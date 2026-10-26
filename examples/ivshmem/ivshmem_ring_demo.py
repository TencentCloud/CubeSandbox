#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""
Minimal ivshmem ring-buffer communication demo.

This script demonstrates a tiny end-to-end protocol over ivshmem:
1. host writes one message into a host->guest ring
2. guest reads it from `resource2`
3. guest writes one reply into a guest->host ring
4. host reads the reply from `/dev/shm/ivshmem-{sandbox_id}`

It is a communication example, not a performance benchmark.
The guest helper is intentionally written in Python for readability. Production
guest-side datapaths that care about latency or throughput should use a lower-
overhead implementation such as C, C++, or Rust.
"""

from __future__ import annotations

import argparse
import mmap
import os
import struct
import time

from cubesandbox import Sandbox
from cubesandbox._config import Config


IVSHMEM_SIZE = 1024 * 1024
RING_SLOT_COUNT = 8
RING_SLOT_DATA_SIZE = 256
RING_HEADER_FORMAT = "<IIII"
RING_HEADER_SIZE = struct.calcsize(RING_HEADER_FORMAT)
RING_SLOT_SIZE = 4 + RING_SLOT_DATA_SIZE
HOST_TO_GUEST_OFFSET = 0
GUEST_TO_HOST_OFFSET = 64 * 1024


def wait_for_shm_file(sandbox_id: str, timeout: int = 60) -> str:
    path = f"/dev/shm/ivshmem-{sandbox_id}"
    deadline = time.time() + timeout
    while time.time() < deadline:
        if os.path.exists(path):
            return path
        time.sleep(1)
    raise FileNotFoundError(f"ivshmem file not found: {path}")


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Minimal ivshmem ring-buffer communication demo")
    parser.add_argument("--sandbox-id", help="Use an existing sandbox")
    parser.add_argument("--template", help="Create a temporary sandbox from this ivshmem-enabled template")
    parser.add_argument("--message", default="hello-from-host", help="Message sent from host to guest")
    parser.add_argument(
        "--reply-prefix",
        default="hello-from-guest",
        help="Prefix used by the guest when replying",
    )
    parser.add_argument("--api-url", default="http://127.0.0.1:3000")
    parser.add_argument("--proxy-node-ip", default="127.0.0.1")
    parser.add_argument("--proxy-port", type=int, default=80)
    parser.add_argument("--sandbox-domain", default="cube.app")
    parser.add_argument("--timeout", type=int, default=300)
    parser.add_argument("--request-timeout", type=int, default=45)
    parser.add_argument("--cleanup", action="store_true", help="Delete a temporary sandbox after the run")
    return parser.parse_args()


def build_config(args: argparse.Namespace) -> Config:
    return Config(
        api_url=args.api_url,
        proxy_node_ip=args.proxy_node_ip,
        proxy_port=args.proxy_port,
        sandbox_domain=args.sandbox_domain,
        timeout=args.timeout,
        request_timeout=args.request_timeout,
    )


def wait_cmd(sb: Sandbox, timeout: int = 60) -> None:
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        try:
            res = sb.commands.run("echo ready", timeout=10)
            if res.exit_code == 0 and "ready" in res.stdout:
                return
            last = (res.exit_code, res.stdout, res.stderr)
        except Exception as exc:  # noqa: BLE001
            last = repr(exc)
        time.sleep(2)
    raise RuntimeError(f"sandbox command not ready: {last}")


def create_or_connect_sandbox(args: argparse.Namespace, cfg: Config) -> tuple[Sandbox, bool]:
    if args.sandbox_id:
        return Sandbox.connect(args.sandbox_id, config=cfg), False
    if not args.template:
        raise SystemExit("--template is required unless --sandbox-id is provided")
    return Sandbox.create(template=args.template, timeout=args.timeout, config=cfg), True


def reset_ring(mm: mmap.mmap, ring_offset: int) -> None:
    struct.pack_into(
        RING_HEADER_FORMAT,
        mm,
        ring_offset,
        0,
        0,
        RING_SLOT_COUNT,
        RING_SLOT_DATA_SIZE,
    )


def read_head_tail(mm: mmap.mmap, ring_offset: int) -> tuple[int, int]:
    head, tail, _, _ = struct.unpack_from(RING_HEADER_FORMAT, mm, ring_offset)
    return head, tail


def write_head(mm: mmap.mmap, ring_offset: int, value: int) -> None:
    struct.pack_into("<I", mm, ring_offset, value)


def write_tail(mm: mmap.mmap, ring_offset: int, value: int) -> None:
    struct.pack_into("<I", mm, ring_offset + 4, value)


def slot_offset(ring_offset: int, index: int) -> int:
    return ring_offset + RING_HEADER_SIZE + (index % RING_SLOT_COUNT) * RING_SLOT_SIZE


def ring_send(mm: mmap.mmap, ring_offset: int, payload: bytes, timeout: float = 5.0) -> None:
    if len(payload) > RING_SLOT_DATA_SIZE:
        raise ValueError(f"payload too large: {len(payload)} > {RING_SLOT_DATA_SIZE}")

    deadline = time.time() + timeout
    while time.time() < deadline:
        head, tail = read_head_tail(mm, ring_offset)
        if tail - head < RING_SLOT_COUNT:
            off = slot_offset(ring_offset, tail)
            struct.pack_into("<I", mm, off, len(payload))
            mm[off + 4 : off + 4 + len(payload)] = payload
            write_tail(mm, ring_offset, tail + 1)
            mm.flush()
            return
        time.sleep(0.0005)
    raise TimeoutError("ring send timed out")


def ring_recv(mm: mmap.mmap, ring_offset: int, timeout: float = 5.0) -> bytes:
    deadline = time.time() + timeout
    while time.time() < deadline:
        head, tail = read_head_tail(mm, ring_offset)
        if tail > head:
            off = slot_offset(ring_offset, head)
            length = struct.unpack_from("<I", mm, off)[0]
            payload = bytes(mm[off + 4 : off + 4 + length])
            write_head(mm, ring_offset, head + 1)
            mm.flush()
            return payload
        time.sleep(0.0005)
    raise TimeoutError("ring recv timed out")


def guest_script(reply_prefix: str) -> str:
    return f"""import mmap
import os
import struct
import time

IVSHMEM_SIZE = {IVSHMEM_SIZE}
RING_SLOT_COUNT = {RING_SLOT_COUNT}
RING_SLOT_DATA_SIZE = {RING_SLOT_DATA_SIZE}
RING_HEADER_FORMAT = {RING_HEADER_FORMAT!r}
RING_HEADER_SIZE = struct.calcsize(RING_HEADER_FORMAT)
RING_SLOT_SIZE = 4 + RING_SLOT_DATA_SIZE
HOST_TO_GUEST_OFFSET = {HOST_TO_GUEST_OFFSET}
GUEST_TO_HOST_OFFSET = {GUEST_TO_HOST_OFFSET}
REPLY_PREFIX = {reply_prefix!r}


def find_resource():
    for name in os.listdir('/sys/bus/pci/devices'):
        d = f'/sys/bus/pci/devices/{{name}}'
        try:
            vendor = open(f'{{d}}/vendor').read().strip()
            device = open(f'{{d}}/device').read().strip()
        except OSError:
            continue
        if vendor == '0x1af4' and device == '0x1110':
            return f'{{d}}/resource2'
    raise RuntimeError('ivshmem PCI device not found')


def read_head_tail(mm, ring_offset):
    head, tail, _, _ = struct.unpack_from(RING_HEADER_FORMAT, mm, ring_offset)
    return head, tail


def write_head(mm, ring_offset, value):
    struct.pack_into('<I', mm, ring_offset, value)


def write_tail(mm, ring_offset, value):
    struct.pack_into('<I', mm, ring_offset + 4, value)


def slot_offset(ring_offset, index):
    return ring_offset + RING_HEADER_SIZE + (index % RING_SLOT_COUNT) * RING_SLOT_SIZE


def ring_send(mm, ring_offset, payload, timeout=5.0):
    deadline = time.time() + timeout
    while time.time() < deadline:
        head, tail = read_head_tail(mm, ring_offset)
        if tail - head < RING_SLOT_COUNT:
            off = slot_offset(ring_offset, tail)
            struct.pack_into('<I', mm, off, len(payload))
            mm[off + 4:off + 4 + len(payload)] = payload
            write_tail(mm, ring_offset, tail + 1)
            mm.flush()
            return
        time.sleep(0.0005)
    raise TimeoutError('guest ring send timed out')


def ring_recv(mm, ring_offset, timeout=5.0):
    deadline = time.time() + timeout
    while time.time() < deadline:
        head, tail = read_head_tail(mm, ring_offset)
        if tail > head:
            off = slot_offset(ring_offset, head)
            length = struct.unpack_from('<I', mm, off)[0]
            payload = bytes(mm[off + 4:off + 4 + length])
            write_head(mm, ring_offset, head + 1)
            mm.flush()
            return payload
        time.sleep(0.0005)
    raise TimeoutError('guest ring recv timed out')


resource = find_resource()
with open(resource, 'r+b', buffering=0) as f:
    mm = mmap.mmap(f.fileno(), IVSHMEM_SIZE)
    incoming = ring_recv(mm, HOST_TO_GUEST_OFFSET).decode('utf-8', 'replace')
    print('guest received:', incoming)
    reply = f'{{REPLY_PREFIX}}: {{incoming}}'.encode()
    ring_send(mm, GUEST_TO_HOST_OFFSET, reply)
    mm.close()
"""


def main() -> int:
    args = parse_args()
    cfg = build_config(args)
    sandbox, temporary = create_or_connect_sandbox(args, cfg)

    try:
        wait_cmd(sandbox)
        shm_path = wait_for_shm_file(sandbox.sandbox_id)

        print(f"sandbox_id: {sandbox.sandbox_id}")
        print(f"host shm:   {shm_path}")

        with open(shm_path, "r+b", buffering=0) as f, mmap.mmap(f.fileno(), IVSHMEM_SIZE) as mm:
            reset_ring(mm, HOST_TO_GUEST_OFFSET)
            reset_ring(mm, GUEST_TO_HOST_OFFSET)
            mm.flush()

            outbound = args.message.encode()
            ring_send(mm, HOST_TO_GUEST_OFFSET, outbound)
            print(f"host sent:  {args.message}")

            guest = sandbox.commands.run(
                "python3 - <<'PY'\n" + guest_script(args.reply_prefix) + "\nPY",
                timeout=20,
            )
            if guest.exit_code != 0:
                raise RuntimeError(guest.stderr or guest.stdout)

            inbound = ring_recv(mm, GUEST_TO_HOST_OFFSET).decode("utf-8", "replace")
            print(guest.stdout.strip())
            print(f"host recv:  {inbound}")

        return 0
    finally:
        if temporary and args.cleanup:
            sandbox.kill()


if __name__ == "__main__":
    raise SystemExit(main())
