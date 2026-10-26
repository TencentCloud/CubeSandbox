#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Restrict Pi's egress to the LLM API and inject the key on the wire.

This is the recommended production ("credential vault") pattern:

* Default-deny egress — the sandbox is created with ``allow_internet_access=False``
  and an ``allow_out`` list containing only the LLM API host, so every other
  destination is dropped before it can leave the sandbox.
* The provider auth header is attached by CubeEgress via ``inject`` rules
  (native ``cubesandbox`` SDK; see docs/guide/security-proxy.md), so the real
  key rides the wire and never enters the sandbox VM. The agent inside only
  sees a placeholder value.

Run:
    python network_policy.py
"""

from __future__ import annotations

import argparse
import os
import shlex
import sys

from cubesandbox import Sandbox, Rule, Match, Action, Inject

from env_utils import (
    build_pi_env,
    int_env,
    load_local_dotenv,
    pi_command,
    pi_llm_host,
    pi_provider,
    pi_workspace,
    provider_inject,
    provider_key_name,
    require_provider_key,
    required,
    shell_join,
)
from _pi_common import ensure_success, run_command, sandbox_identifier

PLACEHOLDER_KEY = "cube-egress-managed-placeholder"

# Pi runs on Node.js, which uses its own bundled CA store and ignores the system
# trust store. On the vault path CubeEgress terminates TLS to inject the
# credential, so Node must trust the CubeEgress root CA or every LLM call fails
# with "Connection error". Point NODE_EXTRA_CA_CERTS at a bundle that includes
# it; the CubeSandbox base image installs the CA into the system bundle below.
DEFAULT_NODE_CA_BUNDLE = "/etc/ssl/certs/ca-certificates.crt"

DEFAULT_PROMPT = (
    "Reply with a single short sentence confirming you can reach the LLM API, "
    "then write that sentence to {workspace}/egress_check.md."
)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Run Pi under a default-deny egress policy with on-the-wire key injection."
    )
    parser.add_argument(
        "--template",
        default=os.environ.get("CUBE_TEMPLATE_ID"),
        help="CubeSandbox template ID. Defaults to CUBE_TEMPLATE_ID.",
    )
    parser.add_argument(
        "--host",
        default=None,
        help="LLM API host to allow. Defaults to PI_LLM_HOST or the provider default.",
    )
    parser.add_argument(
        "--workspace",
        default=pi_workspace(),
        help="Working directory inside the sandbox. Defaults to PI_WORKSPACE.",
    )
    parser.add_argument(
        "--prompt",
        default=None,
        help="Prompt passed to Pi. Defaults to a small egress reachability check.",
    )
    parser.add_argument(
        "--name",
        default="cube-pi-agent-egress",
        help="Pi session name for the egress demo run.",
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
        "--skip-agent",
        action="store_true",
        help="Only show the egress checks; skip the actual Pi run.",
    )
    parser.add_argument(
        "--raw",
        action="store_true",
        help="Stream Pi's raw JSONL instead of the concise transcript.",
    )
    args = parser.parse_args()
    if args.prompt is None:
        args.prompt = DEFAULT_PROMPT.format(workspace=args.workspace)
    return args


def build_rules(provider: str, host: str, secret: str) -> list[Rule]:
    # Canonical CubeEgress rule (see docs/guide/security-proxy.md): allow the LLM
    # host and attach the provider auth header(s) on the wire via ``inject`` so
    # the real key never enters the sandbox VM. Anything matching no rule under
    # default-deny is rejected by CubeEgress with 403.
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
    # allow_internet_access=False makes egress default-deny at L3/L4; the LLM host
    # named in the rules is auto-allowed and its requests are injected at L7 by
    # CubeEgress. Never silently drop this flag, or full egress is re-enabled.
    return Sandbox.create(
        template=template_id,
        allow_internet_access=False,
        network={"rules": rules},
        timeout=timeout,
    )


def show_key_not_in_vm(sandbox: Sandbox, key_name: str) -> None:
    command = f"printenv {shlex.quote(key_name)} || echo '<unset>'"
    result = run_command(sandbox, command, timeout=30)
    ensure_success(result, "read provider key inside sandbox")
    value = getattr(result, "stdout", "").strip()
    print(f"In-VM {key_name}: {value!r} (real secret stays in CubeEgress)")


def show_non_llm_blocked(sandbox: Sandbox) -> None:
    command = (
        "curl -s -o /dev/null -w '%{http_code}' --max-time 8 https://example.com "
        "|| echo blocked"
    )
    result = run_command(sandbox, command, timeout=30)
    status = getattr(result, "stdout", "").strip()
    print(f"Non-LLM host (example.com) response: {status or 'blocked'} "
          "(expected 403/blocked under default-deny)")


def run_agent(sandbox: Sandbox, args: argparse.Namespace, envs: dict[str, str]):
    command = shell_join(
        f"cd {shlex.quote(args.workspace)}",
        pi_command(args.prompt, mode="json", name=args.name),
    )
    return run_command(
        sandbox,
        command,
        cwd=args.workspace,
        envs=envs,
        timeout=args.exec_timeout,
        stream=True,
    )


def main() -> int:
    load_local_dotenv()
    args = parse_args()
    if args.raw:
        os.environ["PI_STREAM_RAW"] = "1"

    template_id = args.template or required("CUBE_TEMPLATE_ID")
    required("E2B_API_URL")
    required("E2B_API_KEY")

    provider = pi_provider()
    secret = require_provider_key(provider)
    host = args.host or pi_llm_host(provider)
    if not host:
        raise SystemExit(
            "Could not resolve the LLM host. Set PI_LLM_HOST in your .env or pass --host."
        )

    rules = build_rules(provider, host, secret)

    sandbox_env = build_pi_env(include_secrets=False)
    key_name = provider_key_name(provider)
    sandbox_env[key_name] = PLACEHOLDER_KEY
    # Let the Node-based Pi CLI trust the CubeEgress interception CA (see note
    # on DEFAULT_NODE_CA_BUNDLE); without this the vault path fails TLS.
    sandbox_env["NODE_EXTRA_CA_CERTS"] = os.environ.get(
        "PI_NODE_EXTRA_CA_CERTS", DEFAULT_NODE_CA_BUNDLE
    )

    print(f"Provider: {provider}")
    print(f"Allowed LLM host (default-deny for everything else): {host}")
    print(f"Creating sandbox from template: {template_id}")

    sandbox = create_sandbox(template_id, rules, args.sandbox_timeout)
    sandbox_id = sandbox_identifier(sandbox)
    result = None
    try:
        print(f"Sandbox ready: {sandbox_id}\n")

        show_key_not_in_vm(sandbox, key_name)
        show_non_llm_blocked(sandbox)

        if args.skip_agent:
            print("\n--skip-agent set: not invoking Pi.")
            return 0

        print("\nRunning Pi through the injected egress path...\n")
        result = run_agent(sandbox, args, sandbox_env)
        exit_code = getattr(result, "exit_code", None)
        print(f"\nPi exit code: {exit_code}")
        return 0 if exit_code is None else int(exit_code)
    finally:
        try:
            sandbox.kill()
            print(f"\nSandbox {sandbox_id} killed.")
        except Exception as exc:  # noqa: BLE001 - cleanup must not mask real errors
            print(
                f"Warning: failed to kill sandbox {sandbox_id}: {exc}",
                file=sys.stderr,
            )


if __name__ == "__main__":
    sys.exit(main())
