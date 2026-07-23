#!/usr/bin/env python3
"""Create a PHP + Composer sandbox, then call its public API through CubeProxy."""

from __future__ import annotations

import requests

from common import application_url, load_local_dotenv, template_id, wait_for_health


def main() -> None:
    load_local_dotenv()
    from e2b_code_interpreter import Sandbox

    with Sandbox.create(template=template_id()) as sandbox:
        version = sandbox.commands.run("php --version && composer --version")
        print(version.stdout)

        base_url = application_url(sandbox)
        health = wait_for_health(requests.get, base_url)
        hello = requests.get(f"{base_url}/api/hello", params={"name": "CubeSandbox"}, timeout=10)
        hello.raise_for_status()

        print(f"sandbox_id={sandbox.sandbox_id}")
        print(f"health={health}")
        print(f"hello={hello.json()}")


if __name__ == "__main__":
    main()
