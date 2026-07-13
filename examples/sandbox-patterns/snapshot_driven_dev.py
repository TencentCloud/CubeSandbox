#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Checkpoint-driven iterative development with snapshot/rollback.

CubeSandbox differentiators shown:
  - Snapshot outlives sandbox: snapshots persist independently — kill the
    source sandbox and still create new sandboxes from the checkpoint
  - Instant rollback: restore sandbox state by re-creating from a snapshot
  - Snapshot management: create_snapshot() + list_snapshots() + delete_snapshot()

Usage:
    python snapshot_driven_dev.py

Workflow:
    1. Create a development sandbox, set up a project, build it.
    2. Take snapshot A (checkpoint) — save the entire workspace state.
    3. Kill the source sandbox — snapshot A still exists independently.
    4. Clone from snapshot A into a new sandbox (fork a workspace).
    5. Make changes in the clone, then roll back by re-creating from snapshot.
"""

from __future__ import annotations

import sys
import time

from e2b import Sandbox

from env_utils import load_local_dotenv, required

PROJECT_FILES: dict[str, str] = {
    "Cargo.toml": r'''[package]
name = "snapshot-demo"
version = "0.1.0"
edition = "2021"
''',
    "src/main.rs": r'''fn main() {
    println!("Checkpoint A: original version");
}
''',
}

NEW_MAIN_RS = r'''fn main() {
    println!("Checkpoint B: modified version with extra features");
    let sum: i32 = (1..=100).sum();
    println!("Sum of 1..100 = {}", sum);
}
'''

WS = "/home/user/workspace/snapshot-demo"


CARGO_ENV = "CARGO_HOME=/home/user/.cargo RUSTUP_HOME=/home/user/.rustup HOME=/home/user"


def run_cmd(sandbox: Sandbox, command: str, *, cwd: str | None = None,
            timeout: int = 60) -> str:
    if command.startswith(("cargo ", "rustup ", "rustc ")):
        command = f"{CARGO_ENV} {command}"
    result = sandbox.commands.run(command, cwd=cwd, timeout=timeout)
    if result.exit_code != 0:
        msg = f"Command failed (exit={result.exit_code}): {command}"
        if result.stderr:
            msg += f"\n  stderr: {result.stderr}"
        print(msg, file=sys.stderr)
        raise SystemExit(1)
    return (result.stdout or "")


def main() -> int:
    load_local_dotenv()

    template_id = required("CUBE_TEMPLATE_ID")
    required("E2B_API_URL")
    required("E2B_API_KEY")

    print("=" * 60)
    print("  CubeSandbox — Checkpoint-Driven Development Demo")
    print("=" * 60)

    # ── Phase 1: Create sandbox + build project ─────────────────────────
    print("\n[Phase 1] Creating workspace and building project...")
    t0 = time.monotonic()

    sandbox = Sandbox.create(
        template=template_id,
        timeout=120,
        lifecycle={"on_timeout": "pause", "auto_resume": True},
    )
    sandbox_id = sandbox.sandbox_id
    info = sandbox.get_info()
    print(f"  Created workspace: {sandbox_id}  state={info.state.value}"
          f"  ({time.monotonic() - t0:.2f}s)")

    run_cmd(sandbox, f"mkdir -p {WS}/src")
    for name, content in PROJECT_FILES.items():
        sandbox.files.write(f"{WS}/{name}", content)
    run_cmd(sandbox, "cargo build", cwd=WS, timeout=120)
    result = run_cmd(sandbox, "./target/debug/snapshot-demo", cwd=WS)
    if "Checkpoint A" not in result:
        print(f"  FAIL: expected 'Checkpoint A', got: {result!r}")
        return 1
    print(f"  ✓ Project built in {time.monotonic() - t0:.1f}s")

    # ── Phase 2: Take snapshot ──────────────────────────────────────────
    t0 = time.monotonic()
    snap = sandbox.create_snapshot(name="checkpoint-a")
    snap_id = snap.snapshot_id
    print(f"\n[Phase 2] Checkpoint saved: {snap_id}  ({time.monotonic() - t0:.2f}s)")

    # Kill the source sandbox before cloning — snapshot is independent
    print("\n[Phase 2b] Killing source workspace — checkpoint still lives...")
    sandbox.kill()
    pager = Sandbox.list_snapshots()
    items = []
    if pager.has_next:
        items = pager.next_items()
    if not items:
        print("  FAIL: checkpoint disappeared after workspace kill!")
        return 1
    print(f"  ✓ Checkpoint independent: {len(items)} snapshot(s) still in list"
          f"  ({time.monotonic() - t0:.2f}s)")

    # ── Phase 3: Clone from snapshot (fork) ─────────────────────────────
    t0 = time.monotonic()
    print(f"\n[Phase 3] Forking workspace from checkpoint...")
    clone = Sandbox.create(template=snap_id, timeout=120)
    clone_id = clone.sandbox_id

    result = run_cmd(clone, "./target/debug/snapshot-demo", cwd=WS, timeout=30)
    if "Checkpoint A" not in result:
        print(f"  FAIL: expected 'Checkpoint A' on fork, got: {result!r}")
        return 1
    print(f"  ✓ Fork ready: {clone_id}"
          f"  output={result.strip()!r}  ({time.monotonic() - t0:.2f}s)")

    # ── Phase 4: Modify clone then roll back ────────────────────────────
    print("\n[Phase 4] Modifying workspace and rolling back...")
    clone.files.write(f"{WS}/src/main.rs", NEW_MAIN_RS)
    run_cmd(clone, "cargo build", cwd=WS, timeout=120)

    result = run_cmd(clone, "./target/debug/snapshot-demo", cwd=WS)
    if "Checkpoint B" not in result:
        print(f"  FAIL: expected 'Checkpoint B', got: {result!r}")
        return 1
    print("  ✓ Changes applied successfully")

    # Rollback to checkpoint A by re-creating from snapshot
    t1 = time.monotonic()
    clone.kill()
    clone = Sandbox.create(template=snap_id, timeout=120)
    rollback_elapsed = time.monotonic() - t1
    print(f"  ✓ Rollback: {rollback_elapsed*1000:.0f}ms")

    result = run_cmd(clone, "./target/debug/snapshot-demo", cwd=WS, timeout=30)
    if "Checkpoint A" not in result:
        print(f"  FAIL: expected 'Checkpoint A' after rollback, got: {result!r}")
        return 1
    print(f"  ✓ Verified: output={result.strip()!r}")

    # ── Phase 5: Fork N workspaces from same checkpoint ─────────────────
    t0 = time.monotonic()
    print(f"\n[Phase 5] Scaling out: fork 3 workspaces from snapshot...")
    clone.kill()
    for i in range(3):
        sb = Sandbox.create(template=snap_id, timeout=60)
        cid = sb.sandbox_id
        result = run_cmd(sb, "./target/debug/snapshot-demo", cwd=WS, timeout=30)
        sb.kill()
        print(f"  fork {i+1}: {cid}  output={result.strip()!r}")
    print(f"  ✓ 3 forks in {time.monotonic() - t0:.2f}s")

    # ── Cleanup ─────────────────────────────────────────────────────────
    print(f"\n[Cleanup] Cleaning up...")
    Sandbox.delete_snapshot(snap_id)

    print(f"\n{'=' * 60}")
    print("  All checkpoint-driven development demos passed!")
    print(f"  Key takeaway: snapshots outlive the source workspace.")
    print(f"  Rollback by re-creating from snapshot.  Fork N for parallel forks.")
    print(f"{'=' * 60}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
