# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Timing and reporting for the bounded SDK comparison."""
import math
import statistics
import time


def summarize(samples):
    rows = []
    workloads = sorted({s["workload"] for s in samples})
    for workload in workloads:
        for provider in ("go", "rust"):
            selected = [s for s in samples if s["provider"] == provider and s["workload"] == workload and s["phase"] == "measured"]
            latencies = sorted(s["latency_ms"] for s in selected if s["outcome"] == "passed")
            median = statistics.median(latencies) if latencies else None
            rows.append(dict(
                provider=provider, workload=workload, samples=len(selected), successes=len(latencies),
                errors=len(selected) - len(latencies), median_ms=median,
                p95_ms=latencies[math.ceil(.95 * len(latencies)) - 1] if latencies else None,
                pair_medians_ms={str(pair): statistics.median(values) for pair in (1, 2, 3)
                                 if (values := [s["latency_ms"] for s in selected if s["pair"] == pair and s["outcome"] == "passed"])},
                mib_per_second=(4 * 1000 / median) if median and workload.startswith("4194304_") else None,
            ))
    for row in rows:
        if row["provider"] == "rust":
            go = next(r for r in rows if r["provider"] == "go" and r["workload"] == row["workload"])
            if go["median_ms"] and row["median_ms"]:
                row["latency_reduction_percent"] = 100 * (go["median_ms"] - row["median_ms"]) / go["median_ms"]
            if go["mib_per_second"] and row["mib_per_second"]:
                row["throughput_gain_percent"] = 100 * (row["mib_per_second"] - go["mib_per_second"]) / go["mib_per_second"]
    return rows


def measure(samples, operation, verify, **identity):
    start = time.perf_counter_ns()
    try:
        result = operation()
    except Exception as exc:
        elapsed = time.perf_counter_ns() - start
        outcome, error = "error", f"{type(exc).__name__}: {exc}"
    else:
        elapsed = time.perf_counter_ns() - start
        try:
            verify(result)
            outcome, error = "passed", None
        except Exception as exc:
            outcome, error = "error", f"{type(exc).__name__}: {exc}"
    samples.append(dict(identity, latency_ms=elapsed / 1_000_000, outcome=outcome, error=error))
