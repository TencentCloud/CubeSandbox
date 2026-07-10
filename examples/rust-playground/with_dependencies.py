#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Demonstrate CubeSandbox's secure network isolation and sandbox configuration.

CubeSandbox differentiators shown:
  - Secure: network isolation via allow_internet_access — one sandbox downloads
    crates from the internet, another builds entirely offline
  - Env injection: set RUST_BACKTRACE, CARGO_TERM_COLOR at creation time
  - Lifecycle: auto-pause/resume via lifecycle config

Usage:
    python with_dependencies.py

This script:
    1. Creates 2 sandboxes concurrently — one with internet, one without.
    2. Both scaffold the same Cargo project with serde_json + chrono.
    3. The online sandbox downloads crates from crates.io.
    4. The offline sandbox fails to fetch (demonstrates network isolation).
"""

from __future__ import annotations

import sys
import time
from concurrent.futures import ThreadPoolExecutor, as_completed

from e2b import Sandbox

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
        sandbox_id = getattr(sandbox, "sandbox_id", getattr(sandbox, "id", "unknown"))

        info = sandbox.get_info()
        print(f"  [{name}] created in {create_elapsed:.2f}s"
              f"  id={sandbox_id}  state={info.get('state', 'N/A')}"
              f"  internet={online}")

        # Scaffold
        scaffold_project(sandbox)
        print(f"  [{name}] project scaffolded")

        # Build
        t1 = time.monotonic()
        result = sandbox.commands.run("cargo build --release", cwd=WORKSPACE, timeout=300)
        build_elapsed = time.monotonic() - t1

        if result.exit_code == 0:
            print(f"  [{name}] build succeeded in {build_elapsed:.1f}s")
        else:
            stderr = (result.stderr or "")[-500:]
            print(f"  [{name}] build FAILED after {build_elapsed:.1f}s"
                  f"  stderr={stderr}", file=sys.stderr)
            return 1

        # Run
        result = sandbox.commands.run("./target/release/sandbox-demo", cwd=WORKSPACE, timeout=30)
        output = (result.stdout or "").strip()
        print(f"  [{name}] output: {output[:100]}")

    return 0


def main() -> int:
    load_local_dotenv()

    template_id = required("CUBE_TEMPLATE_ID")
    required("E2B_API_URL")
    required("E2B_API_KEY")

    print("CubeSandbox — Network Isolation + Concurrent Build Demo")
    print(f"Template: {template_id}")
    print("  sb-1: allow_internet_access=True   (will download crates)")
    print("  sb-2: allow_internet_access=False  (build fails — no network)")
    print()

    t_start = time.monotonic()

    with ThreadPoolExecutor(max_workers=2) as pool:
        f1 = pool.submit(run_demo, template_id, 1, online=True)
        f2 = pool.submit(run_demo, template_id, 2, online=False)
        r1 = f1.result()
        r2 = f2.result()

    total_elapsed = time.monotonic() - t_start

    print()
    print(f"Total: 2 sandboxes in {total_elapsed:.2f}s")
    print(f"  sb-1 (online)    : {'PASS' if r1 == 0 else 'FAIL'} — cargo fetched from crates.io")
    print(f"  sb-2 (offline)   : {'PASS' if r2 == 0 else 'FAIL'} — cargo blocked (network isolation)")
    print(f"  Expected: sb-1=0, sb-2=1  (offline cannot fetch crates)")

    return 0 if r1 == 0 and r2 != 0 else 1


if __name__ == "__main__":
    sys.exit(main())
