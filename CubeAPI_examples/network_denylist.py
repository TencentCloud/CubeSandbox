# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
network_denylist.py — allow internet but block specific IPs/CIDRs.

Original used: network={"deny_out": [...]}
Ported to: metadata={"network-policy": "custom", "network-rules": '{"deny": [...]}'}

Blocks the Cubelet node IP itself (metadata endpoint equivalent) and
verifies a non-blocked target (if reachable) or just confirms deny works.

Data-plane stream (HTTP:80 via CubeProxy) is verified to still work.

Env vars:
    CUBE_API_URL       management plane, e.g. http://9.135.79.34:3000
    CUBE_TEMPLATE_ID   sandbox template
    CUBE_PROXY_NODE_IP data-plane IP (HTTP port 80)
"""
import os
import sys
import json
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from cubesandbox import Sandbox

template_id = os.environ["CUBE_TEMPLATE_ID"]
proxy_ip = os.environ.get("CUBE_PROXY_NODE_IP", "9.135.79.34")

# Block the host metadata-like endpoint (the Cubelet node IP on port 80)
DENIED_CIDRS = [f"{proxy_ip}/32"]

with Sandbox.create(
    template=template_id,
    metadata={
        "network-policy": "custom",
        "network-rules": json.dumps({"deny": DENIED_CIDRS}),
    },
) as sandbox:
    info = sandbox.get_info()
    print("sandbox info %s" % info)
    print("deny-list:", DENIED_CIDRS)

    # Data-plane stream must still work (CubeProxy uses dedicated port 49999, not port 80)
    r = sandbox.run_code("print('data-plane: ok')",
                         on_stdout=lambda m: print(" ", m.text, end=""))
    print()

    # Denied: proxy_ip port 80 should be blocked
    r = sandbox.run_code(f"""
import socket
s = socket.socket()
s.settimeout(4)
_err = None
try:
    s.connect(("{proxy_ip}", 80))
    print("denied IP {proxy_ip}:80 reachable (unexpected)")
except Exception as ex:
    _err = type(ex).__name__
finally:
    s.close()
if _err:
    print(f"denied IP {proxy_ip}:80 blocked as expected ({{_err}})")
""", on_stdout=lambda m: print(" ", m.text, end=""))
    print()

    # Also verify deny-list does not affect 443 on a different IP
    r = sandbox.run_code("""
import socket
s = socket.socket()
s.settimeout(4)
_err = None
try:
    s.connect(("93.184.216.34", 443))
    print("external:443 reachable (not in deny-list)")
except Exception as ex:
    _err = type(ex).__name__
finally:
    s.close()
if _err:
    print(f"external:443 unreachable ({_err}) — may be blocked by devcloud env")
""", on_stdout=lambda m: print(" ", m.text, end=""))
    print()
