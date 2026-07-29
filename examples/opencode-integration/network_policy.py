#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Run OpenCode with default-deny egress and on-the-wire Hy3 key injection."""

from __future__ import annotations

import argparse
import os
import shlex
import sys

from _opencode_common import ensure_success, run_command, sandbox_identifier
from cubesandbox import Action, Inject, Match, Rule, Sandbox
from env_utils import (
    build_opencode_env,
    hy3_host,
    int_env,
    load_local_dotenv,
    opencode_command,
    opencode_workspace,
    required,
)

PLACEHOLDER_KEY = "cube-egress-managed-placeholder"
DEFAULT_CA_BUNDLE = "/etc/ssl/certs/ca-certificates.crt"
DEFAULT_PROMPT = """\
Reply with one short sentence confirming the model endpoint is reachable, then
write the same sentence to {workspace}/egress_check.md.
"""


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--template",
        default=os.environ.get("CUBE_TEMPLATE_ID"),
        help="Cube template ID. Defaults to CUBE_TEMPLATE_ID.",
    )
    parser.add_argument("--workspace", default=opencode_workspace())
    parser.add_argument("--prompt", default=None)
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
    parser.add_argument("--raw", action="store_true")
    args = parser.parse_args()
    if args.prompt is None:
        args.prompt = DEFAULT_PROMPT.format(workspace=args.workspace)
    return args


def build_rules(host: str, secret: str) -> list[Rule]:
    return [
        Rule(
            name="allow_tokenhub_hy3",
            match=Match(scheme="https", sni=host, host=host),
            action=Action(
                allow=True,
                audit="metadata",
                inject=[
                    Inject(
                        header="Authorization",
                        secret=secret,
                        format="Bearer ${SECRET}",
                    )
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


def verify_secret_boundary(
    sandbox: Sandbox,
    process_env: dict[str, str],
) -> None:
    if process_env.get("HY3_API_KEY") != PLACEHOLDER_KEY:
        raise SystemExit("OpenCode environment must contain only the placeholder key")

    ambient = run_command(
        sandbox,
        "printenv HY3_API_KEY || true",
        timeout=30,
    )
    ensure_success(ambient, "inspect the sandbox environment")
    if getattr(ambient, "stdout", "").strip():
        raise SystemExit(
            "HY3_API_KEY unexpectedly exists in the sandbox-wide environment"
        )

    process = run_command(
        sandbox,
        "printenv HY3_API_KEY",
        envs={"HY3_API_KEY": process_env["HY3_API_KEY"]},
        timeout=30,
    )
    ensure_success(process, "inspect the OpenCode process placeholder")
    value = getattr(process, "stdout", "").strip()
    if value != PLACEHOLDER_KEY:
        raise SystemExit("Expected only the CubeEgress placeholder in the process")
    print("Sandbox environment: no HY3_API_KEY")
    print("OpenCode process: placeholder only (real key remains in CubeEgress)")


def show_unrelated_host_is_blocked(sandbox: Sandbox) -> None:
    result = run_command(
        sandbox,
        "curl -s -o /dev/null -w '%{http_code}' --max-time 8 https://example.com "
        "|| echo blocked",
        timeout=30,
    )
    value = getattr(result, "stdout", "").strip() or "blocked"
    if "403" not in value and "blocked" not in value and not value.startswith("000"):
        raise SystemExit(
            f"Default-deny verification failed: unrelated host returned {value!r}"
        )
    print(f"Unrelated host result: {value} (expected 403/blocked)")


def verify_agent_output(sandbox: Sandbox, workspace: str) -> None:
    path = f"{shlex.quote(workspace)}/egress_check.md"
    result = run_command(
        sandbox,
        f"test -s {path} && printf '\\n--- egress_check.md ---\\n' && cat {path}",
        timeout=60,
    )
    ensure_success(result, "verify the egress demo artifact")
    print(getattr(result, "stdout", ""))


def main() -> int:
    load_local_dotenv()
    args = parse_args()
    if args.raw:
        os.environ["OPENCODE_STREAM_RAW"] = "1"

    template_id = args.template or required("CUBE_TEMPLATE_ID")
    required("E2B_API_URL")
    required("E2B_API_KEY")
    secret = required("HY3_API_KEY")
    host = hy3_host()
    rules = build_rules(host, secret)

    envs = build_opencode_env(include_secret=False)
    envs["HY3_API_KEY"] = PLACEHOLDER_KEY
    ca_bundle = os.environ.get("OPENCODE_CA_BUNDLE", DEFAULT_CA_BUNDLE)
    # The standalone OpenCode binary uses a JavaScript runtime. Set both common
    # CA variables so the CubeEgress interception CA is trusted across releases.
    envs["SSL_CERT_FILE"] = ca_bundle
    envs["NODE_EXTRA_CA_CERTS"] = ca_bundle

    print(f"Allowed host: {host}; every other destination is denied")
    sandbox = create_sandbox(template_id, rules, args.sandbox_timeout)
    sandbox_id = sandbox_identifier(sandbox)
    try:
        print(f"Sandbox ready: {sandbox_id}")
        verify_secret_boundary(sandbox, envs)
        show_unrelated_host_is_blocked(sandbox)

        if args.skip_agent:
            print("--skip-agent set: policy checked without invoking OpenCode.")
            return 0

        result = run_command(
            sandbox,
            opencode_command(args.prompt),
            cwd=args.workspace,
            envs=envs,
            timeout=args.exec_timeout,
            stream=True,
        )
        ensure_success(result, "run OpenCode through CubeEgress")
        verify_agent_output(sandbox, args.workspace)
        exit_code = getattr(result, "exit_code", 0)
        return 0 if exit_code is None else int(exit_code)
    finally:
        try:
            sandbox.kill()
            print(f"Sandbox {sandbox_id} killed.")
        except Exception as exc:  # noqa: BLE001 - cleanup must not hide task failure
            print(f"Warning: failed to kill {sandbox_id}: {exc}", file=sys.stderr)


if __name__ == "__main__":
    sys.exit(main())
