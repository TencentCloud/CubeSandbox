#!/usr/bin/env python3
"""Plan a Cubebench concurrency search from completed measurements.

Feed a JSON array on stdin (use [] to start). Each entry has concurrency,
requests, errors and p95_ms. Supply independent rounds and include each
measurement only once. Pass previous campaigns separately with --history.
Use create P95 for 3.2/4.3 and resume P95 for 4.6. Keep measurements for
each scenario and configuration separate.

The JSON output gives the next concurrency to run with 500 requests, or null
when the search stops, together with the verdict for the latency target.
Confirm each side of the boundary using the median P95 of three fresh rounds.
"""

import argparse
import csv
import json
import math
import os
from pathlib import Path
import signal
import stat
from statistics import median
import subprocess
import sys
import tempfile
import time


REQUESTS = 500
START = 16
LIMIT_MS = 100
CONFIRMATIONS = 3
CASE_BUDGET = 48
SECTIONS = ("3.2", "4.3", "4.6")
BENCH = Path(__file__).resolve().with_name("cubebench.sh")


def collect(measurements):
    if not isinstance(measurements, list):
        raise ValueError("expected a JSON array of measurements")

    readings = {}
    operation_error = False
    for row in measurements:
        concurrency, requests, errors = (row[key] for key in ("concurrency", "requests", "errors"))
        if type(concurrency) is not int or not 1 <= concurrency <= REQUESTS:
            raise ValueError("concurrency must be an integer from 1 to 500")
        if type(requests) is not int or requests != REQUESTS:
            raise ValueError("each measurement must account for 500 requests")
        if type(errors) is not int or not 0 <= errors <= requests:
            raise ValueError("invalid operation error count")
        if errors:
            operation_error = True
            continue
        p95 = row["p95_ms"]
        if type(p95) not in (int, float) or not math.isfinite(p95) or p95 < 0:
            raise ValueError("p95_ms must be finite and nonnegative")
        readings.setdefault(concurrency, []).append(p95)

    return readings, operation_error


def prior_curve(readings):
    """Pool adjacent decreasing medians to fit a nondecreasing latency curve."""
    blocks = []
    for c in sorted(readings):
        blocks.append(([c], median(readings[c])))
        while len(blocks) > 1 and blocks[-2][1] > blocks[-1][1]:
            right, right_mean = blocks.pop()
            left, left_mean = blocks.pop()
            levels = left + right
            mean = left_mean + (right_mean - left_mean) * (len(right) / len(levels))
            blocks.append((levels, mean))
    return [(c, mean) for levels, mean in blocks for c in levels]


def crossing(points, target):
    """Interpolate between surrounding levels or extrapolate within the concurrency limits."""
    if not points:
        return None
    above = next((i for i, (_, y) in enumerate(points) if y > target), len(points))
    if len(points) == 1:
        return points[0][0]
    right = min(max(1, above), len(points) - 1)
    x1, y1 = points[right - 1]
    x2, y2 = points[right]
    if y2 <= y1:
        return points[min(above, len(points) - 1)][0]
    fraction = (target - y1) / (y2 - y1)
    estimate = x1 + (x2 - x1) * fraction
    return math.floor(min(REQUESTS, max(1, estimate)))


def next_level(latencies, lower, upper, history, measurements):
    if lower is not None and upper is not None:
        lo, hi = latencies[lower], latencies[upper]
        proposal = crossing([(lower, lo), (upper, hi)], LIMIT_MS)
        # Safeguard regula falsi: bisect if the last probe barely reduced the bracket.
        last = measurements[-1]["concurrency"]
        other = [c for c in latencies if c != last]
        old_lower = max((c for c in other if latencies[c] <= LIMIT_MS), default=None)
        old_upper = min((c for c in other if latencies[c] > LIMIT_MS), default=None)
        if old_lower is not None and old_upper is not None:
            if (upper - lower) * 4 > (old_upper - old_lower) * 3:
                proposal = (lower + upper) // 2
        return min(upper - 1, max(lower + 1, proposal)), "refine"

    curve = prior_curve(history)
    if not latencies:
        estimate = crossing(curve, LIMIT_MS)
        if estimate is None:
            return START, "coarse"
        return min(2 * curve[-1][0], max(curve[0][0] // 2, estimate)), "prediction"

    # Translate the historical curve vertically at the current live boundary.
    # Between measured historical levels, estimate the old latency by interpolation.
    anchor = lower if lower is not None else upper
    estimate = None
    if len(curve) >= 2 and curve[0][0] <= anchor <= curve[-1][0]:
        right = next(i for i, (x, _) in enumerate(curve) if x >= anchor)
        x2, y2 = curve[right]
        old_y = y2
        if x2 != anchor:
            x1, y1 = curve[right - 1]
            old_y = y1 + (y2 - y1) * ((anchor - x1) / (x2 - x1))
        estimate = crossing(curve, LIMIT_MS - (latencies[anchor] - old_y))
    # Once two live levels are available, use the local live slope.
    if len(latencies) >= 2:
        levels = sorted(latencies)
        x1, x2 = levels[-2:] if lower is not None else levels[:2]
        y1, y2 = latencies[x1], latencies[x2]
        if y2 > y1:
            estimate = crossing([(x1, y1), (x2, y2)], LIMIT_MS)
    if lower is not None:
        if estimate is not None and estimate >= lower and len(latencies) == 1:
            return min(REQUESTS, 2 * lower, max(lower + 1, estimate)), "prediction"
        if estimate is not None and estimate > lower + 1:
            return min(REQUESTS, 2 * lower, estimate), "prediction"
        return min(REQUESTS, 2 * lower), "coarse"
    if estimate is not None and estimate <= upper and len(latencies) == 1:
        return max(1, upper // 2, min(upper - 1, estimate)), "prediction"
    if estimate is not None and estimate < upper - 1:
        return max(1, upper // 2, estimate), "prediction"
    return max(1, upper // 2), "coarse"


def plan(measurements, history=None):
    readings, operation_error = collect(measurements)
    priors, _ = collect([] if history is None else history)
    # Keep the exploratory reading until confirmation finishes. Reclassifying
    # after just two samples can skip the third round when the second is noisy.
    latencies = {c: median(rows) if len(rows) >= CONFIRMATIONS else rows[0]
                 for c, rows in readings.items()}
    lower = max((c for c, ms in latencies.items() if ms <= LIMIT_MS), default=None)
    upper = min((c for c, ms in latencies.items() if ms > LIMIT_MS), default=None)

    outcome = None
    endpoints = ()
    if lower is not None and upper is not None and upper <= lower + 1:
        endpoints = (lower, upper)
        outcome = "confirmed_boundary" if upper == lower + 1 else "unstable_or_non_monotonic"
    elif upper == 1:
        endpoints = (upper,)
        outcome = "lowest_concurrency_exceeded_limit"
    elif lower == REQUESTS:
        endpoints = (lower,)
        outcome = "lower_bound_at_cap"

    pending = next((c for c in endpoints if len(readings[c]) < CONFIRMATIONS), None)
    candidate = None
    if operation_error:
        status = "operation_error"
    elif outcome is not None and pending is None:
        status = outcome
    elif len(measurements) >= CASE_BUDGET:
        status = "case_budget_exhausted"
    else:
        status = "searching"
        if pending is not None:
            concurrency, phase = pending, "confirmation"
        else:
            concurrency, phase = next_level(latencies, lower, upper, priors, measurements)
        candidate = dict(concurrency=concurrency, phase=phase, p95_limit_ms=LIMIT_MS, requests=REQUESTS)
    result = dict(status=status, p95_limit_ms=LIMIT_MS, passing_concurrency=lower,
                  failing_concurrency=upper, passing_p95_ms=readings.get(lower, []),
                  failing_p95_ms=readings.get(upper, []),
                  confirmed_concurrency=lower if status in ("confirmed_boundary", "lower_bound_at_cap") else None)
    return {"next_case": candidate, "results": [result]}


def select_sections(args):
    selected = set()
    for arg in args or ["all"]:
        for token in arg.replace(",", " ").split():
            matches = [s for s in SECTIONS if token in ("all", s, s.split(".")[0])]
            if not matches:
                raise ValueError(f"unsupported search section {token!r} (choose 3.2, 4.3 or 4.6)")
            selected.update(matches)
    if not selected:
        raise ValueError("empty section selector")
    return [s for s in SECTIONS if s in selected]


def write_json(path, value):
    temp = path.with_suffix(".tmp")
    temp.write_text(json.dumps(value, indent=2, allow_nan=False) + "\n", encoding="utf-8")
    temp.replace(path)


def check_samples(raw, key, reported_p95):
    if len(raw) != REQUESTS or sorted(row["seq"] for row in raw) != list(range(1, REQUESTS + 1)):
        raise ValueError(f"expected {REQUESTS} distinct samples")
    latencies = []
    for row in raw:
        value = row[key]
        if row.get("error") or type(value) not in (int, float) or not math.isfinite(value) or value < 0:
            raise ValueError("raw sample has an error or an invalid latency")
        latencies.append(value)
    p95 = sorted(latencies)[math.ceil(0.95 * REQUESTS) - 1]
    if not math.isclose(p95, reported_p95, rel_tol=1e-9, abs_tol=1e-6):
        raise ValueError("reported P95 disagrees with the raw samples")
    return p95


def read_measurement(path, section, concurrency):
    doc = json.loads(path.read_text())
    config = doc["config"]
    if config["concurrency"] != concurrency:
        raise ValueError("report concurrency differs from the proposed concurrency")
    if section == "3.2":
        if (config["mode"] != "create-only" or config["warmup"] != 0 or config["total"] != REQUESTS
                or config["template"] != os.environ["CUBE_TEMPLATE_ID"]):
            raise ValueError("unexpected create workload configuration")
        if doc["summary"]["errors"] != 0 or doc["summary"]["successful"] != REQUESTS:
            raise ValueError("incomplete or failed create workload")
        p95 = check_samples(doc["raw"], "create_ms", doc["create"]["p95"])
    else:
        if (doc.get("status") != "complete" or doc.get("valid") is not True
                or doc.get("failure") or doc.get("cleanup_errors")
                or config["requests_per_round"] != REQUESTS or config["rounds"] != 1
                or config["warmup"] is not True):
            raise ValueError("incomplete or invalid snapshot workload")
        if section == "4.3" and config["snapshot_id"] != os.environ["CUBE_SNAPSHOT_ID"]:
            raise ValueError("report snapshot differs from CUBE_SNAPSHOT_ID")
        if section == "4.6" and config["template_id"] != os.environ["CUBE_TEMPLATE_ID"]:
            raise ValueError("report template differs from CUBE_TEMPLATE_ID")
        groups = [doc] if section == "4.3" else [doc["operations"][name] for name in ("pause", "resume")]
        padded = config.get("final_wave") == "padded"
        issued = math.ceil(REQUESTS / concurrency) * concurrency if padded else REQUESTS
        # In 4.6 both operations must succeed; the resume P95 feeds the planner.
        for group in groups:
            summary = group["summary"]
            if summary["errors"] != 0 or summary["successful"] != issued:
                raise ValueError("incomplete or failed snapshot workload")
            if padded and (summary["requests"] != issued or summary["skipped"] != 0
                           or summary["latency_samples"] != REQUESTS):
                raise ValueError("inconsistent padded snapshot workload counts")
            p95 = check_samples(group["raw"], "latency_ms", summary["p95"])
    return dict(concurrency=concurrency, requests=REQUESTS, errors=0, p95_ms=p95)


def run_round(section, concurrency, directory):
    raw = directory / "raw.json"
    env = dict(os.environ, CUBE_BENCH_SLO_CONCURRENCY=str(concurrency),
               CUBE_BENCH_SLO_OUTPUT=str(raw), OUTPUT_MD=str(directory / "report.md"))
    with (directory / "run.log").open("w") as log:
        child = subprocess.Popen(["bash", str(BENCH), "run", "-y", section], env=env,
                                 stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
        try:
            status = child.wait()
        except BaseException:
            # The shell starts SDK workers and subprocesses; forward interruption to the entire round.
            try:
                os.killpg(child.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            child.wait()
            raise
    if status:
        raise RuntimeError(f"benchmark exited {status}; see {directory / 'run.log'}")
    return read_measurement(raw, section, concurrency)


def search_section(section, history, decision, work, report, settle):
    directory = work / section
    directory.mkdir()
    fresh = []
    report.write(f"\n## {section}\n\n| Round | Phase | Concurrency | P95 (ms) |\n|---:|---|---:|---:|\n")
    curve_path = directory / "curve.csv"
    with curve_path.open("w", encoding="utf-8", newline="", buffering=1) as curve:
        writer = csv.writer(curve)
        writer.writerow(("round", "concurrency", "p95_ms"))
        while decision["next_case"] is not None:
            if fresh and settle:
                time.sleep(settle)
            candidate = decision["next_case"]
            number = len(fresh) + 1
            concurrency = candidate["concurrency"]
            phase = candidate["phase"]
            run = directory / f"round-{number:02d}-c{concurrency}"
            run.mkdir()
            write_json(run / "plan.json", decision)
            print(f"{section} round {number}: C{concurrency} ({phase})   {run / 'run.log'}", flush=True)
            row = run_round(section, concurrency, run)
            fresh.append(row)
            write_json(directory / "history.json", fresh)
            writer.writerow((number, concurrency, row["p95_ms"]))
            report.write(f"| {number} | {phase} | {concurrency} | {row['p95_ms']:.3f} |\n")
            print(f"  P95 {row['p95_ms']:.3f} ms", flush=True)
            decision = plan(fresh, history)
    verdict = decision["results"][0]
    write_json(directory / "verdict.json", verdict)
    status = verdict["status"]
    confirmed = verdict["confirmed_concurrency"]
    outcome = f"{section}: {status}; confirmed concurrency: {confirmed if confirmed is not None else '—'}"
    print(outcome, flush=True)
    report.write(f"\n{outcome}\n\nCurve data in `{curve_path}`\n")


def create_results_directory():
    root = Path(tempfile.gettempdir()).resolve()
    name = time.strftime("cubebench-search-%Y%m%d_%H%M%S")
    directory = root / name
    suffix = 0
    while True:
        try:
            directory.mkdir(mode=0o700)
            return directory
        except FileExistsError:
            suffix += 1
            directory = root / f"{name}-{suffix}"


def latest_target(link):
    try:
        metadata = link.lstat()
    except FileNotFoundError:
        return None
    if not stat.S_ISLNK(metadata.st_mode) or metadata.st_uid != os.geteuid():
        raise ValueError(f"{link}: expected a symbolic link owned by the current user")
    return link.resolve(strict=False)


def publish_latest(link, work):
    # Stage inside our private directory on the same filesystem. The relative
    # target is interpreted from link.parent once replace() publishes it.
    latest_target(link)
    staged = work / ".latest-link"
    staged.symlink_to(work.name, target_is_directory=True)
    try:
        staged.replace(link)
    finally:
        staged.unlink(missing_ok=True)


def search_sections(sections, history_root, use_latest, settle, work=None, report=None):
    failed = 0
    for section in sections:
        try:
            history = None
            path = history_root / section / "history.json" if history_root else None
            if use_latest and (path is None or not path.parent.exists()):
                print(f"{section}: no latest history; starting without prior measurements", file=sys.stderr)
            elif path is not None:
                history = json.loads(path.read_text())
                if not isinstance(history, list):
                    raise ValueError(f"{path}: expected a JSON array")
            first_plan = plan([], history)
            if report is None:
                print(section, json.dumps(first_plan["next_case"]))
                continue
            if section == "4.3" and not os.environ.get("CUBE_SNAPSHOT_ID"):
                raise ValueError("set CUBE_SNAPSHOT_ID to a ready benchmark snapshot")
            search_section(section, history, first_plan, work, report, settle)
        except (OSError, ValueError, RuntimeError, KeyError, TypeError, OverflowError) as exc:
            failure = f"{section}: FAILED — {exc}"
            failed += 1
            print(failure, file=sys.stderr, flush=True)
            if report is not None:
                report.write(f"\n{failure}\n")
    if failed:
        raise RuntimeError(f"{failed} of {len(sections)} sections failed")


def run(args):
    sections = select_sections(args.sections)
    if not math.isfinite(args.settle) or args.settle < 0:
        raise ValueError("--settle must be a finite, nonnegative number of seconds")
    latest = Path(tempfile.gettempdir()).resolve() / "cubebench-search-latest"
    use_latest = not args.no_history and args.history in (None, Path("latest"))
    # Pin one completed run so concurrent publication cannot mix its sections.
    history_root = latest_target(latest) if use_latest else args.history
    if args.preview:
        search_sections(sections, history_root, use_latest, args.settle)
        return

    if not os.environ.get("CUBE_TEMPLATE_ID"):
        raise ValueError("set CUBE_TEMPLATE_ID to a ready benchmark source")
    endpoint = (os.environ.get("CUBE_MASTER_IP") or "127.0.0.1") + ":" + (os.environ.get("CUBE_MASTER_PORT") or "8089")
    print(f"WARNING: searching {', '.join(sections)} destroys ALL sandboxes on every node of {endpoint}.",
          file=sys.stderr, flush=True)
    if not args.yes and os.environ.get("CUBE_BENCH_YES") != "1":
        if not sys.stdin.isatty():
            raise ValueError("a terminal or explicit -y/--yes is required")
        if input('Type "yes" to continue: ') != "yes":
            raise ValueError("search cancelled")

    latest_target(latest)  # Detect name collisions before measuring.
    work = create_results_directory()
    report_path = Path(os.environ.get("OUTPUT_MD") or work / "search.md").resolve()
    print(f"Search results: {work}\nReport: {report_path}", flush=True)
    report_path.parent.mkdir(parents=True, exist_ok=True)
    with report_path.open("w", encoding="utf-8", buffering=1) as report:
        report.write(f"# Cubebench P95 concurrency search\n\nTarget: P95 ≤ {LIMIT_MS} ms. "
                     f"Latency samples per round: {REQUESTS}. "
                     f"Boundary confirmation uses the median P95 of {CONFIRMATIONS} rounds at each adjacent "
                     f"concurrency level.\n\nResults: {work}\n")
        try:
            search_sections(sections, history_root, use_latest, args.settle, work, report)
        except (KeyboardInterrupt, SystemExit):
            report.write("\nSearch stopped before completion. See the round logs in the results directory.\n")
            raise
    publish_latest(latest, work)
    print(f"Latest search: {latest} -> {work.name}", flush=True)


def run_search(argv):
    parser = argparse.ArgumentParser(
        prog="cubebench.sh search",
        description="Run a P95 concurrency search with fresh Cubebench measurements.",
    )
    prior = parser.add_mutually_exclusive_group()
    prior.add_argument("--history", type=Path, metavar="DIR",
                       help="read prior rounds from DIR/<section>/history.json. Omit this option or use the "
                            "reserved value 'latest' to use the most recent successful search")
    prior.add_argument("--no-history", action="store_true", help="ignore previous searches and start at concurrency 16")
    parser.add_argument("--settle", type=float, default=os.environ.get("CUBE_BENCH_SLO_SETTLE", "5"), metavar="SEC",
                        help="seconds between rounds of a section (CUBE_BENCH_SLO_SETTLE, default 5, use 0 to skip)")
    parser.add_argument("--preview", action="store_true", help="print the first proposed round for each section")
    parser.add_argument("-y", "--yes", action="store_true", help="skip the cluster-wide sandbox deletion prompt")
    parser.add_argument("sections", nargs="*", help="3.2, 4.3, 4.6, chapter 3/4 or all (default)")
    args = parser.parse_intermixed_args(argv)
    try:
        run(args)
    except (OSError, ValueError, RuntimeError, KeyError, TypeError, OverflowError, EOFError) as exc:
        parser.exit(1, f"search failed: {exc}\n")


def main():
    if sys.argv[1:2] == ["run"]:
        # Let run_round forward termination to the shell and its workers.
        def terminate(signum, frame):
            raise SystemExit(128 + signum)

        signal.signal(signal.SIGTERM, terminate)
        run_search(sys.argv[2:])
        return

    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--history", type=argparse.FileType("r"),
                        help="JSON array of prior measurements from the same scenario and configuration")
    args = parser.parse_args()
    try:
        history = json.load(args.history) if args.history else None
        result = plan(json.load(sys.stdin), history)
    except (KeyError, TypeError, ValueError, OverflowError) as error:
        parser.error(str(error))
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    main()
