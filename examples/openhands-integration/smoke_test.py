# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""No-LLM smoke test for the OpenHands x CubeSandbox integration.

Verifies, against a real CubeSandbox deployment and without needing any LLM
API key:

  1. a sandbox hot-starts from the template with the agent server already live
     (the create -> healthy latency is printed as evidence);
  2. the agent server answers /server_info through the platform proxy;
  3. bash execution round-trips through the OpenHands workspace API;
  4. file upload/download round-trips through the OpenHands workspace API.

For the pause/resume capability, see pause_resume.py.

Usage:
    python smoke_test.py

Requires .env (or exported env): E2B_API_URL, E2B_API_KEY, CUBE_TEMPLATE_ID.
"""

import os
import sys
import tempfile
import time
from pathlib import Path

from dotenv import load_dotenv

from cubesandbox_workspace import CubeSandboxWorkspace

load_dotenv(Path(__file__).resolve().parent / ".env")

PASS = "\033[32mPASS\033[0m"
FAIL = "\033[31mFAIL\033[0m"


def require_env() -> str:
    missing = [
        k
        for k in ("E2B_API_URL", "E2B_API_KEY", "CUBE_TEMPLATE_ID")
        if not os.getenv(k)
    ]
    if missing:
        print(f"{FAIL} missing environment variables: {', '.join(missing)}")
        print("      copy .env.example to .env and fill it in first")
        sys.exit(2)
    return os.environ["CUBE_TEMPLATE_ID"]


def main() -> int:
    template = require_env()
    failures = 0

    print(f"[1/4] creating sandbox from template {template} ...")
    t0 = time.monotonic()
    workspace = CubeSandboxWorkspace(template=template)
    create_ms = (time.monotonic() - t0) * 1000
    print(
        f"      {PASS} sandbox {workspace.sandbox_id} healthy in {create_ms:.0f} ms "
        "(agent server was pre-warmed in the template snapshot)"
    )

    try:
        print("[2/4] querying agent server /server_info through the proxy ...")
        info = workspace.get_server_info()
        print(f"      {PASS} server_info: {str(info)[:120]}")

        print("[3/4] bash round-trip via the OpenHands workspace API ...")
        result = workspace.execute_command("echo hello-from-$(uname -m)-microvm")
        if result.exit_code == 0 and "hello-from-" in result.stdout:
            print(f"      {PASS} stdout: {result.stdout.strip()}")
        else:
            failures += 1
            print(f"      {FAIL} exit={result.exit_code} stdout={result.stdout!r}")

        print("[4/4] file upload/download round-trip ...")
        payload = f"cube-openhands-smoke-{int(time.time())}\n"
        with tempfile.TemporaryDirectory() as tmp:
            src = Path(tmp) / "up.txt"
            src.write_text(payload)
            workspace.file_upload(src, "/workspace/smoke.txt")
            dst = Path(tmp) / "down.txt"
            workspace.file_download("/workspace/smoke.txt", dst)
            if dst.read_text() == payload:
                print(f"      {PASS} payload survived the round-trip")
            else:
                failures += 1
                print(f"      {FAIL} payload mismatch: {dst.read_text()!r}")
    finally:
        workspace.cleanup()

    print()
    if failures == 0:
        print(f"{PASS} all smoke checks passed")
        return 0
    print(f"{FAIL} {failures} smoke check(s) failed")
    return 1


if __name__ == "__main__":
    sys.exit(main())
