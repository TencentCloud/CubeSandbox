# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
bench_rollback_concurrency.py — Rollback latency vs concurrency benchmark.

Creates N sandboxes from the same snapshot, then triggers rollback on all of them
concurrently, measuring wall time and per-rollback amortized time.
"""

import math
import statistics
import sys
import time
from concurrent.futures import ThreadPoolExecutor, as_completed

from cubesandbox import Sandbox
from env import TEMPLATE_ID

CONCURRENCY_LEVELS = [1, 10]
ROUNDS = 5
SETTLE_SECS = 1.0


def prepare_snapshot() -> str:
    sb = Sandbox.create(template=TEMPLATE_ID)
    snap = sb.create_snapshot()
    sb.kill()
    return snap.snapshot_id


def run_round(snap_id: str, concurrency: int) -> dict:
    sandboxes = [Sandbox.create(template=snap_id) for _ in range(concurrency)]
    # Make each sandbox dirty so rollback has real work to do
    for sb in sandboxes:
        sb.run_code("open('/dev/shm/dirty','wb').write(b'x' * 10 * 1024 * 1024)")

    t0 = time.monotonic()
    if concurrency == 1:
        sandboxes[0].rollback(snap_id)
    else:
        with ThreadPoolExecutor(max_workers=concurrency) as pool:
            futures = [pool.submit(sb.rollback, snap_id) for sb in sandboxes]
            for fut in as_completed(futures):
                fut.result()
    wall_ms = (time.monotonic() - t0) * 1000

    for sb in sandboxes:
        sb.kill()
    return {"wall_ms": wall_ms, "per_ms": wall_ms / concurrency}


print(f"{'concurrency':>11}  {'rounds':>6}  {'wall_avg':>10}  {'wall_min':>10}  "
      f"{'wall_p50':>10}  {'wall_p80':>10}  {'wall_max':>10}  {'per_avg':>10}")
print("-" * 95)

for c in CONCURRENCY_LEVELS:
    snap_id = prepare_snapshot()

    # warm-up
    run_round(snap_id, c)
    time.sleep(SETTLE_SECS)

    walls, pers = [], []
    for _ in range(ROUNDS):
        r = run_round(snap_id, c)
        walls.append(r["wall_ms"])
        pers.append(r["per_ms"])
        time.sleep(SETTLE_SECS)

    Sandbox.delete_snapshot(snap_id)

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
