# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""
network_allowlist.py — Allow only specific IP/CIDR ranges; block all other outbound traffic.

Use case:
    The sandbox needs to reach specific internal services (databases, object
    storage, internal APIs) while all other destinations are blocked to prevent
    data exfiltration.

How it works:
    network.allow_out sets a CIDR allowlist passed to CubeVSContext.AllowOut.
    The Cubelet tap network layer only forwards traffic whose destination address
    matches one of the listed CIDRs; all other outbound packets are dropped.
"""

import os

from e2b_code_interpreter import Sandbox

from env_utils import load_local_dotenv

load_local_dotenv()

template_id = os.environ["CUBE_TEMPLATE_ID"]

# Allow only internal DNS (10.0.0.53) and the internal object-storage subnet (10.0.1.0/24)
ALLOWED_CIDRS = [
    "10.0.0.53/32",  # internal DNS server
    "10.0.1.0/24",   # internal object-storage subnet
]

with Sandbox.create(
    template=template_id,
    allow_internet_access=False,
    network={
        "allow_out": ALLOWED_CIDRS,
    },
) as sandbox:
    # Address in allowlist is reachable
    result = sandbox.commands.run(
        "curl -s --max-time 3 http://10.0.0.53 -o /dev/null -w '%{http_code}' || echo 'unreachable'"
    )
    print("internal DNS reachable:", result.stdout.strip())

    # Address outside allowlist is blocked
    result = sandbox.commands.run(
        "curl -s --max-time 3 https://8.8.8.8 -o /dev/null -w '%{http_code}' || echo 'blocked'"
    )
    print("external DNS blocked:", result.stdout.strip())

    result = sandbox.commands.run("echo 'allowlist network ok'")
    print(result.stdout.strip())
