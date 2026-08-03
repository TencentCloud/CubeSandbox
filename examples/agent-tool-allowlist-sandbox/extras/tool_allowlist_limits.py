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
  4b) extra_binaries requires allow_unsafe_allowlist_extension=True
  5) Clean allowlisted command still accepted
  6) Reminder: allowlisting cat/ls is not guest least-privilege
"""

import _path  # noqa: F401
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

# 4b) Extra binaries are not a silent default API
try:
    assert_allowlisted("curl -s https://example.com", extra_binaries={"curl"})
except ValueError as exc:
    print(f"[denied_as_expected] extra_without_unsafe_flag: {exc}")
else:
    raise SystemExit("expected ValueError when extending allowlist without unsafe flag")

expect_allowed(
    "curl_with_unsafe_flag",
    "curl -s https://example.com",
    extra_binaries={"curl"},
    allow_unsafe_allowlist_extension=True,
)
print("  note: allow_unsafe_allowlist_extension=True is required to add binaries")

# 5) Happy path still works
expect_allowed("clean_echo", "echo agent-tool-allowlist-ok")

# 6) Documented non-goal (regression lock): argv gate is NOT path confinement.
# If a future change starts denying `cat /etc/passwd`, that belongs in a
# *separate* guest/path policy layer — do not silently fold it into argv0
# checks without updating the threat model + tests together.
if not is_allowlisted("cat /etc/passwd"):
    raise SystemExit("expected cat /etc/passwd to pass argv gate (non-goal)")
print(
    "[out_of_scope] cat /etc/passwd is allowlisted here — guest isolation "
    "(MicroVM) + path policy must cover that, not this argv gate"
)

# Residual (intentional): simple redirects are not treated as shell-chaining
# meta for this demo. Security note: allowlisted `echo` + `>` is an *arbitrary
# guest file write* vector. MicroVM isolation + least privilege must cover it.
if not is_allowlisted("echo artifact-ok > /tmp/x"):
    raise SystemExit("expected redirect form to remain allowlisted")
print(
    "[out_of_scope] echo … > file remains allowlisted — arbitrary guest "
    "writes are a guest-isolation concern, not covered by argv gating"
)

# Bash-only constructs that keep argv0 allowlisted while doing more work —
# refused by the host gate (see bot review on process substitution /dev/tcp).
expect_denied("process_substitution", "cat <(echo hi)")
expect_denied("dev_tcp", "echo x > /dev/tcp/127.0.0.1/80")

print("LIMITS_DEMO_OK")
