# SPDX-License-Identifier: Apache-2.0
"""Collect per-call SDK latencies for Cubebench snapshot create and pause/resume."""
import argparse
import json
import math
import os
import signal
import statistics
import sys
import tempfile
import time
import traceback
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager
from itertools import chain
from pathlib import Path


def parse_args(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("section", choices=("4.3", "4.6"))
    parser.add_argument("-c", "--concurrency", type=int, default=1)
    parser.add_argument("-n", "--rounds", type=int)
    parser.add_argument("--requests", type=int, default=500, help="requests per measured round")
    parser.add_argument("-s", "--settle-secs", type=float)
    parser.add_argument("--no-warmup", action="store_true")
    parser.add_argument("--snapshot", default=os.environ.get("CUBE_SNAPSHOT_ID"))
    parser.add_argument("--output", type=Path, required=True, help="raw samples and round statistics")
    args = parser.parse_args(argv)
    if args.rounds is None:
        args.rounds = 3 if args.section == "4.3" else 5
    if args.settle_secs is None:
        args.settle_secs = 1.0 if args.section == "4.3" else 2.0
    if not 1 <= args.concurrency <= args.requests or args.rounds < 1:
        parser.error("requests must be at least concurrency and rounds must be positive")
    if not math.isfinite(args.settle_secs) or args.settle_secs < 0:
        parser.error("settle time must be finite and nonnegative")
    if args.section == "4.3":
        args.source = args.snapshot
        if not args.source:
            parser.error("set CUBE_SNAPSHOT_ID or pass --snapshot")
    else:
        args.source = os.environ.get("CUBE_TEMPLATE_ID")
        if not args.source:
            parser.error("set CUBE_TEMPLATE_ID")
    return args


def save_report(path, report):
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(dir=path.parent, mode="w", encoding="utf-8",
                                         suffix=".tmp", delete=False) as stream:
            temporary = Path(stream.name)
            json.dump(report, stream, indent=2, allow_nan=False)
            stream.write("\n")
        os.replace(temporary, path)
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)


def summarize(rows, wall_ms, padding_count=0, padding_errors=()):
    """Use retained samples for latency and all issued calls for counts and throughput."""
    latencies = sorted(row["latency_ms"] for row in rows if row["error"] is None)
    n = len(latencies)
    skipped = sum(row.get("skipped", False) for row in chain(rows, padding_errors))
    issued = len(rows) + padding_count - skipped
    successful = n + padding_count - len(padding_errors)
    return dict(requests=issued, successful=successful, errors=issued - successful,
                skipped=skipped, latency_samples=n,
                avg=statistics.mean(latencies) if n else None,
                min=latencies[0] if n else None, max=latencies[-1] if n else None,
                p95=latencies[math.ceil(n * .95) - 1] if n else None,
                p99=latencies[math.ceil(n * .99) - 1] if n else None,
                wall_ms=wall_ms, amortized_ms=wall_ms / successful if successful else None,
                throughput_qps=successful * 1000 / wall_ms if wall_ms else None)


def terminate(signum, frame):
    raise SystemExit(128 + signum)


@contextmanager
def defer_interrupts():
    """Finish draining worker threads and owned sandboxes before propagating a stop."""
    pending, handlers = [], {}
    for signum in (signal.SIGINT, signal.SIGTERM):
        if signal.getsignal(signum) != signal.SIG_IGN:
            handlers[signum] = signal.signal(signum, lambda sig, frame: pending.append(sig))
    try:
        yield pending
    finally:
        for signum, handler in handlers.items():
            signal.signal(signum, handler)


def propagate_interrupt(pending):
    if pending:
        if pending[0] == signal.SIGINT:
            raise KeyboardInterrupt()
        raise SystemExit(128 + pending[0])


def measure(calls, batch, name, created=None):
    """Time only the SDK call. Keep finished rows and owned objects on interruption."""
    rows = [None] * len(calls)

    def one(index):
        if calls[index] is None:
            rows[index] = dict(seq=index + 1, latency_ms=None, skipped=True,
                               error="resume skipped because pause failed")
            return
        start = time.perf_counter()
        try:
            value = calls[index]()
        except Exception as exc:
            latency_ms = (time.perf_counter() - start) * 1000
            row = dict(seq=index + 1, latency_ms=latency_ms, error=str(exc))
        else:
            latency_ms = (time.perf_counter() - start) * 1000
            row = dict(seq=index + 1, latency_ms=latency_ms, error=None)
            if created is not None:
                created[index] = value
        rows[index] = row

    start = time.perf_counter()
    pool = ThreadPoolExecutor(max_workers=len(calls))
    try:
        list(pool.map(one, range(len(calls))))
    finally:
        with defer_interrupts() as pending:
            pool.shutdown(wait=True, cancel_futures=True)
            wall_ms = (time.perf_counter() - start) * 1000
            batch[name] = dict(raw=[row for row in rows if row is not None], wall_ms=wall_ms)
            if created is not None:
                for row in batch[name]["raw"]:
                    if row["error"] is None:
                        try:
                            sandbox_id = created[row["seq"] - 1].sandbox_id
                            if (not isinstance(sandbox_id, str) or not sandbox_id
                                    or any(c.isspace() for c in sandbox_id)):
                                raise ValueError("successful create has no valid sandbox ID")
                            row["sandbox_id"] = sandbox_id
                        except Exception as exc:
                            row["error"] = str(exc)
    propagate_interrupt(pending)


def run_batch(section, count, create, source, batch, cleanup_errors):
    sandboxes = [None] * count
    try:
        if section == "4.3":
            measure([lambda: create(template=source)] * count, batch, "create", sandboxes)
        else:
            for i in range(count):
                sandboxes[i] = create(template=source)
            measure([sb.pause for sb in sandboxes], batch, "pause")
            resume_calls = [sb.resume if row["error"] is None else None
                            for sb, row in zip(sandboxes, batch["pause"]["raw"])]
            measure(resume_calls, batch, "resume")
    finally:
        with defer_interrupts() as pending:
            for sandbox in sandboxes:
                if sandbox is not None:
                    try:
                        sandbox.kill()
                    except Exception as exc:
                        cleanup_errors.append(str(exc))
    propagate_interrupt(pending)
    if cleanup_errors:
        raise RuntimeError("cleanup failed: " + "; ".join(cleanup_errors))


def run(args, create):
    snapshot = args.section == "4.3"
    names = ("create",) if snapshot else ("pause", "resume")
    groups = {name: dict(raw=[], rounds=[], padding_count=0, padding_errors=[]) for name in names}
    walls = dict.fromkeys(names, 0.0)
    config = dict(concurrency=args.concurrency, requests_per_round=args.requests, rounds=args.rounds,
                  warmup=not args.no_warmup, percentile_method="nearest-rank",
                  latency_scope="single SDK call; queue/preparation/cleanup excluded",
                  wall_scope="sum of operation batch windows; preparation/cleanup excluded",
                  final_wave="padded")
    if snapshot:
        config.update(snapshot_id=args.source, sample_identity="sandbox_id",
                      latency_scope="single SDK call; executor queue/preparation/cleanup excluded",
                      wall_scope="sum of create batch windows; preparation/cleanup excluded")
    else:
        config["template_id"] = args.source
    progress = dict(round=0, offset=0, completed_rounds=0)
    report = dict(config=config, failure=None, cleanup_errors=[], warmup_diagnostic=None, progress=progress)
    if not snapshot:
        report["operations"] = groups

    def checkpoint(status):
        for name, group in groups.items():
            group["summary"] = summarize(group["raw"], walls[name],
                                         group["padding_count"], group["padding_errors"])
        if snapshot:
            report.update(groups["create"])
        report.update(status=status, valid=(status == "complete"))
        save_report(args.output, report)

    checkpoint("running")
    stage = "warmup"
    try:
        if not args.no_warmup:
            warmup = {}
            try:
                run_batch(args.section, args.concurrency, create, args.source, warmup, report["cleanup_errors"])
            finally:
                report["warmup_diagnostic"] = warmup.get("create") if snapshot else warmup
            if any(row["error"] is not None for operation in warmup.values() for row in operation["raw"]):
                raise RuntimeError("warmup operation failed")
            checkpoint("running")
            time.sleep(args.settle_secs)
        stage = "measurement"
        for number in range(1, args.rounds + 1):
            round_walls = dict.fromkeys(names, 0.0)
            for offset in range(0, args.requests, args.concurrency):
                progress.update(round=number, offset=offset)
                batch = {}
                try:
                    run_batch(args.section, args.concurrency, create, args.source, batch,
                              report["cleanup_errors"])
                finally:
                    for name, value in batch.items():
                        round_walls[name] += value["wall_ms"]
                        walls[name] += value["wall_ms"]
                        for row in value["raw"]:
                            sample = dict(row, seq=offset + row["seq"], round=number)
                            if sample["seq"] <= args.requests:
                                groups[name]["raw"].append(sample)
                            else:
                                groups[name]["padding_count"] += 1
                                if sample["error"] is None:
                                    continue
                                del sample["latency_ms"]
                                groups[name]["padding_errors"].append(sample)
                checkpoint("running")
            for name, group in groups.items():
                padding_errors = [row for row in group["padding_errors"] if row["round"] == number]
                padding_count = (-args.requests) % args.concurrency
                group["rounds"].append(summarize(group["raw"][-args.requests:], round_walls[name],
                                                 padding_count, padding_errors))
            progress.update(offset=args.requests, completed_rounds=number)
            checkpoint("running")
            time.sleep(args.settle_secs)
    except BaseException as exc:
        report["failure"] = dict(stage=stage, type=type(exc).__name__, message=str(exc),
                                 exception=repr(exc), traceback=traceback.format_exc())
        try:
            checkpoint("failed")
        except OSError as write_error:
            print(f"failed to save original failure: {write_error}", file=sys.stderr)
        raise

    errors = sum(group["summary"]["errors"] for group in groups.values())
    if errors:
        report["failure"] = dict(stage=stage, type="OperationErrors", message=f"{errors} operation(s) failed")
    checkpoint("failed" if errors else "complete")
    return int(bool(errors))


if __name__ == "__main__":
    args = parse_args()
    from cubesandbox import Sandbox

    signal.signal(signal.SIGTERM, terminate)
    sys.exit(run(args, Sandbox.create))
