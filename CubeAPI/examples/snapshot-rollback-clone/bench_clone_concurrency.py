# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
bench_clone_concurrency.py — Clone latency vs concurrency benchmark.

Clones N sandboxes from a running source sandbox concurrently,
measuring wall time and per-clone amortized time.
"""

import math
import statistics
import sys
import time
from concurrent.futures import ThreadPoolExecutor, as_completed

from cubesandbox import Sandbox
from env import TEMPLATE_ID

# (n_clones, concurrency, rounds) pairs matching the blog post
SCENARIOS = [
    (1,   1,  5),
    (100, 10, 2),
]
DIRTY_MB = 10
SETTLE_SECS = 1.0


def run_round(n: int, concurrency: int) -> dict:
    src = Sandbox.create(template=TEMPLATE_ID)
    clones = []
    try:
        if DIRTY_MB > 0:
            src.run_code(f"open('/dev/shm/dirty','wb').write(b'x' * {DIRTY_MB * 1024 * 1024})")

        t0 = time.monotonic()
        clones = src.clone(n=n, concurrency=concurrency)
        wall_ms = (time.monotonic() - t0) * 1000
    finally:
        try:
            src.kill()
        except Exception:
            pass
        for sb in clones:
            try:
                sb.kill()
            except Exception:
                pass
    return {"wall_ms": wall_ms, "per_ms": wall_ms / n}


print(f"Dirty page per source sandbox: {DIRTY_MB} MB\n")
print(f"{'scenario':>22}  {'n':>4}  {'conc':>4}  {'rounds':>6}  "
      f"{'wall_avg':>10}  {'wall_min':>10}  {'wall_p50':>10}  "
      f"{'wall_p80':>10}  {'wall_max':>10}  {'per_avg':>10}")
print("-" * 120)

for n, c, rounds in SCENARIOS:
    label = f"{n} sandbox {'×' if n > 1 else ' '}{c}-conc"

    # warm-up
    run_round(n, c)
    time.sleep(SETTLE_SECS)

    walls, pers = [], []
    for _ in range(rounds):
        r = run_round(n, c)
        walls.append(r["wall_ms"])
        pers.append(r["per_ms"])
        time.sleep(SETTLE_SECS)

    walls_sorted = sorted(walls)
    p50 = walls_sorted[len(walls_sorted) // 2]
    p80 = walls_sorted[min(int(math.ceil(len(walls_sorted) * 0.8)) - 1, len(walls_sorted) - 1)]

    print(
        f"{label:>22}  {n:>4}  {c:>4}  {rounds:>6}  "
        f"{statistics.mean(walls):>10.1f}  {min(walls):>10.1f}  "
        f"{p50:>10.1f}  {p80:>10.1f}  {max(walls):>10.1f}  "
        f"{statistics.mean(pers):>10.1f}"
    )
    sys.stdout.flush()
