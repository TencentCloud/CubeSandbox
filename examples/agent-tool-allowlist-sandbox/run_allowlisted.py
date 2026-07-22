# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""
run_allowlisted.py — Execute an allowlisted agent tool command inside a sandbox.

Demonstrates the happy path: host-side gate accepts the command, then
Sandbox.commands.run() runs it in a MicroVM and returns stdout.

Also stacks Cube Mode-1 airgap (allow_internet_access=False): argv allowlist
and platform egress are orthogonal — a passed gate does not imply network.

Artifact write uses the SDK files API so the default tool allowlist stays
free of interpreters (code execution is an explicit capability elsewhere).
"""

import os

from e2b_code_interpreter import Sandbox

from allowlist import assert_allowlisted
from env_utils import load_local_dotenv

load_local_dotenv()

template_id = os.environ["CUBE_TEMPLATE_ID"]

# Agent-style tool call: only the allowlisted binary "echo" is invoked.
command = "echo agent-tool-allowlist-ok"

assert_allowlisted(command)

# network-policy Mode 1: no internet; local echo/artifact still work.
print("egress: allow_internet_access=False (airgap; argv gate != network)")
with Sandbox.create(
    template=template_id,
    allow_internet_access=False,
) as sandbox:
    result = sandbox.commands.run(command)
    print(result.stdout.strip())

    # Demo path != privilege path: do not require guest python3 for artifacts.
    sandbox.files.write("/tmp/tool_out.txt", "artifact-ok\n")
    content = sandbox.files.read("/tmp/tool_out.txt")
    print("artifact:", content.strip())
