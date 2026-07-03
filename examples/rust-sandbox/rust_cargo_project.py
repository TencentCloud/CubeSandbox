#!/usr/bin/env python3
"""rust_cargo_project.py — Manage a Cargo project inside a Cube Sandbox.

Demonstrates the full Rust development workflow: create a Cargo project, add a
dependency, build, run, and inspect the result — all through the E2B SDK.

Usage
-----
    cp .env.example .env   # fill in E2B_API_URL, E2B_API_KEY, CUBE_TEMPLATE_ID
    pip install -r requirements.txt
    python rust_cargo_project.py
    python rust_cargo_project.py --project-name my-app --with-serde
"""

from __future__ import annotations

import argparse
import os
import sys
import time
from pathlib import Path

from dotenv import load_dotenv
from e2b import Sandbox

load_dotenv(dotenv_path=Path(__file__).with_name(".env"), override=False)

MAIN_RS_TEMPLATE = """\
/// A small CLI that computes some statistics from command-line numbers.
fn main() {
    let args: Vec<String> = std::env::args().collect();
    println!("Hello from '{}'! ({} args)", args[0], args.len() - 1);

    let numbers: Vec<f64> = args[1..]
        .iter()
        .filter_map(|s| s.parse().ok())
        .collect();

    if numbers.is_empty() {
        println!("Usage: {} <num1> <num2> ...", args[0]);
        println!("Example: {} 10 20 30", args[0]);
        return;
    }

    let sum: f64 = numbers.iter().sum();
    let avg = sum / numbers.len() as f64;
    println!("count = {}", numbers.len());
    println!("sum   = {sum}");
    println!("avg   = {avg:.4}");
}
"""


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Manage a Cargo project inside a Cube Sandbox."
    )
    parser.add_argument(
        "--project-name",
        default="hello-cube",
        help="Name for the Cargo project.",
    )
    parser.add_argument(
        "--with-serde",
        action="store_true",
        help="Add serde + serde_json as dependencies.",
    )
    parser.add_argument(
        "--template",
        default=None,
        help="Cube template ID (default: $CUBE_TEMPLATE_ID).",
    )
    parser.add_argument(
        "--timeout",
        type=int,
        default=300,
        help="Build timeout in seconds.",
    )
    args = parser.parse_args()

    template_id = args.template or os.environ.get("CUBE_TEMPLATE_ID")
    if not template_id:
        print("Error: set CUBE_TEMPLATE_ID in .env or pass --template", file=sys.stderr)
        sys.exit(1)

    project_name = args.project_name
    work_dir = f"/home/user/{project_name}"

    print(f"Template:     {template_id}")
    print(f"Project:      {project_name}")
    print(f"With serde:   {args.with_serde}")
    print()

    with Sandbox.create(template=template_id) as sandbox:
        print(f"Sandbox:      {sandbox.sandbox_id}")
        print()

        # 1. Create the project
        print(f"[1/5] cargo new {project_name}")
        r = sandbox.commands.run(
            f"cd /home/user && cargo new {project_name}",
            timeout=30,
        )
        if r.exit_code != 0:
            print(f"ERROR: {r.stderr}", file=sys.stderr)
            sys.exit(1)
        print(f"      Created {project_name}/")

        # 2. Write source code
        print(f"[2/5] Write src/main.rs ({len(MAIN_RS_TEMPLATE)} bytes)")
        sandbox.files.write(
            f"{work_dir}/src/main.rs",
            MAIN_RS_TEMPLATE,
        )
        print("      Done")

        # 3. Add optional dependencies
        if args.with_serde:
            print("[3/5] Add serde + serde_json dependencies")
            r = sandbox.commands.run(
                f"cd {work_dir} && cargo add serde --features derive && cargo add serde_json",
                timeout=60,
            )
            if r.exit_code != 0:
                print(f"WARNING: cargo add failed: {r.stderr}", file=sys.stderr)
        else:
            print("[3/5] (skip — no extra dependencies)")

        # 4. Build
        print(f"[4/5] cargo build --release (timeout={args.timeout}s)")
        t0 = time.monotonic()
        r = sandbox.commands.run(
            f"cd {work_dir} && cargo build --release",
            timeout=args.timeout,
        )
        build_elapsed = time.monotonic() - t0
        if r.exit_code != 0:
            print(f"      BUILD FAILED after {build_elapsed:.1f}s")
            print(r.stderr[-2000:], file=sys.stderr)
            sys.exit(1)
        print(f"      Build OK ({build_elapsed:.1f}s)")

        # 5. Run
        print(f"[5/5] ./target/release/{project_name} 10 42 99")
        r = sandbox.commands.run(
            f"{work_dir}/target/release/{project_name} 10 42 99",
            timeout=15,
        )
        print()
        print("─" * 50)
        print(r.stdout)
        if r.stderr:
            print(r.stderr, file=sys.stderr)
        print("─" * 50)

        # Show the binary size
        r = sandbox.commands.run(
            f"stat -c %s {work_dir}/target/release/{project_name}",
            timeout=5,
        )
        if r.exit_code != 0 or not r.stdout.strip():
            print(f"\nBinary size: (unavailable)", file=sys.stderr)
            return
        size_bytes = int(r.stdout.strip())
        if size_bytes >= 1_048_576:
            size_str = f"{size_bytes / 1_048_576:.1f} MB"
        elif size_bytes >= 1024:
            size_str = f"{size_bytes / 1024:.1f} KB"
        else:
            size_str = f"{size_bytes} B"
        print(f"\nBinary size: {size_str}")


if __name__ == "__main__":
    main()
