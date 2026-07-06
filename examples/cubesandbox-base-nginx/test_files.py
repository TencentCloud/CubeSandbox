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

with Sandbox.create(template=template_id) as sandbox:
    print("=== read /etc/nginx/nginx.conf ===")
    print(sandbox.files.read("/etc/nginx/nginx.conf", user="root"))

    print("=== GET http://<sandbox>:80/ ===")
    url = f"https://{sandbox.get_host(80)}/"
    print(f"url: {url}")
    with urllib.request.urlopen(url, timeout=10) as resp:
        print(f"status: {resp.status}")
        print(resp.read().decode())
