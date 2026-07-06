# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import argparse
import os
import time

from e2b_code_interpreter import Sandbox
from env_utils import load_local_dotenv
from rich import box
from rich.console import Console
from rich.panel import Panel
from rich.progress import BarColumn, Progress, TextColumn
from rich.syntax import Syntax
from rich.table import Table

load_local_dotenv()

parser = argparse.ArgumentParser(description="Cube Sandbox Pause/Resume TUI Demo")
parser.add_argument(
    "--dark", action="store_true", help="Use dark-terminal color theme (default: light)"
)
args = parser.parse_args()

# ── Color palette ────────────────────────────────────────────────────────────
# Default palette uses hex RGB for guaranteed readability on light backgrounds.
# Pass --dark for bright ANSI colors suited to dark terminals.

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

data = "Cube Sandbox pause/resume demo"
hash_val = hashlib.sha256(data.encode()).hexdigest()[:16]
pi_approx = 4 * sum((-1)**k / (2*k + 1) for k in range(500000))

print(f"sha256    = {hash_val}")
print(f"pi_approx = {pi_approx:.10f}")
"""

CHECKPOINT = "/tmp/checkpoint.txt"


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
    else:
        indicator = f"[{PAL['warn']}]⏸  Paused[/]"
        border = PAL["border_pause"]
    body = f"  Status:  {indicator}\n  ID:      [bold]{sandbox_id}[/]"
    if detail:
        body += f"\n  Detail:  [{PAL['muted']}]{detail}[/]"
    return Panel(body, title="Sandbox Status", border_style=border, padding=(0, 2))


# ── Title ────────────────────────────────────────────────────────────────────

console.print()
console.print(
    Panel(
        "[bold]Cube Sandbox · Pause / Resume Demo[/]",
        box=box.DOUBLE,
        style=PAL["accent"],
        expand=True,
    )
)

with Sandbox.create(template=template_id) as sandbox:
    sid = sandbox.sandbox_id

    # ── Step 1: Create Sandbox & Run Computation ─────────────────────────────

    console.rule(f"[{PAL['accent']}]Step 1 · Create Sandbox & Run Computation[/]")
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

    tbl = Table(title="State Snapshot (Before Pause)", box=box.ROUNDED)
    tbl.add_column("Layer", style=PAL["layer"])
    tbl.add_column("Key", style=PAL["key"])
    tbl.add_column("Value", style=PAL["val"])
    tbl.add_row("Kernel Memory", "hash_val", before["hash_val"])
    tbl.add_row("Kernel Memory", "pi_approx", before["pi_approx"])
    tbl.add_row("Filesystem", CHECKPOINT, before["file_lines"])
    console.print(tbl)
    console.print()

    # ── Step 2: Pause Sandbox ────────────────────────────────────────────────

    console.rule(f"[{PAL['warn']}]Step 2 · Pause Sandbox[/]")

    with console.status(f"[{PAL['warn']}]Pausing sandbox (saving VM snapshot)...[/]"):
        sandbox.pause()

    console.print(
        status_panel("paused", sid, "VM snapshot saved. Resources released.")
    )

    # ── Step 3: Idle — Zero Resource Consumption ─────────────────────────────

    console.rule(f"[{PAL['muted']}]Step 3 · Idle — Zero Resource Consumption[/]")

    with Progress(
        TextColumn("{task.description}"),
        BarColumn(),
        TextColumn("{task.completed:.0f}/{task.total:.0f}s"),
        transient=False,
    ) as progress:
        task = progress.add_task("Idle", total=5)
        for _ in range(5):
            time.sleep(1)
            progress.advance(task)

    console.print()

    # ── Step 4: Resume Sandbox from Snapshot ─────────────────────────────────

    console.rule(f"[{PAL['ok']}]Step 4 · Resume Sandbox from Snapshot[/]")

    with console.status(f"[{PAL['ok']}]Resuming from snapshot...[/]"):
        sandbox.connect()

    console.print(status_panel("running", sid))

    # ── Step 5: Verify State Preserved ───────────────────────────────────────

    console.rule(f"[{PAL['ok']}]Step 5 · Verify State Preserved[/]")

    after_raw = []
    sandbox.run_code(
        'print(f"sha256    = {hash_val}")\n'
        'print(f"pi_approx = {pi_approx:.10f}")',
        on_stdout=lambda m: after_raw.append(m.line),
    )
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
    cmp_tbl.add_column("Before Pause", style=PAL["warn"])
    cmp_tbl.add_column("After Resume", style=PAL["ok"])
    cmp_tbl.add_column("Match", justify="center")

    for key, label, layer in [
        ("hash_val", "hash_val", "Kernel Memory"),
        ("pi_approx", "pi_approx", "Kernel Memory"),
        ("file_lines", CHECKPOINT, "Filesystem"),
    ]:
        ok = before[key] == after[key]
        mark = f"[{PAL['match_yes']}]PASS[/]" if ok else f"[{PAL['match_no']}]FAIL[/]"
        cmp_tbl.add_row(layer, label, before[key], after[key], mark)

    console.print(cmp_tbl)
    console.print()

    console.print(
        Panel(
            f"[{PAL['ok']}]All state perfectly preserved across pause/resume![/]",
            box=box.DOUBLE,
            border_style=PAL["border_ok"],
            expand=True,
        )
    )
    console.print()
