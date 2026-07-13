"""Boot the Ruby template and exercise its public Sinatra endpoint."""

from __future__ import annotations

import argparse
import requests
from cubesandbox import Sandbox

from common import load_environment, public_url, required, tls_verify, wait_for_app


def main() -> None:
    load_environment()
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--timeout", type=int, default=300)
    args = parser.parse_args()

    required("E2B_API_KEY")
    template_id = required("CUBE_TEMPLATE_ID")

    with Sandbox.create(template=template_id, timeout=args.timeout) as sandbox:
        health = wait_for_app(sandbox)
        print("health PASS:", health)

        response = requests.post(
            public_url(sandbox, "/counter"), timeout=10, verify=tls_verify()
        )
        response.raise_for_status()
        counter = response.json()
        if counter != {"counter": 1}:
            raise RuntimeError(f"Expected counter=1, got {counter!r}")
        print("counter increment PASS:", counter)

        response = requests.get(
            public_url(sandbox, "/counter"), timeout=10, verify=tls_verify()
        )
        response.raise_for_status()
        persisted = response.json()
        if persisted != counter:
            raise RuntimeError(f"Counter was not persisted: {persisted!r}")
        print("counter readback PASS:", persisted)
        print("public URL:", public_url(sandbox, "/counter"))


if __name__ == "__main__":
    main()
