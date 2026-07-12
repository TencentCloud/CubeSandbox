"""Prove that Sinatra workspace state survives CubeSandbox pause/resume."""

from __future__ import annotations

import argparse

from cubesandbox import Sandbox

from common import required, wait_for_app


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--timeout", type=int, default=600)
    args = parser.parse_args()

    required("E2B_API_KEY")
    template_id = required("CUBE_TEMPLATE_ID")
    sandbox = Sandbox.create(template=template_id, timeout=args.timeout)
    sandbox_id = sandbox.sandbox_id

    try:
        wait_for_app(sandbox)
        sandbox.commands.run("printf '41\\n' > /workspace/data/counter.txt")
        print(f"Pausing {sandbox_id} with counter=41")
        sandbox.pause()

        sandbox = Sandbox.connect(sandbox_id=sandbox_id)
        wait_for_app(sandbox)
        value = sandbox.files.read("/workspace/data/counter.txt").strip()
        if value != "41":
            raise RuntimeError(f"Expected restored counter 41, got {value!r}")
        print(f"Restored {sandbox_id}; counter={value}")
    finally:
        try:
            sandbox.kill()
        except Exception:
            pass


if __name__ == "__main__":
    main()
