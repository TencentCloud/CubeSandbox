# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
bench_rollback_concurrency.py — Rollback latency benchmark (single tier).

Creates `concurrency` sandboxes. Each sandbox takes its OWN checkpoint
(create_snapshot), then rolls back to its own checkpoint concurrently.
Reports the wall time of the concurrent rollback batch and the
per-rollback amortized time over N rounds. Optionally dirty `-d` MB of
guest memory before each rollback (default 0 = pure rollback).

Note: CubeSandbox enforces snapshot ownership — a sandbox may only roll back to
a checkpoint it created itself. Each sandbox therefore snapshots itself rather
than sharing one snapshot across the batch.

This script provides the mechanism for ONE concurrency tier per invocation,
mirroring cube-bench. Sweep multiple tiers by invoking it repeatedly, e.g.:

    python bench_rollback_concurrency.py -c 1
    python bench_rollback_concurrency.py -c 5
    python bench_rollback_concurrency.py -c 10
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
                   help="number of concurrent rollback requests (default: 1)")
    p.add_argument("-n", "--rounds", type=int, default=5,
                   help="measured rounds after warm-up (default: 5)")
    p.add_argument("-d", "--dirty-mb", type=int, default=0,
                   help="dirty-page size in MB written before rollback (default: 0, no dirty write)")
    p.add_argument("-s", "--settle-secs", type=float, default=1.0,
                   help="sleep seconds between rounds (default: 1.0)")
    p.add_argument("--no-header", action="store_true",
                   help="suppress the table header (useful when sweeping)")
    return p.parse_args()


def rollback_one(sb, checkpoint_id: str, dirty_mb: int):
    """Dirty the sandbox, then roll it back to its own checkpoint."""
    if dirty_mb > 0:
        sb.run_code(f"open('/dev/shm/dirty','wb').write(b'x' * {dirty_mb * 1024 * 1024})")
    sb.rollback(checkpoint_id)


def run_round(concurrency: int, dirty_mb: int) -> dict:
    # Each sandbox takes its own checkpoint (ownership constraint).
    sandboxes = [Sandbox.create(template=TEMPLATE_ID) for _ in range(concurrency)]
    checkpoints = [sb.create_snapshot().snapshot_id for sb in sandboxes]

    t0 = time.monotonic()
    if concurrency == 1:
        rollback_one(sandboxes[0], checkpoints[0], dirty_mb)
    else:
        with ThreadPoolExecutor(max_workers=concurrency) as pool:
            futures = [
                pool.submit(rollback_one, sb, cp, dirty_mb)
                for sb, cp in zip(sandboxes, checkpoints)
            ]
            for fut in as_completed(futures):
                fut.result()
    wall_ms = (time.monotonic() - t0) * 1000

    for sb in sandboxes:
        sb.kill()
    for cp in checkpoints:
        Sandbox.delete_snapshot(cp)
    return {"wall_ms": wall_ms, "per_ms": wall_ms / concurrency}


def percentile(data: list, p: float) -> float:
    s = sorted(data)
    k = int(math.ceil(len(s) * p / 100.0)) - 1
    return s[max(0, min(k, len(s) - 1))]


def main():
    args = parse_args()

    if not args.no_header:
        print(f"Dirty page per sandbox: {args.dirty_mb} MB\n")
        print(f"{'concurrency':>11}  {'rounds':>6}  {'wall_avg':>10}  {'wall_min':>10}  "
              f"{'wall_p95':>10}  {'wall_max':>10}  {'per_avg':>10}")
        print("-" * 85)

    # warm-up
    run_round(args.concurrency, args.dirty_mb)
    time.sleep(args.settle_secs)

    walls, pers = [], []
    for _ in range(args.rounds):
        r = run_round(args.concurrency, args.dirty_mb)
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
