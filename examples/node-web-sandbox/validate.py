# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import json
import os
import ssl
import sys
import urllib.request
from pathlib import Path


def load_local_dotenv() -> None:
    """Best-effort load of a nearby .env file without overriding real env vars."""
    for candidate in (Path(__file__).with_name(".env"), Path.cwd() / ".env"):
        if candidate.is_file():
            for line in candidate.read_text(encoding="utf-8").splitlines():
                stripped = line.strip()
                if not stripped or stripped.startswith("#") or "=" not in stripped:
                    continue
                key, value = stripped.split("=", 1)
                key = key.strip()
                value = value.strip().strip('"').strip("'")
                if key and key not in os.environ:
                    os.environ[key] = value
            return


def ssl_context():
    cert_file = os.environ.get("CUBE_SSL_CERT_FILE") or os.environ.get("SSL_CERT_FILE")
    if cert_file:
        return ssl.create_default_context(cafile=cert_file)
    return ssl.create_default_context()


def read_public_json(url: str) -> dict:
    with urllib.request.urlopen(url, timeout=15, context=ssl_context()) as response:
        body = response.read().decode("utf-8")
        if response.status != 200:
            raise RuntimeError(f"unexpected HTTP status {response.status}: {body}")
        return json.loads(body)


def main() -> int:
    load_local_dotenv()

    template_id = os.environ.get("CUBE_TEMPLATE_ID")
    if not template_id or template_id == "<template-id>":
        print("CUBE_TEMPLATE_ID must be set to a ready CubeSandbox template ID", file=sys.stderr)
        return 2

    from e2b import Sandbox

    port = int(os.environ.get("NODE_WEB_PORT", "3000"))
    print(f"Template: {template_id}")
    print(f"CubeAPI:  {os.environ.get('E2B_API_URL', '<unset>')}")
    print(f"Port:     {port}")

    with Sandbox.create(template=template_id, timeout=120) as sandbox:
        print(f"Sandbox:  {sandbox.sandbox_id}")

        result = sandbox.commands.run(
            "python3 /workspace/node-web-sandbox/smoke_test.py",
            timeout=30,
        )
        stdout = (result.stdout or "").strip()
        stderr = (result.stderr or "").strip()
        if stdout:
            print(stdout)
        if result.exit_code != 0:
            if stderr:
                print(stderr, file=sys.stderr)
            print(f"in-sandbox smoke test failed with exit code {result.exit_code}", file=sys.stderr)
            return int(result.exit_code or 1)

        public_url = f"https://{sandbox.get_host(port)}/api/hello"
        payload = read_public_json(public_url)
        if payload.get("message") != "hello from CubeSandbox Node.js":
            raise RuntimeError(f"unexpected public response: {payload!r}")

        print(f"public HTTP ok: {payload['message']}")
        print("node-web-sandbox validation ok")

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
