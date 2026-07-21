# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""
run_allowlisted_sidecar.py — Host-machine allow path without wildcard DNS.

Uses the in-repo e2b-dev-sidecar (docs/guide/connect-existing-cluster.md
Option D) so the official E2B SDK can reach CubeProxy data plane via a local
proxy instead of resolving *.cube.app.

This does not change the host-side argv gate: assert_allowlisted() still runs
before Sandbox.create() / commands.run().
"""

from __future__ import annotations

import os
import sys
from pathlib import Path

from allowlist import assert_allowlisted
from env_utils import load_local_dotenv

load_local_dotenv()

# Import sibling example without packaging it as a dependency.
_SIDECAR_DIR = Path(__file__).resolve().parents[1] / "e2b-dev-sidecar"
if not (_SIDECAR_DIR / "dev_sidecar.py").is_file():
    raise SystemExit(
        f"missing e2b-dev-sidecar at {_SIDECAR_DIR}; "
        "clone/check out examples/e2b-dev-sidecar from this repository"
    )
sys.path.insert(0, str(_SIDECAR_DIR))

from dev_sidecar import setup_dev_sidecar  # noqa: E402

# Defaults match examples/e2b-dev-sidecar/env.example for QEMU host forwards.
os.environ.setdefault("E2B_API_URL", "http://127.0.0.1:13000")
os.environ.setdefault("E2B_API_KEY", "e2b_000000")
os.environ.setdefault("CUBE_REMOTE_PROXY_BASE", "https://127.0.0.1:11443")
os.environ.setdefault("CUBE_REMOTE_PROXY_VERIFY_SSL", "false")

setup_dev_sidecar()

from e2b_code_interpreter import Sandbox  # noqa: E402

template_id = os.environ["CUBE_TEMPLATE_ID"]

command = "echo agent-tool-allowlist-ok"
assert_allowlisted(command)

with Sandbox.create(template=template_id) as sandbox:
    result = sandbox.commands.run(command)
    print(result.stdout.strip())

    artifact_cmd = "python3 -c \"open('/tmp/tool_out.txt','w').write('artifact-ok\\n')\""
    assert_allowlisted(artifact_cmd)
    sandbox.commands.run(artifact_cmd)

    # Prefer allowlisted `cat` over files.read for host+sidecar setups: some
    # proxy paths mishandle Content-Encoding on the files API (see evidence).
    read_cmd = "cat /tmp/tool_out.txt"
    assert_allowlisted(read_cmd)
    content = sandbox.commands.run(read_cmd).stdout
    print("artifact:", content.strip())
