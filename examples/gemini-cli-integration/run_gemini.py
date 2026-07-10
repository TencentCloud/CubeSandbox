#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Run a Gemini CLI coding task inside a CubeSandbox MicroVM.

The default mode demonstrates host-side secret injection through the E2B-
compatible SDK. For production, use network_policy.py so CubeEgress injects the
key on the wire and the real secret never enters the sandbox.
"""

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
    parser.add_argument(
        "--prompt",
        default=(
            "Inspect the project, run python3 app.py, and write a concise summary "
            "of the result to /workspace/result.md."
        ),
    )
    parser.add_argument("--approve-all", action="store_true", help="Pass --yolo to Gemini CLI.")
    parser.add_argument(
        "--sandbox-timeout", type=int, default=int_env("GEMINI_SANDBOX_TIMEOUT", 1800)
    )
    parser.add_argument("--exec-timeout", type=int, default=int_env("GEMINI_EXEC_TIMEOUT", 900))
    parser.add_argument("--no-seed", action="store_true")
    return parser.parse_args()


def seed_workspace(sandbox: Sandbox, workspace: str) -> None:
    quoted = shlex.quote(workspace)
    command = f"""mkdir -p {quoted}
cat > {quoted}/app.py <<'EOF'
def main() -> None:
    print("hello from CubeSandbox + Gemini CLI")


if __name__ == "__main__":
    main()
EOF
cat > {quoted}/README.md <<'EOF'
# Gemini CLI CubeSandbox smoke project
EOF
"""
    ensure_success(run_command(sandbox, command, timeout=60, user="root"), "seed workspace")


def main() -> int:
    load_dotenv()
    args = parse_args()
    template = args.template or required("CUBE_TEMPLATE_ID")
    required("E2B_API_URL")
    required("E2B_API_KEY")
    api_key = required("GEMINI_API_KEY")
    approve_all = args.approve_all or os.environ.get("GEMINI_APPROVE_ALL") == "1"

    # This local-development path puts the key in the command environment.
    # network_policy.py instead keeps the real key outside the VM.
    command = shell_join(
        f"cd {shlex.quote(args.workspace)}",
        gemini_command(args.prompt, model=args.model, approve_all=approve_all),
    )

    sandbox = Sandbox.create(template=template, timeout=args.sandbox_timeout)
    current_id = sandbox_id(sandbox)
    try:
        print(f"Sandbox ready: {current_id}")
        version = run_command(sandbox, "gemini --version", timeout=60, user="root")
        ensure_success(version, "check Gemini CLI version")
        print(f"Gemini CLI: {getattr(version, 'stdout', '').strip()}")

        if not args.no_seed:
            seed_workspace(sandbox, args.workspace)

        result = run_command(
            sandbox,
            command,
            cwd=args.workspace,
            envs={"GEMINI_API_KEY": api_key},
            timeout=args.exec_timeout,
            user="root",
        )
        ensure_success(result, "run Gemini CLI")
        if getattr(result, "stdout", ""):
            print(result.stdout)

        inspect = run_command(
            sandbox,
            f"test ! -f {shlex.quote(args.workspace)}/result.md || cat {shlex.quote(args.workspace)}/result.md",
            timeout=60,
            user="root",
        )
        ensure_success(inspect, "read generated result")
        if getattr(inspect, "stdout", ""):
            print("\n--- result.md ---\n" + inspect.stdout)
        return 0
    finally:
        try:
            sandbox.kill()
            print(f"Sandbox {current_id} killed.")
        except Exception as exc:  # noqa: BLE001
            print(f"Warning: failed to kill sandbox {current_id}: {exc}", file=sys.stderr)


if __name__ == "__main__":
    sys.exit(main())
