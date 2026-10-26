#!/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Auto-pause / auto-resume TUI demo.
#
# Sister demo to pause.py — same five-step storyline, but the pause/resume
# transitions are driven by the *platform* (cube-proxy + lifecycle manager)
# rather than explicit pause()/connect() calls from the SDK.
#
# Lifecycle config mirrors the e2b SDK
# (https://e2b.dev/docs/sandbox/auto-resume) so existing e2b code ports
# with minimal changes.
#
# Run:
#     export CUBE_API_URL=http://<your-cubeapi>:3000
#     export CUBE_TEMPLATE_ID=<your-template>
#     python auto-resume.py [--dark]

import argparse
import os
import time

from cubesandbox import Sandbox
from rich import box
from rich.console import Console
from rich.panel import Panel
from rich.progress import BarColumn, Progress, TextColumn
from rich.syntax import Syntax
from rich.table import Table

parser = argparse.ArgumentParser(
    description="Cube Sandbox Auto-Pause / Auto-Resume TUI Demo"
)
parser.add_argument(
    "--dark",
    action="store_true",
    help="Use dark-terminal color theme (default: light)",
)
parser.add_argument(
    "--idle-timeout",
    type=int,
    default=30,
    help="Sandbox idle timeout in seconds before the platform auto-pauses it",
)
args = parser.parse_args()

# ── Color palette ────────────────────────────────────────────────────────────

if args.dark:
    PAL = dict(
        accent="bold cyan",
        ok="bold green",
        warn="bold yellow",
        layer="cyan",
        key="magenta",
        val="green",
        muted="dim",
        border_run="green",
        border_pause="yellow",
        border_code="blue",
        border_out="green",
        border_ok="green",
        syntax="monokai",
        match_yes="bold green",
        match_no="bold red",
    )
else:
    PAL = dict(
        accent="bold #1e40af",       # deep blue
        ok="bold #166534",           # forest green
        warn="bold #9a3412",         # burnt sienna
        layer="bold #155e75",        # dark teal
        key="bold #6b21a8",          # dark purple
        val="#15803d",               # medium green
        muted="#6b7280",             # slate gray
        border_run="#166534",
        border_pause="#9a3412",
        border_code="#1e40af",
        border_out="#166534",
        border_ok="#166534",
        syntax="friendly",
        match_yes="bold #166534",
        match_no="bold #b91c1c",
    )

console = Console()
template_id = os.environ["CUBE_TEMPLATE_ID"]

DEMO_CODE = """\
import hashlib

data = "Cube Sandbox auto-pause/auto-resume demo"
hash_val = hashlib.sha256(data.encode()).hexdigest()[:16]
pi_approx = 4 * sum((-1)**k / (2*k + 1) for k in range(500000))

print(f"sha256    = {hash_val}")
print(f"pi_approx = {pi_approx:.10f}")
"""

CHECKPOINT = "/tmp/checkpoint.txt"
IDLE_TIMEOUT_SECONDS = args.idle_timeout


def collect_lines(raw_lines):
    """on_stdout may deliver multi-line blobs; normalize to individual lines."""
    return [l for l in "\n".join(raw_lines).splitlines() if l.strip()]


def parse_kv(line):
    """Extract value from 'key = value' formatted output line."""
    return line.split("=", 1)[1].strip()


def status_panel(status, sandbox_id, detail=None):
    if status == "running":
        indicator = f"[{PAL['ok']}]●  Running[/]"
        border = PAL["border_run"]
    elif status == "paused":
        indicator = f"[{PAL['warn']}]⏸  Paused[/]"
        border = PAL["border_pause"]
    else:
        indicator = f"[{PAL['muted']}]?  {status!s}[/]"
        border = PAL["border_pause"]
    body = f"  Status:  {indicator}\n  ID:      [bold]{sandbox_id}[/]"
    if detail:
        body += f"\n  Detail:  [{PAL['muted']}]{detail}[/]"
    return Panel(body, title="Sandbox Status", border_style=border, padding=(0, 2))


def lifecycle_panel():
    """Render the lifecycle config the demo will pass to Sandbox.create."""
    body = (
        f"  [{PAL['key']}]on_timeout[/]  : "
        f"[{PAL['val']}]\"pause\"[/]   "
        f"[{PAL['muted']}]# park the VM instead of killing it[/]\n"
        f"  [{PAL['key']}]auto_resume[/] : "
        f"[{PAL['val']}]True[/]      "
        f"[{PAL['muted']}]# next request transparently wakes it up[/]\n"
        f"  [{PAL['key']}]timeout[/]     : "
        f"[{PAL['val']}]{IDLE_TIMEOUT_SECONDS}s[/]       "
        f"[{PAL['muted']}]# idle threshold the sidecar uses[/]"
    )
    return Panel(
        body,
        title="Lifecycle Config (e2b-compatible)",
        border_style=PAL["accent"],
        padding=(0, 2),
    )


def fetch_state(sandbox):
    """Best-effort state fetch — backend may briefly 5xx during transitions."""
    try:
        info = sandbox.get_info()
        return info.get("state") or "unknown"
    except Exception as exc:  # noqa: BLE001
        return f"unreachable ({type(exc).__name__})"


# ── Title ────────────────────────────────────────────────────────────────────

console.print()
console.print(
    Panel(
        "[bold]Cube Sandbox · Auto-Pause / Auto-Resume Demo[/]",
        box=box.DOUBLE,
        style=PAL["accent"],
        expand=True,
    )
)

sandbox = Sandbox.create(
    template=template_id,
    timeout=IDLE_TIMEOUT_SECONDS,
    lifecycle={"on_timeout": "pause", "auto_resume": True},
)

try:
    sid = sandbox.sandbox_id

    # ── Step 1: Create Sandbox & Run Computation ─────────────────────────────

    console.rule(f"[{PAL['accent']}]Step 1 · Create Sandbox & Run Computation[/]")
    console.print(lifecycle_panel())
    console.print(status_panel("running", sid))

    console.print(
        Panel(
            Syntax(
                DEMO_CODE.strip(),
                "python",
                theme=PAL["syntax"],
                line_numbers=True,
            ),
            title="Executing in Sandbox",
            border_style=PAL["border_code"],
        )
    )

    stdout_raw = []
    sandbox.run_code(DEMO_CODE, on_stdout=lambda m: stdout_raw.append(m.line))
    output_lines = collect_lines(stdout_raw)

    console.print(
        Panel(
            "\n".join(f"  {line}" for line in output_lines),
            title="Sandbox Output",
            border_style=PAL["border_out"],
        )
    )

    hash_before = parse_kv(output_lines[0])
    pi_before = parse_kv(output_lines[1])

    ckpt_content = f"hash={hash_before}\npi={pi_before}\n"
    sandbox.files.write(CHECKPOINT, ckpt_content)
    console.print(f"  Writing checkpoint file... [{PAL['ok']}]✓[/]\n")

    before = {"hash_val": hash_before, "pi_approx": pi_before, "file_lines": "2 lines"}

    tbl = Table(title="State Snapshot (Before Idle)", box=box.ROUNDED)
    tbl.add_column("Layer", style=PAL["layer"])
    tbl.add_column("Key", style=PAL["key"])
    tbl.add_column("Value", style=PAL["val"])
    tbl.add_row("Kernel Memory", "hash_val", before["hash_val"])
    tbl.add_row("Kernel Memory", "pi_approx", before["pi_approx"])
    tbl.add_row("Filesystem", CHECKPOINT, before["file_lines"])
    console.print(tbl)
    console.print()

    # ── Step 2: Idle — Watch the Platform Pause It For Us ────────────────────

    console.rule(
        f"[{PAL['warn']}]Step 2 · Idle — Watch the Platform Auto-Pause[/]"
    )
    console.print(
        f"  Sandbox timeout is [{PAL['warn']}]{IDLE_TIMEOUT_SECONDS}s[/]. "
        f"We will idle past it and let the lifecycle manager step in.\n"
    )

    wait_for = IDLE_TIMEOUT_SECONDS + 15
    with Progress(
        TextColumn("{task.description}"),
        BarColumn(),
        TextColumn("{task.completed:.0f}/{task.total:.0f}s"),
        transient=False,
    ) as progress:
        task = progress.add_task(
            f"[{PAL['warn']}]Idle (no SDK calls — platform is in charge)[/]",
            total=wait_for,
        )
        for _ in range(wait_for):
            time.sleep(1)
            progress.advance(task)

    state_after_idle = fetch_state(sandbox)
    console.print()
    console.print(
        status_panel(
            state_after_idle,
            sid,
            (
                f"Platform paused the VM after {IDLE_TIMEOUT_SECONDS}s of inactivity"
                if state_after_idle == "paused"
                else "Platform did not pause within window — feature may be misconfigured"
            ),
        )
    )

    # ── Step 3: Issue a Request — Watch the Platform Resume It ───────────────

    console.rule(
        f"[{PAL['ok']}]Step 3 · Issue Request — Watch the Platform Auto-Resume[/]"
    )
    console.print(
        f"  Next [bold]run_code[/] call will hit a paused sandbox. "
        f"[{PAL['ok']}]auto_resume=True[/] tells the platform to wake it up "
        f"transparently before our request lands.\n"
    )

    after_raw = []
    started = time.monotonic()
    with console.status(
        f"[{PAL['ok']}]Sending exec — platform is resuming sandbox in the background...[/]"
    ):
        sandbox.run_code(
            'print(f"sha256    = {hash_val}")\n'
            'print(f"pi_approx = {pi_approx:.10f}")',
            on_stdout=lambda m: after_raw.append(m.line),
        )
    elapsed = time.monotonic() - started

    console.print(status_panel("running", sid, f"Resume + exec took {elapsed:.2f}s"))

    # ── Step 4: Verify State Preserved Across the Auto-Pause/Resume Cycle ────

    console.rule(f"[{PAL['ok']}]Step 4 · Verify State Preserved[/]")

    after_lines = collect_lines(after_raw)
    after_file = sandbox.files.read(CHECKPOINT)

    after = {
        "hash_val": parse_kv(after_lines[0]),
        "pi_approx": parse_kv(after_lines[1]),
        "file_lines": f"{len(after_file.strip().splitlines())} lines",
    }

    cmp_tbl = Table(title="State Comparison", box=box.ROUNDED)
    cmp_tbl.add_column("Layer", style=PAL["layer"])
    cmp_tbl.add_column("Key", style=PAL["key"])
    cmp_tbl.add_column("Before Idle", style=PAL["warn"])
    cmp_tbl.add_column("After Auto-Resume", style=PAL["ok"])
    cmp_tbl.add_column("Match", justify="center")

    all_pass = True
    for key, label, layer in [
        ("hash_val", "hash_val", "Kernel Memory"),
        ("pi_approx", "pi_approx", "Kernel Memory"),
        ("file_lines", CHECKPOINT, "Filesystem"),
    ]:
        ok = before[key] == after[key]
        all_pass = all_pass and ok
        mark = f"[{PAL['match_yes']}]PASS[/]" if ok else f"[{PAL['match_no']}]FAIL[/]"
        cmp_tbl.add_row(layer, label, before[key], after[key], mark)

    console.print(cmp_tbl)
    console.print()

    if all_pass:
        verdict = (
            f"[{PAL['ok']}]All state perfectly preserved across "
            f"auto-pause / auto-resume![/]"
        )
        verdict_border = PAL["border_ok"]
    else:
        verdict = (
            f"[{PAL['warn']}]One or more layers diverged after auto-resume — "
            f"check sidecar / lifecycle config.[/]"
        )
        verdict_border = PAL["border_pause"]

    console.print(
        Panel(
            verdict,
            box=box.DOUBLE,
            border_style=verdict_border,
            expand=True,
        )
    )
    console.print()

finally:
    # Clean up so the demo doesn't leave a sandbox behind on every run.
    # We do this in a try/finally rather than a `with` block because the
    # auto-pause path means the sandbox spends real wall time off-CPU, and
    # we want exactly one explicit kill at the end regardless of failure.
    try:
        sandbox.kill()
    except Exception as exc:  # noqa: BLE001
        console.print(f"[{PAL['muted']}]cleanup: sandbox.kill() raised {exc!r}[/]")
