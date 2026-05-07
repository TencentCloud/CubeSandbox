# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
Example: Sandbox lifecycle — pause, connect, kill.
"""
import time
from cubesandbox import Config, Sandbox

config = Config(
    api_url="http://9.135.79.34:3000",
    template_id="tpl-6265796cee124256b4dcd6a1",
    proxy_node_ip="9.135.79.34",
)

# Create
sb = Sandbox.create(timeout=600, config=config)
print(f"created  : {sb.sandbox_id}")

sb.run_code("state = 42")

# Pause
sb.pause()
print("paused")

# Wait for paused state
for _ in range(10):
    time.sleep(2)
    info = sb.get_info()
    if info.get("state") == "paused":
        break

# Connect (auto-resume) — use Sandbox.connect() from a different process
sb2 = Sandbox.connect(sb.sandbox_id, config=config)
result = sb2.run_code("state")
print(f"state after resume = {result.text}")   # "42"

sb2.kill()
print("destroyed")
