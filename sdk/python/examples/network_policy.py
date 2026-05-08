# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
Example: Network policy — control outbound network access via sandbox metadata.

Tests:
  - metadata["network-policy"] = "allow-all"   → outbound HTTP/HTTPS reachable
  - metadata["network-policy"] = "deny-all"    → outbound blocked (http:80 + https:443)
  - metadata["network-policy"] = "custom"      → only allow-listed domains reachable

Usage:
    export CUBE_API_URL=http://<YOUR_NODE_IP>:3000
    export CUBE_TEMPLATE_ID=tpl-6265796cee124256b4dcd6a1
    export CUBE_PROXY_NODE_IP=<YOUR_NODE_IP>
    python examples/network_policy.py

Note: deny-all blocks outbound TCP including port 80 (HTTP) and port 443 (HTTPS).
The sandbox itself still communicates with CubeProxy via the data-plane path
(port 49999), which is separate from outbound network policies.
"""
import sys
import os
import json

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from cubesandbox import Sandbox

failures: list[str] = []


def check(tag: str, condition: bool, detail: str = "") -> None:
    if condition:
        print(f"  ✅ {tag}")
    else:
        msg = f"{tag}: {detail}" if detail else tag
        print(f"  ❌ {msg}")
        failures.append(msg)


# ── 1. allow-all: HTTP (port 80) reachable ───────────────────────────────────
print("=== allow-all (outbound HTTP port 80) ===")
with Sandbox.create(metadata={"network-policy": "allow-all"}) as sb:
    print(f"  Created: {sb}")
    result = sb.run_code(
        """
import urllib.request
try:
    urllib.request.urlopen('http://example.com', timeout=8)
    print('http: reachable')
except Exception as e:
    print(f'http: blocked ({type(e).__name__}: {e})')
""",
        on_stdout=lambda m: print(" ", m.text, end=""),
    )
    if result.error:
        print(f"  error: {result.error.name}: {result.error.value}")
    reachable_http = any("reachable" in l for l in result.logs.stdout)
    check("allow-all HTTP port 80 reachable", reachable_http,
          f"stdout={result.logs.stdout}")

# ── 2. allow-all: HTTPS (port 443) reachable ────────────────────────────────
print("\n=== allow-all (outbound HTTPS port 443) ===")
with Sandbox.create(metadata={"network-policy": "allow-all"}) as sb:
    print(f"  Created: {sb}")
    result = sb.run_code(
        """
import urllib.request
try:
    urllib.request.urlopen('https://example.com', timeout=8)
    print('https: reachable')
except Exception as e:
    print(f'https: blocked ({type(e).__name__}: {e})')
""",
        on_stdout=lambda m: print(" ", m.text, end=""),
    )
    if result.error:
        print(f"  error: {result.error.name}: {result.error.value}")
    reachable_https = any("reachable" in l for l in result.logs.stdout)
    check("allow-all HTTPS port 443 reachable", reachable_https,
          f"stdout={result.logs.stdout}")

# ── 3. deny-all: HTTP (port 80) blocked ──────────────────────────────────────
print("\n=== deny-all (outbound HTTP port 80 blocked) ===")
with Sandbox.create(metadata={"network-policy": "deny-all"}) as sb:
    print(f"  Created: {sb}")
    result = sb.run_code(
        """
import urllib.request
try:
    urllib.request.urlopen('http://example.com', timeout=5)
    print('http: reachable (unexpected)')
except Exception as e:
    print(f'http: blocked as expected ({type(e).__name__})')
""",
        on_stdout=lambda m: print(" ", m.text, end=""),
    )
    if result.error:
        print(f"  error: {result.error.name}: {result.error.value}")
    blocked_http = any("blocked as expected" in l for l in result.logs.stdout)
    check("deny-all HTTP port 80 blocked", blocked_http,
          f"stdout={result.logs.stdout}")

# ── 4. deny-all: HTTPS (port 443) blocked ────────────────────────────────────
print("\n=== deny-all (outbound HTTPS port 443 blocked) ===")
with Sandbox.create(metadata={"network-policy": "deny-all"}) as sb:
    print(f"  Created: {sb}")
    result = sb.run_code(
        """
import urllib.request
try:
    urllib.request.urlopen('https://example.com', timeout=5)
    print('https: reachable (unexpected)')
except Exception as e:
    print(f'https: blocked as expected ({type(e).__name__})')
""",
        on_stdout=lambda m: print(" ", m.text, end=""),
    )
    if result.error:
        print(f"  error: {result.error.name}: {result.error.value}")
    blocked_https = any("blocked as expected" in l for l in result.logs.stdout)
    check("deny-all HTTPS port 443 blocked", blocked_https,
          f"stdout={result.logs.stdout}")

# ── 5. custom allow-list (IP-based — domain names are NOT supported) ─────────
# network-rules only accepts IP addresses or CIDR ranges, not domain names.
# Replace ALLOWED_IP with an actual IP reachable from your sandbox node.
ALLOWED_IP = "93.184.216.34"   # example.com — replace with a target IP in your env
BLOCKED_IP = "1.2.3.4"        # arbitrary IP not in the allow-list

print(f"\n=== custom allow-list (IP {ALLOWED_IP} allowed, {BLOCKED_IP} blocked) ===")
rules = json.dumps({"allow": [ALLOWED_IP]})
with Sandbox.create(
    metadata={"network-policy": "custom", "network-rules": rules}
) as sb:
    print(f"  Created: {sb}")
    result = sb.run_code(
        f"""
import socket, errno

def tcp_probe(ip, port, timeout=5):
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.settimeout(timeout)
    try:
        s.connect((ip, port))
        return True
    except OSError:
        return False
    finally:
        s.close()

allowed = tcp_probe('{ALLOWED_IP}', 80)
blocked = tcp_probe('{BLOCKED_IP}', 80)
print('allowed_ip: reachable' if allowed else 'allowed_ip: blocked (unexpected)')
print('blocked_ip: blocked as expected' if not blocked else 'blocked_ip: reachable (unexpected)')
""",
        on_stdout=lambda m: print(" ", m.text, end=""),
    )
    if result.error:
        print(f"  error: {result.error.name}: {result.error.value}")
    allowed_ok = any("allowed_ip: reachable" in l for l in result.logs.stdout)
    blocked_ok = any("blocked_ip: blocked as expected" in l for l in result.logs.stdout)
    check(f"custom: {ALLOWED_IP} reachable", allowed_ok, f"stdout={result.logs.stdout}")
    check(f"custom: {BLOCKED_IP} blocked", blocked_ok, f"stdout={result.logs.stdout}")

print("\nAll sandboxes destroyed.")

# ── summary ──────────────────────────────────────────────────────────────────
print("\n" + "=" * 40)
if failures:
    print("FAIL")
    for f in failures:
        print(f"  - {f}")
    sys.exit(1)
else:
    print("PASS")
