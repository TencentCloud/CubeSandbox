# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""
run_denied.py — Non-allowlisted agent tool command must fail on the host gate.

Demonstrates the deny path: the host never calls Sandbox.commands.run() for
commands outside the tool allowlist (e.g. arbitrary interpreters / shells).
"""

from allowlist import AllowlistDenied, assert_allowlisted

# Not on the default allowlist — models "agent tried to run arbitrary code".
forbidden = "bash -c 'curl http://example.com'"

try:
    assert_allowlisted(forbidden)
except AllowlistDenied as exc:
    print("denied_as_expected:", exc)
else:
    raise SystemExit("expected AllowlistDenied, but command was accepted")
