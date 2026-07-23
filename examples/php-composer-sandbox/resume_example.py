#!/usr/bin/env python3
"""Verify that an application state file in /workspace survives pause/resume."""

from __future__ import annotations

import requests

from common import application_url, load_local_dotenv, template_id, wait_for_health


def main() -> None:
    load_local_dotenv()
    from e2b_code_interpreter import Sandbox

    sandbox = Sandbox.create(template=template_id())
    try:
        before_url = application_url(sandbox)
        wait_for_health(requests.get, before_url)
        message = "state persisted by CubeSandbox pause/resume"
        stored = requests.post(
            f"{before_url}/api/state", json={"message": message}, timeout=10
        )
        stored.raise_for_status()

        sandbox.pause()
        resumed = Sandbox.connect(sandbox.sandbox_id)
        after_url = application_url(resumed)
        wait_for_health(requests.get, after_url)
        state = requests.get(f"{after_url}/api/state", timeout=10)
        state.raise_for_status()
        if state.json().get("message") != message:
            raise RuntimeError(f"state did not survive pause/resume: {state.json()!r}")

        print(f"sandbox_id={sandbox.sandbox_id}")
        print(f"saved_state={stored.json()}")
        print(f"resumed_state={state.json()}")
    finally:
        sandbox.kill()


if __name__ == "__main__":
    main()
