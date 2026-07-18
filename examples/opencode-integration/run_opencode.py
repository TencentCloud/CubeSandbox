#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Run a one-shot OpenCode task inside a CubeSandbox.

The key is forwarded per-command via ``sandbox.commands.run(..., envs=...)``,
so it lives only for the lifetime of that exec call — never written to a
persistent file inside the VM.
"""

from __future__ import annotations

import argparse
import os
import shlex
import sys
from typing import Any

from e2b import Sandbox

from _opencode_common import ensure_success, positive_int, run_command, sandbox_identifier
from env_utils import (
    _env_positive_int,
    build_opencode_env,
    opencode_command,
    opencode_model,
    opencode_workspace,
    load_local_dotenv,
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
        description="Run a one-shot OpenCode task inside CubeSandbox."
    )
    parser.add_argument(
        "--template",
        default=os.environ.get("CUBE_TEMPLATE_ID"),
        help="CubeSandbox template ID. Defaults to CUBE_TEMPLATE_ID.",
    )
    parser.add_argument(
        "--prompt",
        default=None,
        help="Prompt passed to OpenCode. Defaults to a small workspace smoke task.",
    )
    parser.add_argument(
        "--workspace",
        default=opencode_workspace(),
        help="Working directory inside the sandbox. Defaults to OPENCODE_WORKSPACE.",
    )
    parser.add_argument(
        "--model",
        default=None,
        help="Model id for the active provider. Defaults to OPENCODE_MODEL.",
    )
    parser.add_argument(
        "--approve",
        action=argparse.BooleanOptionalAction,
        default=True,
        help=(
            "Auto-approve every tool call via opencode's "
            "--dangerously-skip-permissions flag. Required for any non-interactive "
            "run that touches files or commands. Defaults to enabled; pass --no-approve "
            "to let OpenCode prompt for permission (the exec channel cannot answer it, "
            "so this will hang — only use --no-approve if you've tightened the tool "
            "allow-list via opencode.json)."
        ),
    )
    parser.add_argument(
        "--sandbox-timeout",
        type=positive_int,
        # Resolve the env-var through positive_int too so
        # OPENCODE_SANDBOX_TIMEOUT=0 fails the same way as ``--sandbox-timeout 0``
        # (a bare ``int_env(...)`` default would skip that check and let a
        # zero-value env var reach the SDK, which then creates a sandbox with
        # no lifetime).
        default=_env_positive_int("OPENCODE_SANDBOX_TIMEOUT", 1800),
        help="Sandbox lifetime in seconds. Defaults to OPENCODE_SANDBOX_TIMEOUT or 1800.",
    )
    parser.add_argument(
        "--exec-timeout",
        type=positive_int,
        default=_env_positive_int("OPENCODE_AGENT_EXEC_TIMEOUT", 900),
        help="OpenCode command timeout in seconds. Defaults to OPENCODE_AGENT_EXEC_TIMEOUT or 900.",
    )
    parser.add_argument(
        "--no-seed",
        action="store_true",
        help="Skip writing the demo files into the sandbox workspace.",
    )
    args = parser.parse_args()
    if args.prompt is None:
        args.prompt = default_prompt(args.workspace)
    return args


def seed_project(sandbox: Sandbox, workspace: str, timeout: int) -> None:
    quoted_workspace = shlex.quote(workspace)
    command = f"""mkdir -p {quoted_workspace}
cat > {quoted_workspace}/README.md <<'EOF'
# CubeSandbox OpenCode Smoke Project

This tiny project exists so the OpenCode coding agent has a deterministic task to run.
EOF
cat > {quoted_workspace}/app.py <<'EOF'
def main() -> None:
    print("hello from CubeSandbox + OpenCode")


if __name__ == "__main__":
    main()
EOF
"""
    result = run_command(sandbox, command, timeout=timeout)
    ensure_success(result, "seed workspace")


def print_result_summary(result: Any) -> None:
    exit_code = getattr(result, "exit_code", None)
    stderr = getattr(result, "stderr", "")

    print(f"\nOpenCode exit code: {exit_code}")
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

    template_id = args.template or required("CUBE_TEMPLATE_ID")
    required("E2B_API_URL")
    required("E2B_API_KEY")
    require_provider_key()

    opencode_env = build_opencode_env()
    model = args.model or opencode_model()
    # ``--dangerously-skip-permissions`` is added by ``opencode_command`` when
    # ``args.approve`` is True. No env var is needed; OpenCode 1.17+ recognizes
    # the flag directly and translates it into the same bypass the older
    # ``OPENCODE_PERMISSION='{"*":"allow"}'`` env var provided.
    command = shell_join(
        f"cd {shlex.quote(args.workspace)}",
        opencode_command(
            args.prompt,
            dangerously_skip_permissions=args.approve,
            model=model,
        ),
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

        version_result = run_command(sandbox, "opencode --version", timeout=60)
        ensure_success(version_result, "check OpenCode version")
        print(f"OpenCode version: {getattr(version_result, 'stdout', '').strip()}")

        if not args.no_seed:
            seed_project(sandbox, args.workspace, timeout=60)
            print(f"Seeded demo project in {args.workspace}")

        print("\nRunning OpenCode task...\n")
        result = run_command(
            sandbox,
            command,
            cwd=args.workspace,
            envs=opencode_env,
            timeout=args.exec_timeout,
            stream=True,
        )

        print_result_summary(result)
        show_workspace_result(sandbox, args.workspace, timeout=60)

    exit_code = getattr(result, "exit_code", 1)
    return 0 if exit_code is None else int(exit_code)


if __name__ == "__main__":
    sys.exit(main())
