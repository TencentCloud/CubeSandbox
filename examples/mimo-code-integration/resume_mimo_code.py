#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Prove MiMo conversation state survives a CubeSandbox pause and reconnect."""

from __future__ import annotations

import argparse
import os
import secrets
import shlex
import sys

from e2b import Sandbox

from _mimo_common import (
    ensure_success,
    kill_sandbox,
    run_command,
    run_mimo_command,
    sandbox_identifier,
    session_id_from_events,
    session_list_contains,
)
from env_utils import (
    build_mimo_env,
    int_env,
    load_local_dotenv,
    mimo_command,
    mimocode_home,
    mimo_workspace,
    positive_int,
    required,
    session_list_command,
    shell_join,
)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Pause, reconnect, and continue one MiMo Code session."
    )
    parser.add_argument("--template", default=os.environ.get("CUBE_TEMPLATE_ID"))
    parser.add_argument("--workspace", default=mimo_workspace())
    parser.add_argument(
        "--sandbox-timeout",
        type=positive_int,
        default=int_env("MIMO_SANDBOX_TIMEOUT", 1800),
    )
    parser.add_argument(
        "--exec-timeout",
        type=positive_int,
        default=int_env("MIMO_AGENT_EXEC_TIMEOUT", 900),
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
):
    command = mimo_command(
        prompt,
        workspace=workspace,
        session_id=session_id,
        agent="build",
    )
    return run_mimo_command(
        sandbox,
        command,
        cwd=workspace,
        envs=envs,
        timeout=timeout,
    )


def verify_turn_one(
    sandbox: Sandbox, workspace: str, resume_token: str
) -> None:
    plan_file = shlex.quote(f"{workspace}/plan.md")
    directory = shlex.quote(workspace)
    token = shlex.quote(resume_token)
    command = shell_join(
        f"test -f {plan_file}",
        f"! grep -R -Fq -- {token} {directory}",
        f"cat {plan_file}",
    )
    result = run_command(sandbox, command, timeout=60)
    ensure_success(
        result,
        "verify turn one (the resume token must stay in conversation state only)",
    )


def verify_persisted_state(
    sandbox: Sandbox,
    *,
    workspace: str,
    session_id: str,
    envs: dict[str, str],
) -> None:
    home = shlex.quote(mimocode_home())
    plan_file = shlex.quote(f"{workspace}/plan.md")
    command = shell_join(
        f"test -f {plan_file}",
        f"test -d {home}/data",
        f"test -n \"$(ls -A {home}/data)\"",
    )
    result = run_command(sandbox, command, timeout=60)
    ensure_success(result, "verify workspace and MiMo profile persistence")

    sessions = run_command(
        sandbox,
        session_list_command(workspace),
        cwd=workspace,
        envs=envs,
        timeout=60,
    )
    ensure_success(sessions, "list persisted MiMo sessions")
    if not session_list_contains(getattr(sessions, "stdout", ""), session_id):
        raise SystemExit(
            f"Persisted MiMo session list does not contain {session_id!r}"
        )
    print(f"Persisted session found: {session_id}")


def verify_turn_two(
    sandbox: Sandbox, workspace: str, resume_token: str
) -> None:
    result_file = shlex.quote(f"{workspace}/resumed.md")
    command = shell_join(
        f"test -f {result_file}",
        f"grep -Fq CUBE_MIMO_RESUME_OK {result_file}",
        f"grep -Fq -- {shlex.quote(resume_token)} {result_file}",
        f"printf '\\n--- resumed.md ---\\n' && cat {result_file}",
    )
    result = run_command(sandbox, command, timeout=60)
    ensure_success(result, "verify resumed MiMo conversation")
    if getattr(result, "stdout", ""):
        print(result.stdout)


def main() -> int:
    load_local_dotenv()
    args = parse_args()
    if args.raw:
        os.environ["MIMO_STREAM_RAW"] = "1"

    template_id = args.template or required("CUBE_TEMPLATE_ID")
    required("E2B_API_URL")
    required("E2B_API_KEY")
    envs = build_mimo_env(include_secret=True)
    resume_token = f"CUBE-MIMO-{secrets.token_hex(8).upper()}"

    sandbox = Sandbox.create(template=template_id, timeout=args.sandbox_timeout)
    sandbox_id = sandbox_identifier(sandbox)
    try:
        print(f"Sandbox ready: {sandbox_id}")
        first_prompt = (
            f"Remember the exact token {resume_token} for our next turn, but do not "
            f"write it to any file. Create {args.workspace}/plan.md containing a "
            "three-step plan for a small Python CLI. Only write the plan file."
        )
        print("\n=== Turn 1: create a session and plan ===\n")
        first_result, first_events = run_turn(
            sandbox,
            workspace=args.workspace,
            prompt=first_prompt,
            envs=envs,
            timeout=args.exec_timeout,
        )
        ensure_success(first_result, "run MiMo turn one")
        session_id = session_id_from_events(first_events)
        print(f"\nCaptured MiMo session ID: {session_id}")
        verify_turn_one(sandbox, args.workspace, resume_token)

        print(f"\nPausing sandbox {sandbox_id}...")
        # CubeSandbox and E2B pause() both preserve the existing sandbox ID
        # and return None.
        sandbox.pause()
        print(f"Paused. Resume handle: {sandbox_id}")

        sandbox = Sandbox.connect(sandbox_id=sandbox_id)
        print("Reconnected to the paused sandbox.")
        verify_persisted_state(
            sandbox,
            workspace=args.workspace,
            session_id=session_id,
            envs=build_mimo_env(include_secret=False),
        )

        second_prompt = (
            f"Continue the previous task. Write {args.workspace}/resumed.md with "
            "the exact token I asked you to remember and the marker "
            "CUBE_MIMO_RESUME_OK. Do not read the token from workspace files."
        )
        print("\n=== Turn 2: continue the exact session ===\n")
        second_result, second_events = run_turn(
            sandbox,
            workspace=args.workspace,
            prompt=second_prompt,
            envs=envs,
            timeout=args.exec_timeout,
            session_id=session_id,
        )
        ensure_success(second_result, "run MiMo turn two")
        if session_id_from_events(second_events) != session_id:
            raise SystemExit("MiMo Code continued under an unexpected session ID")
        verify_turn_two(sandbox, args.workspace, resume_token)
        return 0
    finally:
        kill_sandbox(
            sandbox,
            sandbox_id,
            run_failed=sys.exc_info()[0] is not None,
        )


if __name__ == "__main__":
    sys.exit(main())
