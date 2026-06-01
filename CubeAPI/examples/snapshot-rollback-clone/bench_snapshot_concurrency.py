# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
bench_snapshot_concurrency.py — Snapshot creation latency vs concurrency benchmark.

Creates N sandboxes in parallel, then triggers create_snapshot() on all of them
concurrently, measuring wall time and per-snapshot amortized time.
"""

import math
import statistics
import sys
import time
from concurrent.futures import ThreadPoolExecutor, as_completed

from cubesandbox import Sandbox
from env import TEMPLATE_ID

CONCURRENCY_LEVELS = [1, 5]
ROUNDS = 5
DIRTY_MB = 10          # fixed dirty-page size across all runs
SETTLE_SECS = 1.0


def run_round(concurrency: int) -> dict:
    """Create `concurrency` sandboxes, snapshot all concurrently, return wall ms."""
    sandboxes = [Sandbox.create(template=TEMPLATE_ID) for _ in range(concurrency)]
    for sb in sandboxes:
        if DIRTY_MB > 0:
            sb.run_code(f"open('/dev/shm/dirty','wb').write(b'x' * {DIRTY_MB * 1024 * 1024})")

    t0 = time.monotonic()
    snap_ids = []
    if concurrency == 1:
        snap_ids.append(sandboxes[0].create_snapshot().snapshot_id)
    else:
        with ThreadPoolExecutor(max_workers=concurrency) as pool:
            futures = {pool.submit(sb.create_snapshot): sb for sb in sandboxes}
            for fut in as_completed(futures):
                snap_ids.append(fut.result().snapshot_id)
    wall_ms = (time.monotonic() - t0) * 1000

    for sb in sandboxes:
        sb.kill()
    for sid in snap_ids:
        Sandbox.delete_snapshot(sid)

    return {"wall_ms": wall_ms, "per_ms": wall_ms / concurrency}


print(f"Dirty page per sandbox: {DIRTY_MB} MB\n")
print(f"{'concurrency':>11}  {'rounds':>6}  {'wall_avg':>10}  {'wall_min':>10}  "
      f"{'wall_p50':>10}  {'wall_p80':>10}  {'wall_max':>10}  {'per_avg':>10}")
print("-" * 100)

for c in CONCURRENCY_LEVELS:
    # warm-up
    run_round(c)
    time.sleep(SETTLE_SECS)

    walls, pers = [], []
    for _ in range(ROUNDS):
        r = run_round(c)
        walls.append(r["wall_ms"])
        pers.append(r["per_ms"])
        time.sleep(SETTLE_SECS)

    walls_sorted = sorted(walls)
    p50 = walls_sorted[len(walls_sorted) // 2]
    p80 = walls_sorted[min(int(math.ceil(len(walls_sorted) * 0.8)) - 1, len(walls_sorted) - 1)]

    print(
        f"{c:>11}  {ROUNDS:>6}  "
        f"{statistics.mean(walls):>10.1f}  {min(walls):>10.1f}  "
        f"{p50:>10.1f}  {p80:>10.1f}  {max(walls):>10.1f}  "
        f"{statistics.mean(pers):>10.1f}"
    )
    sys.stdout.flush()
