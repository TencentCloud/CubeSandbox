#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Restrict Claude Code's egress to the Anthropic API and inject the key on the wire.

This is the recommended production ("credential vault") pattern:

* Default-deny egress — the sandbox is created with ``allow_internet_access=False``
  and an ``allow_out`` list containing only the Anthropic API host, so every other
  destination is dropped before it can leave the sandbox.
* The Anthropic auth headers are attached by CubeEgress via ``inject`` rules
  (native ``cubesandbox`` SDK; see docs/guide/security-proxy.md), so the real
  key rides the wire and never enters the sandbox VM. The agent inside only
  sees a placeholder value.

Usage:
    python network_policy.py
"""

from __future__ import annotations

import argparse
import os
import shlex
import sys

from cubesandbox import Sandbox, Rule, Match, Action, Inject

from common import (
    build_cc_env,
    cc_command,
    cc_llm_host,
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

PLACEHOLDER_KEY = "cube-egress-managed-placeholder"

# Claude Code runs on Node.js, which uses its own bundled CA store and ignores
# the system trust store. On the vault path CubeEgress terminates TLS to inject
# the credential, so Node must trust the CubeEgress root CA or every API call
# fails with a TLS error. Point NODE_EXTRA_CA_CERTS at a bundle that includes
# the CubeEgress CA; the CubeSandbox base image installs it below.
DEFAULT_NODE_CA_BUNDLE = "/etc/ssl/certs/ca-certificates.crt"

DEFAULT_PROMPT = (
    "Reply with a single short sentence confirming you can reach the Anthropic API, "
    "then write that sentence to {workspace}/egress_check.md."
)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Run Claude Code under default-deny egress with on-the-wire key injection."
    )
    parser.add_argument(
        "--template",
        default=os.environ.get("CUBE_TEMPLATE_ID"),
        help="CubeSandbox template ID. Defaults to CUBE_TEMPLATE_ID.",
    )
    parser.add_argument(
        "--host",
        default=None,
        help="Anthropic API host to allow. Defaults to CC_LLM_HOST or api.anthropic.com.",
    )
    parser.add_argument(
        "--workspace",
        default=cc_workspace(),
        help="Working directory inside the sandbox. Defaults to CC_WORKSPACE.",
    )
    parser.add_argument(
        "--prompt",
        default=None,
        help="Prompt passed to Claude Code. Defaults to a small egress check.",
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
        "--sandbox-timeout",
        type=int,
        default=int_env("CC_SANDBOX_TIMEOUT", 1800),
        help="Sandbox lifetime in seconds.",
    )
    parser.add_argument(
        "--exec-timeout",
        type=int,
        default=int_env("CC_EXEC_TIMEOUT", 900),
        help="Claude Code command timeout in seconds.",
    )
    parser.add_argument(
        "--skip-agent",
        action="store_true",
        help="Only show the egress checks; skip the actual Claude Code run.",
    )
    parser.add_argument(
        "--raw",
        action="store_true",
        help="Stream Claude Code's raw JSON instead of the concise transcript.",
    )
    args = parser.parse_args()
    if args.prompt is None:
        args.prompt = DEFAULT_PROMPT.format(workspace=args.workspace)
    return args


def build_rules(host: str, secret: str) -> list[Rule]:
    return [
        Rule(
            name="allow_anthropic_api",
            match=Match(scheme="https", sni=host, host=host),
            action=Action(
                allow=True,
                audit="metadata",
                inject=[
                    Inject(header="x-api-key", secret=secret, format="${SECRET}"),
                    Inject(header="anthropic-version", secret="2023-06-01", format="${SECRET}"),
                ],
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


def show_key_not_in_vm(sandbox: Sandbox) -> None:
    command = "printenv ANTHROPIC_API_KEY || echo '<unset>'"
    result = run_command(sandbox, command, timeout=30)
    ensure_success(result, "read API key inside sandbox")
    value = getattr(result, "stdout", "").strip()
    print(f"In-VM ANTHROPIC_API_KEY: {value!r} (real secret stays in CubeEgress)")


def show_non_llm_blocked(sandbox: Sandbox) -> None:
    command = (
        "curl -s -o /dev/null -w '%{http_code}' --max-time 8 https://example.com "
        "|| echo blocked"
    )
    result = run_command(sandbox, command, timeout=30)
    status = getattr(result, "stdout", "").strip()
    print(f"Non-LLM host (example.com) response: {status or 'blocked'} "
          "(expected 403/blocked under default-deny)")


def main() -> int:
    load_dotenv()
    args = parse_args()
    if args.raw:
        os.environ["CC_STREAM_RAW"] = "1"

    template_id = args.template or required("CUBE_TEMPLATE_ID")
    required("E2B_API_URL")
    required("E2B_API_KEY")

    secret = require_api_key()
    host = args.host or cc_llm_host()
    if not host:
        raise SystemExit(
            "Could not resolve the Anthropic API host. "
            "Set CC_LLM_HOST in your .env or pass --host."
        )

    rules = build_rules(host, secret)

    sandbox_env = build_cc_env(include_secrets=False)
    sandbox_env["ANTHROPIC_API_KEY"] = PLACEHOLDER_KEY
    # The vault path routes all traffic through CubeEgress; proxy variables
    # inside the VM would confuse Claude Code's request routing.
    for proxy_var in ("HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"):
        sandbox_env.pop(proxy_var, None)
    # Let Node-based Claude Code trust the CubeEgress interception CA —
    # without this the vault path fails with TLS errors.
    sandbox_env["NODE_EXTRA_CA_CERTS"] = os.environ.get(
        "CC_NODE_EXTRA_CA_CERTS", DEFAULT_NODE_CA_BUNDLE
    )

    print(f"Allowed Anthropic API host (default-deny for everything else): {host}")
    print(f"Creating sandbox from template: {template_id}")

    sandbox = create_sandbox(template_id, rules, args.sandbox_timeout)
    sandbox_id = sandbox_identifier(sandbox)
    result = None
    try:
        print(f"Sandbox ready: {sandbox_id}\n")
        show_key_not_in_vm(sandbox)
        show_non_llm_blocked(sandbox)

        if args.skip_agent:
            print("\n--skip-agent set: not invoking Claude Code.")
            return 0

        print("\nRunning Claude Code through the injected egress path...\n")
        command = shell_join(
            f"cd {shlex.quote(args.workspace)}",
            cc_command(
                args.prompt,
                model=args.model,
                effort=args.effort,
                dangerously_skip_permissions=True,
            ),
        )
        result = run_command(
            sandbox, command, cwd=args.workspace, envs=sandbox_env,
            timeout=args.exec_timeout, stream=True, user="user",
        )
        exit_code = getattr(result, "exit_code", None)
        print(f"\nClaude Code exit code: {exit_code}")
        return 0 if exit_code is None else int(exit_code)
    finally:
        try:
            sandbox.kill()
            print(f"\nSandbox {sandbox_id} killed.")
        except Exception as exc:
            print(f"Warning: failed to kill sandbox {sandbox_id}: {exc}",
                  file=sys.stderr)


if __name__ == "__main__":
    sys.exit(main())
