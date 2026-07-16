#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Demonstrate OpenCode session persistence across a CubeSandbox pause/resume.

Turn 1 asks OpenCode to write ``/workspace/plan.md``, then the sandbox is paused.
Turn 2 reconnects to the same sandbox, verifies both ``/workspace`` and the
OpenCode state directory survived the snapshot, and asks OpenCode to continue
the work.

Lifecycle note: this script deliberately avoids ``with Sandbox.create(...)``.
A context manager kills the sandbox on ``__exit__``, which would defeat the
pause. The lifecycle is managed manually with try/finally so the sandbox stays
alive between turns and is only killed at the very end.
"""

from __future__ import annotations

import argparse
import os
import shlex
import sys

from e2b import Sandbox

from env_utils import (
    build_opencode_env,
    int_env,
    load_local_dotenv,
    opencode_provider,
    opencode_run_command,
    opencode_workspace,
    require_provider_key,
    required,
    shell_join,
)
from _common import ensure_success, run_command, sandbox_identifier

DEFAULT_OPENCODE_STATE_DIR = "/root/.opencode"

TURN_1_PROMPT = (
    "Create {workspace}/plan.md containing a numbered 3-step plan for building a "
    "small Python CLI that prints the current time. Only write the plan file."
)
TURN_2_PROMPT = (
    "Read {workspace}/plan.md and implement step 1 by creating "
    "{workspace}/progress.md that records which step you completed and why. "
    "Do not delete plan.md."
)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Demonstrate OpenCode session persistence across a CubeSandbox pause/resume."
    )
    parser.add_argument(
        "--template",
        default=os.environ.get("CUBE_TEMPLATE_ID"),
        help="CubeSandbox template ID. Defaults to CUBE_TEMPLATE_ID.",
    )
    parser.add_argument(
        "--workspace",
        default=opencode_workspace(),
        help="Working directory inside the sandbox. Defaults to OPENCODE_WORKSPACE.",
    )
    parser.add_argument(
        "--opencode-state-dir",
        default=os.environ.get("OPENCODE_DIR", DEFAULT_OPENCODE_STATE_DIR),
        help="OpenCode state directory checked for survival after resume.",
    )
    parser.add_argument(
        "--sandbox-timeout",
        type=int,
        default=int_env("OPENCODE_SANDBOX_TIMEOUT", 1800),
        help="Sandbox lifetime in seconds. Defaults to OPENCODE_SANDBOX_TIMEOUT or 1800.",
    )
    parser.add_argument(
        "--exec-timeout",
        type=int,
        default=int_env("OPENCODE_EXEC_TIMEOUT", 900),
        help="OpenCode command timeout in seconds. Defaults to OPENCODE_EXEC_TIMEOUT or 900.",
    )
    parser.add_argument(
        "--raw",
        action="store_true",
        help="Keep OpenCode's raw streamed JSON output.",
    )
    return parser.parse_args()


def run_turn(
    sandbox: Sandbox,
    workspace: str,
    prompt: str,
    exec_timeout: int,
    envs: dict[str, str],
    raw: bool = False,
):
    command = shell_join(
        f"cd {shlex.quote(workspace)}",
        opencode_run_command(prompt),
    )
    return run_command(
        sandbox,
        command,
        cwd=workspace,
        envs=envs,
        timeout=exec_timeout,
        stream=True,
        raw=raw,
    )


def assert_state_survived(sandbox: Sandbox, workspace: str, state_dir: str) -> None:
    quoted_workspace = shlex.quote(workspace)
    quoted_state = shlex.quote(state_dir)
    command = shell_join(
        f"test -f {quoted_workspace}/plan.md",
        f"test -d {quoted_state}",
        "printf '\\n--- plan.md (survived pause/resume) ---\\n'",
        f"cat {quoted_workspace}/plan.md",
    )
    result = run_command(sandbox, command, timeout=60)
    ensure_success(result, "verify /workspace and OpenCode state survived pause/resume")
    if getattr(result, "stdout", ""):
        print(result.stdout)


def show_final_workspace(sandbox: Sandbox, workspace: str) -> None:
    quoted_workspace = shlex.quote(workspace)
    command = shell_join(
        f"ls -la {quoted_workspace}",
        f"test ! -f {quoted_workspace}/progress.md || "
        f"(printf '\\n--- progress.md ---\\n' && cat {quoted_workspace}/progress.md)",
    )
    result = run_command(sandbox, command, timeout=60)
    ensure_success(result, "inspect final workspace")
    if getattr(result, "stdout", ""):
        print(result.stdout)


def main() -> int:
    load_local_dotenv()
    args = parse_args()

    template_id = args.template or required("CUBE_TEMPLATE_ID")
    required("E2B_API_URL")
    required("E2B_API_KEY")
    require_provider_key(opencode_provider())

    opencode_env = build_opencode_env()
    turn_1_prompt = TURN_1_PROMPT.format(workspace=args.workspace)
    turn_2_prompt = TURN_2_PROMPT.format(workspace=args.workspace)

    print(f"Creating sandbox from template: {template_id}")
    # SECURITY: like run_opencode.py this demo keeps egress open and injects the
    # key per command. The pause() snapshot also captures the in-VM env and any
    # credentials OpenCode caches under /root/.opencode, widening exposure — for
    # shared clusters prefer the default-deny + vault pattern in network_policy.py.
    sandbox = None
    sandbox_id = "uncreated"
    try:
        sandbox = Sandbox.create(template=template_id, timeout=args.sandbox_timeout)
        sandbox_id = sandbox_identifier(sandbox)
        print(f"Sandbox ready: {sandbox_id}")

        version_result = run_command(sandbox, "opencode --version", timeout=60)
        ensure_success(version_result, "check OpenCode version")
        print(f"OpenCode version: {getattr(version_result, 'stdout', '').strip()}")

        print("\n=== Turn 1: create plan.md ===\n")
        result_1 = run_turn(
            sandbox, args.workspace, turn_1_prompt, args.exec_timeout, opencode_env, args.raw
        )
        ensure_success(result_1, "run OpenCode turn 1")

        print(f"\nPausing sandbox {sandbox_id} (snapshotting VM + rootfs)...")
        paused_id = sandbox.pause()
        if not paused_id:
            raise SystemExit("Failed to pause sandbox: no snapshot ID returned")
        # The sandbox_id is stable across pause. Some SDK versions return the
        # resume handle as a string; others return a bool (success). Only adopt
        # a string handle, otherwise keep the original id for connect().
        if isinstance(paused_id, str):
            sandbox_id = paused_id
        print(f"Paused. Resume handle: {sandbox_id}")

        print(f"\nReconnecting to {sandbox_id}...")
        sandbox = Sandbox.connect(sandbox_id=sandbox_id, timeout=args.sandbox_timeout)
        print("Reconnected after resume.")

        print("\n=== Verifying persistence after resume ===\n")
        assert_state_survived(sandbox, args.workspace, args.opencode_state_dir)

        print("\n=== Turn 2: continue the work ===\n")
        result_2 = run_turn(
            sandbox, args.workspace, turn_2_prompt, args.exec_timeout, opencode_env, args.raw
        )
        ensure_success(result_2, "run OpenCode turn 2")

        show_final_workspace(sandbox, args.workspace)

        exit_code = getattr(result_2, "exit_code", 0)
        return 0 if exit_code is None else int(exit_code)
    finally:
        if sandbox is not None:
            try:
                sandbox.kill()
                print(f"\nSandbox {sandbox_id} killed.")
            except Exception as exc:  # noqa: BLE001 - cleanup must not mask real errors
                print(
                    f"Warning: failed to kill sandbox {sandbox_id}: {exc}",
                    file=sys.stderr,
                )


if __name__ == "__main__":
    sys.exit(main())
