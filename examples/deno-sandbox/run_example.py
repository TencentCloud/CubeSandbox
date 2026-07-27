#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Create a Deno sandbox, verify the template, and exercise its HTTP API."""

from __future__ import annotations

import argparse

from cubesandbox import Sandbox

from common import (
    assert_public_access_restricted,
    assert_public_egress_blocked,
    counter_request,
    load_environment,
    public_url,
    required,
    run_checked,
    sandbox_create_options,
    sandbox_identifier,
    start_service,
    wait_for_app,
)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--template", help="Defaults to CUBE_TEMPLATE_ID")
    parser.add_argument("--timeout", type=int, default=300)
    parser.add_argument("--poll-timeout", type=float, default=60)
    return parser.parse_args()


def main() -> int:
    load_environment()
    args = parse_args()
    required("E2B_API_URL")
    required("E2B_API_KEY")
    template_id = args.template or required("CUBE_TEMPLATE_ID")

    print(f"Creating sandbox from template: {template_id}")
    with Sandbox.create(**sandbox_create_options(template_id, args.timeout)) as sandbox:
        print(f"Sandbox ready: {sandbox_identifier(sandbox)}")

        version = run_checked(
            sandbox,
            "deno --version",
            action="check Deno version",
            timeout=30,
        )
        print(getattr(version, "stdout", "").strip())

        verification = run_checked(
            sandbox,
            "deno task verify",
            action="run Deno format/lint/check/tests",
            timeout=180,
        )
        print(getattr(verification, "stdout", "").strip())

        egress = assert_public_egress_blocked(sandbox)
        print(f"Default-deny egress PASS: {egress}")

        pid = start_service(sandbox)
        health = wait_for_app(sandbox, timeout=args.poll_timeout)
        print(f"Service ready (pid={pid}): {health}")

        blocked_status = assert_public_access_restricted(sandbox)
        print(f"Restricted public access PASS: HTTP {blocked_status} without token")

        first = counter_request(sandbox, "POST")
        second = counter_request(sandbox, "POST")
        persisted = counter_request(sandbox)
        if first != {"counter": 1} or second != {"counter": 2}:
            raise RuntimeError(f"Unexpected increments: {first!r}, {second!r}")
        if persisted != second:
            raise RuntimeError(f"Counter was not persisted: {persisted!r}")

        print(f"Counter persistence PASS: {persisted}")
        print(
            "Restricted counter URL (traffic token required): "
            f"{public_url(sandbox, '/counter')}"
        )

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
