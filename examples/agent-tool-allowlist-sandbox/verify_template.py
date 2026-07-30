# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""
verify_template.py — Smoke-check the agent-tool allowlist BYOI template.

Requires a live CubeAPI + CUBE_TEMPLATE_ID built from this directory's
Dockerfile. Reuses the host argv gate from code-sandbox-quickstart.
"""

from __future__ import annotations

import os
import sys
from pathlib import Path

from e2b_code_interpreter import Sandbox

ROOT = Path(__file__).resolve().parent
QUICKSTART = ROOT.parent / "code-sandbox-quickstart"
sys.path.insert(0, str(QUICKSTART))

from env_utils import load_local_dotenv  # noqa: E402
from tool_allowlist import AllowlistDenied, assert_allowlisted  # noqa: E402

load_local_dotenv()
local_env = ROOT / ".env"
if local_env.is_file():
    from dotenv import load_dotenv

    load_dotenv(local_env, override=True)

template_id = os.environ["CUBE_TEMPLATE_ID"]

# Host deny still works before create.
try:
    assert_allowlisted("bash -c id")
except AllowlistDenied as exc:
    print("host_deny:", exc)
else:
    raise SystemExit("expected host deny for bash")

with Sandbox.create(
    template=template_id,
    allow_internet_access=False,
    timeout=60,
) as sandbox:
    sid = getattr(sandbox, "sandbox_id", None) or getattr(sandbox, "id", "?")
    print("sandbox_id:", sid)

    profile_cmd = "cat /etc/cube-sandbox/tool-profile.txt"
    assert_allowlisted(profile_cmd)
    profile = sandbox.commands.run(profile_cmd).stdout.strip()
    print("tool-profile:\n" + profile)
    required = {"echo", "uname", "cat", "sha256sum"}
    present = {line.strip() for line in profile.splitlines() if line.strip()}
    missing = required - present
    if missing:
        raise SystemExit(f"tool-profile missing {missing}")

    hello = "echo agent-tool-allowlist-ok"
    assert_allowlisted(hello)
    out = sandbox.commands.run(hello).stdout.strip()
    if out != "agent-tool-allowlist-ok":
        raise SystemExit(f"unexpected echo: {out!r}")
    print("echo:", out)

print("TEMPLATE_VERIFY_OK")
