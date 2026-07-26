#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Continue one OpenCode task in a new sandbox forked from a checkpoint."""

from __future__ import annotations

import argparse
import os
import shlex
import sys

from cubesandbox import Rule, Sandbox

from _opencode_common import (
    ensure_success,
    run_command,
    sandbox_identifier,
)
from egress_policy import (
    PLACEHOLDER_KEY,
    build_rules,
    create_sandbox as create_restricted_sandbox,
    require_native_sdk_env,
    verify_key_not_in_vm,
    verify_non_llm_blocked,
    verify_placeholder_env,
)
from env_utils import (
    build_opencode_env,
    int_env,
    load_local_dotenv,
    opencode_llm_host,
    opencode_command,
    opencode_config_json,
    opencode_model,
    opencode_provider,
    opencode_workspace,
    provider_key_name,
    require_provider_key,
    required,
    shell_join,
)

TURN_1_PROMPT = (
    "Create {workspace}/checkpoint_plan.md containing exactly this single line: "
    "OPENCODE_CHECKPOINT_READY. Do not read or print environment variables."
)

TURN_2_PROMPT = (
    "Continue the previous task. Read checkpoint_plan.md, then create "
    "fork_result.md containing exactly two lines: OPENCODE_CHECKPOINT_READY and "
    "OPENCODE_FORK_OK. Do not read or print environment variables."
)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description=(
            "Run OpenCode, create a CubeSandbox checkpoint, fork a new sandbox, "
            "and continue the same task in the fork."
        )
    )
    parser.add_argument("--template", default=os.environ.get("CUBE_TEMPLATE_ID"))
    parser.add_argument("--workspace", default=opencode_workspace())
    parser.add_argument("--title", default="cube-opencode-checkpoint-fork")
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
    return parser.parse_args()


def create_sandbox(template_id: str, rules: list[Rule], timeout: int) -> Sandbox:
    return create_restricted_sandbox(template_id, rules, timeout)


def verify_restricted_environment(
    sandbox: Sandbox, key_name: str, envs: dict[str, str]
) -> None:
    verify_key_not_in_vm(sandbox, key_name)
    verify_placeholder_env(sandbox, key_name, envs)
    verify_non_llm_blocked(sandbox)


def seed_project(sandbox: Sandbox, workspace: str) -> None:
    quoted_workspace = shlex.quote(workspace)
    command = f"""mkdir -p {quoted_workspace}
cat > {quoted_workspace}/opencode.json <<'EOF'
{opencode_config_json()}
EOF
cat > {quoted_workspace}/calculator.py <<'EOF'
def add(a: int, b: int) -> int:
    return a + b
EOF
cat > {quoted_workspace}/test_calculator.py <<'EOF'
import unittest

from calculator import add


class CalculatorTest(unittest.TestCase):
    def test_adds_two_numbers(self) -> None:
        self.assertEqual(add(2, 3), 5)


if __name__ == "__main__":
    unittest.main()
EOF
"""
    result = run_command(sandbox, command, timeout=60)
    ensure_success(result, "seed checkpoint/fork workspace")


def run_turn(
    sandbox: Sandbox,
    workspace: str,
    prompt: str,
    model: str,
    envs: dict[str, str],
    timeout: int,
    *,
    title: str | None = None,
    continue_last: bool = False,
):
    command = opencode_command(
        prompt,
        workspace=workspace,
        model=model,
        title=title,
        continue_last=continue_last,
    )
    return run_command(
        sandbox,
        command,
        cwd=workspace,
        envs=envs,
        timeout=timeout,
        stream=True,
    )


def verify_checkpoint_state(sandbox: Sandbox, workspace: str) -> None:
    quoted_workspace = shlex.quote(workspace)
    command = shell_join(
        f"cd {quoted_workspace}",
        "test -f checkpoint_plan.md",
        "grep -q OPENCODE_CHECKPOINT_READY checkpoint_plan.md",
        "test -d /root/.local/share/opencode",
        "cat checkpoint_plan.md",
    )
    result = run_command(sandbox, command, timeout=60)
    ensure_success(result, "verify checkpoint state in forked sandbox")
    if getattr(result, "stdout", ""):
        print(result.stdout)


def verify_fork_result(sandbox: Sandbox, workspace: str) -> None:
    quoted_workspace = shlex.quote(workspace)
    command = shell_join(
        f"cd {quoted_workspace}",
        "python3 -m unittest discover -v",
        "test -f fork_result.md",
        "grep -q OPENCODE_CHECKPOINT_READY fork_result.md",
        "grep -q OPENCODE_FORK_OK fork_result.md",
        "cat fork_result.md",
    )
    result = run_command(sandbox, command, timeout=120)
    ensure_success(result, "verify continued task in forked sandbox")
    if getattr(result, "stdout", ""):
        print(result.stdout)


def main() -> int:
    load_local_dotenv()
    args = parse_args()

    template_id = args.template or required("CUBE_TEMPLATE_ID")
    require_native_sdk_env()
    model = opencode_model()
    provider = opencode_provider(model)
    secret = require_provider_key(provider)
    host = opencode_llm_host(provider)
    if not host:
        raise SystemExit("Set OPENCODE_LLM_HOST or OPENCODE_BASE_URL for this provider.")

    key_name = provider_key_name(provider)
    envs = build_opencode_env(include_secrets=False)
    envs[key_name] = PLACEHOLDER_KEY
    rules = build_rules(provider, host, secret)

    source = None
    forked = None
    snapshot_id = None
    source_id = None
    try:
        print(f"Creating source sandbox from template: {template_id}")
        source = create_sandbox(template_id, rules, args.sandbox_timeout)
        source_id = sandbox_identifier(source)
        print(f"Source sandbox ready: {source_id}")
        verify_restricted_environment(source, key_name, envs)

        seed_project(source, args.workspace)

        print("\n=== Turn 1: prepare checkpoint state ===\n")
        result_1 = run_turn(
            source,
            args.workspace,
            TURN_1_PROMPT.format(workspace=args.workspace),
            model,
            envs,
            args.exec_timeout,
            title=args.title,
        )
        ensure_success(result_1, "run OpenCode before checkpoint")
        verify_checkpoint_state(source, args.workspace)

        snapshot = source.create_snapshot(name="opencode-checkpoint-fork")
        snapshot_id = snapshot.snapshot_id
        print(f"Checkpoint created: {snapshot_id}")

        forked = create_sandbox(snapshot_id, rules, args.sandbox_timeout)
        forked_id = sandbox_identifier(forked)
        if forked_id == source_id:
            raise RuntimeError("Forked sandbox unexpectedly reused the source sandbox ID")
        print(f"Forked sandbox ready: {forked_id}")
        verify_restricted_environment(forked, key_name, envs)
        verify_checkpoint_state(forked, args.workspace)

        print("\n=== Turn 2: continue inside the fork ===\n")
        result_2 = run_turn(
            forked,
            args.workspace,
            TURN_2_PROMPT,
            model,
            envs,
            args.exec_timeout,
            continue_last=True,
        )
        ensure_success(result_2, "continue OpenCode in forked sandbox")
        verify_fork_result(forked, args.workspace)
        print("\nCheckpoint/fork workflow passed.")
        return 0
    finally:
        for label, sandbox in (("forked", forked), ("source", source)):
            if sandbox is None:
                continue
            try:
                sandbox.kill()
                print(f"{label.capitalize()} sandbox killed.")
            except Exception as exc:  # noqa: BLE001
                print(f"Warning: failed to kill {label} sandbox: {exc}", file=sys.stderr)
        if snapshot_id is not None:
            try:
                Sandbox.delete_snapshot(snapshot_id)
                print(f"Checkpoint deleted: {snapshot_id}")
            except Exception as exc:  # noqa: BLE001
                print(f"Warning: failed to delete checkpoint {snapshot_id}: {exc}", file=sys.stderr)


if __name__ == "__main__":
    sys.exit(main())
