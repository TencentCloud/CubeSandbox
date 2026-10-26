#!/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Restrict-public-access TUI demo.
#
# Sister demo to auto-resume.py — same five-step storyline, but the focus
# is the per-sandbox traffic-access-token gate enforced by CubeProxy when
# the sandbox is created with network={"allow_public_traffic": False}.
#
# Storyline mirrors the e2b SDK example
# (https://e2b.dev/docs/network/restrict-public-access) so existing e2b
# code ports with minimal changes — we accept the same
# `e2b-traffic-access-token` header e2b uses, plus a CubeSandbox-native
# `cube-traffic-access-token` alias.
#
# The demo assumes the all-in-one CubeSandbox test image which already
# exposes nginx on port 80 — no in-sandbox server bootstrap needed.
#
# Run:
#     export CUBE_API_URL=http://<your-cubeapi>:3000
#     export CUBE_TEMPLATE_ID=<your-template>
#     python restrict_public_access.py [--dark] [--port 80]

import argparse
import os

import requests
from cubesandbox import Sandbox
from rich import box
from rich.console import Console
from rich.panel import Panel
from rich.syntax import Syntax
from rich.table import Table

parser = argparse.ArgumentParser(
    description="Cube Sandbox Restrict-Public-Access TUI Demo"
)
parser.add_argument(
    "--dark",
    action="store_true",
    help="Use dark-terminal color theme (default: light)",
)
parser.add_argument(
    "--port",
    type=int,
    default=80,
    help=(
        "Port inside the sandbox to probe (default: 80, served by nginx in "
        "the all-in-one test image). Any exposed port works."
    ),
)
args = parser.parse_args()

# ── Color palette ────────────────────────────────────────────────────────────

if args.dark:
    PAL = dict(
        accent="bold cyan",
        ok="bold green",
        warn="bold yellow",
        err="bold red",
        layer="cyan",
        key="magenta",
        val="green",
        muted="dim",
        border_run="green",
        border_block="yellow",
        border_err="red",
        border_code="blue",
        border_out="green",
        border_ok="green",
        syntax="monokai",
        match_yes="bold green",
        match_no="bold red",
    )
else:
    PAL = dict(
        accent="bold #1e40af",
        ok="bold #166534",
        warn="bold #9a3412",
        err="bold #b91c1c",
        layer="bold #155e75",
        key="bold #6b21a8",
        val="#15803d",
        muted="#6b7280",
        border_run="#166534",
        border_block="#9a3412",
        border_err="#b91c1c",
        border_code="#1e40af",
        border_out="#166534",
        border_ok="#166534",
        syntax="friendly",
        match_yes="bold #166534",
        match_no="bold #b91c1c",
    )

console = Console()
template_id = os.environ["CUBE_TEMPLATE_ID"]
PORT = args.port


def network_panel():
    """Render the network config the demo will pass to Sandbox.create."""
    body = (
        f"  [{PAL['key']}]allow_public_traffic[/] : "
        f"[{PAL['val']}]False[/]   "
        f"[{PAL['muted']}]# CubeProxy now requires a token on every hit[/]"
    )
    return Panel(
        body,
        title="Network Config (e2b-compatible)",
        border_style=PAL["accent"],
        padding=(0, 2),
    )


def token_panel(token):
    """Show the per-sandbox token returned by CubeMaster."""
    if token:
        body = (
            f"  [{PAL['key']}]traffic_access_token[/] : "
            f"[{PAL['val']}]{token}[/]\n"
            f"  [{PAL['muted']}]Send via either of these request headers:[/]\n"
            f"    • [{PAL['key']}]e2b-traffic-access-token[/]   "
            f"[{PAL['muted']}]# E2B-compatible[/]\n"
            f"    • [{PAL['key']}]cube-traffic-access-token[/]  "
            f"[{PAL['muted']}]# CubeSandbox-native alias[/]"
        )
        border = PAL["accent"]
    else:
        body = (
            f"  [{PAL['err']}]Sandbox returned no traffic_access_token![/]\n"
            f"  [{PAL['muted']}]Check that CubeMaster is built with the[/]\n"
            f"  [{PAL['muted']}]restrict-public-access feature enabled.[/]"
        )
        border = PAL["border_err"]
    return Panel(body, title="Issued Token", border_style=border, padding=(0, 2))


def request_panel(label, status, headers, color, border):
    """Pretty-print a single curl-equivalent request + response status."""
    if headers:
        hdr_lines = "\n".join(
            f"      [{PAL['key']}]{k}[/]: [{PAL['val']}]{v}[/]"
            for k, v in headers.items()
        )
    else:
        hdr_lines = f"      [{PAL['muted']}](no auth headers)[/]"
    body = (
        f"  Headers:\n{hdr_lines}\n"
        f"  Response: [{color}]HTTP {status}[/]"
    )
    return Panel(body, title=label, border_style=border, padding=(0, 2))


def probe(url, headers):
    """One HTTP probe; tolerate transport errors so a 5xx-during-boot does
    not crash the whole demo."""
    try:
        resp = requests.get(url, headers=headers, timeout=10)
        return resp.status_code, None
    except requests.RequestException as exc:
        return None, type(exc).__name__


# ── Title ────────────────────────────────────────────────────────────────────

console.print()
console.print(
    Panel(
        "[bold]Cube Sandbox · Restrict Public Access Demo[/]",
        box=box.DOUBLE,
        style=PAL["accent"],
        expand=True,
    )
)

sandbox = Sandbox.create(
    template=template_id,
    network={"allow_public_traffic": False},
)

try:
    sid = sandbox.sandbox_id
    token = sandbox.traffic_access_token
    host = sandbox.get_host(PORT)
    url = f"http://{host}/"

    # ── Step 1: Create Restricted Sandbox & Show Token ───────────────────────

    console.rule(f"[{PAL['accent']}]Step 1 · Create Restricted Sandbox[/]")
    console.print(network_panel())
    console.print(token_panel(token))

    target_tbl = Table(title="Probe Target", box=box.ROUNDED)
    target_tbl.add_column("Layer", style=PAL["layer"])
    target_tbl.add_column("Key", style=PAL["key"])
    target_tbl.add_column("Value", style=PAL["val"])
    target_tbl.add_row("Sandbox", "sandbox_id", sid)
    target_tbl.add_row("Network", "host", host)
    target_tbl.add_row("Network", "url", url)
    target_tbl.add_row("Network", "in-sandbox port", str(PORT))
    console.print(target_tbl)
    console.print()

    if not token:
        console.print(
            Panel(
                f"[{PAL['err']}]No token issued — aborting demo.[/]",
                border_style=PAL["border_err"],
            )
        )
        raise SystemExit(1)

    # ── Step 2: Unauthenticated Request → expect 403 ─────────────────────────

    console.rule(
        f"[{PAL['warn']}]Step 2 · Hit Without Token — Expect 403[/]"
    )
    console.print(
        Panel(
            Syntax(
                f"requests.get({url!r})",
                "python",
                theme=PAL["syntax"],
                line_numbers=False,
            ),
            title="Probe",
            border_style=PAL["border_code"],
        )
    )
    status_unauth, err_unauth = probe(url, headers={})
    if err_unauth:
        console.print(
            Panel(
                f"  Transport error: [{PAL['err']}]{err_unauth}[/]\n"
                f"  [{PAL['muted']}]This usually means CubeProxy is unreachable, "
                f"not that the gate is broken.[/]",
                title="Unauthenticated Probe",
                border_style=PAL["border_err"],
                padding=(0, 2),
            )
        )
        unauth_blocked = False
    else:
        is_blocked = status_unauth in (401, 403)
        unauth_blocked = is_blocked
        console.print(
            request_panel(
                "Unauthenticated Probe",
                status_unauth,
                headers={},
                color=PAL["ok"] if is_blocked else PAL["err"],
                border=PAL["border_block"] if is_blocked else PAL["border_err"],
            )
        )
    console.print()

    # ── Step 3: Authenticated With e2b Header → expect 200 ───────────────────

    console.rule(
        f"[{PAL['ok']}]Step 3 · Hit With e2b-traffic-access-token — Expect 200[/]"
    )
    e2b_headers = {"e2b-traffic-access-token": token}
    status_e2b, err_e2b = probe(url, headers=e2b_headers)
    if err_e2b:
        console.print(
            Panel(
                f"  Transport error: [{PAL['err']}]{err_e2b}[/]",
                title="E2B-Header Probe",
                border_style=PAL["border_err"],
                padding=(0, 2),
            )
        )
        e2b_ok = False
    else:
        e2b_ok = 200 <= status_e2b < 300
        console.print(
            request_panel(
                "E2B-Header Probe",
                status_e2b,
                headers=e2b_headers,
                color=PAL["ok"] if e2b_ok else PAL["err"],
                border=PAL["border_ok"] if e2b_ok else PAL["border_err"],
            )
        )
    console.print()

    # ── Step 4: Authenticated With cube Header → expect 200 ──────────────────

    console.rule(
        f"[{PAL['ok']}]Step 4 · Hit With cube-traffic-access-token — Expect 200[/]"
    )
    cube_headers = {"cube-traffic-access-token": token}
    status_cube, err_cube = probe(url, headers=cube_headers)
    if err_cube:
        console.print(
            Panel(
                f"  Transport error: [{PAL['err']}]{err_cube}[/]",
                title="Cube-Header Probe",
                border_style=PAL["border_err"],
                padding=(0, 2),
            )
        )
        cube_ok = False
    else:
        cube_ok = 200 <= status_cube < 300
        console.print(
            request_panel(
                "Cube-Header Probe",
                status_cube,
                headers=cube_headers,
                color=PAL["ok"] if cube_ok else PAL["err"],
                border=PAL["border_ok"] if cube_ok else PAL["border_err"],
            )
        )
    console.print()

    # ── Step 5: Verdict ──────────────────────────────────────────────────────

    console.rule(f"[{PAL['accent']}]Step 5 · Verdict[/]")

    cmp_tbl = Table(title="Probe Summary", box=box.ROUNDED)
    cmp_tbl.add_column("Probe", style=PAL["layer"])
    cmp_tbl.add_column("Expected", style=PAL["key"])
    cmp_tbl.add_column("Actual", style=PAL["val"])
    cmp_tbl.add_column("Match", justify="center")

    rows = [
        ("No header",      "403 (blocked)",
         f"{status_unauth}" if status_unauth is not None else f"err:{err_unauth}",
         unauth_blocked),
        ("e2b-...-token",  "200 (allowed)",
         f"{status_e2b}" if status_e2b is not None else f"err:{err_e2b}",
         e2b_ok),
        ("cube-...-token", "200 (allowed)",
         f"{status_cube}" if status_cube is not None else f"err:{err_cube}",
         cube_ok),
    ]
    all_pass = True
    for probe_name, expected, actual, ok in rows:
        all_pass = all_pass and ok
        mark = (
            f"[{PAL['match_yes']}]PASS[/]" if ok
            else f"[{PAL['match_no']}]FAIL[/]"
        )
        cmp_tbl.add_row(probe_name, expected, actual, mark)

    console.print(cmp_tbl)
    console.print()

    if all_pass:
        verdict = (
            f"[{PAL['ok']}]Public-access gate working as intended — "
            f"unauthenticated callers blocked, both header aliases honored.[/]"
        )
        verdict_border = PAL["border_ok"]
    else:
        verdict = (
            f"[{PAL['warn']}]One or more probes diverged — check that CubeProxy, "
            f"CubeMaster, and the test image are all up to date.[/]"
        )
        verdict_border = PAL["border_block"]

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
    # Mirror auto-resume.py: explicit kill in a try/finally rather than a
    # `with` block, so a failure mid-demo still releases the sandbox.
    try:
        sandbox.kill()
    except Exception as exc:  # noqa: BLE001
        console.print(f"[{PAL['muted']}]cleanup: sandbox.kill() raised {exc!r}[/]")
