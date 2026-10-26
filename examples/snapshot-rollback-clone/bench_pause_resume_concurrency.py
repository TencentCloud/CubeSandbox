# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
bench_pause_resume_concurrency.py — Pause / Resume latency benchmark (single tier).

Creates `concurrency` sandboxes, pauses all of them concurrently, then resumes
all of them concurrently. Reports wall time and per-sandbox amortised time for
both operations over N measured rounds.

Current implementation note: CubeSandbox pause uses **full-memory-copy mode**
(copies all anonymous pages). A future soft-dirty / incremental version will
only copy dirty pages since the last checkpoint, which is expected to reduce
pause latency significantly for sandboxes with low write activity.

This script measures ONE concurrency tier per invocation. Sweep multiple tiers
by invoking it repeatedly, e.g.:

    python bench_pause_resume_concurrency.py -c 1  -n 5
    python bench_pause_resume_concurrency.py -c 10 -n 5 --no-header
    python bench_pause_resume_concurrency.py -c 20 -n 5 --no-header
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
    p = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument(
        "-c", "--concurrency", type=int, default=1,
        help="number of concurrent sandboxes (default: 1)",
    )
    p.add_argument(
        "-n", "--rounds", type=int, default=5,
        help="measured rounds after warm-up (default: 5)",
    )
    p.add_argument(
        "-s", "--settle-secs", type=float, default=2.0,
        help="sleep seconds between rounds (default: 2.0)",
    )
    p.add_argument(
        "--no-header", action="store_true",
        help="suppress the table header (useful when sweeping)",
    )
    return p.parse_args()


def percentile(data: list, p: float) -> float:
    s = sorted(data)
    k = int(math.ceil(len(s) * p / 100.0)) - 1
    return s[max(0, min(k, len(s) - 1))]


def _pause_one(sb):
    sb.pause()


def _resume_one(sb):
    sb.resume()


def cleanup_sandboxes(sandboxes):
    """Resume-then-kill all sandboxes, tolerating errors."""
    def _cleanup(sb):
        try:
            # connect() auto-resumes a paused sandbox
            sb2 = Sandbox.connect(sandbox_id=sb.sandbox_id)
            sb2.kill()
        except Exception:
            try:
                sb.kill()
            except Exception:
                pass
    with ThreadPoolExecutor(max_workers=len(sandboxes) or 1) as pool:
        list(pool.map(_cleanup, sandboxes))


def run_round(concurrency: int) -> dict:
    """
    One round:
      1. Create `concurrency` sandboxes
      2. Concurrent pause  → record wall time
      3. Concurrent resume → record wall time
      4. Kill & clean up
    """
    sandboxes = [Sandbox.create(template=TEMPLATE_ID) for _ in range(concurrency)]

    # ── Pause ────────────────────────────────────────────────────────────────
    t0 = time.monotonic()
    if concurrency == 1:
        sandboxes[0].pause()
    else:
        with ThreadPoolExecutor(max_workers=concurrency) as pool:
            futures = [pool.submit(_pause_one, sb) for sb in sandboxes]
            for fut in as_completed(futures):
                fut.result()
    pause_wall_ms = (time.monotonic() - t0) * 1000

    # ── Resume ───────────────────────────────────────────────────────────────
    t1 = time.monotonic()
    if concurrency == 1:
        sandboxes[0].resume()
    else:
        with ThreadPoolExecutor(max_workers=concurrency) as pool:
            futures = [pool.submit(_resume_one, sb) for sb in sandboxes]
            for fut in as_completed(futures):
                fut.result()
    resume_wall_ms = (time.monotonic() - t1) * 1000

    # ── Cleanup ──────────────────────────────────────────────────────────────
    cleanup_sandboxes(sandboxes)

    return {
        "pause_wall_ms":  pause_wall_ms,
        "pause_per_ms":   pause_wall_ms / concurrency,
        "resume_wall_ms": resume_wall_ms,
        "resume_per_ms":  resume_wall_ms / concurrency,
    }


def fmt(v: float) -> str:
    return f"{v:>10.1f}"


def main():
    args = parse_args()

    hdr_fields = [
        f"{'concurrency':>11}", f"{'rounds':>6}",
        f"{'pause_avg':>10}", f"{'pause_min':>10}",
        f"{'pause_p95':>10}", f"{'pause_max':>10}",
        f"{'per_pause':>10}",
        f"{'resume_avg':>11}", f"{'resume_min':>11}",
        f"{'resume_p95':>11}", f"{'resume_max':>11}",
        f"{'per_resume':>11}",
    ]
    if not args.no_header:
        print("  ".join(hdr_fields))
        print("-" * 130)

    # warm-up (result discarded)
    try:
        run_round(args.concurrency)
    except Exception as e:
        print(f"[warn] warm-up failed: {e}, retrying once...", file=sys.stderr)
        time.sleep(3)
        run_round(args.concurrency)
    time.sleep(args.settle_secs)

    pause_walls, resume_walls = [], []
    for i in range(args.rounds):
        r = run_round(args.concurrency)
        pause_walls.append(r["pause_wall_ms"])
        resume_walls.append(r["resume_wall_ms"])
        time.sleep(args.settle_secs)

    print(
        f"{args.concurrency:>11}  {args.rounds:>6}  "
        f"{fmt(statistics.mean(pause_walls))}  {fmt(min(pause_walls))}  "
        f"{fmt(percentile(pause_walls, 95))}  {fmt(max(pause_walls))}  "
        f"{fmt(statistics.mean(pause_walls) / args.concurrency)}  "
        f"{fmt(statistics.mean(resume_walls)):>11}  {fmt(min(resume_walls)):>11}  "
        f"{fmt(percentile(resume_walls, 95)):>11}  {fmt(max(resume_walls)):>11}  "
        f"{fmt(statistics.mean(resume_walls) / args.concurrency):>11}"
    )
    sys.stdout.flush()


if __name__ == "__main__":
    main()
