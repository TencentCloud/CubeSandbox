# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import json
import os
import sys
import urllib.request


def main() -> int:
    port = int(os.environ.get("NODE_WEB_PORT") or os.environ.get("PORT", "3000"))
    url = f"http://127.0.0.1:{port}/api/hello"

    with urllib.request.urlopen(url, timeout=10) as response:
        body = response.read().decode("utf-8")

    payload = json.loads(body)
    if response.status != 200:
        print(f"unexpected status: {response.status}", file=sys.stderr)
        return 1
    if payload.get("message") != "hello from CubeSandbox Node.js":
        print(f"unexpected payload: {payload!r}", file=sys.stderr)
        return 1

    print(f"localhost smoke ok: {payload['message']}")
    print(f"runtime: {payload.get('runtime', '<unknown>')}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
