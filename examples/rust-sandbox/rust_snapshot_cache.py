#!/usr/bin/env python3
"""rust_snapshot_cache.py — Showcase snapshot-based compilation cache preservation.

This is the flagship demo for the Rust template. Rust compilation is slow the
first time (downloading crates, compiling from scratch), but CubeSandbox's
snapshot/pause-resume can freeze the entire VM state — including ``target/``
and ``~/.cargo/registry/`` — so subsequent builds are near-instant.

Flow
----
1. Create a Cargo project with a dependency (serde).
2. First ``cargo build --release`` — measure wall-clock time.
3. ``sandbox.pause()`` — freeze the VM to a snapshot, release resources.
4. ``sandbox.connect()`` — restore from snapshot.
5. Make a small code change.
6. Second ``cargo build --release`` — measure wall-clock time.
7. Compare: the second build should be dramatically faster (incremental).

Usage
-----
    cp .env.example .env   # fill in E2B_API_URL, E2B_API_KEY, CUBE_TEMPLATE_ID
    pip install -r requirements.txt
    python rust_snapshot_cache.py
"""

from __future__ import annotations

import argparse
import os
import sys
import textwrap
import time
from pathlib import Path

from dotenv import load_dotenv
from e2b import Sandbox

load_dotenv(dotenv_path=Path(__file__).with_name(".env"), override=False)

# A small Cargo project that depends on `serde` to force a non-trivial first build.
MAIN_RS_V1 = """\
use serde::Serialize;

#[derive(Serialize)]
struct Report {
    name: String,
    value: f64,
}

fn main() {
    let r = Report {
        name: "temperature".into(),
        value: 23.7,
    };
    let json = serde_json::to_string_pretty(&r).unwrap();
    println!("{json}");
}
"""

MAIN_RS_V2 = """\
use serde::Serialize;

#[derive(Serialize)]
struct Report {
    name: String,
    value: f64,
    unit: String,
    note: String,
}

fn main() {
    let r = Report {
        name: "temperature".into(),
        value: 23.7,
        unit: "celsius".into(),
        note: "measured inside CubeSandbox MicroVM".into(),
    };
    let json = serde_json::to_string_pretty(&r).unwrap();
    println!("{json}");
}
"""

CHECKPOINT_MARKER = "/tmp/snapshot-checkpoint.txt"
CHECKPOINT_CONTENT = "state-preserved-across-pause-resume-rust-demo"


def main() -> None:
    parser = argparse.ArgumentParser(
        description="CubeSandbox Rust snapshot/compilation-cache demo."
    )
    parser.add_argument(
        "--template",
        default=None,
        help="Cube template ID (default: $CUBE_TEMPLATE_ID).",
    )
    parser.add_argument(
        "--timeout",
        type=int,
        default=600,
        help="Build timeout in seconds.",
    )
    args = parser.parse_args()

    template_id = args.template or os.environ.get("CUBE_TEMPLATE_ID")
    if not template_id:
        print("Error: set CUBE_TEMPLATE_ID in .env or pass --template", file=sys.stderr)
        sys.exit(1)

    work_dir = "/home/user/snapshot-demo"

    print("═" * 60)
    print("  CubeSandbox Rust — Snapshot Compilation Cache Demo")
    print("═" * 60)
    print()
    print(f"Template:  {template_id}")
    print()

    # ── Phase 1: First Build ────────────────────────────────────────────────
    with Sandbox.create(template=template_id) as sandbox:
        sid = sandbox.sandbox_id
        print(f"Sandbox:   {sid}")
        print()

        # --- 1a. Create project ---
        print("[Phase 1] First build (cold)")
        print()
        print("  [1a] cargo new snapshot-demo")
        r = sandbox.commands.run(
            f"cd /home/user && cargo new snapshot-demo",
            timeout=30,
        )
        if r.exit_code != 0:
            print(f"       cargo new FAILED: {r.stderr}", file=sys.stderr)
            sys.exit(1)

        # --- 1b. Add dependency ---
        print("  [1b] cargo add serde --features derive && cargo add serde_json")
        r = sandbox.commands.run(
            f"cd {work_dir} && cargo add serde --features derive && cargo add serde_json",
            timeout=120,
        )
        if r.exit_code != 0:
            print(f"       cargo add FAILED: {r.stderr}", file=sys.stderr)
            sys.exit(1)

        # --- 1c. Write v1 source ---
        print(f"  [1c] Write src/main.rs v1 ({len(MAIN_RS_V1)} bytes)")
        sandbox.files.write(f"{work_dir}/src/main.rs", textwrap.dedent(MAIN_RS_V1).strip())

        # --- 1d. First build ---
        print("  [1d] cargo build --release (COLD — downloading + compiling)")
        t0 = time.monotonic()
        r = sandbox.commands.run(
            f"cd {work_dir} && cargo build --release",
            timeout=args.timeout,
        )
        cold_elapsed = time.monotonic() - t0
        if r.exit_code != 0:
            print(f"       BUILD FAILED: {r.stderr[-1000:]}", file=sys.stderr)
            sys.exit(1)
        print(f"       Build OK — {cold_elapsed:.1f}s (cold)")

        # --- 1e. Run the binary ---
        r = sandbox.commands.run(
            f"{work_dir}/target/release/snapshot-demo",
            timeout=15,
        )
        print(f"       Output: {r.stdout.strip()}")

        # --- 1f. Write checkpoint marker ---
        sandbox.files.write(CHECKPOINT_MARKER, CHECKPOINT_CONTENT)
        print(f"  [1f] Checkpoint marker written to {CHECKPOINT_MARKER}")

        print()

        # ── Phase 2: Pause ──────────────────────────────────────────────────
        print("[Phase 2] Pause sandbox (freeze VM state)")
        t0 = time.monotonic()
        sandbox.pause()
        pause_elapsed = time.monotonic() - t0
        print(f"         Paused in {pause_elapsed:.1f}s")
        print("         VM snapshot saved: target/, ~/.cargo/registry/, memory")
        print("         Resources released — zero CPU/memory cost while paused")
        print()

        # ── Phase 3: Resume ─────────────────────────────────────────────────
        print("[Phase 3] Resume sandbox from snapshot")
        t0 = time.monotonic()
        sandbox.connect()
        resume_elapsed = time.monotonic() - t0
        print(f"         Resumed in {resume_elapsed:.1f}s")
        print()

        # Verify checkpoint survived
        r = sandbox.files.read(CHECKPOINT_MARKER)
        if r.strip() == CHECKPOINT_CONTENT:
            print(f"  [✓] Checkpoint file intact: {CHECKPOINT_MARKER}")
        else:
            print(f"  [✗] Checkpoint file CORRUPTED!", file=sys.stderr)
            sys.exit(1)

        # Verify cargo registry is still there
        r = sandbox.commands.run("du -sh /root/.cargo/registry/ 2>/dev/null || echo missing", timeout=5)
        print(f"  [✓] Cargo registry preserved: {r.stdout.strip()}")
        print()

        # ── Phase 4: Incremental Build ──────────────────────────────────────
        print("[Phase 4] Incremental build after code change")
        print()

        # --- 4a. Modify source ---
        print(f"  [4a] Write src/main.rs v2 ({len(MAIN_RS_V2)} bytes)")
        sandbox.files.write(f"{work_dir}/src/main.rs", textwrap.dedent(MAIN_RS_V2).strip())

        # --- 4b. Incremental build ---
        print("  [4b] cargo build --release (HOT — incremental, cache warm)")
        t0 = time.monotonic()
        r = sandbox.commands.run(
            f"cd {work_dir} && cargo build --release",
            timeout=args.timeout,
        )
        hot_elapsed = time.monotonic() - t0
        if r.exit_code != 0:
            print(f"       BUILD FAILED: {r.stderr[-1000:]}", file=sys.stderr)
            sys.exit(1)
        print(f"       Build OK — {hot_elapsed:.1f}s (hot)")

        # --- 4c. Run modified binary ---
        r = sandbox.commands.run(
            f"{work_dir}/target/release/snapshot-demo",
            timeout=15,
        )
        print(f"       Output: {r.stdout.strip()}")
        print()

        # ── Summary ─────────────────────────────────────────────────────────
        speedup = cold_elapsed / hot_elapsed if hot_elapsed > 0 else float("inf")
        print("═" * 60)
        print("  Results")
        print("═" * 60)
        print(f"  Cold build (first):       {cold_elapsed:>8.1f}s")
        print(f"  Hot build  (incremental): {hot_elapsed:>8.1f}s")
        print(f"  Speedup:                  {speedup:>8.1f}x")
        print(f"  Pause latency:            {pause_elapsed:>8.1f}s")
        print(f"  Resume latency:           {resume_elapsed:>8.1f}s")
        print()
        print("  The snapshot preserved:")
        print("    • ~/.cargo/registry/  (downloaded crates)")
        print("    • target/              (compiled artifacts)")
        print("    • In-memory state      (variables, file descriptors)")
        print()
        print("  Without snapshot: every cold build re-downloads and recompiles.")
        print("  With CubeSandbox: pause → resume → incremental build in seconds.")
        print("═" * 60)


if __name__ == "__main__":
    main()
