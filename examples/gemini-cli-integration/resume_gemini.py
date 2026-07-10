#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Verify Gemini CLI workspace persistence across CubeSandbox pause/resume."""

from __future__ import annotations

import argparse
import os
import shlex
import sys

from e2b import Sandbox

from common import (
    ensure_success,
    gemini_command,
    int_env,
    load_dotenv,
    required,
    run_command,
    sandbox_id,
    shell_join,
)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--template", default=os.environ.get("CUBE_TEMPLATE_ID"))
    parser.add_argument("--model", default=os.environ.get("GEMINI_MODEL"))
    parser.add_argument("--workspace", default=os.environ.get("GEMINI_WORKSPACE", "/workspace"))
    parser.add_argument("--approve-all", action="store_true")
    parser.add_argument(
        "--sandbox-timeout", type=int, default=int_env("GEMINI_SANDBOX_TIMEOUT", 1800)
    )
    parser.add_argument("--exec-timeout", type=int, default=int_env("GEMINI_EXEC_TIMEOUT", 900))
    return parser.parse_args()


def run_turn(sandbox: Sandbox, prompt: str, args: argparse.Namespace, envs: dict[str, str]):
    command = shell_join(
        f"cd {shlex.quote(args.workspace)}",
        gemini_command(prompt, model=args.model, approve_all=args.approve_all),
    )
    return run_command(
        sandbox,
        command,
        cwd=args.workspace,
        envs=envs,
        timeout=args.exec_timeout,
        user="root",
    )


def main() -> int:
    load_dotenv()
    args = parse_args()
    template = args.template or required("CUBE_TEMPLATE_ID")
    required("E2B_API_URL")
    required("E2B_API_KEY")
    envs = {"GEMINI_API_KEY": required("GEMINI_API_KEY")}
    args.approve_all = args.approve_all or os.environ.get("GEMINI_APPROVE_ALL") == "1"

    sandbox = Sandbox.create(template=template, timeout=args.sandbox_timeout)
    current_id = sandbox_id(sandbox)
    try:
        setup = run_command(
            sandbox,
            f"mkdir -p {shlex.quote(args.workspace)}",
            timeout=60,
            user="root",
        )
        ensure_success(setup, "create workspace")

        first = run_turn(
            sandbox,
            f"Create {args.workspace}/plan.md with a numbered three-step plan for a small Python CLI. Only create plan.md.",
            args,
            envs,
        )
        ensure_success(first, "run Gemini CLI turn 1")

        print(f"Pausing {current_id}; CubeSandbox snapshots the VM and writable filesystem.")
        resume_handle = sandbox.pause()
        if isinstance(resume_handle, str) and resume_handle:
            current_id = resume_handle
        sandbox = Sandbox.connect(sandbox_id=current_id)

        persisted = run_command(
            sandbox,
            f"test -f {shlex.quote(args.workspace)}/plan.md && cat {shlex.quote(args.workspace)}/plan.md",
            timeout=60,
            user="root",
        )
        ensure_success(persisted, "verify plan.md survived pause/resume")
        print("\n--- persisted plan.md ---\n" + getattr(persisted, "stdout", ""))

        second = run_turn(
            sandbox,
            f"Read {args.workspace}/plan.md and create {args.workspace}/progress.md describing completion of step 1. Do not delete plan.md.",
            args,
            envs,
        )
        ensure_success(second, "run Gemini CLI turn 2")
        return 0
    finally:
        try:
            sandbox.kill()
            print(f"Sandbox {current_id} killed.")
        except Exception as exc:  # noqa: BLE001
            print(f"Warning: failed to kill sandbox {current_id}: {exc}", file=sys.stderr)


if __name__ == "__main__":
    sys.exit(main())
