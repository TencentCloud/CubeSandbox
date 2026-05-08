# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from cubesandbox import Sandbox

template_id = os.environ["CUBE_TEMPLATE_ID"]

ALLOWED_CIDRS = [
    "10.0.0.53/32",   # 内部 DNS
    "10.0.1.0/24",    # 内部对象存储网段
]

with Sandbox.create(
    template=template_id,
    allow_internet_access=False,
    network={
        "allow_out": ALLOWED_CIDRS,
    },
) as sandbox:
    result = sandbox.commands.run(
        "curl -s --max-time 3 http://10.0.0.53 -o /dev/null -w '%{http_code}' || echo 'unreachable'"
    )
    print("internal DNS reachable:", result.stdout.strip())

    result = sandbox.commands.run(
        "curl -s --max-time 3 https://8.8.8.8 -o /dev/null -w '%{http_code}' || echo 'blocked'"
    )
    print("external DNS blocked:", result.stdout.strip())

    result = sandbox.commands.run("echo 'allowlist network ok'")
    print(result.stdout.strip())
