#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Run Gemini CLI with default-deny egress and CubeEgress key injection."""

from __future__ import annotations

import argparse
import os
import shlex
import sys

from cubesandbox import Action, Inject, Match, Rule, Sandbox

from common import (
    ensure_success,
    gemini_command,
    int_env,
    load_dotenv,
    required,
    run_command,
    sandbox_id,
    shell_join,
)

DEFAULT_HOST = "generativelanguage.googleapis.com"
PLACEHOLDER_KEY = "cube-egress-managed-placeholder"


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--template", default=os.environ.get("CUBE_TEMPLATE_ID"))
    parser.add_argument("--model", default=os.environ.get("GEMINI_MODEL"))
    parser.add_argument("--host", default=os.environ.get("GEMINI_API_HOST", DEFAULT_HOST))
    parser.add_argument("--workspace", default=os.environ.get("GEMINI_WORKSPACE", "/workspace"))
    parser.add_argument("--approve-all", action="store_true")
    parser.add_argument("--sandbox-timeout", type=int, default=int_env("GEMINI_SANDBOX_TIMEOUT", 1800))
    parser.add_argument("--exec-timeout", type=int, default=int_env("GEMINI_EXEC_TIMEOUT", 900))
    return parser.parse_args()


def main() -> int:
    load_dotenv()
    args = parse_args()
    template = args.template or required("CUBE_TEMPLATE_ID")
    required("E2B_API_URL")
    required("E2B_API_KEY")
    secret = required("GEMINI_API_KEY")
    args.approve_all = args.approve_all or os.environ.get("GEMINI_APPROVE_ALL") == "1"

    # API-key mode sends x-goog-api-key to the public Generative Language API.
    # Lock both SNI and Host. Unmatched egress remains unavailable.
    rules = [
        Rule(
            name="allow_google_gemini_api",
            match=Match(scheme="https", sni=args.host, host=args.host),
            action=Action(
                allow=True,
                audit="metadata",
                inject=[Inject(header="x-goog-api-key", secret=secret)],
            ),
        )
    ]
    sandbox = Sandbox.create(
        template=template,
        allow_internet_access=False,
        network={"rules": rules},
        timeout=args.sandbox_timeout,
    )
    current_id = sandbox_id(sandbox)
    try:
        print(f"Sandbox ready: {current_id}; only {args.host} is allowed.")
        no_secret = run_command(sandbox, "printenv GEMINI_API_KEY || true", timeout=30, user="root")
        ensure_success(no_secret, "inspect in-VM key")
        value = getattr(no_secret, "stdout", "").strip() or "<unset>"
        print(f"In-VM GEMINI_API_KEY before command: {value}")

        command = shell_join(
            f"mkdir -p {shlex.quote(args.workspace)}",
            f"cd {shlex.quote(args.workspace)}",
            gemini_command(
                "Reply with one sentence confirming the secure egress route is available.",
                model=args.model,
                approve_all=args.approve_all,
            ),
        )
        # Gemini CLI receives a placeholder. CubeEgress replaces it only on the
        # allowed outbound request, so the true key never reaches the VM.
        result = run_command(
            sandbox,
            command,
            cwd=args.workspace,
            envs={
                "GEMINI_API_KEY": PLACEHOLDER_KEY,
                "NODE_EXTRA_CA_CERTS": "/etc/ssl/certs/ca-certificates.crt",
            },
            timeout=args.exec_timeout,
            user="root",
        )
        ensure_success(result, "run Gemini CLI through CubeEgress")
        print(getattr(result, "stdout", ""))
        return 0
    finally:
        try:
            sandbox.kill()
            print(f"Sandbox {current_id} killed.")
        except Exception as exc:  # noqa: BLE001
            print(f"Warning: failed to kill sandbox {current_id}: {exc}", file=sys.stderr)


if __name__ == "__main__":
    sys.exit(main())
