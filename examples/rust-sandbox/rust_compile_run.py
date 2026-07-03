#!/usr/bin/env python3
"""rust_compile_run.py — Compile and execute Rust code snippets inside a Cube Sandbox.

This is the simplest entry point: write a Rust source file, compile it with
``rustc``, and run the resulting binary — all inside a single sandbox session.

Usage
-----
    cp .env.example .env   # fill in E2B_API_URL, E2B_API_KEY, CUBE_TEMPLATE_ID
    pip install -r requirements.txt
    python rust_compile_run.py
    python rust_compile_run.py --code 'fn main() { println!("hello from rust!"); }'
"""

from __future__ import annotations

import argparse
import os
import sys
import textwrap
from pathlib import Path

from dotenv import load_dotenv
from e2b import Sandbox

load_dotenv(dotenv_path=Path(__file__).with_name(".env"), override=False)

DEFAULT_CODE = """\
fn main() {
    println!("Hello from Rust inside CubeSandbox!");
    println!("rustc version: {}", "built in-sandbox");
    println!();
    println!("Fibonacci(10) = {}", fib(10));
}

fn fib(n: u64) -> u64 {
    match n {
        0 => 0,
        1 => 1,
        _ => fib(n - 1) + fib(n - 2),
    }
}
"""


def build_and_run(sandbox: Sandbox, code: str, *, timeout: int = 120) -> dict:
    """Write Rust source to the sandbox, compile, and execute it.

    Returns a dict with keys: exit_code, stdout, stderr, elapsed_ms.
    """
    src_path = "/tmp/main.rs"
    bin_path = "/tmp/main"

    sandbox.files.write(src_path, code)

    compile_cmd = f"rustc -C opt-level=0 -o {bin_path} {src_path}"
    result = sandbox.commands.run(compile_cmd, timeout=timeout)
    if result.exit_code != 0:
        return {
            "exit_code": result.exit_code,
            "stdout": result.stdout,
            "stderr": result.stderr,
            "elapsed_ms": None,
        }

    run_result = sandbox.commands.run(bin_path, timeout=timeout)
    return {
        "exit_code": run_result.exit_code,
        "stdout": run_result.stdout,
        "stderr": run_result.stderr,
        "elapsed_ms": None,
    }


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Compile and run Rust code in a Cube Sandbox."
    )
    parser.add_argument(
        "--code",
        default=DEFAULT_CODE,
        help="Rust source code to compile and execute.",
    )
    parser.add_argument(
        "--template",
        default=None,
        help="Cube template ID (default: $CUBE_TEMPLATE_ID).",
    )
    parser.add_argument(
        "--timeout",
        type=int,
        default=120,
        help="Compilation timeout in seconds.",
    )
    args = parser.parse_args()

    template_id = args.template or os.environ.get("CUBE_TEMPLATE_ID")
    if not template_id:
        print("Error: set CUBE_TEMPLATE_ID in .env or pass --template", file=sys.stderr)
        sys.exit(1)

    code = textwrap.dedent(args.code).strip()

    print(f"Template:  {template_id}")
    print(f"Code size: {len(code)} bytes")
    print()

    with Sandbox.create(template=template_id) as sandbox:
        print(f"Sandbox:   {sandbox.sandbox_id}")
        print()

        # Show toolchain versions
        rustc_ver = sandbox.commands.run("rustc --version", timeout=10)
        cargo_ver = sandbox.commands.run("cargo --version", timeout=10)
        print(f"[toolchain] {rustc_ver.stdout.strip()}")
        print(f"[toolchain] {cargo_ver.stdout.strip()}")
        print()

        # Compile
        print("[compile] rustc -o /tmp/main /tmp/main.rs")
        src_path = "/tmp/main.rs"
        bin_path = "/tmp/main"
        sandbox.files.write(src_path, code)
        compile_result = sandbox.commands.run(
            f"rustc -C opt-level=0 -o {bin_path} {src_path}",
            timeout=args.timeout,
        )
        if compile_result.exit_code != 0:
            print(f"[compile] FAILED (exit={compile_result.exit_code})")
            if compile_result.stderr:
                print(compile_result.stderr)
            sys.exit(1)
        print("[compile] OK")

        # Run
        print(f"[run] {bin_path}")
        run_result = sandbox.commands.run(bin_path, timeout=args.timeout)
        print(f"[run] (exit={run_result.exit_code})")
        print()
        print("─" * 50)
        if run_result.stdout:
            print(run_result.stdout)
        if run_result.stderr:
            print(run_result.stderr, file=sys.stderr)
        print("─" * 50)


if __name__ == "__main__":
    main()
