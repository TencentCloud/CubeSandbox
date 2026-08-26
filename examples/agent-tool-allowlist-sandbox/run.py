# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""run.py — Happy-path template entry (build/register first; see README).

One command for reviewers:

    python run.py

Flow: host deny bash → Sandbox.create → cube-tool toolbox-hello (workload) →
read artifact → guest deny bash via cube-tool → RUN_OK.
"""

from __future__ import annotations

import os

from e2b_code_interpreter import Sandbox

from env_utils import load_local_dotenv
from tool_allowlist import (
    AllowlistDenied,
    assert_allowlisted,
    coerce_exit_code,
    exit_code_from_exc,
)


def main() -> None:
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
        timeout=90,
    ) as sandbox:
        sid = getattr(sandbox, "sandbox_id", None) or getattr(sandbox, "id", "?")
        print("sandbox_id:", sid)

        workload = "cube-tool toolbox-hello"
        assert_allowlisted(workload)
        out = sandbox.commands.run(workload).stdout.strip()
        print("workload:\n" + out)
        if "WORKLOAD_OK" not in out.splitlines():
            raise SystemExit(f"missing WORKLOAD_OK in {out!r}")

        read_cmd = "cube-tool cat /workspace/out/hello.txt"
        assert_allowlisted(read_cmd)
        artifact = sandbox.commands.run(read_cmd).stdout.strip()
        if artifact != "toolbox-hello-ok":
            raise SystemExit(f"unexpected artifact: {artifact!r}")
        print("artifact:", artifact)

        deny_guest = "cube-tool bash -c id"
        assert_allowlisted(deny_guest)
        try:
            result = sandbox.commands.run(deny_guest)
            code = coerce_exit_code(result.exit_code, what="command result")
        except Exception as exc:
            code = exit_code_from_exc(exc)
        if code == 0:
            raise SystemExit("cube-tool unexpectedly allowed bash")
        print(f"guest_deny: cube-tool bash exit={code}")

    print("RUN_OK")


if __name__ == "__main__":
    main()
