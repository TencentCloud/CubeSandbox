# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from cubesandbox import Sandbox

template_id = os.environ["CUBE_TEMPLATE_ID"]

with Sandbox.create(
    template=template_id,
    allow_internet_access=False,
) as sandbox:
    result = sandbox.commands.run(
        "curl -s --max-time 3 https://example.com -o /dev/null -w '%{http_code}' || echo 'blocked'"
    )
    print("internet access blocked:", result.stdout.strip() == "blocked" or result.exit_code != 0)

    result = sandbox.commands.run("echo 'isolated execution ok'")
    print(result.stdout.strip())
