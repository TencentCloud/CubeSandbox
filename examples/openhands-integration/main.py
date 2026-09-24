# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Minimal end-to-end demo: an OpenHands agent completes a coding task with
every command and file operation executed inside a CubeSandbox MicroVM.

The conversation runs against the agent server that is pre-baked into the
CubeSandbox template, so there is no per-session environment build. The LLM
is any OpenAI-compatible endpoint (configured via .env); the agent loop itself
runs inside the sandbox, which means the MicroVM needs egress to the LLM
endpoint (CubeSandbox's default egress policy allows this; see the README's
security section for allowlist-restricted variants).

Usage:
    python main.py

Requires .env: E2B_API_URL, E2B_API_KEY, CUBE_TEMPLATE_ID,
               LLM_MODEL, LLM_API_KEY, and optionally LLM_BASE_URL.
"""

import os
import sys
from pathlib import Path

from dotenv import load_dotenv
from openhands.sdk import LLM, Conversation
from openhands.tools.preset.default import get_default_agent
from pydantic import SecretStr

from cubesandbox_workspace import CubeSandboxWorkspace

load_dotenv(Path(__file__).resolve().parent / ".env")

TASK = (
    "Create /workspace/fib.py that prints the first 20 Fibonacci numbers, "
    "one per line. Run it with python3 and fix any errors until the output "
    "is correct."
)


def main() -> int:
    missing = [
        k
        for k in (
            "E2B_API_URL",
            "E2B_API_KEY",
            "CUBE_TEMPLATE_ID",
            "LLM_MODEL",
            "LLM_API_KEY",
        )
        if not os.getenv(k)
    ]
    if missing:
        print(f"missing environment variables: {', '.join(missing)}")
        print("copy .env.example to .env and fill it in first")
        return 2

    llm = LLM(
        model=os.environ["LLM_MODEL"],
        api_key=SecretStr(os.environ["LLM_API_KEY"]),
        base_url=os.getenv("LLM_BASE_URL") or None,
        usage_id="agent",
    )
    # cli_mode=True keeps the toolset to bash + file editing — no browser
    # stack is needed inside the template for this demo.
    agent = get_default_agent(llm=llm, cli_mode=True)

    with CubeSandboxWorkspace(template=os.environ["CUBE_TEMPLATE_ID"]) as workspace:
        print(f"sandbox {workspace.sandbox_id} up, agent server at {workspace.host}")
        conversation = Conversation(agent=agent, workspace=workspace)
        try:
            print(f"task: {TASK}\n")
            conversation.send_message(TASK)
            conversation.run()
        finally:
            conversation.close()

        # Ground truth straight from inside the MicroVM, independent of any
        # claim the agent made in conversation. The script's own output is
        # checked in isolation so source-code contents can't satisfy it.
        print("\n--- verification from inside the sandbox ---")
        proof_src = workspace.execute_command("cat /workspace/fib.py")
        print(proof_src.stdout)
        print("---")
        proof_run = workspace.execute_command("python3 /workspace/fib.py")
        print(proof_run.stdout)
        run_lines = proof_run.stdout.strip().splitlines()
        # Accept either canonical start (F0=0 or F1=1) — both are legitimate
        # readings of "the first 20 Fibonacci numbers", and the model picks
        # one or the other from run to run. Verifying the recurrence keeps
        # the check deterministic and stricter than any fixed anchor value.
        try:
            nums = [int(line.strip()) for line in run_lines]
        except ValueError:
            nums = []
        ok = (
            proof_run.exit_code == 0
            and len(nums) == 20
            and nums[:2] in ([0, 1], [1, 1])
            and all(nums[i] == nums[i - 1] + nums[i - 2] for i in range(2, 20))
        )
        if not ok and proof_run.stderr:
            print(f"stderr: {proof_run.stderr.strip()}")
        print("PASS" if ok else "FAIL: expected 20 Fibonacci numbers, one per line")
        return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
