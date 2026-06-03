# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
bench_clone_concurrency.py — Clone latency benchmark (single scenario).

Clones `n` sandboxes from one running source sandbox at the given `concurrency`,
reporting wall time and per-clone amortized time over `rounds` measured rounds.

This script provides the mechanism for ONE scenario per invocation, mirroring
cube-bench. Sweep multiple scenarios by invoking it repeatedly, e.g.:

    python bench_clone_concurrency.py -n 1   -c 1  --rounds 5
    python bench_clone_concurrency.py -n 100 -c 10 --rounds 2
    python bench_clone_concurrency.py -n 100 -c 20 --rounds 2
    python bench_clone_concurrency.py -n 100 -c 50 --rounds 2
"""

import argparse
import math
import statistics
import sys
import time

from cubesandbox import Sandbox
from env import TEMPLATE_ID


def parse_args():
    p = argparse.ArgumentParser(description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("-n", "--num", type=int, default=1,
                   help="total number of clones to create (default: 1)")
    p.add_argument("-c", "--concurrency", type=int, default=1,
                   help="max parallel clone requests (default: 1)")
    p.add_argument("--rounds", type=int, default=5,
                   help="measured rounds after warm-up (default: 5)")
    p.add_argument("-d", "--dirty-mb", type=int, default=0,
                   help="dirty-page size in MB written on the source sandbox before clone (default: 0, no dirty write)")
    p.add_argument("-s", "--settle-secs", type=float, default=1.0,
                   help="sleep seconds between rounds (default: 1.0)")
    p.add_argument("--no-header", action="store_true",
                   help="suppress the table header (useful when sweeping)")
    return p.parse_args()


def run_round(n: int, concurrency: int, dirty_mb: int) -> dict:
    src = Sandbox.create(template=TEMPLATE_ID)
    clones = []
    try:
        if dirty_mb > 0:
            src.run_code(f"open('/dev/shm/dirty','wb').write(b'x' * {dirty_mb * 1024 * 1024})")

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


def percentile(data: list, p: float) -> float:
    s = sorted(data)
    k = int(math.ceil(len(s) * p / 100.0)) - 1
    return s[max(0, min(k, len(s) - 1))]


def main():
    args = parse_args()

    if not args.no_header:
        print(f"Dirty page per source sandbox: {args.dirty_mb} MB\n")
        print(f"{'n':>4}  {'conc':>4}  {'rounds':>6}  "
              f"{'wall_avg':>10}  {'wall_min':>10}  "
              f"{'wall_p95':>10}  {'wall_max':>10}  {'per_avg':>10}")
        print("-" * 80)

    # warm-up
    run_round(args.num, args.concurrency, args.dirty_mb)
    time.sleep(args.settle_secs)

    walls, pers = [], []
    for _ in range(args.rounds):
        r = run_round(args.num, args.concurrency, args.dirty_mb)
        walls.append(r["wall_ms"])
        pers.append(r["per_ms"])
        time.sleep(args.settle_secs)

    print(
        f"{args.num:>4}  {args.concurrency:>4}  {args.rounds:>6}  "
        f"{statistics.mean(walls):>10.1f}  {min(walls):>10.1f}  "
        f"{percentile(walls, 95):>10.1f}  {max(walls):>10.1f}  "
        f"{statistics.mean(pers):>10.1f}"
    )
    sys.stdout.flush()


if __name__ == "__main__":
    main()
