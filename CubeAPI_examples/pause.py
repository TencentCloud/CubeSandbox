# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
pause.py — Pause a sandbox then reconnect (auto-resume) via connect().

Original used: sandbox.pause() + sandbox.connect()
Ported to: cubesandbox v0.1.0 sb.pause() + Sandbox.connect().

Data-plane stream (HTTP:80) is exercised after resume to verify state.

Env vars:
    CUBE_API_URL       management plane, e.g. http://9.135.79.34:3000
    CUBE_TEMPLATE_ID   sandbox template
    CUBE_PROXY_NODE_IP data-plane IP (HTTP port 80)
"""
import os
import sys
import time
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from cubesandbox import Sandbox

template_id = os.environ["CUBE_TEMPLATE_ID"]

with Sandbox.create(template=template_id) as sandbox:
    # Set state before pause so we can verify it survives resume
    sandbox.run_code("state_var = 'hello after resume'")

    sandbox.pause()
    print("paused, waiting for snapshot...")

    # Wait for paused state
    for _ in range(15):
        time.sleep(2)
        info = sandbox.get_info()
        if info.get("state") == "paused":
            break
    print("state:", info.get("state"))

    # connect() auto-resumes the paused sandbox
    sandbox2 = Sandbox.connect(sandbox.sandbox_id)
    info = sandbox2.get_info()
    print("sandbox info %s" % info)

    # Verify state survived the pause/resume cycle
    result = sandbox2.run_code("state_var")
    print("state_var after resume:", result.text)

    sandbox2.kill()
