# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Shared default-deny CubeEgress helpers for OpenCode workflows."""

from __future__ import annotations

import shlex

from cubesandbox import Action, Inject, Match, Rule, Sandbox

from _opencode_common import ensure_success, run_command
from env_utils import provider_inject, required


PLACEHOLDER_KEY = "cube-egress-managed-placeholder"


def require_native_sdk_env() -> None:
    required("CUBE_API_URL")
    required("CUBE_PROXY_NODE_IP")


def build_rules(provider: str, host: str, secret: str) -> list[Rule]:
    return [
        Rule(
            name=f"allow_{provider}_llm",
            match=Match(scheme="https", sni=host, host=host),
            action=Action(
                allow=True,
                audit="metadata",
                inject=[Inject(**spec) for spec in provider_inject(provider, secret)],
            ),
        )
    ]


def create_sandbox(template_id: str, rules: list[Rule], timeout: int) -> Sandbox:
    return Sandbox.create(
        template=template_id,
        allow_internet_access=False,
        network={"rules": rules},
        timeout=timeout,
    )


def verify_key_not_in_vm(sandbox: Sandbox, key_name: str) -> None:
    command = f"printenv {shlex.quote(key_name)} || echo '<unset>'"
    result = run_command(sandbox, command, timeout=30)
    ensure_success(result, "read provider key inside sandbox")
    value = getattr(result, "stdout", "").strip()
    if value != "<unset>":
        raise SystemExit(
            f"Security check failed: {key_name} exists in the sandbox's global environment."
        )
    print(f"In-VM global {key_name}: '<unset>' (real secret stays in CubeEgress)")


def verify_placeholder_env(
    sandbox: Sandbox, key_name: str, envs: dict[str, str]
) -> None:
    command = f"printenv {shlex.quote(key_name)}"
    result = run_command(sandbox, command, envs=envs, timeout=30)
    ensure_success(result, "verify provider placeholder inside the agent command")
    value = getattr(result, "stdout", "").strip()
    if value != PLACEHOLDER_KEY:
        raise SystemExit(
            f"Security check failed: {key_name} was not replaced by the CubeEgress placeholder."
        )
    print(f"Agent command {key_name}: '<placeholder>'")


def verify_non_llm_blocked(sandbox: Sandbox) -> None:
    command = (
        "curl -s -o /dev/null -w '%{http_code}' --max-time 8 https://example.com "
        "|| echo blocked"
    )
    result = run_command(sandbox, command, timeout=30)
    status = getattr(result, "stdout", "").strip()
    blocked = status == "403" or status.endswith("blocked")
    if not blocked:
        raise SystemExit(
            "Security check failed: non-LLM host example.com was reachable "
            f"(curl status {status or '<empty>'})."
        )
    print(f"Non-LLM host example.com: {status or 'blocked'} (blocked as expected)")
