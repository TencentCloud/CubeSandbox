# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
Cube Sandbox Benchmark — async concurrent create/delete performance test.

Directly calls CubeAPI HTTP endpoints with httpx (no SDK overhead, no GIL
contention) and renders a rich terminal UI suitable for screen recording.

Usage:
    python benchmark.py [--concurrency N] [--total N] [--template ID]
                        [--warmup N] [--mode MODE] [--output FILE]
                        [--theme {dark,light,auto}]

Examples:
    python benchmark.py -c 10 -n 100
    python benchmark.py -c 20 -n 200 --warmup 3 --output report.json
    python benchmark.py -c 5 -n 50 --mode create-only
    python benchmark.py --dry-run --theme light

Environment variables (required unless passed as flags):
    CUBE_TEMPLATE_ID   — sandbox template to boot
    E2B_API_KEY        — API key (any non-empty string for local server)
    E2B_API_URL        — base URL of the API server, e.g. http://localhost:3000
"""

from __future__ import annotations

import argparse
import asyncio
import json
import math
import os
import platform
import random
import socket
import statistics
import sys
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import List, Optional

import httpx
from rich import box
from rich.align import Align
from rich.console import Console, Group
from rich.layout import Layout
from rich.live import Live
from rich.panel import Panel
from rich.progress import (
    BarColumn,
    MofNCompleteColumn,
    Progress,
    SpinnerColumn,
    TaskProgressColumn,
    TextColumn,
    TimeElapsedColumn,
    TimeRemainingColumn,
)
from rich.table import Table
from rich.text import Text

console = Console()

# ─── Theme system ─────────────────────────────────────────────────────────────

SPARK_CHARS = "▁▂▃▄▅▆▇█"
BAR_CHARS = "▏▎▍▌▋▊▉█"


@dataclass(frozen=True)
class Theme:
    banner: str
    heading: str
    value: str
    accent: str
    border: str
    border_ok: str
    muted: str
    ok: str
    error: str
    warn: str
    bar_active: str
    bar_done: str
    lat_fast: str
    lat_ok: str
    lat_warn: str
    lat_slow: str
    lat_crit: str
    grade_s: str
    grade_a: str
    grade_b: str
    grade_c: str
    grade_d: str


DARK_THEME = Theme(
    banner="bold cyan",
    heading="bold bright_white",
    value="cyan",
    accent="bright_cyan",
    border="bright_blue",
    border_ok="bright_green",
    muted="dim",
    ok="bright_green",
    error="red",
    warn="yellow",
    bar_active="bright_cyan",
    bar_done="bright_green",
    lat_fast="bright_green",
    lat_ok="green",
    lat_warn="yellow",
    lat_slow="bright_red",
    lat_crit="red",
    grade_s="bold bright_magenta",
    grade_a="bold green",
    grade_b="bold yellow",
    grade_c="bold bright_red",
    grade_d="bold red",
)

LIGHT_THEME = Theme(
    banner="bold dark_blue",
    heading="bold",
    value="dark_cyan",
    accent="blue",
    border="blue",
    border_ok="dark_green",
    muted="grey50",
    ok="dark_green",
    error="red",
    warn="dark_orange",
    bar_active="blue",
    bar_done="dark_green",
    lat_fast="dark_green",
    lat_ok="green4",
    lat_warn="dark_orange",
    lat_slow="red",
    lat_crit="dark_red",
    grade_s="bold dark_magenta",
    grade_a="bold dark_green",
    grade_b="bold dark_orange",
    grade_c="bold red",
    grade_d="bold dark_red",
)

T: Theme = DARK_THEME


def detect_theme() -> Theme:
    """Heuristic: COLORFGBG='15;0' means light-on-dark, '0;15' means dark-on-light."""
    colorfgbg = os.environ.get("COLORFGBG", "")
    if colorfgbg:
        parts = colorfgbg.split(";")
        try:
            bg = int(parts[-1])
            if bg >= 8:
                return LIGHT_THEME
        except ValueError:
            pass
    if os.environ.get("TERM_LIGHT"):
        return LIGHT_THEME
    return DARK_THEME


BANNER = r"""
   ██████╗██╗   ██╗██████╗ ███████╗    ██████╗ ███████╗███╗   ██╗ ██████╗██╗  ██╗
  ██╔════╝██║   ██║██╔══██╗██╔════╝    ██╔══██╗██╔════╝████╗  ██║██╔════╝██║  ██║
  ██║     ██║   ██║██████╔╝█████╗      ██████╔╝█████╗  ██╔██╗ ██║██║     ███████║
  ██║     ██║   ██║██╔══██╗██╔══╝      ██╔══██╗██╔══╝  ██║╚██╗██║██║     ██╔══██║
  ╚██████╗╚██████╔╝██████╔╝███████╗    ██████╔╝███████╗██║ ╚████║╚██████╗██║  ██║
   ╚═════╝ ╚═════╝ ╚═════╝ ╚══════╝    ╚═════╝ ╚══════╝╚═╝  ╚═══╝ ╚═════╝╚═╝  ╚═╝
"""


# ─── Data types ───────────────────────────────────────────────────────────────


@dataclass
class IterResult:
    seq: int
    create_ms: float = 0.0
    delete_ms: float = 0.0
    error: Optional[str] = None
    timestamp: float = 0.0


@dataclass
class BenchState:
    """Mutable state shared across coroutines (single-threaded, no lock needed)."""

    total: int = 0
    completed: int = 0
    errors: int = 0
    results: List[IterResult] = field(default_factory=list)
    start_time: float = 0.0
    qps_window: List[float] = field(default_factory=list)

    @property
    def ok_results(self) -> List[IterResult]:
        return [r for r in self.results if r.error is None]

    @property
    def elapsed(self) -> float:
        return time.perf_counter() - self.start_time if self.start_time else 0.0

    @property
    def current_qps(self) -> float:
        now = time.perf_counter()
        self.qps_window = [t for t in self.qps_window if now - t < 5.0]
        return len(self.qps_window) / min(5.0, self.elapsed) if self.elapsed > 0 else 0.0


# ─── Stats helpers ────────────────────────────────────────────────────────────


def pct(data: List[float], p: float) -> float:
    if not data:
        return float("nan")
    s = sorted(data)
    k = min(max(0, int(math.ceil(len(s) * p / 100.0)) - 1), len(s) - 1)
    return s[k]


def sparkline(values: List[float], width: int = 40) -> str:
    if not values:
        return ""
    if len(values) > width:
        chunk = len(values) / width
        buckets = []
        for i in range(width):
            lo = int(i * chunk)
            hi = int((i + 1) * chunk)
            buckets.append(statistics.mean(values[lo:hi]) if hi > lo else 0)
        values = buckets
    lo, hi = min(values), max(values)
    spread = hi - lo if hi > lo else 1.0
    return "".join(SPARK_CHARS[min(int((v - lo) / spread * 7), 7)] for v in values)


def histogram_bar(count: int, max_count: int, width: int = 30) -> str:
    if max_count == 0:
        return ""
    full = count / max_count * width
    whole = int(full)
    frac = full - whole
    bar = BAR_CHARS[-1] * whole
    if frac > 0 and whole < width:
        bar += BAR_CHARS[min(int(frac * 8), 7)]
    return bar


def latency_color(ms: float) -> str:
    if ms < 100:
        return T.lat_fast
    if ms < 300:
        return T.lat_ok
    if ms < 500:
        return T.lat_warn
    if ms < 1000:
        return T.lat_slow
    return T.lat_crit


def grade_result(p99_ms: float, success_rate: float) -> tuple[str, str]:
    grades = [
        (100, 0.999, "S", T.grade_s),
        (200, 0.99, "A", T.grade_a),
        (500, 0.95, "B", T.grade_b),
        (1000, 0.90, "C", T.grade_c),
        (float("inf"), 0.0, "D", T.grade_d),
    ]
    for threshold, rate_min, letter, style in grades:
        if p99_ms <= threshold and success_rate >= rate_min:
            return letter, style
    return "D", T.grade_d


# ─── Phase 1: Config panel ───────────────────────────────────────────────────


def render_banner() -> None:
    console.print(Align.center(Text(BANNER, style=T.banner)))
    console.print()


def render_config(
    template_id: str,
    api_url: str,
    concurrency: int,
    total: int,
    warmup: int,
    mode: str,
) -> None:
    grid = Table(show_header=False, box=box.SIMPLE_HEAVY, padding=(0, 2), expand=True)
    grid.add_column("Key", style=T.heading, ratio=1)
    grid.add_column("Value", style=T.value, ratio=3)

    grid.add_row("Template", template_id)
    grid.add_row("API URL", api_url)
    grid.add_row("Concurrency", str(concurrency))
    grid.add_row("Total Requests", str(total))
    grid.add_row("Warmup Rounds", str(warmup))
    grid.add_row("Mode", mode)
    grid.add_row("Host", socket.gethostname())
    grid.add_row("Python", platform.python_version())
    grid.add_row("Time", datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M:%S UTC"))

    console.print(
        Panel(grid, title=f"[{T.heading}] Configuration [/]", border_style=T.border, padding=(1, 2))
    )
    console.print()


# ─── Phase 2: Live dashboard ─────────────────────────────────────────────────


def build_dashboard(state: BenchState, progress: Progress) -> Layout:
    layout = Layout()
    layout.split_column(
        Layout(name="progress", size=3),
        Layout(name="body", size=12),
    )

    layout["progress"].update(progress)

    stats_table = Table(show_header=False, box=box.SIMPLE, expand=True, padding=(0, 1))
    stats_table.add_column("k", style="bold", ratio=1)
    stats_table.add_column("v", ratio=1)

    ok = len(state.ok_results)
    create_avg = statistics.mean([r.create_ms for r in state.ok_results]) if ok else 0
    delete_avg = statistics.mean([r.delete_ms for r in state.ok_results]) if ok else 0

    stats_table.add_row("Completed", f"[{T.ok}]{ok}[/] / {state.total}")
    stats_table.add_row("Errors", f"[{T.error if state.errors else T.muted}]{state.errors}[/]")
    stats_table.add_row("QPS", f"[{T.accent}]{state.current_qps:.1f}[/] req/s")
    stats_table.add_row("Avg Create", f"[{latency_color(create_avg)}]{create_avg:.0f} ms[/]")
    stats_table.add_row("Avg Delete", f"[{latency_color(delete_avg)}]{delete_avg:.0f} ms[/]")
    stats_table.add_row("Elapsed", f"{state.elapsed:.1f}s")

    stats_panel = Panel(stats_table, title="[bold] Live Stats [/]", border_style=T.border)

    recent = state.results[-8:]
    log_lines = []
    for r in reversed(recent):
        if r.error:
            log_lines.append(f"  [{T.error}]#{r.seq:>4d}  ERR  {r.error[:60]}[/]")
        else:
            cc = latency_color(r.create_ms)
            dc = latency_color(r.delete_ms)
            log_lines.append(
                f"  [{T.muted}]#{r.seq:>4d}[/]  "
                f"CREATE [{cc}]{r.create_ms:>7.0f}ms[/]  "
                f"DELETE [{dc}]{r.delete_ms:>7.0f}ms[/]"
            )

    log_panel = Panel(
        "\n".join(log_lines) if log_lines else f"[{T.muted}]waiting...[/]",
        title="[bold] Recent Operations [/]",
        border_style=T.border,
    )

    body_layout = Layout()
    body_layout.split_row(
        Layout(stats_panel, name="stats", ratio=1),
        Layout(log_panel, name="log", ratio=2),
    )
    layout["body"].update(body_layout)
    return layout


# ─── Phase 3: Final report ───────────────────────────────────────────────────


def render_report(state: BenchState, mode: str) -> None:
    ok = state.ok_results
    total_elapsed = state.elapsed
    success_rate = len(ok) / state.total if state.total else 0
    overall_qps = len(ok) / total_elapsed if total_elapsed > 0 else 0

    console.print()

    overview = Table(show_header=False, box=box.SIMPLE, expand=True, padding=(0, 2))
    overview.add_column("k", style=T.heading, ratio=1)
    overview.add_column("v", ratio=2)

    overview.add_row("Total Time", f"[{T.accent}]{total_elapsed:.2f}s[/]")
    rate_color = T.ok if success_rate >= 0.99 else T.warn if success_rate >= 0.9 else T.error
    overview.add_row("Success Rate", f"[{rate_color}]{success_rate:.1%}[/]  ({len(ok)}/{state.total})")
    overview.add_row("Throughput", f"[{T.accent}]{overall_qps:.2f}[/] sandboxes/sec")

    console.print(
        Panel(overview, title=f"[{T.heading}] Summary [/]", border_style=T.border_ok, padding=(1, 2))
    )

    if not ok:
        console.print(f"[bold {T.error}]No successful results to report.[/]")
        return

    create_times = [r.create_ms for r in ok]
    delete_times = [r.delete_ms for r in ok]

    sections = [("CREATE", create_times)]
    if mode == "create-delete":
        sections.append(("DELETE", delete_times))

    for label, times in sections:
        _render_latency_section(label, times)

    # ── Latency timeline (sparklines) ──
    console.print()
    spark_table = Table(show_header=False, box=box.SIMPLE, expand=True, padding=(0, 2))
    spark_table.add_column("Label", style="bold", width=10)
    spark_table.add_column("Sparkline", ratio=1)
    spark_table.add_column("Range", style=T.muted, width=24)

    spark_table.add_row(
        "CREATE",
        Text(sparkline(create_times, width=60)),
        f"{min(create_times):.0f} .. {max(create_times):.0f} ms",
    )
    if mode == "create-delete":
        spark_table.add_row(
            "DELETE",
            Text(sparkline(delete_times, width=60)),
            f"{min(delete_times):.0f} .. {max(delete_times):.0f} ms",
        )

    console.print(
        Panel(
            spark_table,
            title=f"[{T.heading}] Latency Timeline [/]",
            subtitle=f"[{T.muted}]each char = avg of a time bucket, left=first right=last[/]",
            border_style=T.border,
            padding=(1, 2),
        )
    )

    # ── Errors ──
    error_results = [r for r in state.results if r.error]
    if error_results:
        err_table = Table(box=box.ROUNDED, border_style=T.error, show_lines=True, expand=True)
        err_table.add_column("#", style=T.muted, width=6)
        err_table.add_column("Error", style=T.error)
        for r in error_results[:20]:
            err_table.add_row(str(r.seq), r.error or "")
        if len(error_results) > 20:
            err_table.add_row("...", f"and {len(error_results) - 20} more")
        console.print(
            Panel(err_table, title=f"[bold {T.error}] Errors ({len(error_results)}) [/]", border_style=T.error)
        )

    # ── Grade ──
    p99_create = pct(create_times, 99)
    letter, style = grade_result(p99_create, success_rate)
    grade_text = Text()
    grade_text.append("  Performance Grade:  ", style="bold")
    grade_text.append(f" {letter} ", style=f"{style} reverse")
    grade_text.append(f"   (P99={p99_create:.0f}ms, success={success_rate:.1%})", style=T.muted)
    console.print()
    console.print(Panel(Align.center(grade_text), border_style=style, padding=(1, 0)))
    console.print()


def _render_latency_section(label: str, times: List[float]) -> None:
    avg = statistics.mean(times)
    std = statistics.stdev(times) if len(times) > 1 else 0.0

    ptable = Table(box=box.ROUNDED, expand=True, border_style=T.border)
    for h in ["min", "avg", "std", "P50", "P90", "P95", "P99", "max"]:
        ptable.add_column(h, justify="right")

    vals = [
        min(times), avg, std,
        pct(times, 50), pct(times, 90), pct(times, 95), pct(times, 99), max(times),
    ]
    ptable.add_row(*[f"[{latency_color(v)}]{v:.1f}[/]" for v in vals])

    num_buckets = 12
    lo, hi = min(times), max(times)
    if hi == lo:
        hi = lo + 1
    bucket_width = (hi - lo) / num_buckets
    buckets = [0] * num_buckets
    for v in times:
        idx = min(int((v - lo) / bucket_width), num_buckets - 1)
        buckets[idx] += 1

    max_count = max(buckets) if buckets else 1

    hist_lines = []
    for i, cnt in enumerate(buckets):
        lo_edge = lo + i * bucket_width
        hi_edge = lo_edge + bucket_width
        bar = histogram_bar(cnt, max_count, width=35)
        color = latency_color((lo_edge + hi_edge) / 2)
        pct_of_total = cnt / len(times) * 100 if times else 0
        hist_lines.append(
            f"  [{color}]{lo_edge:>7.0f} - {hi_edge:>7.0f} ms[/]  "
            f"[{color}]{bar}[/]  "
            f"[{T.muted}]{cnt:>4d} ({pct_of_total:4.1f}%)[/]"
        )

    console.print()
    console.print(
        Panel(
            Group(
                ptable,
                Text(""),
                Text("  Distribution:", style="bold"),
                Text.from_markup("\n".join(hist_lines)),
            ),
            title=f"[{T.heading}] {label} Latency [/]",
            border_style=T.border,
            padding=(1, 2),
        )
    )


# ─── Core benchmark ──────────────────────────────────────────────────────────


async def bench_one(
    client: httpx.AsyncClient,
    sem: asyncio.Semaphore,
    api_url: str,
    headers: dict,
    payload: dict,
    seq: int,
    state: BenchState,
    mode: str,
) -> IterResult:
    result = IterResult(seq=seq, timestamp=time.perf_counter())
    async with sem:
        sandbox_id: Optional[str] = None
        try:
            t0 = time.perf_counter()
            resp = await client.post(f"{api_url}/sandboxes", json=payload, headers=headers)
            result.create_ms = (time.perf_counter() - t0) * 1000
            if resp.status_code not in (200, 201):
                result.error = f"create HTTP {resp.status_code}: {resp.text[:200]}"
                state.errors += 1
                state.results.append(result)
                state.completed += 1
                state.qps_window.append(time.perf_counter())
                return result
            data = resp.json()
            sandbox_id = data.get("sandboxID") or data.get("sandbox_id")
        except Exception as exc:
            result.error = f"create exception: {exc}"
            state.errors += 1
            state.results.append(result)
            state.completed += 1
            state.qps_window.append(time.perf_counter())
            return result

        if mode == "create-delete" and sandbox_id:
            try:
                t0 = time.perf_counter()
                resp = await client.delete(f"{api_url}/sandboxes/{sandbox_id}", headers=headers)
                result.delete_ms = (time.perf_counter() - t0) * 1000
                if resp.status_code not in (200, 204):
                    result.error = f"delete HTTP {resp.status_code}: {resp.text[:200]}"
                    state.errors += 1
            except Exception as exc:
                result.error = f"delete exception: {exc}"
                state.errors += 1

    state.results.append(result)
    state.completed += 1
    state.qps_window.append(time.perf_counter())
    return result


async def bench_one_dry(
    sem: asyncio.Semaphore,
    seq: int,
    state: BenchState,
    mode: str,
    dry_latency: tuple[float, float],
    dry_error_rate: float,
) -> IterResult:
    """Simulated benchmark iteration for TUI debugging without a live API."""
    result = IterResult(seq=seq, timestamp=time.perf_counter())
    async with sem:
        base, jitter = dry_latency
        create_lat = max(1.0, random.gauss(base, jitter))
        await asyncio.sleep(create_lat / 1000.0)
        result.create_ms = create_lat

        if random.random() < dry_error_rate:
            result.error = f"simulated error (seq={seq})"
            state.errors += 1
            state.results.append(result)
            state.completed += 1
            state.qps_window.append(time.perf_counter())
            return result

        if mode == "create-delete":
            delete_lat = max(1.0, random.gauss(base * 0.4, jitter * 0.5))
            await asyncio.sleep(delete_lat / 1000.0)
            result.delete_ms = delete_lat

    state.results.append(result)
    state.completed += 1
    state.qps_window.append(time.perf_counter())
    return result


async def run_warmup(
    client: httpx.AsyncClient,
    api_url: str,
    headers: dict,
    payload: dict,
    rounds: int,
    mode: str,
) -> None:
    if rounds <= 0:
        return

    console.print(f"  [{T.muted}]Running {rounds} warmup round(s)...[/]", highlight=False)

    for i in range(rounds):
        try:
            resp = await client.post(f"{api_url}/sandboxes", json=payload, headers=headers)
            if resp.status_code in (200, 201):
                data = resp.json()
                sid = data.get("sandboxID") or data.get("sandbox_id")
                if mode == "create-delete" and sid:
                    await client.delete(f"{api_url}/sandboxes/{sid}", headers=headers)
            console.print(f"    warmup [{i + 1}/{rounds}] [{T.ok}]ok[/]")
        except Exception as exc:
            console.print(f"    warmup [{i + 1}/{rounds}] [{T.error}]failed: {exc}[/]")

    console.print()


async def run_benchmark(
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
    sem = asyncio.Semaphore(concurrency)

    progress = Progress(
        SpinnerColumn("dots"),
        TextColumn(f"[{T.heading}]{{task.description}}"),
        BarColumn(bar_width=40, complete_style=T.bar_active, finished_style=T.bar_done),
        TaskProgressColumn(),
        MofNCompleteColumn(),
        TimeElapsedColumn(),
        TimeRemainingColumn(),
        expand=True,
    )
    label = "Benchmarking (dry-run)" if dry_run else "Benchmarking"
    task_id = progress.add_task(label, total=total)

    async def _run_with_client(client: Optional[httpx.AsyncClient]) -> None:
        state.start_time = time.perf_counter()

        tasks: list[asyncio.Task] = []
        for i in range(total):
            if dry_run:
                t = asyncio.create_task(
                    bench_one_dry(sem, i + 1, state, mode, dry_latency, dry_error_rate)
                )
            else:
                assert client is not None
                t = asyncio.create_task(
                    bench_one(client, sem, api_url, headers, payload, i + 1, state, mode)
                )
            tasks.append(t)

        with Live(
            build_dashboard(state, progress),
            console=console,
            refresh_per_second=8,
            transient=True,
        ) as live:
            prev_completed = 0
            while not all(t.done() for t in tasks):
                await asyncio.sleep(0.12)
                done_now = state.completed
                if done_now > prev_completed:
                    progress.update(task_id, completed=done_now)
                    prev_completed = done_now
                live.update(build_dashboard(state, progress))

            progress.update(task_id, completed=total)
            live.update(build_dashboard(state, progress))

        await asyncio.gather(*tasks, return_exceptions=True)

    if dry_run:
        await _run_with_client(None)
    else:
        timeout = httpx.Timeout(connect=10.0, read=120.0, write=10.0, pool=120.0)
        limits = httpx.Limits(
            max_connections=concurrency + 10,
            max_keepalive_connections=concurrency + 5,
        )
        async with httpx.AsyncClient(timeout=timeout, limits=limits) as client:
            await run_warmup(client, api_url, headers, payload, warmup, mode)
            await _run_with_client(client)

    return state


# ─── JSON export ──────────────────────────────────────────────────────────────


def export_json(state: BenchState, filepath: str, args: argparse.Namespace) -> None:
    ok = state.ok_results
    create_times = [r.create_ms for r in ok]
    delete_times = [r.delete_ms for r in ok]

    def stat_block(values: List[float]) -> dict:
        if not values:
            return {}
        return {
            "count": len(values),
            "min": round(min(values), 2),
            "max": round(max(values), 2),
            "avg": round(statistics.mean(values), 2),
            "std": round(statistics.stdev(values), 2) if len(values) > 1 else 0,
            "p50": round(pct(values, 50), 2),
            "p90": round(pct(values, 90), 2),
            "p95": round(pct(values, 95), 2),
            "p99": round(pct(values, 99), 2),
        }

    report = {
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "config": {
            "template": args.template,
            "api_url": args.api_url,
            "concurrency": args.concurrency,
            "total": args.total,
            "warmup": args.warmup,
            "mode": args.mode,
        },
        "summary": {
            "total_time_s": round(state.elapsed, 3),
            "successful": len(ok),
            "errors": state.errors,
            "success_rate": round(len(ok) / state.total, 4) if state.total else 0,
            "throughput_qps": round(len(ok) / state.elapsed, 3) if state.elapsed else 0,
        },
        "create": stat_block(create_times),
        "delete": stat_block(delete_times) if args.mode == "create-delete" else {},
        "raw": [
            {
                "seq": r.seq,
                "create_ms": round(r.create_ms, 2),
                "delete_ms": round(r.delete_ms, 2),
                "error": r.error,
            }
            for r in state.results
        ],
    }

    with open(filepath, "w", encoding="utf-8") as f:
        json.dump(report, f, indent=2, ensure_ascii=False)

    console.print(f"  [{T.muted}]Report saved to[/] [bold]{filepath}[/]")


# ─── Main ─────────────────────────────────────────────────────────────────────


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Cube Sandbox Benchmark",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=__doc__,
    )
    parser.add_argument(
        "--concurrency", "-c", type=int, default=5,
        help="Max parallel in-flight requests (default: 5)",
    )
    parser.add_argument(
        "--total", "-n", type=int, default=20,
        help="Total create(/delete) iterations (default: 20)",
    )
    parser.add_argument(
        "--template", "-t", type=str, default=None,
        help="Template ID (overrides CUBE_TEMPLATE_ID env var)",
    )
    parser.add_argument(
        "--warmup", "-w", type=int, default=0,
        help="Warmup rounds before measurement (default: 0)",
    )
    parser.add_argument(
        "--mode", "-m", choices=["create-delete", "create-only"],
        default="create-delete",
        help="Benchmark mode (default: create-delete)",
    )
    parser.add_argument(
        "--output", "-o", type=str, default=None,
        help="Export JSON report to file",
    )
    parser.add_argument(
        "--api-url", type=str, default=None,
        help="CubeAPI base URL (overrides E2B_API_URL env var)",
    )
    parser.add_argument(
        "--api-key", type=str, default=None,
        help="API key (overrides E2B_API_KEY env var)",
    )
    parser.add_argument(
        "--theme", choices=["dark", "light", "auto"], default="auto",
        help="Color theme (default: auto — detect from terminal)",
    )
    parser.add_argument(
        "--dry-run", action="store_true", default=False,
        help="Simulate API calls with random latencies (no server needed, for TUI debugging)",
    )
    parser.add_argument(
        "--dry-latency", type=str, default="80,30",
        help="Dry-run latency: mean,stddev in ms (default: 80,30)",
    )
    parser.add_argument(
        "--dry-error-rate", type=float, default=0.02,
        help="Dry-run simulated error rate 0.0-1.0 (default: 0.02)",
    )
    return parser.parse_args()


async def async_main() -> None:
    global T
    args = parse_args()

    if args.theme == "light":
        T = LIGHT_THEME
    elif args.theme == "dark":
        T = DARK_THEME
    else:
        T = detect_theme()

    dry_run: bool = args.dry_run

    if dry_run:
        template_id = args.template or "dry-run-template"
        api_url = args.api_url or "http://localhost:3000 (dry-run)"
        api_key = args.api_key or "dry-run"
    else:
        template_id = args.template or os.environ.get("CUBE_TEMPLATE_ID", "")
        api_url = (args.api_url or os.environ.get("E2B_API_URL", "")).rstrip("/")
        api_key = args.api_key or os.environ.get("E2B_API_KEY", "")

        if not template_id:
            console.print(f"[bold {T.error}]ERROR:[/] template ID not set. Use --template or set CUBE_TEMPLATE_ID.")
            sys.exit(1)
        if not api_url:
            console.print(f"[bold {T.error}]ERROR:[/] API URL not set. Use --api-url or set E2B_API_URL.")
            sys.exit(1)
        if not api_key:
            console.print(f"[bold {T.error}]ERROR:[/] API key not set. Use --api-key or set E2B_API_KEY.")
            sys.exit(1)

    args.template = template_id
    args.api_url = api_url

    concurrency = max(1, args.concurrency)
    total = max(1, args.total)

    try:
        parts = args.dry_latency.split(",")
        dry_latency = (float(parts[0]), float(parts[1]))
    except (IndexError, ValueError):
        dry_latency = (80.0, 30.0)

    render_banner()
    render_config(template_id, api_url, concurrency, total, args.warmup, args.mode)

    if dry_run:
        console.print(
            Panel(
                f"[bold {T.warn}]DRY-RUN MODE[/] — simulating API calls with random latencies\n"
                f"  latency: [{T.accent}]N({dry_latency[0]:.0f}, {dry_latency[1]:.0f})[/] ms   "
                f"  error rate: [{T.accent}]{args.dry_error_rate:.0%}[/]",
                border_style=T.warn,
                padding=(0, 2),
            )
        )
        console.print()

    state = await run_benchmark(
        api_url=api_url,
        api_key=api_key,
        template_id=template_id,
        concurrency=concurrency,
        total=total,
        warmup=args.warmup,
        mode=args.mode,
        dry_run=dry_run,
        dry_latency=dry_latency,
        dry_error_rate=args.dry_error_rate,
    )

    render_report(state, args.mode)

    if args.output:
        export_json(state, args.output, args)

    if state.errors and not dry_run:
        sys.exit(1)


def main() -> None:
    asyncio.run(async_main())


if __name__ == "__main__":
    main()
