#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Resume a paused CodeBuddy CI session from a CubeSandbox snapshot."""

from __future__ import annotations

import argparse
import shlex
import sys

from e2b import Sandbox

from config import codebuddy_command, codebuddy_env, load_local_dotenv, positive_int, required, shell_join, workspace


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("sandbox_id", help="Paused CubeSandbox ID to reconnect.")
    parser.add_argument(
        "--prompt",
        default="Continue the CI task, read the existing report, and update it with the final test result. Do not commit or push changes.",
    )
    parser.add_argument("--session-id", default=None)
    return parser.parse_args()


def ensure_success(result, action: str) -> None:
    if getattr(result, "exit_code", None) not in (None, 0):
        raise SystemExit(f"Failed to {action}: {getattr(result, 'stderr', '')}")


def main() -> int:
    load_local_dotenv()
    args = parse_args()
    required("E2B_API_URL")
    required("E2B_API_KEY")
    command_env = codebuddy_env()
    session_id = args.session_id or required("CODEBUDDY_SESSION_ID")
    target_workspace = workspace()
    command = shell_join(
        f"cd {shlex.quote(target_workspace)}",
        codebuddy_command(
            args.prompt,
            session_id=session_id,
            max_turns=positive_int("CODEBUDDY_MAX_TURNS", 3),
            resume=True,
        ),
    )
    sandbox = Sandbox.connect(args.sandbox_id)
    result = sandbox.commands.run(
        command,
        cwd=target_workspace,
        envs=command_env,
        timeout=positive_int("CODEBUDDY_EXEC_TIMEOUT", 900),
        user="root",
    )
    ensure_success(result, "resume CodeBuddy CI task")
    print(getattr(result, "stdout", ""))
    return 0


if __name__ == "__main__":
    sys.exit(main())
