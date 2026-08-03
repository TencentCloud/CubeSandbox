# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""
tool_allowlist_allow.py — Allowlisted tool runs in a MicroVM under airgap.

Host-side argv gate accepts the command, then Sandbox.commands.run() executes
it. Stacks allow_internet_access=False (same Mode-1 idea as network_no_internet.py):
argv allow ≠ network allow.
"""

import os

import _path  # noqa: F401

from e2b_code_interpreter import Sandbox

from env_utils import load_local_dotenv
from tool_allowlist import assert_allowlisted

load_local_dotenv()

template_id = os.environ["CUBE_TEMPLATE_ID"]
command = "echo agent-tool-allowlist-ok"

assert_allowlisted(command)

print("egress: allow_internet_access=False (airgap; argv gate != network)")
with Sandbox.create(
    template=template_id,
    allow_internet_access=False,
) as sandbox:
    result = sandbox.commands.run(command)
    print(result.stdout.strip())

    # Asymmetric I/O on purpose:
    # - Write via files.write: default allowlist has no dedicated write tool;
    #   `echo … > file` works but is a documented residual (arbitrary write),
    #   so this demo uses the SDK files API for the write half.
    # - Read via allowlisted `cat`: some proxy paths mishandle Content-Encoding
    #   on files.read (see e2b-dev-sidecar notes).
    sandbox.files.write("/tmp/tool_out.txt", "artifact-ok\n")
    read_cmd = "cat /tmp/tool_out.txt"
    assert_allowlisted(read_cmd)
    content = sandbox.commands.run(read_cmd).stdout
    print("artifact:", content.strip())
