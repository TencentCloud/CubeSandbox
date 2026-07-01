#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""
network_policy.py — run Claude Code inside a sandbox where:

  * The Anthropic API key is *never* forwarded into the sandbox env.
  * Only api.anthropic.com is reachable; every other domain is default-denied.
  * CubeEgress injects `x-api-key: sk-ant-...` on the wire, so the header
    only lives in the operator's rule list.

This is the recommended "credential vault" pattern for running third-party
coding agents. The sandbox sees a bare API request; the secret never enters
the VM's environment, filesystem, or process space.
"""

from __future__ import annotations

import os
import shlex
import sys

from e2b import Sandbox

from env_utils import load_local_dotenv, required


PROMPT = "Write a file called ok.txt containing 'ok'."


def main() -> int:
    load_local_dotenv()
    template_id = required("CUBE_TEMPLATE_ID")
    api_key = required("ANTHROPIC_API_KEY")

    rules = [
        {
            "name": "allow_anthropic_api",
            "match": {
                "scheme": "https",
                "sni": "api.anthropic.com",
                "host": "api.anthropic.com",
            },
            "action": {
                "allow": True,
                "audit": "metadata",
                "inject": [
                    {
                        "header": "x-api-key",
                        "format": "${SECRET}",
                        "secret": api_key,
                    },
                    {
                        "header": "anthropic-version",
                        "format": "2023-06-01",
                    },
                ],
            },
        },
    ]

    print("[cube] creating sandbox with default-deny egress + Anthropic inject rule")
    with Sandbox.create(
        template=template_id,
        allow_internet_access=True,
        network={"rules": rules},
    ) as sandbox:
        # Deliberately do NOT forward ANTHROPIC_API_KEY into the sandbox.
        # The proxy will attach the auth header on the way out.
        envs: dict[str, str] = {}
        for name in ("ANTHROPIC_MODEL", "ANTHROPIC_BASE_URL"):
            value = os.environ.get(name)
            if value:
                envs[name] = value

        # Confirm the key is not present inside the sandbox.
        leak_check = sandbox.commands.run(
            "printenv | grep -c ANTHROPIC_API_KEY || true", user="root", timeout=10,
        )
        print(f"[verify] ANTHROPIC_API_KEY visible inside sandbox: {leak_check.stdout.strip()} times")

        cmd = (
            "cd /workspace && "
            f"claude --print --allowedTools 'Edit,Write,Read' -- {shlex.quote(PROMPT)}"
        )
        result = sandbox.commands.run(
            cmd, envs=envs, user="root", timeout=300,
            on_stdout=lambda m: sys.stdout.write(m),
            on_stderr=lambda m: sys.stderr.write(m),
        )
        print(f"\n[claude] exit_code={result.exit_code}")

        listing = sandbox.commands.run("ls -la /workspace", user="root")
        print(listing.stdout)

        return 0 if result.exit_code == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
