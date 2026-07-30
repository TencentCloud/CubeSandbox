# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""
verify_template.py — Smoke-check this example's BYOI template on a live CubeAPI.

Expect: host deny for bash, then tool-profile + echo → TEMPLATE_VERIFY_OK.
"""

from __future__ import annotations

import os

from e2b_code_interpreter import Sandbox

from env_utils import load_local_dotenv
from tool_allowlist import AllowlistDenied, assert_allowlisted, coerce_exit_code, exit_code_from_exc


def main() -> None:
    # Same .env policy as other demos: load nearby file, do not override shell env.
    load_local_dotenv()

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

        # Image capability: guest cube-tool re-checks the profile.
        wrapped = "cube-tool echo via-cube-tool"
        assert_allowlisted(wrapped)
        wout = sandbox.commands.run(wrapped).stdout.strip()
        if wout != "via-cube-tool":
            raise SystemExit(f"unexpected cube-tool echo: {wout!r}")
        print("cube-tool echo:", wout)

        deny_guest = "cube-tool bash -c id"
        assert_allowlisted(deny_guest)
        try:
            result = sandbox.commands.run(deny_guest)
            code = coerce_exit_code(result.exit_code, what="command result")
        except Exception as exc:
            code = exit_code_from_exc(exc)
        if code == 0:
            raise SystemExit("cube-tool unexpectedly allowed bash")
        print(f"cube-tool bash denied: exit={code}")

    print("TEMPLATE_VERIFY_OK")


if __name__ == "__main__":
    main()
