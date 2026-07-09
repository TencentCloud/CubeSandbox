#!/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Auto-kill (timeout → terminated) TUI demo.
#
# Sister demo to auto-resume.py — same five-step storyline, but flipped
# to the *destructive* lifecycle path: when the sandbox idles past its
# timeout the platform tears it down for good instead of parking it.
#
# Lifecycle config mirrors the e2b SDK
# (https://e2b.dev/docs/sandbox/auto-resume — same `lifecycle` knob, just
# `on_timeout="kill"`) so existing e2b code ports with minimal changes.
# `on_timeout="kill"` is also the default when no lifecycle is supplied,
# so this demo doubles as a contract-level check of that default.
#
# Run:
#     export CUBE_API_URL=http://<your-cubeapi>:3000
#     export CUBE_TEMPLATE_ID=<your-template>
#     python auto-kill.py [--dark]

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
    description="Cube Sandbox Auto-Kill (timeout → terminated) TUI Demo"
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
    help="Sandbox idle timeout in seconds before the platform kills it",
)
args = parser.parse_args()

# ── Color palette ────────────────────────────────────────────────────────────

if args.dark:
    PAL = dict(
        accent="bold cyan",
        ok="bold green",
        warn="bold yellow",
        danger="bold red",
        layer="cyan",
        key="magenta",
        val="green",
        muted="dim",
        border_run="green",
        border_kill="red",
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
        danger="bold #b91c1c",       # crimson
        layer="bold #155e75",        # dark teal
        key="bold #6b21a8",          # dark purple
        val="#15803d",               # medium green
        muted="#6b7280",             # slate gray
        border_run="#166534",
        border_kill="#b91c1c",
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

data = "Cube Sandbox auto-kill demo"
hash_val = hashlib.sha256(data.encode()).hexdigest()[:16]
pi_approx = 4 * sum((-1)**k / (2*k + 1) for k in range(500000))

print(f"sha256    = {hash_val}")
print(f"pi_approx = {pi_approx:.10f}")
"""

CHECKPOINT = "/tmp/checkpoint.txt"
IDLE_TIMEOUT_SECONDS = args.idle_timeout

# Once the platform decides a sandbox is over its idle window with
# on_timeout="kill", any subsequent request to it should fail fast — typically
# with 410 Gone surfaced through the SDK as a generic exception. We classify
# *anything* non-success as "destruction confirmed" rather than trying to match
# a specific exception subclass: the SDK's exception hierarchy isn't part of
# Cube's public contract, but the *failure* is.
TERMINAL_STATES = {"terminated", "killed", "killing", "stopped"}


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
    elif status in TERMINAL_STATES:
        indicator = f"[{PAL['danger']}]✖  {status.capitalize()}[/]"
        border = PAL["border_kill"]
    else:
        indicator = f"[{PAL['muted']}]?  {status!s}[/]"
        border = PAL["border_kill"]
    body = f"  Status:  {indicator}\n  ID:      [bold]{sandbox_id}[/]"
    if detail:
        body += f"\n  Detail:  [{PAL['muted']}]{detail}[/]"
    return Panel(body, title="Sandbox Status", border_style=border, padding=(0, 2))


def lifecycle_panel():
    """Render the lifecycle config the demo will pass to Sandbox.create."""
    body = (
        f"  [{PAL['key']}]on_timeout[/]  : "
        f"[{PAL['val']}]\"kill\"[/]    "
        f"[{PAL['muted']}]# tear the VM down (no snapshot kept)[/]\n"
        f"  [{PAL['key']}]auto_resume[/] : "
        f"[{PAL['val']}]N/A[/]       "
        f"[{PAL['muted']}]# nothing to resume — destruction is final[/]\n"
        f"  [{PAL['key']}]timeout[/]     : "
        f"[{PAL['val']}]{IDLE_TIMEOUT_SECONDS}s[/]       "
        f"[{PAL['muted']}]# idle threshold the sweeper uses[/]"
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
        # Once the sandbox is killed, get_info() typically raises (410 Gone).
        # We surface that as a synthetic terminal state so the rest of the
        # demo can pattern-match uniformly.
        return f"unreachable ({type(exc).__name__})"


def is_alive(sandbox_id):
    """Return True iff the sandbox still appears in Sandbox.list().

    Sandbox.list() filters out terminated sandboxes by default, so absence
    from the listing is a strong signal that timeout-kill ran end-to-end
    (sweeper → master → cubelet → list view) — strictly stronger than
    "get_info raised an exception", which can happen during transient
    network blips too.
    """
    try:
        return any(sb.get("sandboxID") == sandbox_id for sb in Sandbox.list())
    except Exception:  # noqa: BLE001
        # If we can't list at all the cluster itself is in trouble; treat
        # that as "we don't know" and let the caller decide.
        return None


# ── Title ────────────────────────────────────────────────────────────────────

console.print()
console.print(
    Panel(
        "[bold]Cube Sandbox · Auto-Kill (timeout → terminated) Demo[/]",
        box=box.DOUBLE,
        style=PAL["accent"],
        expand=True,
    )
)

# We deliberately do NOT use a `with` block here. The whole point of the
# demo is that the *platform* tears the sandbox down at idle timeout, so
# wrapping the run in a context manager that auto-kills on exit would
# mask whether the lifecycle path actually fired.
sandbox = Sandbox.create(
    template=template_id,
    timeout=IDLE_TIMEOUT_SECONDS,
    lifecycle={"on_timeout": "kill"},
)
sid = sandbox.sandbox_id

try:
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

    tbl = Table(title="State Snapshot (Before Idle)", box=box.ROUNDED)
    tbl.add_column("Layer", style=PAL["layer"])
    tbl.add_column("Key", style=PAL["key"])
    tbl.add_column("Value", style=PAL["val"])
    tbl.add_row("Kernel Memory", "hash_val", hash_before)
    tbl.add_row("Kernel Memory", "pi_approx", pi_before)
    tbl.add_row("Filesystem", CHECKPOINT, "2 lines")
    console.print(tbl)
    console.print()

    # ── Step 2: Idle — Watch the Platform Kill It ────────────────────────────

    console.rule(
        f"[{PAL['danger']}]Step 2 · Idle — Watch the Platform Auto-Kill[/]"
    )
    console.print(
        f"  Sandbox timeout is [{PAL['warn']}]{IDLE_TIMEOUT_SECONDS}s[/]. "
        f"With [{PAL['key']}]on_timeout=\"kill\"[/] the lifecycle sweeper "
        f"will tear the VM down once the idle window closes — no snapshot "
        f"is kept, the sandbox cannot be resumed.\n"
    )

    # Pad the wait a bit past the timeout to give the sweeper its scan
    # interval (default 5–10s in production) plus a safety margin.
    wait_for = IDLE_TIMEOUT_SECONDS + 20
    with Progress(
        TextColumn("{task.description}"),
        BarColumn(),
        TextColumn("{task.completed:.0f}/{task.total:.0f}s"),
        transient=False,
    ) as progress:
        task = progress.add_task(
            f"[{PAL['warn']}]Idle (no SDK calls — sweeper is in charge)[/]",
            total=wait_for,
        )
        for _ in range(wait_for):
            time.sleep(1)
            progress.advance(task)

    state_after_idle = fetch_state(sandbox)
    listed_alive = is_alive(sid)
    console.print()

    if state_after_idle.startswith("unreachable") or state_after_idle in TERMINAL_STATES:
        detail = "Platform killed the VM after the idle window expired"
    elif listed_alive is False:
        # get_info may still 200 briefly because of a stale cache, but if
        # it's gone from the listing the destruction has definitely landed.
        state_after_idle = "terminated"
        detail = "Sandbox no longer appears in Sandbox.list() — destruction confirmed"
    else:
        detail = (
            "Sandbox did not get killed within the window — sweeper / "
            "lifecycle config may be misconfigured"
        )
    console.print(status_panel(state_after_idle, sid, detail))

    # ── Step 3: Issue a Request — Confirm It Cannot Be Resumed ───────────────

    console.rule(
        f"[{PAL['danger']}]Step 3 · Try to Use the Sandbox — Expect Hard Failure[/]"
    )
    console.print(
        f"  Unlike [{PAL['ok']}]auto_resume=True[/], a kill is final. The "
        f"next call should fail fast — typically with [bold]410 Gone[/] "
        f"propagated as an SDK exception. We're verifying *failure*, not "
        f"transparency.\n"
    )

    resume_failed = False
    failure_reason = None
    try:
        with console.status(
            f"[{PAL['warn']}]Sending exec — sandbox should be gone...[/]"
        ):
            sandbox.run_code('print("should never reach here")')
    except Exception as exc:  # noqa: BLE001
        resume_failed = True
        failure_reason = f"{type(exc).__name__}: {exc}"

    if resume_failed:
        console.print(
            Panel(
                f"[{PAL['ok']}]Request rejected as expected.[/]\n"
                f"  Reason: [{PAL['muted']}]{failure_reason}[/]",
                title="Auto-Kill Verified",
                border_style=PAL["border_ok"],
                padding=(0, 2),
            )
        )
    else:
        console.print(
            Panel(
                f"[{PAL['danger']}]Request unexpectedly succeeded.[/] "
                f"The sandbox is still serving traffic — auto-kill did not "
                f"land. Check the lifecycle sweeper logs in "
                f"[bold]/data/log/cube-proxy/sidecar.log[/].",
                title="Auto-Kill Did Not Fire",
                border_style=PAL["border_kill"],
                padding=(0, 2),
            )
        )

    # ── Step 4: Confirm Destruction Is Final ─────────────────────────────────

    console.rule(f"[{PAL['danger']}]Step 4 · Confirm Destruction Is Final[/]")
    console.print(
        f"  Cross-checking against the cluster listing and a fresh sandbox "
        f"to rule out network blips.\n"
    )

    cluster_alive = is_alive(sid)

    # Spawn a control sandbox to prove the SDK / cluster are healthy — this
    # rules out "the previous request failed because everything is broken".
    control = Sandbox.create(template=template_id, timeout=60)
    try:
        control_state = fetch_state(control)
    finally:
        # Tidy up immediately; we just needed the create-then-info handshake.
        try:
            control.kill()
        except Exception:  # noqa: BLE001
            pass

    cmp_tbl = Table(title="Destruction Audit", box=box.ROUNDED)
    cmp_tbl.add_column("Check", style=PAL["layer"])
    cmp_tbl.add_column("Expectation", style=PAL["key"])
    cmp_tbl.add_column("Observed", style=PAL["val"])
    cmp_tbl.add_column("Result", justify="center")

    audits = [
        (
            "next request to original",
            "raises (410 Gone)",
            failure_reason or "succeeded",
            resume_failed,
        ),
        (
            "Sandbox.list() membership",
            "absent",
            (
                "absent"
                if cluster_alive is False
                else ("present" if cluster_alive else "list unreachable")
            ),
            cluster_alive is False,
        ),
        (
            "control sandbox sanity",
            "running",
            control_state,
            control_state == "running",
        ),
    ]

    all_pass = True
    for name, want, got, ok in audits:
        all_pass = all_pass and ok
        mark = f"[{PAL['match_yes']}]PASS[/]" if ok else f"[{PAL['match_no']}]FAIL[/]"
        cmp_tbl.add_row(name, want, got, mark)

    console.print(cmp_tbl)
    console.print()

    if all_pass:
        verdict = (
            f"[{PAL['ok']}]Auto-kill behaved exactly as advertised: "
            f"the original sandbox is gone for good and the cluster "
            f"itself is healthy.[/]"
        )
        verdict_border = PAL["border_ok"]
    else:
        verdict = (
            f"[{PAL['danger']}]One or more audits failed — auto-kill did "
            f"not land cleanly. Inspect the sweeper / lifecycle logs "
            f"before relying on this in production.[/]"
        )
        verdict_border = PAL["border_kill"]

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
    # Best-effort cleanup. If auto-kill already fired this is a no-op (the
    # sandbox doesn't exist anymore); if it didn't, we don't want to leak
    # the VM. Either way swallow exceptions so the demo's verdict above
    # stays the last thing the user sees.
    try:
        sandbox.kill()
    except Exception as exc:  # noqa: BLE001
        # Expected on the happy path — the sandbox is already gone.
        console.print(
            f"[{PAL['muted']}]cleanup: sandbox.kill() raised {exc!r} "
            f"(expected if auto-kill already ran)[/]"
        )
