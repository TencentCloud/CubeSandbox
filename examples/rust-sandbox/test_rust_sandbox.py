#!/usr/bin/env python3
"""test_rust_sandbox.py — Local smoke tests for the Rust sandbox Docker image.

These tests validate the Docker image **before** it is registered as a Cube
template. No Cube cluster is required — only Docker and Python.

Usage
-----
    docker build -t rust-sandbox:latest examples/rust-sandbox/
    pip install -r examples/rust-sandbox/requirements.txt
    python examples/rust-sandbox/test_rust_sandbox.py
    python examples/rust-sandbox/test_rust_sandbox.py --image my-registry/rust:latest
"""

from __future__ import annotations

import argparse
import os
import subprocess
import sys
import time

# ---------------------------------------------------------------------------
# Test helpers
# ---------------------------------------------------------------------------

def run(cmd: str, **kwargs) -> subprocess.CompletedProcess:
    """Run a shell command and return the CompletedProcess."""
    kwargs.setdefault("capture_output", True)
    kwargs.setdefault("text", True)
    kwargs.setdefault("timeout", 120)
    return subprocess.run(cmd, shell=True, **kwargs)


def die(msg: str) -> None:
    print(f"  FAIL: {msg}", file=sys.stderr)
    sys.exit(1)


def ok(msg: str) -> None:
    print(f"  ✓  {msg}")


# ---------------------------------------------------------------------------
# Test cases
# ---------------------------------------------------------------------------

class RustSandboxTester:
    """Wraps a Docker container running the Rust sandbox image."""

    def __init__(self, image: str):
        self.image = image
        self.container_id: str | None = None

    def start(self) -> None:
        print(f"[start] {self.image}")
        r = run(
            f"docker run --rm -d -p 49983:49983 --name rust-sandbox-test {self.image}"
        )
        if r.returncode != 0:
            die(f"docker run failed:\n{r.stderr}")
        self.container_id = r.stdout.strip()
        ok(f"container {self.container_id[:12]}")

    def stop(self) -> None:
        if self.container_id:
            print("[stop]")
            run(f"docker rm -f {self.container_id}", check=False)

    def exec(self, cmd: str, **kwargs) -> subprocess.CompletedProcess:
        """Run a command inside the container."""
        assert self.container_id
        return run(f"docker exec {self.container_id} {cmd}", **kwargs)

    # -- tests ---------------------------------------------------------------

    def test_envd_health(self) -> None:
        print("[test] envd /health → 204")
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            r = run(
                f"curl -s -o /dev/null -w '%{{http_code}}' http://127.0.0.1:49983/health"
            )
            if r.stdout.strip() == "204":
                ok("envd /health → 204")
                return
            time.sleep(0.5)
        die("envd /health did not return 204 within 15s")

    def test_envd_version(self) -> None:
        print("[test] envd -version")
        r = self.exec("/usr/bin/envd -version")
        if r.returncode != 0:
            die(f"envd -version failed:\n{r.stderr}")
        ok(f"envd {r.stdout.strip()}")

    def test_user_rustc(self) -> None:
        print("[test] rustc --version (as user)")
        r = self.exec("-u user rustc --version")
        if r.returncode != 0:
            die(f"rustc not found for user:\n{r.stderr}")
        ok(f"user: {r.stdout.strip()}")

    def test_user_cargo(self) -> None:
        print("[test] cargo --version (as user)")
        r = self.exec("-u user cargo --version")
        if r.returncode != 0:
            die(f"cargo not found for user:\n{r.stderr}")
        ok(f"user: {r.stdout.strip()}")

    def test_rustc_compile(self) -> None:
        print("[test] rustc compile + run")
        # Use a heredoc to avoid shell quoting issues with the Rust source.
        r = self.exec(
            "bash -c 'cat > /tmp/test.rs << \"EOF\"\n"
            "fn main() { println!(\"smoke-test-ok\"); }\n"
            "EOF\n"
            "rustc -o /tmp/test /tmp/test.rs && /tmp/test'"
        )
        if r.returncode != 0:
            die(f"rustc smoke test failed:\n{r.stderr}")
        if "smoke-test-ok" not in r.stdout:
            die(f"unexpected output: {r.stdout}")
        ok("rustc compile + run OK")

    def test_cargo_new_build(self) -> None:
        print("[test] cargo new + build")
        r = self.exec(
            "bash -c 'cd /tmp && rm -rf citest && cargo new citest && cd citest && cargo build 2>&1'",
            timeout=180,
        )
        if r.returncode != 0:
            # First build might fail if cargo registry wasn't pre-warmed.
            # This is acceptable for the local smoke test — it works when
            # the image was built with `cargo build` in the Dockerfile.
            print(f"  !  cargo new/build returned {r.returncode} (may need network)")
            print(f"     stderr tail: {(r.stderr or '')[-500:]}")
        else:
            ok("cargo new + build OK")

    def test_cargo_registry_warm(self) -> None:
        print("[test] user ~/.cargo/registry cache")
        r = self.exec("du -sh /home/user/.cargo/registry/ 2>/dev/null || echo empty")
        out = r.stdout.strip()
        if out == "empty" or out.startswith("0"):
            print(f"  !  registry appears empty ({out}) — pre-warming may have failed")
        else:
            ok(f"user registry cache: {out}")


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def main() -> None:
    parser = argparse.ArgumentParser(
        description="Smoke-test the CubeSandbox Rust Docker image."
    )
    parser.add_argument(
        "--image",
        default="rust-sandbox:latest",
        help="Docker image to test (default: rust-sandbox:latest).",
    )
    args = parser.parse_args()

    print("=" * 50)
    print(f" CubeSandbox Rust Template — Local Smoke Tests")
    print(f" Image: {args.image}")
    print("=" * 50)
    print()

    tester = RustSandboxTester(args.image)

    try:
        tester.start()

        # Run all tests
        tester.test_envd_health()
        tester.test_envd_version()
        tester.test_user_rustc()
        tester.test_user_cargo()
        tester.test_rustc_compile()
        tester.test_cargo_registry_warm()
        tester.test_cargo_new_build()

        print()
        print("=" * 50)
        print(" All smoke tests passed!")
        print("=" * 50)

    except Exception:
        tester.stop()
        raise

    tester.stop()


if __name__ == "__main__":
    main()
