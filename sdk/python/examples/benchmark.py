# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
benchmark.py — Cube Sandbox 并发创建/删除性能测试

直接调用 CubeAPI HTTP 接口（绕过 SDK 开销），多线程并发测量创建和删除延迟。

Usage:
    python examples/benchmark.py [--concurrency N] [--total N] [--mode MODE]
                                  [--warmup N] [--output FILE] [--dry-run]

Examples:
    python examples/benchmark.py -c 10 -n 100
    python examples/benchmark.py -c 5  -n 50 --mode create-only
    python examples/benchmark.py --dry-run -c 20 -n 200

Environment variables:
    CUBE_TEMPLATE_ID   sandbox 模板 ID
    CUBE_API_URL       CubeAPI 地址，默认 http://127.0.0.1:3000
"""

from __future__ import annotations

import argparse
import json
import math
import os
import random
import statistics
import sys
import threading
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import List, Optional
from concurrent.futures import ThreadPoolExecutor, as_completed

import requests

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

# ── Data types ───────────────────────────────────────────────────────────────

@dataclass
class IterResult:
    seq: int
    create_ms: float = 0.0
    delete_ms: float = 0.0
    error: Optional[str] = None


@dataclass
class BenchState:
    total: int = 0
    results: List[IterResult] = field(default_factory=list)
    _lock: threading.Lock = field(default_factory=threading.Lock, repr=False)
    start_time: float = 0.0

    def add(self, r: IterResult) -> None:
        with self._lock:
            self.results.append(r)

    @property
    def completed(self) -> int:
        return len(self.results)

    @property
    def errors(self) -> int:
        return sum(1 for r in self.results if r.error)

    @property
    def ok_results(self) -> List[IterResult]:
        return [r for r in self.results if r.error is None]

    @property
    def elapsed(self) -> float:
        return time.perf_counter() - self.start_time if self.start_time else 0.0


# ── Stats helpers ─────────────────────────────────────────────────────────────

def pct(data: List[float], p: float) -> float:
    if not data:
        return float("nan")
    s = sorted(data)
    k = min(max(0, int(math.ceil(len(s) * p / 100.0)) - 1), len(s) - 1)
    return s[k]


def sparkline(values: List[float], width: int = 40) -> str:
    chars = "▁▂▃▄▅▆▇█"
    if not values:
        return ""
    if len(values) > width:
        chunk = len(values) / width
        buckets = [statistics.mean(values[int(i * chunk):int((i + 1) * chunk)] or [0])
                   for i in range(width)]
        values = buckets
    lo, hi = min(values), max(values)
    spread = hi - lo if hi > lo else 1.0
    return "".join(chars[min(int((v - lo) / spread * 7), 7)] for v in values)


def grade(p99_ms: float, success_rate: float) -> str:
    if p99_ms <= 100 and success_rate >= 0.999: return "S"
    if p99_ms <= 200 and success_rate >= 0.99:  return "A"
    if p99_ms <= 500 and success_rate >= 0.95:  return "B"
    if p99_ms <= 1000 and success_rate >= 0.90: return "C"
    return "D"


# ── Benchmark core ────────────────────────────────────────────────────────────

def bench_one(
    api_url: str,
    headers: dict,
    payload: dict,
    seq: int,
    mode: str,
    session: requests.Session,
) -> IterResult:
    result = IterResult(seq=seq)
    sandbox_id: Optional[str] = None

    try:
        t0 = time.perf_counter()
        resp = session.post(f"{api_url}/sandboxes", json=payload, headers=headers, timeout=30)
        result.create_ms = (time.perf_counter() - t0) * 1000
        if resp.status_code not in (200, 201):
            result.error = f"create HTTP {resp.status_code}: {resp.text[:120]}"
            return result
        data = resp.json()
        sandbox_id = data.get("sandboxID") or data.get("sandbox_id")
    except Exception as exc:
        result.error = f"create exception: {exc}"
        return result

    if mode == "create-delete" and sandbox_id:
        try:
            t0 = time.perf_counter()
            resp = session.delete(f"{api_url}/sandboxes/{sandbox_id}", headers=headers, timeout=30)
            result.delete_ms = (time.perf_counter() - t0) * 1000
            if resp.status_code not in (200, 204):
                result.error = f"delete HTTP {resp.status_code}: {resp.text[:120]}"
        except Exception as exc:
            result.error = f"delete exception: {exc}"

    return result


def bench_one_dry(seq: int, mode: str, latency: tuple[float, float], error_rate: float) -> IterResult:
    result = IterResult(seq=seq)
    mean, std = latency
    create_ms = max(1.0, random.gauss(mean, std))
    time.sleep(create_ms / 1000.0)
    result.create_ms = create_ms
    if random.random() < error_rate:
        result.error = f"simulated error (seq={seq})"
        return result
    if mode == "create-delete":
        delete_ms = max(1.0, random.gauss(mean * 0.4, std * 0.5))
        time.sleep(delete_ms / 1000.0)
        result.delete_ms = delete_ms
    return result


def run_warmup(api_url: str, headers: dict, payload: dict, rounds: int, mode: str) -> None:
    if rounds <= 0:
        return
    print(f"  Warmup: {rounds} round(s)...")
    s = requests.Session()
    for i in range(rounds):
        try:
            resp = s.post(f"{api_url}/sandboxes", json=payload, headers=headers, timeout=30)
            if resp.status_code in (200, 201):
                sid = resp.json().get("sandboxID") or resp.json().get("sandbox_id")
                if mode == "create-delete" and sid:
                    s.delete(f"{api_url}/sandboxes/{sid}", headers=headers, timeout=30)
            print(f"    warmup [{i+1}/{rounds}] ok")
        except Exception as exc:
            print(f"    warmup [{i+1}/{rounds}] failed: {exc}")
    print()


def run_benchmark(
    api_url: str,
    api_key: str,
    template_id: str,
    concurrency: int,
    total: int,
    warmup: int,
    mode: str,
    dry_run: bool = False,
    dry_latency: tuple[float, float] = (80.0, 30.0),
    dry_error_rate: float = 0.02,
) -> BenchState:
    headers = {"Authorization": f"Bearer {api_key}"}
    payload = {"templateID": template_id}

    state = BenchState(total=total)

    if not dry_run:
        run_warmup(api_url, headers, payload, warmup, mode)

    print(f"  Running {total} iterations (concurrency={concurrency}, mode={mode})...")
    state.start_time = time.perf_counter()

    last_report = [0]

    def progress_printer(r: IterResult) -> None:
        state.add(r)
        done = state.completed
        if done - last_report[0] >= max(1, total // 20):
            elapsed = state.elapsed
            qps = done / elapsed if elapsed > 0 else 0
            print(f"  [{done}/{total}]  errors={state.errors}  qps={qps:.1f}")
            last_report[0] = done

    with ThreadPoolExecutor(max_workers=concurrency) as pool:
        if dry_run:
            futs = [
                pool.submit(bench_one_dry, i + 1, mode, dry_latency, dry_error_rate)
                for i in range(total)
            ]
        else:
            sessions = [requests.Session() for _ in range(concurrency)]
            futs = [
                pool.submit(bench_one, api_url, headers, payload, i + 1, mode, sessions[i % concurrency])
                for i in range(total)
            ]
        for fut in as_completed(futs):
            progress_printer(fut.result())

    return state


# ── Report ────────────────────────────────────────────────────────────────────

def print_report(state: BenchState, mode: str) -> None:
    ok = state.ok_results
    elapsed = state.elapsed
    success_rate = len(ok) / state.total if state.total else 0
    qps = len(ok) / elapsed if elapsed > 0 else 0

    print()
    print("=" * 60)
    print(f"  Total time:    {elapsed:.2f}s")
    print(f"  Success rate:  {success_rate:.1%}  ({len(ok)}/{state.total})")
    print(f"  Throughput:    {qps:.2f} sandboxes/sec")
    print(f"  Errors:        {state.errors}")

    if not ok:
        print("  No successful results.")
        return

    for label, times in [("CREATE", [r.create_ms for r in ok])] + (
        [("DELETE", [r.delete_ms for r in ok])] if mode == "create-delete" else []
    ):
        avg = statistics.mean(times)
        std = statistics.stdev(times) if len(times) > 1 else 0.0
        print()
        print(f"  {label} latency (ms):")
        print(f"    min={min(times):.0f}  avg={avg:.0f}  std={std:.0f}"
              f"  p50={pct(times,50):.0f}  p90={pct(times,90):.0f}"
              f"  p95={pct(times,95):.0f}  p99={pct(times,99):.0f}  max={max(times):.0f}")
        print(f"    {sparkline(times, width=50)}")

    p99_create = pct([r.create_ms for r in ok], 99)
    g = grade(p99_create, success_rate)
    print()
    print(f"  Grade: {g}  (P99={p99_create:.0f}ms, success={success_rate:.1%})")
    print("=" * 60)


def export_json(state: BenchState, filepath: str, args: argparse.Namespace) -> None:
    ok = state.ok_results

    def stat(times: List[float]) -> dict:
        if not times:
            return {}
        return {
            "count": len(times), "min": round(min(times), 2), "max": round(max(times), 2),
            "avg": round(statistics.mean(times), 2),
            "std": round(statistics.stdev(times), 2) if len(times) > 1 else 0,
            "p50": round(pct(times, 50), 2), "p90": round(pct(times, 90), 2),
            "p95": round(pct(times, 95), 2), "p99": round(pct(times, 99), 2),
        }

    report = {
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "config": vars(args),
        "summary": {
            "total_time_s": round(state.elapsed, 3),
            "successful": len(ok), "errors": state.errors,
            "success_rate": round(len(ok) / state.total, 4) if state.total else 0,
            "throughput_qps": round(len(ok) / state.elapsed, 3) if state.elapsed else 0,
        },
        "create": stat([r.create_ms for r in ok]),
        "delete": stat([r.delete_ms for r in ok]) if args.mode == "create-delete" else {},
        "raw": [{"seq": r.seq, "create_ms": round(r.create_ms, 2),
                 "delete_ms": round(r.delete_ms, 2), "error": r.error}
                for r in state.results],
    }
    with open(filepath, "w", encoding="utf-8") as f:
        json.dump(report, f, indent=2, ensure_ascii=False)
    print(f"  Report saved to: {filepath}")


# ── Main ──────────────────────────────────────────────────────────────────────

def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Cube Sandbox Benchmark")
    parser.add_argument("--concurrency", "-c", type=int, default=5, help="并发数（默认 5）")
    parser.add_argument("--total", "-n", type=int, default=20, help="总迭代次数（默认 20）")
    parser.add_argument("--template", "-t", type=str, default=None, help="模板 ID")
    parser.add_argument("--warmup", "-w", type=int, default=0, help="预热轮次（默认 0）")
    parser.add_argument("--mode", "-m", choices=["create-delete", "create-only"],
                        default="create-delete", help="测试模式（默认 create-delete）")
    parser.add_argument("--output", "-o", type=str, default=None, help="导出 JSON 报告路径")
    parser.add_argument("--api-url", type=str, default=None, help="CubeAPI 地址（覆盖 CUBE_API_URL）")
    parser.add_argument("--dry-run", action="store_true", help="模拟模式（无需真实服务）")
    parser.add_argument("--dry-latency", type=str, default="80,30", help="dry-run 延迟 mean,std（ms）")
    parser.add_argument("--dry-error-rate", type=float, default=0.02, help="dry-run 错误率")
    return parser.parse_args()


def main() -> None:
    args = parse_args()

    if args.dry_run:
        template_id = args.template or "dry-run-template"
        api_url = args.api_url or "http://127.0.0.1:3000"
    else:
        template_id = args.template or os.environ.get("CUBE_TEMPLATE_ID", "")
        api_url = (args.api_url or os.environ.get("CUBE_API_URL", "")).rstrip("/")
        if not template_id:
            print("ERROR: CUBE_TEMPLATE_ID not set. Use --template or env var.")
            sys.exit(1)
        if not api_url:
            print("ERROR: CUBE_API_URL not set. Use --api-url or env var.")
            sys.exit(1)

    api_key = "dummy"

    args.template = template_id
    args.api_url = api_url

    try:
        parts = args.dry_latency.split(",")
        dry_latency = (float(parts[0]), float(parts[1]))
    except (IndexError, ValueError):
        dry_latency = (80.0, 30.0)

    print(f"Cube Sandbox Benchmark")
    print(f"  template={template_id}  url={api_url}")
    print(f"  concurrency={args.concurrency}  total={args.total}  mode={args.mode}")
    if args.dry_run:
        print(f"  [DRY-RUN] latency=N({dry_latency[0]:.0f},{dry_latency[1]:.0f})ms"
              f"  error_rate={args.dry_error_rate:.0%}")
    print()

    state = run_benchmark(
        api_url=api_url,
        api_key=api_key,
        template_id=template_id,
        concurrency=args.concurrency,
        total=args.total,
        warmup=args.warmup,
        mode=args.mode,
        dry_run=args.dry_run,
        dry_latency=dry_latency,
        dry_error_rate=args.dry_error_rate,
    )

    print_report(state, args.mode)

    if args.output:
        export_json(state, args.output, args)

    if state.errors > 0 and not args.dry_run:
        sys.exit(1)


if __name__ == "__main__":
    main()
