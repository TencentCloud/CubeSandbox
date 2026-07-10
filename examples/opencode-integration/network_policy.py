#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Run OpenCode with default-deny egress and on-the-wire credentials."""

from __future__ import annotations

import shlex
import sys

from cubesandbox import Action, Inject, Match, Rule, Sandbox

from _opencode_common import (
    ensure_success,
    opencode_command,
    redacted_result_output,
    run_command,
    safe_kill,
    sandbox_identifier,
)
import env_utils


RESULT_FILE = "egress_check.md"
PROMPT = (
    "Work only in {workspace}. Read README.md, then write {result_file} "
    "containing one sentence confirming that this OpenCode task completed."
)
SEED_README = (
    "# CubeEgress OpenCode example\n\n"
    "This task must use injected egress.\n"
)


def build_rules(config: env_utils.RunConfig) -> list[Rule]:
    """Allow only the configured LLM host and inject its auth header."""
    provider = config.provider
    return [
        Rule(
            name=f"allow_{provider.name}_llm",
            match=Match(
                scheme="https",
                sni=provider.host,
                host=provider.host,
            ),
            action=Action(
                allow=True,
                audit="metadata",
                inject=[
                    Inject(**spec)
                    for spec in env_utils.provider_inject(provider)
                ],
            ),
        )
    ]


def seed_project(
    sandbox: Sandbox,
    workspace: str,
    config: env_utils.RunConfig,
) -> None:
    """Create a non-secret project for the restricted OpenCode task."""
    result = run_command(
        sandbox,
        f"mkdir -p {shlex.quote(workspace)}",
        timeout=60,
    )
    ensure_success(
        result,
        "create the workspace",
        secrets=(config.provider.secret,),
    )
    sandbox.files.write(
        f"{workspace.rstrip('/')}/README.md",
        SEED_README,
        user="root",
    )


def print_redacted_result(
    result: object,
    config: env_utils.RunConfig,
) -> None:
    """Print OpenCode output without exposing the provider credential."""
    stdout, stderr = redacted_result_output(
        result,
        secrets=(config.provider.secret,),
    )
    if stdout:
        print(stdout, end="" if stdout.endswith("\n") else "\n")
    if stderr:
        print(
            stderr,
            file=sys.stderr,
            end="" if stderr.endswith("\n") else "\n",
        )


def verify_result(
    sandbox: Sandbox,
    workspace: str,
    config: env_utils.RunConfig,
) -> None:
    """Confirm the restricted OpenCode task wrote its expected artifact."""
    result = run_command(
        sandbox,
        f"test -s {shlex.quote(RESULT_FILE)}",
        cwd=workspace,
        timeout=60,
    )
    ensure_success(
        result,
        "verify the OpenCode egress-check artifact",
        secrets=(config.provider.secret,),
    )


def main() -> int:
    """Create a default-deny sandbox and run OpenCode through CubeEgress."""
    env_utils.load_local_dotenv()
    config = env_utils.run_config()
    workspace = config.workspace
    rules = build_rules(config)
    command_env = env_utils.build_opencode_env(
        config.provider,
        include_secret=False,
    )
    command_env["NODE_EXTRA_CA_CERTS"] = config.node_ca_bundle

    sandbox: Sandbox | None = None
    try:
        print(f"Allowed LLM host: {config.provider.host}")
        print("Creating default-deny sandbox with credential injection.")
        sandbox = Sandbox.create(
            template=config.template_id,
            allow_internet_access=False,
            network={"rules": rules},
            timeout=config.sandbox_timeout,
        )
        print(f"Sandbox ready: {sandbox_identifier(sandbox)}")

        seed_project(sandbox, workspace, config)
        command = opencode_command(
            PROMPT.format(workspace=workspace, result_file=RESULT_FILE),
            model=config.provider.model,
            workspace=workspace,
            title="cube-opencode-egress",
        )
        result = run_command(
            sandbox,
            command,
            cwd=workspace,
            envs=command_env,
            timeout=config.exec_timeout,
        )
        ensure_success(
            result,
            "run OpenCode through the restricted egress policy",
            secrets=(config.provider.secret,),
        )
        print_redacted_result(result, config)
        verify_result(sandbox, workspace, config)
        print(
            f"Verified {RESULT_FILE}; the provider key stayed outside the VM."
        )
        return 0
    finally:
        safe_kill(sandbox)


if __name__ == "__main__":
    sys.exit(main())
