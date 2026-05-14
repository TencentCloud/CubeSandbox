# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""
network_denylist.py — Allow normal internet access but block specific IP/CIDR ranges.

Use case:
    The sandbox needs full internet access (to install packages, fetch resources,
    etc.) but specific addresses must be blocked — for example, cloud provider
    metadata endpoints and internal management subnets — to prevent sandbox code
    from probing host information or performing lateral movement.

How it works:
    network.deny_out sets a CIDR denylist passed to CubeVSContext.DenyOut.
    The Cubelet tap network layer drops all outbound packets whose destination
    address matches one of the listed CIDRs; all other traffic is allowed.
"""

import os

from e2b_code_interpreter import Sandbox

from env_utils import load_local_dotenv

load_local_dotenv()

template_id = os.environ["CUBE_TEMPLATE_ID"]

# Block cloud provider metadata endpoints and internal management subnets
DENIED_CIDRS = [
    "169.254.0.0/16",       # link-local — AWS/GCP/Tencent Cloud metadata range
    "100.100.100.200/32",   # Alibaba Cloud metadata endpoint
    "10.0.0.0/8",           # internal management subnet
]

with Sandbox.create(
    template=template_id,
    allow_internet_access=True,
    network={
        "deny_out": DENIED_CIDRS,
    },
) as sandbox:
    # Public internet is still accessible
    result = sandbox.commands.run(
        "curl -s --max-time 5 https://example.com -o /dev/null -w '%{http_code}'"
    )
    print("public internet:", result.stdout.strip())

    # Cloud metadata endpoint is blocked
    result = sandbox.commands.run(
        "curl -s --max-time 3 http://169.254.169.254/latest/meta-data/ || echo 'blocked'"
    )
    print("metadata endpoint blocked:", "blocked" in result.stdout or result.exit_code != 0)

    result = sandbox.commands.run("echo 'denylist network ok'")
    print(result.stdout.strip())
