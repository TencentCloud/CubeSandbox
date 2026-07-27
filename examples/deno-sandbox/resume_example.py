#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Verify Deno state and dependency cache across CubeSandbox pause/resume."""

from __future__ import annotations

import argparse
import sys

from cubesandbox import Sandbox

from common import (
    cache_fingerprint,
    counter_request,
    load_environment,
    required,
    sandbox_create_options,
    sandbox_identifier,
    start_service,
    wait_for_app,
)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--template", help="Defaults to CUBE_TEMPLATE_ID")
    parser.add_argument("--timeout", type=int, default=600)
    parser.add_argument("--poll-timeout", type=float, default=60)
    return parser.parse_args()


def main() -> int:
    load_environment()
    args = parse_args()
    required("E2B_API_URL")
    required("E2B_API_KEY")
    template_id = args.template or required("CUBE_TEMPLATE_ID")

    sandbox = Sandbox.create(**sandbox_create_options(template_id, args.timeout))
    sandbox_id = "<unknown>"

    try:
        sandbox_id = sandbox_identifier(sandbox)
        print(f"Sandbox ready: {sandbox_id}")
        start_service(sandbox)
        wait_for_app(sandbox, timeout=args.poll_timeout)

        before_state = counter_request(sandbox, "POST")
        before_cache = cache_fingerprint(sandbox)
        print(f"Before pause: state={before_state}, cache={before_cache}")

        print(f"Pausing {sandbox_id}...")
        sandbox.pause(wait=True, timeout=60)

        print(f"Reconnecting to {sandbox_id}...")
        # connect() resumes the VM and returns a handle for SDK operations. Keep
        # the original create-time handle for CubeProxy requests because only it
        # carries the traffic token.
        resumed = Sandbox.connect(sandbox_id=sandbox_id)
        if sandbox_identifier(resumed) != sandbox_id:
            raise RuntimeError("CubeSandbox connected to an unexpected sandbox")

        # The create-time handle retains the traffic token required by CubeProxy.
        wait_for_app(sandbox, timeout=args.poll_timeout)

        after_state = counter_request(sandbox)
        after_cache = cache_fingerprint(resumed)
        if after_state != before_state:
            raise RuntimeError(
                f"Counter changed across pause/resume: {before_state!r} -> {after_state!r}"
            )
        if after_cache != before_cache:
            raise RuntimeError(
                f"Deno cache changed across pause/resume: {before_cache} -> {after_cache}"
            )

        continued = counter_request(sandbox, "POST")
        if continued["counter"] != before_state["counter"] + 1:
            raise RuntimeError(f"Counter did not continue after resume: {continued!r}")

        print(f"State restore PASS: {after_state}")
        print(f"Dependency cache restore PASS: {after_cache}")
        print(f"Post-resume write PASS: {continued}")
        return 0
    finally:
        try:
            # Both handles identify the same sandbox; killing the original
            # handle also terminates the sandbox referenced by resumed.
            sandbox.kill()
            print(f"Sandbox {sandbox_id} killed.")
        except Exception as exc:  # noqa: BLE001 - cleanup must not mask the test result
            print(
                f"Warning: failed to kill sandbox {sandbox_id}: {exc}",
                file=sys.stderr,
            )


if __name__ == "__main__":
    raise SystemExit(main())
