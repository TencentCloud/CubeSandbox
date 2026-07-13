#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import argparse
import os
import shlex
import sys

from e2b import Sandbox

from _pi_common import ensure_success, run_command, sandbox_identifier
from env_utils import (
    build_pi_env,
    int_env,
    load_local_dotenv,
    pi_command,
    pi_provider,
    pi_workspace,
    require_provider_key,
    required,
    shell_join,
)

DEFAULT_PROMPT_TEMPLATE = (
    "Inspect the project in {workspace}, run python3 app.py, and write a "
    "concise summary of the result to {workspace}/result.md."
)


def default_prompt(workspace: str) -> str:
    return DEFAULT_PROMPT_TEMPLATE.format(workspace=workspace)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Run a one-shot Pi coding-agent task inside CubeSandbox."
    )
    parser.add_argument(
        "--template",
        default=os.environ.get("CUBE_TEMPLATE_ID"),
        help="CubeSandbox template ID. Defaults to CUBE_TEMPLATE_ID.",
    )
    parser.add_argument(
        "--prompt",
        default=None,
        help="Prompt passed to Pi. Defaults to a small workspace smoke task.",
    )
    parser.add_argument(
        "--workspace",
        default=pi_workspace(),
        help="Working directory inside the sandbox. Defaults to PI_WORKSPACE.",
    )
    parser.add_argument(
        "--name",
        default="cube-pi-agent-demo",
        help="Pi session name for the one-shot run.",
    )
    parser.add_argument(
        "--sandbox-timeout",
        type=int,
        default=int_env("PI_SANDBOX_TIMEOUT", 1800),
        help="Sandbox lifetime in seconds. Defaults to PI_SANDBOX_TIMEOUT or 1800.",
    )
    parser.add_argument(
        "--exec-timeout",
        type=int,
        default=int_env("PI_AGENT_EXEC_TIMEOUT", 900),
        help="Pi command timeout in seconds. Defaults to PI_AGENT_EXEC_TIMEOUT or 900.",
    )
    parser.add_argument(
        "--no-seed",
        action="store_true",
        help="Skip writing the demo files into the sandbox workspace.",
    )
    parser.add_argument(
        "--raw",
        action="store_true",
        help="Stream Pi's raw JSONL instead of the concise transcript.",
    )
    args = parser.parse_args()
    if args.prompt is None:
        args.prompt = default_prompt(args.workspace)
    return args


def seed_project(sandbox: Sandbox, workspace: str, timeout: int) -> None:
    quoted_workspace = shlex.quote(workspace)
    command = f"""mkdir -p {quoted_workspace}
cat > {quoted_workspace}/README.md <<'EOF'
# CubeSandbox Pi Agent Smoke Project

This tiny project exists so the Pi coding agent has a deterministic task to run.
EOF
cat > {quoted_workspace}/app.py <<'EOF'
def main() -> None:
    print("hello from CubeSandbox + Pi")


if __name__ == "__main__":
    main()
EOF
"""
    result = run_command(sandbox, command, timeout=timeout)
    ensure_success(result, "seed workspace")


def print_result_summary(result) -> None:
    exit_code = getattr(result, "exit_code", None)
    stderr = getattr(result, "stderr", "")

    print(f"\nPi exit code: {exit_code}")
    if stderr:
        print("\nCaptured stderr:", file=sys.stderr)
        print(stderr, file=sys.stderr)


def show_workspace_result(sandbox: Sandbox, workspace: str, timeout: int) -> None:
    quoted_workspace = shlex.quote(workspace)
    command = shell_join(
        f"ls -la {quoted_workspace}",
        f"test ! -f {quoted_workspace}/result.md || "
        f"(printf '\\n--- result.md ---\\n' && cat {quoted_workspace}/result.md)",
    )
    result = run_command(sandbox, command, timeout=timeout)
    ensure_success(result, "inspect workspace")
    if getattr(result, "stdout", ""):
        print(result.stdout)


def main() -> int:
    load_local_dotenv()
    args = parse_args()
    if args.raw:
        os.environ["PI_STREAM_RAW"] = "1"

    template_id = args.template or required("CUBE_TEMPLATE_ID")
    required("E2B_API_URL")
    required("E2B_API_KEY")
    require_provider_key(pi_provider())

    pi_env = build_pi_env()
    command = shell_join(
        f"cd {shlex.quote(args.workspace)}",
        pi_command(args.prompt, mode="json", name=args.name),
    )

    print(f"Creating sandbox from template: {template_id}")
    result = None
    # SECURITY: this direct-key demo keeps egress open (allow_internet_access
    # defaults to True) for simplicity, and injects the provider key per command
    # via envs=. A compromised agent with open egress could exfiltrate that key.
    # For shared/production use prefer network_policy.py, which pairs default-deny
    # egress with the CubeEgress credential vault (the key never enters the VM).
    with Sandbox.create(template=template_id, timeout=args.sandbox_timeout) as sandbox:
        sandbox_id = sandbox_identifier(sandbox)
        print(f"Sandbox ready: {sandbox_id}")

        version_result = run_command(sandbox, "pi --version", timeout=60)
        ensure_success(version_result, "check Pi version")
        print(f"Pi version: {getattr(version_result, 'stdout', '').strip()}")

        if not args.no_seed:
            seed_project(sandbox, args.workspace, timeout=60)
            print(f"Seeded demo project in {args.workspace}")

        print("\nRunning Pi task...\n")
        result = run_command(
            sandbox,
            command,
            cwd=args.workspace,
            envs=pi_env,
            timeout=args.exec_timeout,
            stream=True,
        )

        print_result_summary(result)
        show_workspace_result(sandbox, args.workspace, timeout=60)

    exit_code = getattr(result, "exit_code", 1)
    return 0 if exit_code is None else int(exit_code)


if __name__ == "__main__":
    sys.exit(main())
