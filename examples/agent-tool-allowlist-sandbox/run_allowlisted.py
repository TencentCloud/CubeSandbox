# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""
run_allowlisted.py — Execute an allowlisted agent tool command inside a sandbox.

Demonstrates the happy path: host-side gate accepts the command, then
Sandbox.commands.run() runs it in a MicroVM and returns stdout.
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

with Sandbox.create(template=template_id) as sandbox:
    result = sandbox.commands.run(command)
    print(result.stdout.strip())

    # Optional artifact: write a small file and read it back (product recovery).
    artifact_cmd = "python3 -c \"open('/tmp/tool_out.txt','w').write('artifact-ok\\n')\""
    assert_allowlisted(artifact_cmd)
    sandbox.commands.run(artifact_cmd)
    content = sandbox.files.read("/tmp/tool_out.txt")
    print("artifact:", content.strip())
