# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import os
import urllib.request
from pathlib import Path

from dotenv import load_dotenv
from e2b import Sandbox

for candidate in (Path(__file__).with_name(".env"), Path.cwd() / ".env"):
    if candidate.is_file():
        load_dotenv(dotenv_path=candidate, override=False)
        break

template_id = os.environ["CUBE_TEMPLATE_ID"]


def require_success(result, label):
    if result.exit_code != 0:
        raise RuntimeError(f"{label} failed ({result.exit_code}): {result.stderr}")

print(f"template_id: {template_id}")
with Sandbox.create(template=template_id) as sandbox:
    print(f"sandbox_id: {sandbox.sandbox_id}")
    print("=== go version ===")
    r = sandbox.commands.run("go version")
    require_success(r, "go version")
    print(r.stdout)
    print("exit_code:", r.exit_code)

    print("=== GOPATH / GOROOT ===")
    r = sandbox.commands.run("go env GOROOT GOPATH")
    require_success(r, "go env")
    print(r.stdout)
    print("exit_code:", r.exit_code)

    print("=== read /app/main.go ===")
    print(sandbox.files.read("/app/main.go", user="root"))

    print("=== GET https://<sandbox>:8080/ (TLS terminates at the sandbox tunnel) ===")
    url = f"https://{sandbox.get_host(8080)}/"
    print(f"url: {url}")
    with urllib.request.urlopen(url, timeout=10) as resp:
        if resp.status != 200:
            raise RuntimeError(f"GET / returned HTTP {resp.status}")
        print(f"status: {resp.status}")
        body = resp.read().decode()
        if "Hello from Go inside a CubeSandbox MicroVM" not in body:
            raise RuntimeError("GET / response did not contain the expected page")
        print(body)
