# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
bench_create_concurrency.py — Create-sandbox-from-snapshot latency benchmark (single tier).

Creates `concurrency` sandboxes in parallel from one snapshot and reports
wall time + per-sandbox amortized time over N rounds.

This script provides the mechanism for ONE concurrency tier per invocation,
mirroring cube-bench. Sweep multiple tiers by invoking it repeatedly, e.g.:

    python bench_create_concurrency.py -c 1
    python bench_create_concurrency.py -c 10
    python bench_create_concurrency.py -c 20
    python bench_create_concurrency.py -c 50
"""

import argparse
import math
import statistics
import sys
import time
from concurrent.futures import ThreadPoolExecutor, as_completed

from cubesandbox import Sandbox
from env import TEMPLATE_ID


def parse_args():
    p = argparse.ArgumentParser(description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("-c", "--concurrency", type=int, default=1,
                   help="number of concurrent create requests (default: 1)")
    p.add_argument("-n", "--rounds", type=int, default=3,
                   help="measured rounds after warm-up (default: 3)")
    p.add_argument("-s", "--settle-secs", type=float, default=1.0,
                   help="sleep seconds between rounds (default: 1.0)")
    p.add_argument("--no-header", action="store_true",
                   help="suppress the table header (useful when sweeping)")
    return p.parse_args()


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


def percentile(data: list, p: float) -> float:
    s = sorted(data)
    k = int(math.ceil(len(s) * p / 100.0)) - 1
    return s[max(0, min(k, len(s) - 1))]


def main():
    args = parse_args()

    if not args.no_header:
        print(f"{'concurrency':>11}  {'n_total':>7}  {'rounds':>6}  {'wall_avg':>10}  {'wall_min':>10}  "
              f"{'wall_p95':>10}  {'wall_max':>10}  {'per_avg':>10}")
        print("-" * 95)

    snap_id = prepare_snapshot()

    # warm-up: first restore eliminates page-cache cold-miss (~150 ms spike)
    wb = Sandbox.create(template=snap_id)
    wb.kill()
    time.sleep(args.settle_secs)

    walls, pers = [], []
    for _ in range(args.rounds):
        r = run_round(snap_id, args.concurrency)
        walls.append(r["wall_ms"])
        pers.append(r["per_ms"])
        time.sleep(args.settle_secs)

    Sandbox.delete_snapshot(snap_id)

    print(
        f"{args.concurrency:>11}  {args.concurrency:>7}  {args.rounds:>6}  "
        f"{statistics.mean(walls):>10.1f}  {min(walls):>10.1f}  "
        f"{percentile(walls, 95):>10.1f}  {max(walls):>10.1f}  "
        f"{statistics.mean(pers):>10.1f}"
    )
    sys.stdout.flush()


if __name__ == "__main__":
    main()
