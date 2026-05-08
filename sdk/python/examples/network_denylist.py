# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from cubesandbox import Sandbox

template_id = os.environ["CUBE_TEMPLATE_ID"]

DENIED_CIDRS = [
    "169.254.0.0/16",      # link-local（metadata 通用段）
    "100.100.100.200/32",  # 阿里云 metadata
    "10.0.0.0/8",          # 内网管理段
]

with Sandbox.create(
    template=template_id,
    allow_internet_access=True,
    network={
        "deny_out": DENIED_CIDRS,
    },
) as sandbox:
    result = sandbox.commands.run(
        "curl -s --max-time 5 https://example.com -o /dev/null -w '%{http_code}'"
    )
    print("public internet:", result.stdout.strip())

    result = sandbox.commands.run(
        "curl -s --max-time 3 http://169.254.169.254/latest/meta-data/ || echo 'blocked'"
    )
    print("metadata endpoint blocked:", "blocked" in result.stdout or result.exit_code != 0)

    result = sandbox.commands.run("echo 'denylist network ok'")
    print(result.stdout.strip())
