#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Prove workspace and OpenCode session persistence across pause/resume."""

from __future__ import annotations

import argparse
import os
import shlex
import sys

from _opencode_common import (
    ensure_success,
    extract_session_id,
    run_command,
    sandbox_identifier,
)
from e2b import Sandbox
from env_utils import (
    DEFAULT_STATE_DIR,
    build_opencode_env,
    int_env,
    load_local_dotenv,
    opencode_command,
    opencode_workspace,
    required,
    shell_join,
)

TURN_1 = """\
Create {workspace}/plan.md with exactly three numbered steps for a tiny Python
CLI that prints the current UTC time. Only create the plan and briefly confirm.
"""
TURN_2 = """\
Continue the same task. Read the plan you created in the previous turn and
implement only step 1 in {workspace}/progress.md. Mention the previous plan in
your final response. Do not delete plan.md.
"""


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--template",
        default=os.environ.get("CUBE_TEMPLATE_ID"),
        help="Cube template ID. Defaults to CUBE_TEMPLATE_ID.",
    )
    parser.add_argument("--workspace", default=opencode_workspace())
    parser.add_argument(
        "--state-dir",
        default=os.environ.get("OPENCODE_STATE_DIR", DEFAULT_STATE_DIR),
    )
    parser.add_argument(
        "--sandbox-timeout",
        type=int,
        default=int_env("OPENCODE_SANDBOX_TIMEOUT", 1800),
    )
    parser.add_argument(
        "--exec-timeout",
        type=int,
        default=int_env("OPENCODE_EXEC_TIMEOUT", 900),
    )
    parser.add_argument("--raw", action="store_true")
    return parser.parse_args()


def run_turn(
    sandbox: Sandbox,
    *,
    workspace: str,
    prompt: str,
    envs: dict[str, str],
    timeout: int,
    session_id: str | None = None,
) -> object:
    result = run_command(
        sandbox,
        opencode_command(prompt, session_id=session_id),
        cwd=workspace,
        envs=envs,
        timeout=timeout,
        stream=True,
    )
    ensure_success(result, "run OpenCode turn")
    return result


def verify_state(sandbox: Sandbox, workspace: str, state_dir: str) -> None:
    command = shell_join(
        f"test -s {shlex.quote(workspace)}/plan.md",
        f"test -d {shlex.quote(state_dir)}",
        "printf '\\n--- plan.md survived pause/resume ---\\n'",
        f"cat {shlex.quote(workspace)}/plan.md",
    )
    result = run_command(sandbox, command, timeout=60)
    ensure_success(result, "verify workspace and OpenCode state")
    print(getattr(result, "stdout", ""))


def verify_continuation(sandbox: Sandbox, workspace: str) -> None:
    command = shell_join(
        f"test -s {shlex.quote(workspace)}/progress.md",
        "printf '\\n--- progress.md ---\\n'",
        f"cat {shlex.quote(workspace)}/progress.md",
    )
    result = run_command(sandbox, command, timeout=60)
    ensure_success(result, "verify the resumed turn")
    print(getattr(result, "stdout", ""))


def main() -> int:
    load_local_dotenv()
    args = parse_args()
    if args.raw:
        os.environ["OPENCODE_STREAM_RAW"] = "1"

    template_id = args.template or required("CUBE_TEMPLATE_ID")
    required("E2B_API_URL")
    required("E2B_API_KEY")
    envs = build_opencode_env(include_secret=True)

    sandbox = Sandbox.create(template=template_id, timeout=args.sandbox_timeout)
    sandbox_id = sandbox_identifier(sandbox)
    try:
        print(f"Sandbox ready: {sandbox_id}")
        turn_1 = TURN_1.format(workspace=args.workspace)
        result_1 = run_turn(
            sandbox,
            workspace=args.workspace,
            prompt=turn_1,
            envs=envs,
            timeout=args.exec_timeout,
        )
        session_id = extract_session_id(getattr(result_1, "stdout", ""))
        print(f"OpenCode session: {session_id}")

        paused = sandbox.pause()
        if isinstance(paused, str) and paused:
            sandbox_id = paused
        print(f"Paused. Resume handle: {sandbox_id}")

        sandbox = Sandbox.connect(sandbox_id=sandbox_id)
        print("Reconnected.")
        verify_state(sandbox, args.workspace, args.state_dir)

        turn_2 = TURN_2.format(workspace=args.workspace)
        result_2 = run_turn(
            sandbox,
            workspace=args.workspace,
            prompt=turn_2,
            envs=envs,
            timeout=args.exec_timeout,
            session_id=session_id,
        )
        verify_continuation(sandbox, args.workspace)
        exit_code = getattr(result_2, "exit_code", 0)
        return 0 if exit_code is None else int(exit_code)
    finally:
        try:
            sandbox.kill()
            print(f"Sandbox {sandbox_id} killed.")
        except Exception as exc:  # noqa: BLE001 - cleanup must not mask the task error
            print(f"Warning: failed to kill {sandbox_id}: {exc}", file=sys.stderr)


if __name__ == "__main__":
    sys.exit(main())
