"""Boot the Ruby template and exercise its public Sinatra endpoint."""

from __future__ import annotations

import argparse
import os

import requests
from cubesandbox import Sandbox

from common import public_url, required, wait_for_app


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--timeout", type=int, default=300)
    args = parser.parse_args()

    required("E2B_API_KEY")
    template_id = required("CUBE_TEMPLATE_ID")
    verify = os.getenv("REQUESTS_CA_BUNDLE", True)

    with Sandbox.create(template=template_id, timeout=args.timeout) as sandbox:
        print("health:", wait_for_app(sandbox))
        response = requests.post(public_url(sandbox, "/counter"), timeout=10, verify=verify)
        response.raise_for_status()
        print("counter:", response.json())
        print("public URL:", public_url(sandbox, "/counter"))


if __name__ == "__main__":
    main()
