"""Prove that Sinatra workspace state survives CubeSandbox pause/resume."""

from __future__ import annotations

import argparse
import sys

import requests
from cubesandbox import Sandbox

from common import (
    load_environment,
    public_url,
    required,
    tls_verify,
    wait_for_app,
    wait_until_paused,
)


def main() -> None:
    load_environment()
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--timeout", type=int, default=600)
    parser.add_argument("--poll-timeout", type=int, default=60)
    parser.add_argument("--port", type=int, default=4567)
    args = parser.parse_args()

    required("E2B_API_URL")
    required("E2B_API_KEY")
    template_id = required("CUBE_TEMPLATE_ID")
    sandbox = Sandbox.create(template=template_id, timeout=args.timeout)
    sandbox_id = sandbox.sandbox_id

    try:
        wait_for_app(sandbox, timeout=args.poll_timeout, port=args.port)
        response = requests.post(
            public_url(sandbox, "/counter", args.port), timeout=10, verify=tls_verify()
        )
        response.raise_for_status()
        before_pause = response.json()
        if before_pause != {"counter": 1}:
            raise RuntimeError(f"Expected counter=1 before pause, got {before_pause!r}")
        print(f"Counter API before pause PASS: {before_pause}")

        print(f"Pausing {sandbox_id} with counter=1")
        counter_url = public_url(sandbox, "/counter", args.port)
        paused_id = sandbox.pause()
        if isinstance(paused_id, str) and paused_id:
            sandbox_id = paused_id
        wait_until_paused(counter_url)
        print("Pause state PASS: public endpoint is unreachable")

        sandbox = Sandbox.connect(sandbox_id=sandbox_id)
        wait_for_app(sandbox, timeout=args.poll_timeout, port=args.port)
        response = requests.get(
            public_url(sandbox, "/counter", args.port), timeout=10, verify=tls_verify()
        )
        response.raise_for_status()
        after_resume = response.json()
        if after_resume != before_pause:
            raise RuntimeError(
                f"Expected restored counter {before_pause!r}, got {after_resume!r}"
            )
        print(f"Counter API after resume PASS: {after_resume}")
        print(f"Restored {sandbox_id}; application state persisted")
    finally:
        try:
            sandbox.kill()
        except Exception as exc:
            warning = f"Warning: failed to kill sandbox {sandbox_id}: {exc}"
            print(warning, file=sys.stderr)


if __name__ == "__main__":
    main()
