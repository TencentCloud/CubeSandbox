#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Enforce egress network policies across sandbox environments.

CubeSandbox differentiators shown:
  - Secure: per-sandbox network isolation via allow_internet_access —
    one sandbox has full internet access, another is fully air-gapped
  - Env injection: inject environment variables at sandbox creation
  - Lifecycle: auto-pause/resume via lifecycle config
  - Side-by-side comparison: same workload, different network policies

Usage:
    python network_isolation.py

This script:
    1. Creates 2 sandboxes side-by-side — one online, one offline.
    2. Both scaffold the same Cargo project with external dependencies.
    3. The online sandbox downloads crates from the internet.
    4. The offline sandbox is blocked (demonstrates egress policy enforcement).
"""

from __future__ import annotations

import sys
import time
from concurrent.futures import ThreadPoolExecutor, as_completed

from e2b import Sandbox
try:
    from e2b.sandbox.commands.command_handle import CommandExitException
except ModuleNotFoundError:
    raise RuntimeError(
        "CommandExitException not available. Run via the verification harness:\n"
        "  python tests/run_verification.py") from None

from env_utils import load_local_dotenv, required

CARGO_TOML = r'''[package]
name = "sandbox-demo"
version = "0.1.0"
edition = "2021"

[dependencies]
serde_json = "1"
chrono = "0.4"
'''

MAIN_RS = r'''use chrono::Utc;
use serde_json::json;

fn main() {
    let now = Utc::now();
    let message = json!({
        "greeting": "Hello from CubeSandbox!",
        "language": "Rust",
        "timestamp": now.to_rfc3339(),
        "crates": ["serde_json", "chrono"],
        "answer": 42,
    });
    println!("{}", serde_json::to_string_pretty(&message).unwrap());
}
'''


WORKSPACE = "/home/user/workspace/sandbox-demo"


def scaffold_project(sandbox: Sandbox) -> None:
    sandbox.commands.run(f"mkdir -p {WORKSPACE}/src", timeout=30)
    sandbox.files.write(f"{WORKSPACE}/Cargo.toml", CARGO_TOML)
    sandbox.files.write(f"{WORKSPACE}/src/main.rs", MAIN_RS)


def run_demo(template_id: str, index: int, online: bool) -> int:
    name = f"sb-{index}"

    print(f"\n  [{name}] creating sandbox (internet={online})...")
    t0 = time.monotonic()

    with Sandbox.create(
        template=template_id,
        timeout=120,
        envs={"RUST_BACKTRACE": "1", "CARGO_TERM_COLOR": "always"},
        lifecycle={"on_timeout": "pause", "auto_resume": True},
        allow_internet_access=online,
    ) as sandbox:
        create_elapsed = time.monotonic() - t0
        sandbox_id = sandbox.sandbox_id

        info = sandbox.get_info()
        print(f"  [{name}] created in {create_elapsed:.2f}s"
              f"  id={sandbox_id}  state={info.state.value}"
              f"  internet={online}")

        scaffold_project(sandbox)
        print(f"  [{name}] project scaffolded")

        t1 = time.monotonic()
        try:
            result = sandbox.commands.run("cargo build --release", cwd=WORKSPACE, timeout=300)
            build_elapsed = time.monotonic() - t1
            exit_code = result.exit_code
        except CommandExitException as exc:
            build_elapsed = time.monotonic() - t1
            exit_code = exc.exit_code if hasattr(exc, 'exit_code') else 1
            print(f"  [{name}] build FAILED (code {exit_code}) after {build_elapsed:.1f}s"
                  f"  error={str(exc)[:200]}", file=sys.stderr)
            return 1

        if exit_code == 0:
            print(f"  [{name}] build succeeded in {build_elapsed:.1f}s")
        else:
            return 1

        result = sandbox.commands.run("./target/release/sandbox-demo", cwd=WORKSPACE, timeout=30)
        output = (result.stdout or "").strip()
        print(f"  [{name}] output: {output[:100]}")

    return 0


def main() -> int:
    load_local_dotenv()

    template_id = required("CUBE_TEMPLATE_ID")
    required("E2B_API_URL")
    required("E2B_API_KEY")

    print("CubeSandbox — Egress Network Policy Enforcement Demo")
    print(f"  Scenario: same workload, different egress policies")
    print(f"  Template: {template_id}")
    print("    sb-1: allow_internet_access=True   (can pull dependencies)")
    print("    sb-2: allow_internet_access=False  (air-gapped — build fails)")
    print()

    t_start = time.monotonic()

    with ThreadPoolExecutor(max_workers=2) as pool:
        f1 = pool.submit(run_demo, template_id, 1, online=True)
        f2 = pool.submit(run_demo, template_id, 2, online=False)
        r1 = f1.result()
        r2 = f2.result()

    total_elapsed = time.monotonic() - t_start

    print()
    print(f"  Total: 2 sandboxes in {total_elapsed:.2f}s")
    print(f"    sb-1 (online)    : {'PASS' if r1 == 0 else 'FAIL'} — pulled dependencies successfully")
    print(f"    sb-2 (offline)   : {'PASS' if r2 == 0 else 'FAIL'} — blocked by egress policy")
    print(f"    Expected: sb-1=0, sb-2=1  (offline cannot fetch external resources)")
    print()
    print("  Key takeaway: per-sandbox allow_internet_access enforces")
    print("  network isolation without changing the workload.")

    return 0 if r1 == 0 and r2 != 0 else 1


if __name__ == "__main__":
    sys.exit(main())
