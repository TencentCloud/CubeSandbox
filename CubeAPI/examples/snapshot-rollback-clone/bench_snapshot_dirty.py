# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
bench_snapshot_dirty.py — Snapshot latency vs dirty-page size benchmark.

For each write size in DIRTY_SIZES_MB:
  1. Create a sandbox, write N MB to /dev/shm (tmpfs → pure RAM dirty pages)
  2. Create a snapshot, measure wall time
  3. Warm up one sandbox from the snapshot (discard, eliminates cache-miss spike)
  4. Create a second sandbox from the snapshot, measure wall time
  5. Read actual bytes written from vmm.log

Results are printed as a table and saved to dirty_vs_latency.json.
"""

import json
import os
import re
import statistics
import subprocess
import sys
import time

from cubesandbox import Sandbox
from env import TEMPLATE_ID

VMM_LOG = os.environ.get("VMM_LOG", "/data/log/CubeVmm/vmm.log")
DIRTY_SIZES_MB = [0, 10, 50, 100, 200, 500, 800, 1024]
REPEAT = 3
SETTLE_SECS = 0.5
OUTPUT_JSON = "dirty_vs_latency.json"


_BYTES_RE = re.compile(
    r"(?:PagemapAnon|Soft-dirty) snapshot saved:\s+(\d+)\s+\w+ bytes written"
)


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
        # grep not available on this host — skip dirty-page reporting
        return -1
    except subprocess.CalledProcessError:
        # no matching lines
        return -1

    for line in reversed(out.strip().splitlines()):
        m = _BYTES_RE.search(line)
        if m:
            return int(m.group(1))
    return -1


results = []

print(
    f"{'write_MB':>8}  {'dirty_MB_avg':>12}  "
    f"{'snap_avg':>10}  {'snap_min':>10}  {'snap_max':>10}  "
    f"{'create_avg':>11}  {'create_min':>11}  {'create_max':>11}"
)
print("-" * 110)

for size_mb in DIRTY_SIZES_MB:
    snap_times, create_times, dirty_list = [], [], []

    for _ in range(REPEAT):
        snap_id = None
        sb = None
        try:
            # 1. Create sandbox + write dirty data to RAM
            sb = Sandbox.create(template=TEMPLATE_ID)
            sid = sb.sandbox_id
            if size_mb > 0:
                sb.run_code(
                    f"open('/dev/shm/dirty','wb').write(b'x' * {size_mb * 1024 * 1024})"
                )

            # 2. Create snapshot (1st → PagemapAnon full mode)
            t0 = time.monotonic()
            snap = sb.create_snapshot()
            snap_times.append((time.monotonic() - t0) * 1000)
            snap_id = snap.snapshot_id
            sb.kill()
            sb = None

            dirty_list.append(grep_snapshot_bytes(sid))

            # 3. Warm-up: first restore (discard result)
            sa = Sandbox.create(template=snap_id)
            sa.kill()

            # 4. Timed restore (cache already warm)
            t1 = time.monotonic()
            sb2 = Sandbox.create(template=snap_id)
            create_times.append((time.monotonic() - t1) * 1000)
            sb2.kill()
        finally:
            if sb is not None:
                try:
                    sb.kill()
                except Exception:
                    pass
            if snap_id is not None:
                try:
                    Sandbox.delete_snapshot(snap_id)
                except Exception:
                    pass

        time.sleep(SETTLE_SECS)

    dirty_mb_avg = statistics.mean(dirty_list) / (1024 * 1024) if dirty_list[0] >= 0 else -1
    row = {
        "write_mb": size_mb,
        "dirty_mb_avg": dirty_mb_avg,
        "snap_avg_ms": statistics.mean(snap_times),
        "snap_min_ms": min(snap_times),
        "snap_max_ms": max(snap_times),
        "create_avg_ms": statistics.mean(create_times),
        "create_min_ms": min(create_times),
        "create_max_ms": max(create_times),
    }
    results.append(row)

    print(
        f"{size_mb:>8}  {dirty_mb_avg:>12.1f}  "
        f"{row['snap_avg_ms']:>10.1f}  {row['snap_min_ms']:>10.1f}  {row['snap_max_ms']:>10.1f}  "
        f"{row['create_avg_ms']:>11.1f}  {row['create_min_ms']:>11.1f}  {row['create_max_ms']:>11.1f}"
    )
    sys.stdout.flush()

with open(OUTPUT_JSON, "w") as f:
    json.dump(results, f, indent=2)
print(f"\nsaved → {OUTPUT_JSON}")
