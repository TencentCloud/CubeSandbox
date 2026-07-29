#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Run a one-shot Claude Code task inside CubeSandbox.

This is the simplest headless pattern: create a sandbox, run ``claude -p`` with a
prompt, stream the JSON output, and collect results. The sandbox is ephemeral —
when the script exits the sandbox is killed.

Usage:
    cp .env.example .env   # fill in E2B_API_URL, CUBE_TEMPLATE_ID, ANTHROPIC_API_KEY
    pip install -r requirements.txt
    python run_claude_code.py
    python run_claude_code.py --prompt "Refactor the auth module."
"""

from __future__ import annotations

import argparse
import os
import shlex
import sys

from e2b import Sandbox

from common import (
    build_cc_env,
    cc_command,
    cc_model,
    cc_workspace,
    ensure_success,
    int_env,
    load_dotenv,
    optional,
    require_api_key,
    required,
    run_command,
    sandbox_identifier,
    shell_join,
)

DEFAULT_PROMPT_TEMPLATE = (
    "Inspect the project in {workspace}, run python3 app.py if it exists, "
    "and write a concise summary of what you find to {workspace}/result.md."
)


def default_prompt(workspace: str) -> str:
    return DEFAULT_PROMPT_TEMPLATE.format(workspace=workspace)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Run a one-shot Claude Code task inside CubeSandbox."
    )
    parser.add_argument(
        "--template",
        default=os.environ.get("CUBE_TEMPLATE_ID"),
        help="CubeSandbox template ID. Defaults to CUBE_TEMPLATE_ID.",
    )
    parser.add_argument(
        "--prompt",
        default=None,
        help="Prompt passed to Claude Code. Defaults to a small workspace smoke task.",
    )
    parser.add_argument(
        "--workspace",
        default=cc_workspace(),
        help="Working directory inside the sandbox. Defaults to CC_WORKSPACE.",
    )
    parser.add_argument(
        "--model",
        default=cc_model(),
        help="Model for Claude Code. Defaults to CC_MODEL or claude-sonnet-4-6.",
    )
    parser.add_argument(
        "--effort",
        default=optional("CC_EFFORT"),
        help="Effort level: low, medium, high, xhigh, max.",
    )
    parser.add_argument(
        "--permission-mode",
        default=optional("CC_PERMISSION_MODE"),
        help="Permission mode: plan, acceptEdits, bypassPermissions.",
    )
    parser.add_argument(
        "--sandbox-timeout",
        type=int,
        default=int_env("CC_SANDBOX_TIMEOUT", 1800),
        help="Sandbox lifetime in seconds. Defaults to CC_SANDBOX_TIMEOUT or 1800.",
    )
    parser.add_argument(
        "--exec-timeout",
        type=int,
        default=int_env("CC_EXEC_TIMEOUT", 900),
        help="Claude Code command timeout. Defaults to CC_EXEC_TIMEOUT or 900.",
    )
    parser.add_argument(
        "--no-seed",
        action="store_true",
        help="Skip writing the demo files into the sandbox workspace.",
    )
    parser.add_argument(
        "--raw",
        action="store_true",
        help="Stream Claude Code's raw JSON instead of the concise transcript.",
    )
    args = parser.parse_args()
    if args.prompt is None:
        args.prompt = default_prompt(args.workspace)
    return args


def seed_project(sandbox: Sandbox, workspace: str, timeout: int) -> None:
    quoted = shlex.quote(workspace)
    command = f"""mkdir -p {quoted}
cat > {quoted}/README.md <<'EOF'
# CubeSandbox Claude Code Smoke Project

This tiny project exists so Claude Code has a deterministic task to run.
EOF
cat > {quoted}/app.py <<'EOF'
def main() -> None:
    print("hello from CubeSandbox + Claude Code")


if __name__ == "__main__":
    main()
EOF
"""
    result = run_command(sandbox, command, timeout=timeout)
    ensure_success(result, "seed workspace")


def show_workspace_result(sandbox: Sandbox, workspace: str, timeout: int) -> None:
    quoted = shlex.quote(workspace)
    command = shell_join(
        f"ls -la {quoted}",
        f"test ! -f {quoted}/result.md || "
        f"(printf '\\n--- result.md ---\\n' && cat {quoted}/result.md)",
    )
    result = run_command(sandbox, command, timeout=timeout)
    ensure_success(result, "inspect workspace")
    if getattr(result, "stdout", ""):
        print(result.stdout)


def main() -> int:
    load_dotenv()
    args = parse_args()
    if args.raw:
        os.environ["CC_STREAM_RAW"] = "1"

    template_id = args.template or required("CUBE_TEMPLATE_ID")
    required("E2B_API_URL")
    required("E2B_API_KEY")
    require_api_key()

    cc_env = build_cc_env()
    # Headless sandbox default: when no explicit permission_mode is set, use
    # --dangerously-skip-permissions + non-root user so Claude Code can write
    # files without interactive approval.
    headless = args.permission_mode is None
    command = shell_join(
        f"cd {shlex.quote(args.workspace)}",
        cc_command(
            args.prompt,
            model=args.model,
            effort=args.effort,
            permission_mode=args.permission_mode,
            dangerously_skip_permissions=headless,
        ),
    )

    print(f"Creating sandbox from template: {template_id}")
    result = None
    # SECURITY: this direct-key demo keeps egress open (allow_internet_access
    # defaults to True) for simplicity, and injects the API key per command via
    # envs=. For shared/production use prefer network_policy.py, which pairs
    # default-deny egress with the CubeEgress credential vault.
    with Sandbox.create(template=template_id, timeout=args.sandbox_timeout) as sandbox:
        sandbox_id = sandbox_identifier(sandbox)
        print(f"Sandbox ready: {sandbox_id}")

        version_result = run_command(sandbox, "claude --version", timeout=60)
        ensure_success(version_result, "check Claude Code version")
        print(f"Claude Code version: {getattr(version_result, 'stdout', '').strip()}")

        if not args.no_seed:
            seed_project(sandbox, args.workspace, timeout=60)
            print(f"Seeded demo project in {args.workspace}")

        print("\nRunning Claude Code task...\n")
        result = run_command(
            sandbox,
            command,
            cwd=args.workspace,
            envs=cc_env,
            timeout=args.exec_timeout,
            stream=True,
            user="user" if headless else "root",
        )

        exit_code = getattr(result, "exit_code", None)
        print(f"\nClaude Code exit code: {exit_code}")
        stderr = getattr(result, "stderr", "")
        if stderr:
            print("\nCaptured stderr:", file=sys.stderr)
            print(stderr, file=sys.stderr)
        show_workspace_result(sandbox, args.workspace, timeout=60)

    return 0 if exit_code is None else int(exit_code)


if __name__ == "__main__":
    sys.exit(main())
