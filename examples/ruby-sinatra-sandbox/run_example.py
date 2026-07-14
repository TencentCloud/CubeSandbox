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
    parser.add_argument("--poll-timeout", type=int, default=60)
    parser.add_argument("--port", type=int, default=4567)
    args = parser.parse_args()

    required("E2B_API_URL")
    required("E2B_API_KEY")
    template_id = required("CUBE_TEMPLATE_ID")

    with Sandbox.create(template=template_id, timeout=args.timeout) as sandbox:
        health = wait_for_app(sandbox, timeout=args.poll_timeout, port=args.port)
        print("health PASS:", health)

        response = requests.post(
            public_url(sandbox, "/counter", args.port), timeout=10, verify=tls_verify()
        )
        response.raise_for_status()
        counter = response.json()
        if counter != {"counter": 1}:
            raise RuntimeError(f"Expected counter=1, got {counter!r}")
        print("counter increment PASS:", counter)

        response = requests.post(
            public_url(sandbox, "/counter", args.port),
            timeout=10,
            verify=tls_verify(),
        )
        response.raise_for_status()
        counter = response.json()
        if counter != {"counter": 2}:
            raise RuntimeError(f"Expected counter=2, got {counter!r}")
        print("second counter increment PASS:", counter)

        response = requests.get(
            public_url(sandbox, "/counter", args.port), timeout=10, verify=tls_verify()
        )
        response.raise_for_status()
        persisted = response.json()
        if persisted != counter:
            raise RuntimeError(f"Counter was not persisted: {persisted!r}")
        print("counter readback PASS:", persisted)
        print("public URL:", public_url(sandbox, "/counter", args.port))


if __name__ == "__main__":
    main()
