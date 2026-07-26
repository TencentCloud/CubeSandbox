#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Run OpenCode with default-deny egress and CubeEgress credential injection."""

from __future__ import annotations

import argparse
import os
import shlex
import sys

from cubesandbox import Sandbox

from _opencode_common import ensure_success, run_command, sandbox_identifier
from egress_policy import (
    PLACEHOLDER_KEY,
    build_rules,
    create_sandbox,
    require_native_sdk_env,
    verify_key_not_in_vm,
    verify_non_llm_blocked,
    verify_placeholder_env,
)
from env_utils import (
    build_opencode_env,
    int_env,
    load_local_dotenv,
    opencode_command,
    opencode_config_json,
    opencode_llm_host,
    opencode_model,
    opencode_provider,
    opencode_workspace,
    provider_key_name,
    require_provider_key,
    required,
    shell_join,
)

DEFAULT_PROMPT = (
    "Create {workspace}/egress_check.md containing exactly OPENCODE_EGRESS_OK. "
    "Use the write tool or shell redirection, then stop only after the file exists."
)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Run OpenCode under a restricted CubeEgress policy."
    )
    parser.add_argument("--template", default=os.environ.get("CUBE_TEMPLATE_ID"))
    parser.add_argument("--host", default=None)
    parser.add_argument("--workspace", default=opencode_workspace())
    parser.add_argument("--prompt", default=None)
    parser.add_argument("--title", default="cube-opencode-egress")
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
    parser.add_argument("--skip-agent", action="store_true")
    args = parser.parse_args()
    if args.prompt is None:
        args.prompt = DEFAULT_PROMPT.format(workspace=args.workspace)
    return args


def seed_workspace(sandbox: Sandbox, workspace: str) -> None:
    quoted_workspace = shlex.quote(workspace)
    command = f"""mkdir -p {quoted_workspace}
cat > {quoted_workspace}/opencode.json <<'EOF'
{opencode_config_json()}
EOF
"""
    result = run_command(sandbox, command, timeout=60)
    ensure_success(result, "seed egress workspace")


def verify_agent_output(sandbox: Sandbox, workspace: str) -> None:
    quoted_workspace = shlex.quote(workspace)
    command = shell_join(
        f"test -f {quoted_workspace}/egress_check.md",
        f"grep -q OPENCODE_EGRESS_OK {quoted_workspace}/egress_check.md",
        f"cat {quoted_workspace}/egress_check.md",
    )
    result = run_command(sandbox, command, timeout=60)
    ensure_success(result, "verify egress demo output")
    if getattr(result, "stdout", ""):
        print(result.stdout)


def print_command_output(result) -> None:
    stdout = getattr(result, "stdout", "")
    stderr = getattr(result, "stderr", "")
    if stdout:
        print(stdout)
    if stderr:
        print(stderr, file=sys.stderr)


def main() -> int:
    load_local_dotenv()
    args = parse_args()

    template_id = args.template or required("CUBE_TEMPLATE_ID")
    require_native_sdk_env()
    model = opencode_model()
    provider = opencode_provider(model)
    secret = require_provider_key(provider)
    host = args.host or opencode_llm_host(provider)
    if not host:
        raise SystemExit("Set OPENCODE_LLM_HOST or OPENCODE_BASE_URL for this provider.")

    key_name = provider_key_name(provider)
    envs = build_opencode_env(include_secrets=False)
    envs[key_name] = PLACEHOLDER_KEY

    rules = build_rules(provider, host, secret)
    sandbox = None
    sandbox_id = "unknown"
    try:
        print(f"Provider: {provider}")
        print(f"Allowed LLM host: {host}")
        print(f"Creating sandbox from template: {template_id}")
        sandbox = create_sandbox(template_id, rules, args.sandbox_timeout)
        sandbox_id = sandbox_identifier(sandbox)
        print(f"Sandbox ready: {sandbox_id}\n")

        verify_key_not_in_vm(sandbox, key_name)
        verify_placeholder_env(sandbox, key_name, envs)
        verify_non_llm_blocked(sandbox)
        seed_workspace(sandbox, args.workspace)

        if args.skip_agent:
            print("\n--skip-agent set: not invoking OpenCode.")
            return 0

        command = opencode_command(
            args.prompt,
            workspace=args.workspace,
            model=model,
            title=args.title,
        )
        print("\nRunning OpenCode through CubeEgress injection...\n")
        result = run_command(
            sandbox,
            command,
            cwd=args.workspace,
            envs=envs,
            timeout=args.exec_timeout,
            stream=True,
        )
        ensure_success(result, "run OpenCode with restricted egress")
        print_command_output(result)
        verify_agent_output(sandbox, args.workspace)
        return 0
    finally:
        if sandbox is not None:
            try:
                sandbox.kill()
                print(f"\nSandbox {sandbox_id} killed.")
            except Exception as exc:  # noqa: BLE001
                print(
                    f"Warning: failed to kill sandbox {sandbox_id}: {exc}",
                    file=sys.stderr,
                )


if __name__ == "__main__":
    sys.exit(main())
