#!/usr/bin/env python3
"""rust_secure_eval.py — Secure evaluation of untrusted Rust code in CubeSandbox.

Demonstrates CubeSandbox's security isolation capabilities applied to a classic
"online judge" / coding-interview scenario: compile and run user-submitted Rust
code in a fully network-isolated sandbox with resource limits.

Security layers demonstrated
-----------------------------
1. ``allow_internet_access=False`` — the sandbox cannot reach the internet.
2. Compilation timeout — prevent resource exhaustion during build.
3. Runtime resource limits — ``timeout`` + ``ulimit`` on the executed binary.
4. Output capture — stdout/stderr are captured and size-capped.
5. Disposable sandbox — the ``with`` block destroys the VM on exit.

Usage
-----
    cp .env.example .env   # fill in E2B_API_URL, E2B_API_KEY, CUBE_TEMPLATE_ID
    pip install -r requirements.txt
    python rust_secure_eval.py
    python rust_secure_eval.py --code 'fn main() { println!("safe!"); }'
    python rust_secure_eval.py --malicious   # demonstrates blocked network access
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

# A benign user submission — compute a SHA-256 digest (local-only, no network).
SAFE_CODE = """\
use std::collections::HashMap;

fn main() {
    let mut scores = HashMap::new();
    scores.insert("Alice", 95);
    scores.insert("Bob",    87);
    scores.insert("Carol",  92);

    let total: i32 = scores.values().sum();
    let avg = total as f64 / scores.len() as f64;

    println!("Scores: {scores:?}");
    println!("Total:  {total}");
    println!("Avg:    {avg:.2}");
}
"""

# Code that attempts to make a network request — must be blocked.
MALICIOUS_CODE = """\
use std::io::Read;
use std::net::TcpStream;

fn main() {
    println!("Attempting outbound TCP connection...");
    match TcpStream::connect("1.1.1.1:80") {
        Ok(mut stream) => {
            println!("CONNECTED — network isolation FAILED!");
            let mut buf = [0u8; 1024];
            let _ = stream.read(&mut buf);
        }
        Err(e) => {
            // This is the expected outcome inside a sandbox with
            // allow_internet_access=False.
            println!("BLOCKED (expected): {e}");
        }
    }
}
"""

WORK_DIR = "/tmp/secure-eval"


def evaluate(sandbox: Sandbox, code: str, *, build_timeout: int = 60, run_timeout: int = 10) -> dict:
    """Compile and run Rust code inside the sandbox.  Returns a result dict."""

    # Ensure workspace
    sandbox.commands.run(f"rm -rf {WORK_DIR} && mkdir -p {WORK_DIR}/src", timeout=5)

    # Write Cargo.toml
    cargo_toml = textwrap.dedent("""\
        [package]
        name = "secure-eval"
        version = "0.0.0"
        edition = "2021"
        [[bin]]
        name = "secure-eval"
        path = "src/main.rs"
    """)
    sandbox.files.write(f"{WORK_DIR}/Cargo.toml", cargo_toml)
    sandbox.files.write(f"{WORK_DIR}/src/main.rs", code)

    # Compile
    t0 = time.monotonic()
    r = sandbox.commands.run(
        f"cd {WORK_DIR} && cargo build --release 2>&1",
        timeout=build_timeout,
    )
    build_elapsed = time.monotonic() - t0
    if r.exit_code != 0:
        return {
            "verdict": "COMPILE_ERROR",
            "build_elapsed": build_elapsed,
            "run_elapsed": None,
            "stdout": "",
            "stderr": r.stderr[-4000:] if r.stderr else "",
        }

    # Run with resource limits: timeout + 512 MB virtual memory limit.
    binary = f"{WORK_DIR}/target/release/secure-eval"
    t0 = time.monotonic()
    r = sandbox.commands.run(
        f"timeout {run_timeout} sh -c 'ulimit -v 524288 && {binary}'",
        timeout=run_timeout + 5,
    )
    run_elapsed = time.monotonic() - t0

    verdict = "OK"
    if r.exit_code == 124:
        verdict = "TIMEOUT"
    elif r.exit_code == 137:
        verdict = "MEMORY_LIMIT"
    elif r.exit_code != 0:
        verdict = f"RUNTIME_ERROR(exit={r.exit_code})"

    return {
        "verdict": verdict,
        "build_elapsed": build_elapsed,
        "run_elapsed": run_elapsed,
        "stdout": (r.stdout or "")[-4000:],
        "stderr": (r.stderr or "")[-4000:],
    }


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Secure Rust code evaluation in a CubeSandbox."
    )
    parser.add_argument(
        "--code",
        default=None,
        help="Rust source code to evaluate (wraps in fn main).",
    )
    parser.add_argument(
        "--malicious",
        action="store_true",
        help="Run the malicious (network-egress) test case.",
    )
    parser.add_argument(
        "--template",
        default=None,
        help="Cube template ID (default: $CUBE_TEMPLATE_ID).",
    )
    parser.add_argument(
        "--build-timeout",
        type=int,
        default=60,
        help="Compilation timeout in seconds.",
    )
    parser.add_argument(
        "--run-timeout",
        type=int,
        default=10,
        help="Execution timeout in seconds.",
    )
    args = parser.parse_args()

    template_id = args.template or os.environ.get("CUBE_TEMPLATE_ID")
    if not template_id:
        print("Error: set CUBE_TEMPLATE_ID in .env or pass --template", file=sys.stderr)
        sys.exit(1)

    if args.malicious:
        code = MALICIOUS_CODE
        label = "malicious (network egress test)"
    elif args.code:
        code = args.code
        label = "custom"
    else:
        code = SAFE_CODE
        label = "safe (local computation)"

    print("═" * 60)
    print("  CubeSandbox Rust — Secure Code Evaluation")
    print("═" * 60)
    print(f"  Template:  {template_id}")
    print(f"  Scenario:  {label}")
    print(f"  Network:   allow_internet_access=False")
    print()

    # Create the sandbox with network isolation.
    with Sandbox.create(
        template=template_id,
        allow_internet_access=False,
    ) as sandbox:
        print(f"  Sandbox:   {sandbox.sandbox_id}")
        print()

        result = evaluate(
            sandbox,
            code,
            build_timeout=args.build_timeout,
            run_timeout=args.run_timeout,
        )

        print("─" * 40)
        print(f"  Verdict:       {result['verdict']}")
        print(f"  Build time:    {result['build_elapsed']:.1f}s")
        if result["run_elapsed"] is not None:
            print(f"  Run time:      {result['run_elapsed']:.3f}s")
        print("─" * 40)
        if result["stdout"]:
            print(f"\n[stdout]\n{result['stdout']}")
        if result["stderr"]:
            print(f"\n[stderr]\n{result['stderr'][-2000:]}")
        print()

        # Verify network isolation by trying to curl from inside the sandbox
        print("[verify] Network isolation check...")
        r = sandbox.commands.run("curl -m 5 -s -o /dev/null -w '%{http_code}' https://example.com 2>&1 || true", timeout=10)
        output = (r.stdout + r.stderr).strip()
        if "200" in output:
            print(f"         WARNING: network appears reachable ({output})")
        else:
            print(f"         OK — outbound internet blocked (got: {output})")


if __name__ == "__main__":
    main()
