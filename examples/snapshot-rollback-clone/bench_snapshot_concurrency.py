# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
bench_snapshot_concurrency.py — Snapshot creation latency benchmark (single tier).

Creates `concurrency` sandboxes, triggers create_snapshot() on all of them
concurrently, and reports wall time + per-snapshot amortized time over N rounds.

This script provides the mechanism for ONE concurrency tier per invocation,
mirroring cube-bench. Sweep multiple tiers by invoking it repeatedly, e.g.:

    python bench_snapshot_concurrency.py -c 1
    python bench_snapshot_concurrency.py -c 5
    python bench_snapshot_concurrency.py -c 10
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
                   help="number of concurrent snapshot requests (default: 1)")
    p.add_argument("-n", "--rounds", type=int, default=5,
                   help="measured rounds after warm-up (default: 5)")
    p.add_argument("-s", "--settle-secs", type=float, default=1.0,
                   help="sleep seconds between rounds (default: 1.0)")
    p.add_argument("--no-header", action="store_true",
                   help="suppress the table header (useful when sweeping)")
    return p.parse_args()


def run_round(concurrency: int) -> dict:
    sandboxes = [Sandbox.create(template=TEMPLATE_ID) for _ in range(concurrency)]

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


def percentile(data: list, p: float) -> float:
    s = sorted(data)
    k = int(math.ceil(len(s) * p / 100.0)) - 1
    return s[max(0, min(k, len(s) - 1))]


def main():
    args = parse_args()

    if not args.no_header:
        print(f"{'concurrency':>11}  {'rounds':>6}  {'wall_avg':>10}  {'wall_min':>10}  "
              f"{'wall_p95':>10}  {'wall_max':>10}  {'per_avg':>10}")
        print("-" * 90)

    # warm-up
    run_round(args.concurrency)
    time.sleep(args.settle_secs)

    walls, pers = [], []
    for _ in range(args.rounds):
        r = run_round(args.concurrency)
        walls.append(r["wall_ms"])
        pers.append(r["per_ms"])
        time.sleep(args.settle_secs)

    print(
        f"{args.concurrency:>11}  {args.rounds:>6}  "
        f"{statistics.mean(walls):>10.1f}  {min(walls):>10.1f}  "
        f"{percentile(walls, 95):>10.1f}  {max(walls):>10.1f}  "
        f"{statistics.mean(pers):>10.1f}"
    )
    sys.stdout.flush()


if __name__ == "__main__":
    main()
