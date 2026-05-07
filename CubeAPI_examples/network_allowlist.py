# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
network_allowlist.py — only allow specific CIDRs, block everything else.

Original used: allow_internet_access=False, network={"allow_out": [...]}
Ported to: metadata={"network-policy": "custom", "network-rules": '{"allow": [...]}'}

The allow-list is expressed as destination IPs/CIDRs.
This example uses IPs reachable from the Cubelet node (internal):
  - 9.135.79.34:3000  (CubeAPI itself — always reachable from same host)
  - 93.184.216.34:80  (example.com public IP — expected blocked in devcloud)

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

# Allow only the Cubelet/CubeAPI node itself; block everything else
ALLOWED_CIDRS = [f"{proxy_ip}/32"]

with Sandbox.create(
    template=template_id,
    metadata={
        "network-policy": "custom",
        "network-rules": json.dumps({"allow": ALLOWED_CIDRS}),
    },
) as sandbox:
    info = sandbox.get_info()
    print("sandbox info %s" % info)
    print("allow-list:", ALLOWED_CIDRS)

    # Data-plane stream must still work (routes via CubeProxy on proxy_ip)
    r = sandbox.run_code("print('data-plane: ok')",
                         on_stdout=lambda m: print(" ", m.text, end=""))
    print()

    # Allowed: proxy_ip:3000 (CubeAPI) — TCP connect
    r = sandbox.run_code(f"""
import socket
s = socket.socket()
s.settimeout(4)
_err = None
try:
    s.connect(("{proxy_ip}", 3000))
    print("allowed IP {proxy_ip}:3000 reachable (expected)")
except Exception as ex:
    _err = type(ex).__name__
finally:
    s.close()
if _err:
    print(f"allowed IP {proxy_ip}:3000 unreachable ({{_err}})")
""", on_stdout=lambda m: print(" ", m.text, end=""))
    print()

    # Blocked: external IP (93.184.216.34 = example.com)
    r = sandbox.run_code("""
import socket
s = socket.socket()
s.settimeout(4)
_err = None
try:
    s.connect(("93.184.216.34", 80))
    print("external 93.184.216.34:80 reachable (unexpected)")
except Exception as ex:
    _err = type(ex).__name__
finally:
    s.close()
if _err:
    print(f"external 93.184.216.34:80 blocked as expected ({_err})")
""", on_stdout=lambda m: print(" ", m.text, end=""))
    print()
