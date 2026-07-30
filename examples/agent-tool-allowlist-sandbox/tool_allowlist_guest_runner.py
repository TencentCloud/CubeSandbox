# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""
tool_allowlist_guest_runner.py — Host allows cube-tool; guest re-checks profile.

Shows why this BYOI image is more than a text file: /usr/local/bin/cube-tool
only execs names listed in /etc/cube-sandbox/tool-profile.txt.

Recommended production shape for this example:
  host allowlist ≈ {cube-tool}  (+ optional bare tools for demos)
  guest cube-tool               → profile enforcement
"""

from __future__ import annotations

import os

from e2b_code_interpreter import Sandbox

from env_utils import load_local_dotenv
from tool_allowlist import AllowlistDenied, assert_allowlisted

load_local_dotenv()

template_id = os.environ["CUBE_TEMPLATE_ID"]


def run_ok(sandbox: Sandbox, command: str) -> str:
    assert_allowlisted(command)
    result = sandbox.commands.run(command)
    out = (result.stdout or "").strip()
    code = int(result.exit_code)
    if code != 0:
        raise SystemExit(f"expected exit 0 for {command!r}, got {code} out={out!r}")
    return out


def run_expect_fail(sandbox: Sandbox, command: str) -> None:
    assert_allowlisted(command)
    try:
        result = sandbox.commands.run(command)
        code = int(result.exit_code)
        out = (result.stdout or "").strip()
        err = (getattr(result, "stderr", None) or "").strip()
    except Exception as exc:  # SDK may raise on non-zero
        if not hasattr(exc, "exit_code"):
            raise
        code = int(exc.exit_code)
        out = (getattr(exc, "stdout", None) or "").strip()
        err = (getattr(exc, "stderr", None) or "").strip()
    print(f"guest_deny observation: exit={code} stdout={out!r} stderr={err!r}")
    if code == 0:
        raise SystemExit(f"expected cube-tool to refuse: {command!r}")


# Host still refuses bare shells.
try:
    assert_allowlisted("bash -c id")
except AllowlistDenied as exc:
    print(f"host_deny: {exc}")
else:
    raise SystemExit("expected host deny for bash")

# Host accepts cube-tool (first token); guest decides on the real tool.
ok_cmd = "cube-tool echo guest-runner-ok"
assert_allowlisted(ok_cmd)

with Sandbox.create(
    template=template_id,
    allow_internet_access=False,
    timeout=60,
) as sandbox:
    sid = getattr(sandbox, "sandbox_id", None) or getattr(sandbox, "id", "?")
    print(f"sandbox_id: {sid}")

    which = run_ok(sandbox, "cube-tool pwd")
    print(f"pwd via cube-tool: {which!r}")

    out = run_ok(sandbox, ok_cmd)
    print(f"echo via cube-tool: {out!r}")
    if out != "guest-runner-ok":
        raise SystemExit(f"unexpected: {out!r}")

    # Passes host gate (argv0=cube-tool) but must fail inside the guest.
    print("propose: cube-tool bash -c id  (host allow, guest deny)")
    run_expect_fail(sandbox, "cube-tool bash -c id")

print("GUEST_RUNNER_OK")
