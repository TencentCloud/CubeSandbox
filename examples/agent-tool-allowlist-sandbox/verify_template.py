# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""
verify_template.py — Smoke-check this example's BYOI template on a live CubeAPI.

Expect: host deny for bash, then tool-profile + echo → TEMPLATE_VERIFY_OK.
"""

from __future__ import annotations

import os
from pathlib import Path

from e2b_code_interpreter import Sandbox

from env_utils import load_local_dotenv
from tool_allowlist import AllowlistDenied, assert_allowlisted

ROOT = Path(__file__).resolve().parent


def main() -> None:
    load_local_dotenv()
    local_env = ROOT / ".env"
    if local_env.is_file():
        from dotenv import load_dotenv

        load_dotenv(local_env, override=True)

    template_id = os.environ["CUBE_TEMPLATE_ID"]

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


if __name__ == "__main__":
    main()
