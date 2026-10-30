# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
bench_snapshot_dirty.py — Snapshot latency vs dirty-page size benchmark (single size).

For one write size (`-d` MB):
  1. Create a sandbox, fill and retain `-d` MB of anonymous process memory
  2. Create a snapshot, measure wall time
  3. Warm up one sandbox from the snapshot (discard, eliminates cache-miss spike)
  4. Create a second sandbox from the snapshot, measure wall time
  5. Read actual bytes written from vmm.log

This script provides the mechanism for ONE dirty-page size per invocation,
mirroring cube-bench. Sweep multiple sizes by invoking it repeatedly, e.g.:

    python bench_snapshot_dirty.py -d 0
    python bench_snapshot_dirty.py -d 10
    python bench_snapshot_dirty.py -d 100
    python bench_snapshot_dirty.py -d 1024
"""

import argparse
import math
import os
import re
import statistics
import subprocess
import sys
import time

from cubesandbox import Sandbox
from env import TEMPLATE_ID

VMM_LOG = os.environ.get("VMM_LOG", "/data/log/CubeVmm/vmm.log")

_BYTES_RE = re.compile(
    r"(?:PagemapAnon|Soft-dirty) snapshot saved:\s+(\d+)\s+\w+ bytes written"
)


def parse_args():
    p = argparse.ArgumentParser(description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("-d", "--dirty-mb", type=int, default=10,
                   help="dirty-page size in MB written before snapshot (default: 10)")
    p.add_argument("-n", "--rounds", type=int, default=3,
                   help="measured rounds (default: 3)")
    p.add_argument("-s", "--settle-secs", type=float, default=0.5,
                   help="sleep seconds between rounds (default: 0.5)")
    p.add_argument("--no-header", action="store_true",
                   help="suppress the table header (useful when sweeping)")
    return p.parse_args()


def grep_snapshot_bytes(sandbox_id: str) -> int:
    """
    Return actual bytes written from vmm.log for this sandbox's snapshot.
    Matches both:
      - "PagemapAnon snapshot saved: N anon bytes written to ..."  (1st snapshot)
      - "Soft-dirty snapshot saved: N dirty bytes written to ..."  (2nd+ snapshot)
    Returns -1 if the log is unavailable or no matching line is found.
    """
    try:
        out = subprocess.check_output(
            ["grep", "-i", sandbox_id, VMM_LOG],
            text=True, stderr=subprocess.DEVNULL,
        )
    except FileNotFoundError:
        return -1
    except subprocess.CalledProcessError:
        return -1

    for line in reversed(out.strip().splitlines()):
        m = _BYTES_RE.search(line)
        if m:
            return int(m.group(1))
    return -1


def percentile(data: list, p: float) -> float:
    s = sorted(data)
    k = int(math.ceil(len(s) * p / 100.0)) - 1
    return s[max(0, min(k, len(s) - 1))]


def run_round(size_mb: int) -> dict:
    snap_id = None
    sb = None
    sa = None
    sb2 = None
    try:
        sb = Sandbox.create(template=TEMPLATE_ID)
        sid = sb.sandbox_id
        # Keep incompressible anonymous memory alive in the notebook kernel
        # through snapshot creation. Fill in chunks so generating the data does
        # not temporarily allocate a second size_mb buffer.
        execution = sb.run_code(f"""
import hashlib
import os

_cubebench_dirty_size = {size_mb * 1024 * 1024}
_cubebench_dirty_buf = bytearray(_cubebench_dirty_size)
_cubebench_dirty_view = memoryview(_cubebench_dirty_buf)
_cubebench_dirty_chunk = 1024 * 1024
for _cubebench_dirty_offset in range(0, _cubebench_dirty_size, _cubebench_dirty_chunk):
    _cubebench_dirty_length = min(
        _cubebench_dirty_chunk,
        _cubebench_dirty_size - _cubebench_dirty_offset,
    )
    _cubebench_dirty_view[
        _cubebench_dirty_offset:_cubebench_dirty_offset + _cubebench_dirty_length
    ] = os.urandom(_cubebench_dirty_length)
assert len(_cubebench_dirty_buf) == _cubebench_dirty_size
_cubebench_dirty_sha256 = hashlib.sha256(_cubebench_dirty_view).hexdigest()
""")
        if execution.error is not None:
            raise RuntimeError(
                f"failed to allocate {size_mb} MB anonymous memory: "
                f"{execution.error.name}: {execution.error.value}"
            )

        t0 = time.monotonic()
        snap = sb.create_snapshot()
        snap_ms = (time.monotonic() - t0) * 1000
        snap_id = snap.snapshot_id
        sb.kill()
        sb = None

        dirty_bytes = grep_snapshot_bytes(sid)

        # warm-up: first restore (discard)
        sa = Sandbox.create(template=snap_id)
        sa.kill()
        sa = None

        # timed restore (cache warm)
        t1 = time.monotonic()
        sb2 = Sandbox.create(template=snap_id)
        create_ms = (time.monotonic() - t1) * 1000
        sb2.kill()
        sb2 = None
        return {"snap_ms": snap_ms, "create_ms": create_ms, "dirty_bytes": dirty_bytes}
    finally:
        for s in (sb, sa, sb2):
            if s is not None:
                s.kill()
        if snap_id is not None:
            Sandbox.delete_snapshot(snap_id)


def main():
    args = parse_args()

    if not args.no_header:
        print(
            f"{'write_MB':>8}  {'dirty_MB_avg':>12}  "
            f"{'snap_avg':>10}  {'snap_min':>10}  {'snap_p95':>10}  {'snap_max':>10}  "
            f"{'create_avg':>11}  {'create_min':>11}  {'create_p95':>11}  {'create_max':>11}"
        )
        print("-" * 130)

    snap_times, create_times, dirty_list = [], [], []
    for _ in range(args.rounds):
        r = run_round(args.dirty_mb)
        snap_times.append(r["snap_ms"])
        create_times.append(r["create_ms"])
        dirty_list.append(r["dirty_bytes"])
        time.sleep(args.settle_secs)

    dirty_mb_avg = statistics.mean(dirty_list) / (1024 * 1024) if dirty_list[0] >= 0 else -1

    print(
        f"{args.dirty_mb:>8}  {dirty_mb_avg:>12.1f}  "
        f"{statistics.mean(snap_times):>10.1f}  {min(snap_times):>10.1f}  "
        f"{percentile(snap_times, 95):>10.1f}  {max(snap_times):>10.1f}  "
        f"{statistics.mean(create_times):>11.1f}  {min(create_times):>11.1f}  "
        f"{percentile(create_times, 95):>11.1f}  {max(create_times):>11.1f}"
    )
    sys.stdout.flush()


if __name__ == "__main__":
    main()
