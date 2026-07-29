# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Demonstrate full-VM pause/resume and cross-object re-attach underneath a
live OpenHands agent server.

OpenHands sessions are long by nature — an agent can spend minutes to hours
on a task. With DockerWorkspace, stopping means losing the session.
CubeSandbox pauses the *entire MicroVM*: agent server, bash sessions, and any
in-flight processes are frozen mid-instruction and thawed later, bit-for-bit.

This demo needs no LLM key and shows the full round trip:

  1. start a 1-second ticker inside the sandbox (via the agent server);
  2. pause the VM while the host keeps counting wall-clock time;
  3. drop the workspace object entirely (``kill_on_exit=False`` keeps the
     paused sandbox alive) — as if the Python process had exited;
  4. re-attach from a fresh ``CubeSandboxWorkspace(sandbox_id=...)``, which
     auto-resumes the VM, and show the ticker continued from the exact
     frozen instant — the wall-clock gap left no hole in the sequence.

Usage:
    python pause_resume.py

Requires .env (or exported env): E2B_API_URL, E2B_API_KEY, CUBE_TEMPLATE_ID.
"""

import os
import sys
import time
from pathlib import Path

from dotenv import load_dotenv
from e2b_code_interpreter import Sandbox

from cubesandbox_workspace import CubeSandboxWorkspace

load_dotenv(Path(__file__).resolve().parent / ".env")

PAUSE_WALL_SECONDS = 8


def tick_count(workspace: CubeSandboxWorkspace) -> int:
    result = workspace.execute_command("wc -l < /workspace/ticks.log || echo 0")
    try:
        return int(result.stdout.strip().splitlines()[-1])
    except (ValueError, IndexError):
        return 0


def main() -> int:
    missing = [
        k
        for k in ("E2B_API_URL", "E2B_API_KEY", "CUBE_TEMPLATE_ID")
        if not os.getenv(k)
    ]
    if missing:
        print(f"missing environment variables: {', '.join(missing)}")
        print("copy .env.example to .env and fill it in first")
        return 2

    # kill_on_exit=False: cleanup() detaches instead of killing, so the
    # paused sandbox survives this object — and would survive this process.
    workspace = CubeSandboxWorkspace(
        template=os.environ["CUBE_TEMPLATE_ID"], kill_on_exit=False
    )
    sandbox_id = workspace.sandbox_id
    # Until the re-attach hand-off succeeds, any failure must reap the
    # deliberately-kept sandbox — nobody else would.
    try:
        print(f"sandbox {sandbox_id} up, agent server at {workspace.host}")

        print("starting a 1s ticker inside the MicroVM (via the agent server) ...")
        workspace.execute_command(
            "rm -f /workspace/ticks.log && "
            "nohup sh -c 'i=0; while true; do i=$((i+1)); "
            "echo $i >> /workspace/ticks.log; sleep 1; done' "
            ">/dev/null 2>&1 & echo started"
        )
        time.sleep(4)
        before = tick_count(workspace)
        print(f"ticks before pause: {before}")

        print("pausing the VM (memory + filesystem snapshot) ...")
        workspace.pause()
        print("dropping the workspace object — the paused sandbox stays alive ...")
        workspace.cleanup()

        print(
            f"host waits {PAUSE_WALL_SECONDS}s of wall-clock time while the VM is frozen ..."
        )
        time.sleep(PAUSE_WALL_SECONDS)

        print(
            f"re-attaching to {sandbox_id} from a fresh workspace object (auto-resume) ..."
        )
        reattached = CubeSandboxWorkspace(sandbox_id=sandbox_id, kill_on_exit=True)
    except BaseException:
        try:
            Sandbox.connect(sandbox_id).kill()
        except Exception:  # noqa: BLE001, S110 - best-effort reap before re-raise
            pass
        raise
    try:
        just_after = tick_count(reattached)
        time.sleep(3)
        later = tick_count(reattached)
    finally:
        reattached.cleanup()

    print()
    print(f"ticks before pause              : {before}")
    print(f"ticks right after re-attach     : {just_after}")
    print(f"ticks 3s after re-attach        : {later}")
    print(f"host wall-clock frozen gap      : {PAUSE_WALL_SECONDS}s")

    frozen_ok = just_after - before <= 2  # no ticks happened while frozen
    resumed_ok = later > just_after  # ticker continued after thaw
    if frozen_ok and resumed_ok:
        print(
            "\nPASS: the ticker (and the agent server around it) was frozen "
            "mid-flight, survived the death of the original workspace object, "
            "and continued from the exact same instant after re-attach — "
            f"the {PAUSE_WALL_SECONDS}s host-side gap left no hole in the sequence."
        )
        return 0
    print("\nFAIL: unexpected tick progression — see numbers above")
    return 1


if __name__ == "__main__":
    sys.exit(main())
