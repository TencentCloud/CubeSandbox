#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""
resume_claude.py — long-running Claude Code sessions with pause/resume.

Demonstrates:
  1. Create sandbox from the Claude Code template.
  2. Ask Claude to start a task; write a small note into /workspace so we can
     later verify the pause/resume round-trip preserved the filesystem.
  3. Pause the sandbox — resources released, VM snapshot on disk.
  4. Resume; ask Claude to continue by inspecting the note. The rootfs, plus
     any partial state under /root/.claude/, is intact.

Usage:
    python resume_claude.py
"""

from __future__ import annotations

import argparse
import shlex
import sys
import time

from e2b import Sandbox

from env_utils import build_agent_env, load_local_dotenv, required


PROMPT_1 = (
    "Write a file called plan.md that lists three ideas for improving a "
    "Python CLI. Then say 'done'."
)
PROMPT_2 = (
    "Read plan.md, pick the first idea, and write a one-paragraph rationale "
    "into rationale.md."
)


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--workspace", default="/workspace")
    p.add_argument("--pause-seconds", type=int, default=5)
    return p.parse_args()


def run_claude(sandbox: Sandbox, prompt: str, workspace: str, envs: dict[str, str]) -> int:
    cmd = (
        f"cd {shlex.quote(workspace)} && "
        f"claude --print --allowedTools 'Edit,Write,Read,Bash(ls:*)' -- {shlex.quote(prompt)}"
    )
    result = sandbox.commands.run(
        cmd, envs=envs, user="root", timeout=300,
        on_stdout=lambda m: sys.stdout.write(m),
        on_stderr=lambda m: sys.stderr.write(m),
    )
    print(f"\n[claude] exit_code={result.exit_code}")
    return result.exit_code or 0


def main() -> int:
    args = parse_args()
    load_local_dotenv()
    template_id = required("CUBE_TEMPLATE_ID")
    required("ANTHROPIC_API_KEY")
    envs = build_agent_env()

    print(f"[cube] creating sandbox from template={template_id}")
    sandbox = Sandbox.create(template=template_id, timeout=1800)

    sid: str | None = None
    failures = 0
    try:
        sid = sandbox.sandbox_id
        print(f"[cube] sandbox_id={sid}")

        sandbox.commands.run(f"mkdir -p {shlex.quote(args.workspace)}", user="root", timeout=15)

        print("\n=== Step 1: initial task ===")
        failures += 1 if run_claude(sandbox, PROMPT_1, args.workspace, envs) else 0

        listing1 = sandbox.commands.run(f"ls -la {shlex.quote(args.workspace)}", user="root")
        print(listing1.stdout)

        print(f"\n=== Step 2: pausing sandbox for {args.pause_seconds}s ===")
        sandbox.pause()
        for _ in range(args.pause_seconds):
            time.sleep(1)
            print(".", end="", flush=True)
        print()

        print("\n=== Step 3: resume + continue ===")
        sandbox = Sandbox.connect(sid)
        failures += 1 if run_claude(sandbox, PROMPT_2, args.workspace, envs) else 0

        listing2 = sandbox.commands.run(f"ls -la {shlex.quote(args.workspace)}", user="root")
        print(listing2.stdout)

        rationale = sandbox.commands.run(
            f"cat {shlex.quote(args.workspace)}/rationale.md", user="root",
        )
        print("\n=== rationale.md ===")
        print(rationale.stdout)

        return 0 if failures == 0 else 1
    finally:
        try:
            sandbox.kill()
        except Exception as exc:  # noqa: BLE001
            print(f"[cleanup] sandbox.kill() raised {exc!r}")


if __name__ == "__main__":
    sys.exit(main())
