# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""
tool_allowlist_egress.py — Host argv gate stacked with CIDR egress policy.

Two orthogonal axes (#645 soft bar: restricted egress):
  - argv allowlist refuses tools on the host
  - network.allow_out / airgap refuse destinations in the MicroVM

This is not a replacement for examples/network-policy; it shows the stack.
"""

from __future__ import annotations

import os

from e2b_code_interpreter import Sandbox

from env_utils import load_local_dotenv
from tool_allowlist import AllowlistDenied, assert_allowlisted, coerce_exit_code, exit_code_from_exc

load_local_dotenv()

template_id = os.environ["CUBE_TEMPLATE_ID"]

# Same shape as network-policy Mode-2: internal CIDRs only.
ALLOWED_CIDRS = [
    "10.0.0.53/32",
    "10.0.1.0/24",
]

# 1) Host gate alone — never creates a sandbox for illegal tools.
for label, cmd in (
    ("shell", "bash -c id"),
    ("curl", "curl -s https://example.com"),
):
    try:
        assert_allowlisted(cmd)
    except AllowlistDenied as exc:
        print(f"host_deny[{label}]: {exc}")
    else:
        raise SystemExit(f"expected host deny for {label}")

# 2) Allowlisted tool under airgap + CIDR allow_out.
hello = "echo egress-stack-ok"
assert_allowlisted(hello)
print(f"network: allow_internet_access=False allow_out={ALLOWED_CIDRS}")

with Sandbox.create(
    template=template_id,
    allow_internet_access=False,
    network={"allow_out": ALLOWED_CIDRS},
    timeout=60,
) as sandbox:
    out = sandbox.commands.run(hello).stdout.strip()
    print(f"allowlisted: {out!r}")
    if out != "egress-stack-ok":
        raise SystemExit(f"unexpected: {out!r}")

    # 3) Even with unsafe argv extension, public egress should still fail.
    probe = "curl -s --max-time 3 https://example.com -o /dev/null"
    print(f"propose airgap probe (unsafe argv extend): {probe!r}")
    assert_allowlisted(
        probe,
        extra_binaries={"curl"},
        allow_unsafe_allowlist_extension=True,
    )
    try:
        result = sandbox.commands.run(probe)
        code = coerce_exit_code(result.exit_code, what="command result")
        stdout = (result.stdout or "").strip()
    except Exception as exc:  # SDK may raise on non-zero
        code = exit_code_from_exc(exc)
        stdout = (getattr(exc, "stdout", None) or "").strip()
    print(f"curl observation: exit={code} stdout={stdout!r}")
    if code == 127 or "not found" in stdout.lower():
        print("note: curl missing in image — argv+CIDR stack still demonstrated")
    elif code == 0:
        raise SystemExit("public curl unexpectedly succeeded under allow_out")
    else:
        print("check: public curl failed (egress held)")

print("EGRESS_STACK_OK")
