# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""
network_no_internet.py — Fully disable outbound internet access for a sandbox.

Use case:
    Tasks that do not require external network access (code execution, data
    processing, etc.) where you want to prevent any data exfiltration.

How it works:
    Setting allow_internet_access=False instructs Cubelet to set
    CubeVSContext.AllowInternetAccess=false in the tap network layer,
    which drops all outbound traffic to public IP addresses.
"""

import os

from e2b_code_interpreter import Sandbox

from env_utils import load_local_dotenv

load_local_dotenv()

template_id = os.environ["CUBE_TEMPLATE_ID"]

with Sandbox.create(
    template=template_id,
    allow_internet_access=False,
) as sandbox:
    # Verify: public internet is unreachable (curl should time out or be blocked)
    result = sandbox.commands.run(
        "curl -s --max-time 3 https://example.com -o /dev/null -w '%{http_code}' || echo 'blocked'"
    )
    print("internet access blocked:", result.stdout.strip() == "blocked" or result.exit_code != 0)

    # Internal sandbox logic still works normally
    result = sandbox.commands.run("echo 'isolated execution ok'")
    print(result.stdout.strip())
