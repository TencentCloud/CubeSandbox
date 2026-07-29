# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""
tool_allowlist_limits.py — What the host gate covers (and what it does not).

Runs entirely on the host: no Sandbox.create. Use this to read the threat
model before trusting tool_allowlist_allow.py.

Cases:
  1) Shell chaining rejected (would bypass naive first-token-only checks)
  2) Path-style binary rejected
  3) Interpreters denied by default
  4) enable_code_execution=True is an explicit privilege escalation
  5) Clean allowlisted command still accepted
  6) Reminder: allowlisting cat/ls is not guest least-privilege
"""

from tool_allowlist import AllowlistDenied, assert_allowlisted, is_allowlisted


def expect_denied(label: str, command: str, **kwargs) -> None:
    try:
        assert_allowlisted(command, **kwargs)
    except AllowlistDenied as exc:
        print(f"[denied_as_expected] {label}: {exc}")
    else:
        raise SystemExit(f"expected AllowlistDenied for {label!r}, got accept")


def expect_allowed(label: str, command: str, **kwargs) -> None:
    assert_allowlisted(command, **kwargs)
    print(f"[allowed_as_expected] {label}: {command!r}")


# 1) Naive first-token check would see "echo" and allow; host gate refuses.
expect_denied("shell_metachar", "echo ok; bash -c 'id'")

# 2) Absolute/relative path as argv0
expect_denied("path_binary", "/bin/echo hi")

# 3) Default set excludes interpreters
expect_denied("python_default_off", "python3 -c 'print(1)'")

# 4) Explicit escalation — still host policy only; guest can do anything Python can
expect_allowed(
    "python_code_exec_flag",
    "python3 -c 'print(1)'",
    enable_code_execution=True,
)
print(
    "  note: enable_code_execution=True ≈ full guest FS/process power; "
    "pair with airgap + short timeout in real agents"
)

# 5) Happy path still works
expect_allowed("clean_echo", "echo agent-tool-allowlist-ok")

# 6) Documentation-only: allowlisted tools are not confined by this gate
assert is_allowlisted("cat /etc/passwd")
print(
    "[out_of_scope] cat /etc/passwd is allowlisted here — guest isolation "
    "(MicroVM) + path policy must cover that, not this argv gate"
)

print("LIMITS_DEMO_OK")
