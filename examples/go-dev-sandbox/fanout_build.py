# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
fanout_build.py — parallel cross-compile fan-out across sandboxes.

One sandbox per GOOS/GOARCH target, all building the same source tree in
parallel. The source is shared into every sandbox through a read-only
host mount; each sandbox drops its binary into a shared read-write host
mount, so when the fan-out finishes the host holds one `dist/` directory
with a binary per platform — no file upload or download involved.

  host: <work-dir>/<run-id>/src   ──ro──►  /mnt/src   (same tree in every sandbox)
  host: <work-dir>/<run-id>/dist  ◄──rw──  /mnt/dist  (one binary per target)

This script must run on the sandbox host node (any single-node install
qualifies): host mounts map node-local paths, and `hostPath` must live
under an allowed prefix — `/data/shared/` by default. One-time setup:

  sudo install -d -o "$(id -u)" -g "$(id -g)" /data/shared/go-fanout

Environment (besides the shared ones in env.py):
  FANOUT_TARGETS   comma-separated GOOS/GOARCH pairs
                   (default: linux/amd64,linux/arm64)
  FANOUT_WORK_DIR  workspace root under an allowed mount prefix
                   (default: /data/shared/go-fanout)
"""

import json
import os
import sys
import time
import uuid
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

from cubesandbox import Sandbox

from env import TEMPLATE_ID, check

TARGETS = [
    tuple(t.strip().split("/"))
    for t in os.environ.get("FANOUT_TARGETS", "linux/amd64,linux/arm64").split(",")
]
WORK_ROOT = Path(os.environ.get("FANOUT_WORK_DIR", "/data/shared/go-fanout"))

GO_MOD = """module fanout

go 1.24
"""

MAIN_GO = """package main

import (
	"fmt"
	"runtime"
)

func main() {
	fmt.Printf("built for %s/%s by %s\\n", runtime.GOOS, runtime.GOARCH, runtime.Version())
}
"""


def prepare_workspace() -> Path:
    run_dir = WORK_ROOT / uuid.uuid4().hex[:8]
    try:
        (run_dir / "src").mkdir(parents=True)
        (run_dir / "dist").mkdir()
    except PermissionError:
        sys.stderr.write(
            f"ERROR: cannot create {run_dir}.\n"
            f"  Create the workspace root once with:\n"
            f'    sudo install -d -o "$(id -u)" -g "$(id -g)" {WORK_ROOT}\n'
        )
        sys.exit(2)
    (run_dir / "src" / "go.mod").write_text(GO_MOD)
    (run_dir / "src" / "main.go").write_text(MAIN_GO)
    return run_dir


def build_one(run_dir: Path, goos: str, goarch: str, verify_ro: bool) -> float:
    """Build one target in its own sandbox; returns the wall time in seconds."""
    tag = f"[{goos}/{goarch}]"
    mounts = json.dumps([
        {"hostPath": str(run_dir / "src"), "mountPath": "/mnt/src", "readOnly": True},
        {"hostPath": str(run_dir / "dist"), "mountPath": "/mnt/dist", "readOnly": False},
    ])
    started = time.monotonic()
    with Sandbox.create(template=TEMPLATE_ID, metadata={"host-mount": mounts}) as sb:
        print(f"{tag} sandbox: {sb.sandbox_id}")

        if verify_ro:
            r = sb.commands.run("touch /mnt/src/should-fail 2>&1 || echo read-only enforced")
            print(f"{tag} {r.stdout.strip()}")

        out = f"/mnt/dist/hello-{goos}-{goarch}" + (".exe" if goos == "windows" else "")
        check(
            sb.commands.run(f"GOOS={goos} GOARCH={goarch} go build -C /mnt/src -o {out} ."),
            f"go build {goos}/{goarch}",
        )
        print(f"{tag} build ok")

        # If this target matches the sandbox's own platform, prove the binary
        # is real by executing it right out of the shared dist mount.
        native = sb.commands.run("go env GOOS GOARCH").stdout.split()
        if [goos, goarch] == native:
            r = check(sb.commands.run(out), "run native binary")
            print(f"{tag} runs natively: {r.stdout.strip()}")
    return time.monotonic() - started


run_dir = prepare_workspace()
print(f"workspace: {run_dir}")
print(f"targets:   {', '.join('/'.join(t) for t in TARGETS)}")

def build_with_retry(run_dir: Path, goos: str, goarch: str, verify_ro: bool) -> float:
    """One retry absorbs the occasional creation timeout on slow hosts."""
    try:
        return build_one(run_dir, goos, goarch, verify_ro)
    except Exception as exc:  # noqa: BLE001 — creation timeouts surface as ApiError
        print(f"[{goos}/{goarch}] retrying after: {exc}")
        return build_one(run_dir, goos, goarch, verify_ro)


started = time.monotonic()
with ThreadPoolExecutor(max_workers=len(TARGETS)) as pool:
    futures = [
        pool.submit(build_with_retry, run_dir, goos, goarch, verify_ro=(i == 0))
        for i, (goos, goarch) in enumerate(TARGETS)
    ]
    times = [f.result() for f in futures]
total = time.monotonic() - started

print("\ndist/ on the host:")
for p in sorted((run_dir / "dist").iterdir()):
    print(f"  {p.name:24s} {p.stat().st_size / 1e6:5.1f} MB")

print(f"\nper-target: {', '.join(f'{t:.0f}s' for t in times)}  wall total: {total:.0f}s")
print(f"artifacts left in {run_dir / 'dist'} — clean up old runs when done")
print("OK")
