# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
network_no_internet.py — deny-all outbound: all external TCP is blocked.

Original used: allow_internet_access=False
Ported to: metadata={"network-policy": "deny-all"}

Verifies:
  - HTTP  port 80  outbound → blocked
  - HTTPS port 443 outbound → blocked
  - Data-plane stream (HTTP:80 via CubeProxy) still works (not affected by policy)

Env vars:
    CUBE_API_URL       management plane, e.g. http://9.135.79.34:3000
    CUBE_TEMPLATE_ID   sandbox template
    CUBE_PROXY_NODE_IP data-plane IP (HTTP port 80)
"""
import os
import sys
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from cubesandbox import Sandbox

template_id = os.environ["CUBE_TEMPLATE_ID"]

with Sandbox.create(
    template=template_id,
    metadata={"network-policy": "deny-all"},
) as sandbox:
    info = sandbox.get_info()
    print("sandbox info %s" % info)

    # Data-plane stream must still work
    r = sandbox.run_code("print('data-plane: ok')",
                         on_stdout=lambda m: print(" ", m.text, end=""))
    print()

    # HTTP port 80 → blocked
    r = sandbox.run_code("""
import socket
s = socket.socket()
s.settimeout(4)
try:
    s.connect(("93.184.216.34", 80))
    print("http:80 reachable (unexpected)")
except Exception as e:
    print(f"http:80 blocked as expected ({type(e).__name__})")
finally:
    s.close()
""", on_stdout=lambda m: print(" ", m.text, end=""))
    print()

    # HTTPS port 443 → blocked
    r = sandbox.run_code("""
import socket
s = socket.socket()
s.settimeout(4)
try:
    s.connect(("93.184.216.34", 443))
    print("https:443 reachable (unexpected)")
except Exception as e:
    print(f"https:443 blocked as expected ({type(e).__name__})")
finally:
    s.close()
""", on_stdout=lambda m: print(" ", m.text, end=""))
    print()
