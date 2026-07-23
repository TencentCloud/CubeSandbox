#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Run MiMo Code with default-deny egress and CubeEgress key injection."""

from __future__ import annotations

import argparse
import os
import shlex
import sys

from cubesandbox import Action, Config, Inject, Match, Rule, Sandbox

from _mimo_common import (
    ensure_success,
    is_unexpected_keyword_error,
    kill_sandbox,
    run_command,
    run_mimo_command,
    sandbox_identifier,
    session_id_from_events,
)
from env_utils import (
    MIMO_API_KEY_ENV,
    build_mimo_env,
    int_env,
    load_local_dotenv,
    mimo_api_host,
    mimo_command,
    mimo_inject,
    mimo_workspace,
    positive_int,
    required,
    shell_join,
)

PLACEHOLDER_KEY = "cube-egress-managed-placeholder"
DEFAULT_NODE_CA_BUNDLE = "/etc/ssl/certs/ca-certificates.crt"
DEFAULT_PROMPT = (
    "Reply with one short sentence, then write {workspace}/egress_check.md. "
    "The file must contain the exact marker CUBE_MIMO_EGRESS_OK."
)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Run MiMo Code through CubeEgress credential injection."
    )
    parser.add_argument("--template", default=os.environ.get("CUBE_TEMPLATE_ID"))
    parser.add_argument("--workspace", default=mimo_workspace())
    parser.add_argument(
        "--prompt",
        help=(
            "Custom task. It must create egress_check.md with "
            "CUBE_MIMO_EGRESS_OK unless --skip-result-check is set."
        ),
    )
    parser.add_argument(
        "--sandbox-timeout",
        type=positive_int,
        default=int_env("MIMO_SANDBOX_TIMEOUT", 1800),
    )
    parser.add_argument(
        "--exec-timeout",
        type=positive_int,
        default=int_env("MIMO_AGENT_EXEC_TIMEOUT", 900),
    )
    parser.add_argument("--skip-agent", action="store_true")
    parser.add_argument("--skip-result-check", action="store_true")
    parser.add_argument("--raw", action="store_true")
    args = parser.parse_args()
    if args.prompt is None:
        args.prompt = DEFAULT_PROMPT.format(workspace=args.workspace)
    return args


def build_rules(secret: str) -> list[Rule]:
    host = mimo_api_host()
    return [
        Rule(
            name="allow_mimo_platform",
            match=Match(scheme="https", sni=host, host=host),
            action=Action(
                allow=True,
                audit="metadata",
                inject=[Inject(**spec) for spec in mimo_inject(secret)],
            ),
        )
    ]


def create_sandbox(
    template_id: str,
    secret: str,
    timeout: int,
    *,
    api_url: str,
    api_key: str,
) -> Sandbox:
    try:
        config = Config(
            api_url=api_url,
            api_key=api_key,
            template_id=template_id,
        )
    except TypeError as exc:
        if not is_unexpected_keyword_error(exc, "api_key"):
            raise
        print(
            "Installed CubeSandbox SDK does not support Cube API key forwarding; "
            "CubeAPI authentication must be disabled or the SDK must be upgraded.",
            file=sys.stderr,
        )
        config = Config(api_url=api_url, template_id=template_id)
    return Sandbox.create(
        template=template_id,
        allow_internet_access=False,
        network={"rules": build_rules(secret)},
        timeout=timeout,
        config=config,
    )


def verify_ca_bundle(sandbox: Sandbox, envs: dict[str, str]) -> None:
    ca_bundle = shlex.quote(envs["NODE_EXTRA_CA_CERTS"])
    result = run_command(
        sandbox,
        f"test -r {ca_bundle}",
        timeout=30,
    )
    ensure_success(
        result,
        f"verify CubeEgress CA bundle at {envs['NODE_EXTRA_CA_CERTS']!r}",
    )
    print(f"CubeEgress CA bundle is readable: {envs['NODE_EXTRA_CA_CERTS']}")


def show_secret_boundary(sandbox: Sandbox, envs: dict[str, str]) -> None:
    key = shlex.quote(MIMO_API_KEY_ENV)
    home = shlex.quote(envs["MIMOCODE_HOME"])
    result = run_command(
        sandbox,
        shell_join(
            f"test \"$(printenv {key})\" = {shlex.quote(PLACEHOLDER_KEY)}",
            f"test ! -f {home}/data/auth.json",
            f"printf 'In-VM {MIMO_API_KEY_ENV}: %s\\n' \"$(printenv {key})\"",
        ),
        envs=envs,
        timeout=30,
    )
    ensure_success(result, "verify the CubeEgress secret boundary")
    print(getattr(result, "stdout", "").strip())


def show_unmatched_host_blocked(sandbox: Sandbox) -> None:
    result = run_command(
        sandbox,
        "curl -s -o /dev/null -w '%{http_code}' --max-time 8 https://example.com",
        timeout=30,
    )
    status = getattr(result, "stdout", "").strip()
    if status not in {"000", "403"}:
        raise SystemExit(
            "Expected unmatched egress to be blocked (curl status 000 or 403), "
            f"got {status or 'no response'}"
        )
    if status == "403":
        print("Unmatched host example.com: 403 (blocked by CubeEgress as expected)")
    else:
        print("Unmatched host example.com: 000 (blocked by the network data plane)")


def verify_agent_result(sandbox: Sandbox, workspace: str) -> None:
    result_file = shlex.quote(f"{workspace}/egress_check.md")
    result = run_command(
        sandbox,
        shell_join(
            f"test -f {result_file}",
            f"grep -Fq CUBE_MIMO_EGRESS_OK {result_file}",
            f"cat {result_file}",
        ),
        timeout=60,
    )
    ensure_success(result, "verify the CubeEgress MiMo result")
    if getattr(result, "stdout", ""):
        print(f"\n--- egress_check.md ---\n{result.stdout}")


def main() -> int:
    load_local_dotenv()
    args = parse_args()
    if args.raw:
        os.environ["MIMO_STREAM_RAW"] = "1"

    template_id = args.template or required("CUBE_TEMPLATE_ID")
    api_url = required("E2B_API_URL")
    api_key = required("E2B_API_KEY")
    secret = required(MIMO_API_KEY_ENV)

    envs = build_mimo_env(include_secret=False)
    envs[MIMO_API_KEY_ENV] = PLACEHOLDER_KEY
    envs["NODE_EXTRA_CA_CERTS"] = os.environ.get(
        "MIMO_NODE_EXTRA_CA_CERTS", DEFAULT_NODE_CA_BUNDLE
    )

    print(f"Allowed MiMo host: {mimo_api_host()}")
    print("All other internet egress is denied.")
    sandbox = create_sandbox(
        template_id,
        secret,
        args.sandbox_timeout,
        api_url=api_url,
        api_key=api_key,
    )
    sandbox_id = sandbox_identifier(sandbox)
    try:
        print(f"Sandbox ready: {sandbox_id}")
        verify_ca_bundle(sandbox, envs)
        show_secret_boundary(sandbox, envs)
        show_unmatched_host_blocked(sandbox)
        if args.skip_agent:
            print("--skip-agent set; CubeEgress checks complete.")
            return 0

        command = mimo_command(args.prompt, workspace=args.workspace, agent="build")
        print("\nRunning MiMo Code through CubeEgress...\n")
        result, events = run_mimo_command(
            sandbox,
            command,
            cwd=args.workspace,
            envs=envs,
            timeout=args.exec_timeout,
        )
        ensure_success(result, "run MiMo Code through CubeEgress")
        print(f"\nMiMo session ID: {session_id_from_events(events)}")
        if not args.skip_result_check:
            verify_agent_result(sandbox, args.workspace)
        show_secret_boundary(sandbox, envs)
        return 0
    finally:
        kill_sandbox(
            sandbox,
            sandbox_id,
            run_failed=sys.exc_info()[0] is not None,
        )


if __name__ == "__main__":
    sys.exit(main())
