# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""
tool_allowlist_checkpoint.py — Host argv gate + pause/resume state survival.

Differentiated scenario (#645 soft bar): long-running agent workspace that
checkpoints via sandbox.pause(), then continues under the same host allowlist.

Not an LLM agent. Flow:
  allowlisted write → pause → connect → allowlisted read survives
  mid-session deny still skips commands.run
"""

from __future__ import annotations

import os

from e2b_code_interpreter import Sandbox

from env_utils import load_local_dotenv
from tool_allowlist import AllowlistDenied, assert_allowlisted

load_local_dotenv()

template_id = os.environ["CUBE_TEMPLATE_ID"]
artifact = "/tmp/allowlist_checkpoint.txt"
payload = "checkpoint-v1"


def run_gated(sandbox: Sandbox, command: str) -> str:
    assert_allowlisted(command)
    return sandbox.commands.run(command).stdout.strip()


sandbox = Sandbox.create(
    template=template_id,
    allow_internet_access=False,
    timeout=120,
)
sandbox_id = getattr(sandbox, "sandbox_id", None) or getattr(sandbox, "id", "?")
print(f"sandbox_id: {sandbox_id}")

try:
    write_cmd = f"echo {payload} > {artifact}"
    print(f"propose write: {write_cmd!r}")
    run_gated(sandbox, write_cmd)

    read_cmd = f"cat {artifact}"
    print(f"propose read: {read_cmd!r}")
    before = run_gated(sandbox, read_cmd)
    print(f"before pause: {before!r}")
    if before != payload:
        raise SystemExit(f"unexpected before-pause content: {before!r}")

    print("action: sandbox.pause()")
    paused = sandbox.pause()
    if isinstance(paused, str) and paused:
        sandbox_id = paused
    print(f"paused handle: {sandbox_id}")

    print(f"action: Sandbox.connect({sandbox_id!r})")
    sandbox = Sandbox.connect(sandbox_id=sandbox_id)

    after = run_gated(sandbox, read_cmd)
    print(f"after resume: {after!r}")
    if after != payload:
        raise SystemExit(f"artifact lost across pause/resume: {after!r}")

    deny_cmd = "bash -c id"
    print(f"propose mid-session deny: {deny_cmd!r}")
    try:
        assert_allowlisted(deny_cmd)
    except AllowlistDenied as exc:
        print(f"host_deny (still): {exc}")
    else:
        raise SystemExit("expected mid-session deny after resume")

    print("CHECKPOINT_OK")
finally:
    try:
        sandbox.kill()
        print("lifecycle: sandbox.kill()")
    except Exception as exc:  # noqa: BLE001 — best-effort cleanup
        print(f"warn: kill failed: {exc}")
