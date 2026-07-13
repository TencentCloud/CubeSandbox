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
    print("=== java -version ===")
    # java -version writes to stderr; merge so we can see it via stdout.
    r = sandbox.commands.run("java -version 2>&1")
    require_success(r, "java -version")
    print(r.stdout)
    print("exit_code:", r.exit_code)

    print("=== mvn -version ===")
    r = sandbox.commands.run("mvn -version 2>&1")
    require_success(r, "mvn -version")
    print(r.stdout)
    print("exit_code:", r.exit_code)

    print("=== read /app/HelloWorldServer.java ===")
    print(sandbox.files.read("/app/HelloWorldServer.java", user="root"))

    print("=== GET https://<sandbox>:8080/ (TLS terminates at the sandbox tunnel) ===")
    url = f"https://{sandbox.get_host(8080)}/"
    print(f"url: {url}")
    with urllib.request.urlopen(url, timeout=10) as resp:
        if resp.status != 200:
            raise RuntimeError(f"GET / returned HTTP {resp.status}")
        print(f"status: {resp.status}")
        body = resp.read().decode()
        if "Hello from Java inside a CubeSandbox MicroVM" not in body:
            raise RuntimeError("GET / response did not contain the expected page")
        print(body)
