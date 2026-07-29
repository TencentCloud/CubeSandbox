# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""
tool_allowlist_deny.py — Non-allowlisted agent tool fails on the host gate.

Host never calls Sandbox.create / commands.run for commands outside the
tool allowlist (e.g. arbitrary shells). Orthogonal to network_*.py egress.
"""

from tool_allowlist import AllowlistDenied, assert_allowlisted

forbidden = "bash -c 'curl http://example.com'"

try:
    assert_allowlisted(forbidden)
except AllowlistDenied as exc:
    print("denied_as_expected:", exc)
else:
    raise SystemExit("expected AllowlistDenied, but command was accepted")
