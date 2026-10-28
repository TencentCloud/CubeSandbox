#!/usr/bin/env python3
"""Reproduce the API lifecycle cells in jfs-data.md (milliseconds).

Consolidates sec5_bench.py and cross_snapshot_start_bench.py. Each worker
measures its own API and immediate readiness wait; placement checks and
cleanup are outside the measured interval. Failures remain in the raw rows.
"""

from __future__ import annotations

import argparse
from concurrent.futures import ThreadPoolExecutor
from contextlib import nullcontext
import math
import random
import statistics
import time
import uuid

from jfs_bench_common import (
    CONTROL,
    NODE_IPS,
    Sandbox,
    create_on,
    pause_row,
    preflight,
    save_result,
    snap_row,
    target_only,
    wait_ready,
)


SECTION_LABELS = {
    "template": "Template start",
    "snapshot": "Create snapshot",
    "fromsnap-local": "Local snapshot start",
    "fromsnap-cold": "Cross-node cold start",
    "fromsnap-warm": "Cross-node warm start",
    "pause-local": "Local pause and resume",
    "pause-cross": "Cross-node pause and resume",
}

METRIC_LABELS = {
    "create_ms": "Create/start",
    "snapshot_ms": "Snapshot creation",
    "snapshot_share_ms": "Snapshot data ready",
    "snapshot_total_ready_ms": "Snapshot total ready",
    "pause_ms": "Pause",
    "pause_share_ms": "Paused data ready",
    "pause_total_ready_ms": "Pause total ready",
    "resume_ms": "Resume",
}


def print_run_header(args, profile):
    kind = "Local lifecycle" if profile == "local" else "Cross-node lifecycle"
    node_list = ", ".join(f"{name}={ip}" for name, ip in NODE_IPS.items())
    if profile == "local":
        scope = f"{args.origin} ({NODE_IPS[args.origin]})"
    else:
        scope = (
            f"{args.origin} ({NODE_IPS[args.origin]}) → "
            f"{args.target} ({NODE_IPS[args.target]})"
        )
    print(f"\n=== CubeBench JuiceFS · {kind} ===", flush=True)
    print(f"{'Control node:':<16} {CONTROL}", flush=True)
    print(f"{'Cluster nodes:':<16} {node_list}", flush=True)
    print(f"{'Test scope:':<16} {scope}", flush=True)
    print(f"{'Template:':<16} {args.template}", flush=True)
    print(
        f"{'Samples:':<16} {args.samples} | "
        f"Concurrency: {', '.join(map(str, args.concurrency))} | "
        f"Warmup batches: {args.warmups} | Gap: {args.round_gap:g}s",
        flush=True,
    )
    print(f"{'Result file:':<16} {args.out}\n", flush=True)


def print_cell_summary(section, concurrency, summary):
    label = SECTION_LABELS[section]
    if not summary["valid"]:
        print(
            f"FAIL  {label:<26} concurrency {concurrency:>2} | "
            f"samples {summary['n']:>2}/{summary['expected']:<2} | errors {summary['errors']}",
            flush=True,
        )
        return
    print(
        f"PASS  {label:<26} concurrency {concurrency:>2} | "
        f"samples {summary['n']:>2}/{summary['expected']:<2}",
        flush=True,
    )
    for metric, values in summary["metrics"].items():
        print(
            f"      {METRIC_LABELS.get(metric, metric):<26} "
            f"Avg {values['avg']:>9.1f} ms | P95 {values['p95']:>9.1f} ms",
            flush=True,
        )


def parallel(n, fn):
    with ThreadPoolExecutor(max_workers=n) as pool:
        return list(pool.map(fn, range(n)))


def elapsed(start):
    return round((time.monotonic() - start) * 1000, 1)


def summarize(rows, expected):
    errors = [r for r in rows if r.get("error") or r.get("cleanup_errors")]
    valid = len(rows) == expected and not errors
    metrics = {}
    for key in sorted({k for r in rows for k in r if k.endswith("_ms")}):
        values = [
            r[key]
            for r in rows
            if key in r and not r.get("error") and not r.get("cleanup_errors")
        ]
        metrics[key] = {
            "n": len(values),
            "avg": statistics.mean(values)
            if valid and len(values) == expected
            else None,
            "p95": sorted(values)[math.ceil(0.95 * len(values)) - 1]
            if valid and len(values) == expected
            else None,
        }
    return {
        "valid": valid,
        "expected": expected,
        "n": len(rows),
        "errors": len(errors),
        "metrics": metrics,
    }


class Bench:
    def __init__(self, args):
        self.a = args

    def cleanup(self, row, sandboxes=(), snapshots=()):
        # Only resources allocated by this sample are removed.
        for obj in sandboxes:
            if obj is not None:
                try:
                    obj.kill()
                except Exception as exc:
                    row.setdefault("cleanup_errors", []).append(
                        f"sandbox {obj.sandbox_id}: {exc}"
                    )
        for sid in snapshots:
            if sid:
                try:
                    Sandbox.delete_snapshot(sid)
                except Exception as exc:
                    row.setdefault("cleanup_errors", []).append(
                        f"snapshot {sid}: {exc}"
                    )

    def verify(self, sb, node, row):
        # distribution_scope requests the intended Cube scheduler placement.
        # This benchmark intentionally does not inspect remote hosts to verify it.
        del sb, node, row

    def snapshot(self, row):
        sb, sid = None, None
        try:
            sb = create_on(self.a.template, self.a.origin)
            started = time.monotonic()
            info = sb.create_snapshot(name="perf-" + uuid.uuid4().hex[:12])
            row["snapshot_ms"] = elapsed(started)
            sid = info.snapshot_id
            row["snapshot_id"] = sid
            if self.a.backend != "xfs":
                row["snapshot_share_ms"] = wait_ready(lambda: snap_row(sid))
                row["snapshot_total_ready_ms"] = (
                    row["snapshot_ms"] + row["snapshot_share_ms"]
                )
            return sid
        except Exception:
            self.cleanup(row, snapshots=[sid])
            raise
        finally:
            self.cleanup(row, sandboxes=[sb])

    def start(self, source, node, row, key):
        started = time.monotonic()
        sb = Sandbox.create(source, timeout=3600, distribution_scope=[NODE_IPS[node]])
        row[key] = elapsed(started)
        row["_sandbox"] = sb
        row["sandbox_id"] = sb.sandbox_id
        self.verify(sb, node, row)

    def sample(self, section, source=None):
        row, sid = {}, None
        try:
            if section == "template":
                self.start(self.a.template, self.a.origin, row, "create_ms")
            elif section == "snapshot":
                sid = self.snapshot(row)
            elif section == "fromsnap-cold":
                sid = self.snapshot(row)
                self.start(sid, self.a.target, row, "create_ms")
            else:
                node = self.a.origin if section == "fromsnap-local" else self.a.target
                self.start(source, node, row, "create_ms")
        except Exception as exc:
            row["error"] = repr(exc)
        finally:
            row["_snapshot"] = sid
        return row

    def pause_batch(self, concurrency, cross):
        rows = [{} for _ in range(concurrency)]
        sandboxes = [None] * concurrency

        def pause(i):
            row = rows[i]
            try:
                sb = sandboxes[i] = create_on(self.a.template, self.a.origin)
                row["sandbox_id"] = sb.sandbox_id
            except Exception as exc:
                row["error"] = repr(exc)

        # Finish setup of all sandboxes before launching the measured API batch.
        parallel(concurrency, pause)

        def pause_ready(i):
            row, sb = rows[i], sandboxes[i]
            if row.get("error"):
                return
            try:
                started = time.monotonic()
                sb.pause(timeout=600, interval=0.05)
                row["pause_ms"] = elapsed(started)
                if self.a.backend != "xfs":
                    row["pause_share_ms"] = wait_ready(lambda: pause_row(sb.sandbox_id))
                    row["pause_total_ready_ms"] = (
                        row["pause_ms"] + row["pause_share_ms"]
                    )
            except Exception as exc:
                row["error"] = repr(exc)

        def resume(i):
            row, sb = rows[i], sandboxes[i]
            if row.get("error"):
                return
            try:
                started = time.monotonic()
                Sandbox.connect(sb.sandbox_id)
                row["resume_ms"] = elapsed(started)
                self.verify(sb, self.a.target if cross else self.a.origin, row)
            except Exception as exc:
                row["error"] = repr(exc)

        try:
            # Legacy S3 tables used serial pause + parallel resume. Explicitly
            # label that mode; never report those pause times as concurrent.
            if self.a.backend == "s3" and self.a.s3_serial_pause:
                for i in range(concurrency):
                    pause_ready(i)
            else:
                parallel(concurrency, pause_ready)
            with (
                target_only(self.a.target, self.a.allow_node_isolation)
                if cross
                else nullcontext()
            ):
                parallel(concurrency, resume)
        finally:
            for i, sb in enumerate(sandboxes):
                self.cleanup(rows[i], sandboxes=[sb])
        return rows

    def cell(self, section, concurrency, result):
        cell = {
            "rows": [],
            "warmups": [],
            "expected": self.a.samples,
            "s3_serial_pause": self.a.backend == "s3" and self.a.s3_serial_pause,
        }
        result["cells"][f"{section}_c{concurrency}"] = cell
        source, setup = None, {}
        try:
            if section in ("fromsnap-local", "fromsnap-warm"):
                source = self.snapshot(setup)
                cell["setup"] = setup
                if setup.get("cleanup_errors"):
                    raise RuntimeError(f"snapshot setup cleanup failed: {setup}")
            warmups = 0 if section == "fromsnap-cold" else self.a.warmups
            for batch in range(warmups + self.a.samples // concurrency):
                if section.startswith("pause"):
                    rows = self.pause_batch(concurrency, section == "pause-cross")
                elif section == "snapshot":
                    rows = self.snapshot_batch(concurrency)
                else:
                    rows = parallel(concurrency, lambda _: self.sample(section, source))
                # Finish the whole batch before deleting resources; otherwise
                # early workers' cleanup would overlap later workers' API timing.
                for row in rows:
                    self.cleanup(
                        row,
                        sandboxes=[row.pop("_sandbox", None)],
                        snapshots=[row.pop("_snapshot", None)],
                    )
                cell["warmups" if batch < warmups else "rows"].extend(rows)
                save_result(self.a.out, result)
                if any(r.get("error") or r.get("cleanup_errors") for r in rows):
                    raise RuntimeError(
                        "batch failed; partial cell excluded from summary"
                    )
                time.sleep(
                    random.uniform(*self.a.gap_range)
                    if self.a.gap_range
                    else self.a.round_gap
                )
        except Exception as exc:
            cell["error"] = repr(exc)
        finally:
            self.cleanup(cell, snapshots=[source])
            cell["summary"] = summarize(cell["rows"], self.a.samples)
            if cell.get("error") or cell.get("cleanup_errors"):
                cell["summary"]["valid"] = False
                for metric in cell["summary"]["metrics"].values():
                    metric["avg"] = metric["p95"] = None
            save_result(self.a.out, result)
            print_cell_summary(section, concurrency, cell["summary"])
            if cell.get("error"):
                print(f"{section} c={concurrency} error: {cell['error']}", flush=True)
                sample_errors = [r["error"] for r in cell["rows"] if r.get("error")]
                if sample_errors:
                    print(
                        f"{section} c={concurrency} sample errors: "
                        f"{'; '.join(sample_errors)}",
                        flush=True,
                    )
            if cell.get("cleanup_errors"):
                print(
                    f"{section} c={concurrency} cleanup errors: "
                    f"{'; '.join(cell['cleanup_errors'])}",
                    flush=True,
                )

    def snapshot_batch(self, concurrency):
        rows, sandboxes = [{} for _ in range(concurrency)], [None] * concurrency

        def setup(i):
            try:
                sandboxes[i] = create_on(self.a.template, self.a.origin)
            except Exception as exc:
                rows[i]["error"] = repr(exc)

        parallel(concurrency, setup)

        def snapshot(i):
            row, sid = rows[i], None
            try:
                if row.get("error"):
                    return
                started = time.monotonic()
                sid = (
                    sandboxes[i]
                    .create_snapshot(name="perf-" + uuid.uuid4().hex[:12])
                    .snapshot_id
                )
                row["snapshot_ms"] = elapsed(started)
                row["snapshot_id"] = sid
                if self.a.backend != "xfs":
                    row["snapshot_share_ms"] = wait_ready(lambda: snap_row(sid))
                    row["snapshot_total_ready_ms"] = (
                        row["snapshot_ms"] + row["snapshot_share_ms"]
                    )
            except Exception as exc:
                row["error"] = repr(exc)
            finally:
                row["_snapshot"] = sid

        parallel(concurrency, snapshot)
        for i, row in enumerate(rows):
            self.cleanup(
                row, sandboxes=[sandboxes[i]], snapshots=[row.pop("_snapshot", None)]
            )
        return rows


def main(profile="local"):
    parser = argparse.ArgumentParser(
        description=(
            __doc__
            if profile == "local"
            else "Measure JuiceFS lifecycle operations across Cube nodes."
        )
    )
    parser.add_argument("template")
    parser.add_argument("--backend", choices=("jfs",), default="jfs")
    parser.add_argument(
        "--section",
        choices=(
            ("local", "template", "snapshot", "fromsnap-local", "pause-local")
            if profile == "local"
            else ("all", "fromsnap-cold", "fromsnap-warm", "pause-cross")
        ),
        default="local" if profile == "local" else "all",
    )
    parser.add_argument("--origin", default="c2")
    if profile == "cross":
        parser.add_argument("--target", required=True)
    parser.add_argument("--samples", type=int, default=10)
    parser.add_argument("--concurrency", type=int, nargs="+", default=[1, 5, 10])
    parser.add_argument("--warmups", type=int, default=1)
    parser.add_argument("--round-gap", type=float, default=0)
    parser.add_argument(
        "--gap-range",
        type=float,
        nargs=2,
        help="random gap seconds, e.g. 0.5 5.5 for readiness A/B",
    )
    parser.add_argument("--allow-node-isolation", action="store_true")
    parser.add_argument(
        "--s3-serial-pause",
        action="store_true",
        help="legacy S3: serial pause preparation, parallel resume",
    )
    parser.add_argument(
        "--label", default="", help="record deployment/version/object-store description"
    )
    parser.add_argument("--out", required=True)
    a = parser.parse_args()
    if a.samples <= 0 or any(c <= 0 or a.samples % c for c in a.concurrency):
        parser.error("samples must be positive and divisible by every concurrency")
    if (
        a.warmups < 0
        or a.round_gap < 0
        or (a.gap_range and (a.gap_range[0] < 0 or a.gap_range[1] < a.gap_range[0]))
    ):
        parser.error("invalid warmup count or interval")
    if a.section == "pause-cross" and not a.allow_node_isolation:
        parser.error("pause-cross requires --allow-node-isolation")
    if profile == "cross" and a.origin == a.target:
        parser.error("origin and target must differ")
    preflight(a.origin, *([a.target] if profile == "cross" else []))
    sections = [a.section]
    skipped = []
    if profile == "local" and a.section == "local":
        sections = ["template", "snapshot", "fromsnap-local", "pause-local"]
    elif profile == "cross" and a.section == "all":
        sections = ["fromsnap-cold", "fromsnap-warm"]
        if a.allow_node_isolation:
            sections.append("pause-cross")
        else:
            skipped.append("pause-cross: needs --allow-node-isolation")
    result = {
        "suite": "lifecycle",
        "config": vars(a),
        "started": time.strftime("%FT%T%z"),
        "units": "ms",
        "skipped": skipped,
        "cells": {},
    }
    print_run_header(a, profile)
    bench = Bench(a)
    completed = False
    try:
        for section in sections:
            for c in a.concurrency:
                print(
                    f"RUN   {SECTION_LABELS[section]:<26} concurrency {c:>2}",
                    flush=True,
                )
                bench.cell(section, c, result)
                if not result["cells"][f"{section}_c{c}"]["summary"]["valid"]:
                    return 1
        completed = True
    finally:
        result["finished"] = time.strftime("%FT%T%z")
        save_result(a.out, result)
        message = "Completed" if completed else "Stopped; partial result"
        print(f"\n{message}: {a.out}", flush=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
