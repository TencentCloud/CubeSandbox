# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
bench_create_concurrency.py — Create-sandbox-from-snapshot latency vs concurrency benchmark.

For each concurrency level, creates N sandboxes in parallel from the same snapshot
and measures wall time + per-sandbox amortized time.
"""

import math
import statistics
import sys
import time
from concurrent.futures import ThreadPoolExecutor, as_completed

from cubesandbox import Sandbox
from env import TEMPLATE_ID

CONCURRENCY_LEVELS = [1, 10, 20]
ROUNDS = 3
SETTLE_SECS = 1.0


def prepare_snapshot() -> str:
    sb = Sandbox.create(template=TEMPLATE_ID)
    snap = sb.create_snapshot()
    sb.kill()
    return snap.snapshot_id


def run_round(snap_id: str, concurrency: int) -> dict:
    t0 = time.monotonic()
    sandboxes = []
    if concurrency == 1:
        sandboxes.append(Sandbox.create(template=snap_id))
    else:
        with ThreadPoolExecutor(max_workers=concurrency) as pool:
            futures = [pool.submit(Sandbox.create, template=snap_id) for _ in range(concurrency)]
            for fut in as_completed(futures):
                sandboxes.append(fut.result())
    wall_ms = (time.monotonic() - t0) * 1000

    for sb in sandboxes:
        sb.kill()
    return {"wall_ms": wall_ms, "per_ms": wall_ms / concurrency}


print(f"{'concurrency':>11}  {'n_total':>7}  {'rounds':>6}  {'wall_avg':>10}  {'wall_min':>10}  "
      f"{'wall_p50':>10}  {'wall_p80':>10}  {'wall_max':>10}  {'per_avg':>10}")
print("-" * 105)

for c in CONCURRENCY_LEVELS:
    snap_id = prepare_snapshot()

    # warm-up: first restore eliminates page-cache cold-miss (~150 ms spike)
    wb = Sandbox.create(template=snap_id)
    wb.kill()
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
        f"{c:>11}  {c:>7}  {ROUNDS:>6}  "
        f"{statistics.mean(walls):>10.1f}  {min(walls):>10.1f}  "
        f"{p50:>10.1f}  {p80:>10.1f}  {max(walls):>10.1f}  "
        f"{statistics.mean(pers):>10.1f}"
    )
    sys.stdout.flush()
