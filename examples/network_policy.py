"""
Example: Network policy — control outbound network access via sandbox metadata.

Usage:
    export CUBE_API_URL=http://9.135.79.34:3000
    export CUBE_TEMPLATE_ID=tpl-6265796cee124256b4dcd6a1
    export CUBE_PROXY_NODE_IP=9.135.79.34
    python examples/network_policy.py

Network policy is expressed as a ``network-policy`` metadata key.
Supported values (platform-defined):
    "allow-all"   — no restrictions (default)
    "deny-all"    — block all outbound traffic
    "custom"      — apply allow-list rules in ``network-rules``
"""
import json
from cubesandbox import Sandbox

# ── 1. Default sandbox (allow-all) ───────────────────────────────────────────
print("=== allow-all ===")
with Sandbox.create(metadata={"network-policy": "allow-all"}) as sb:
    print(f"Created: {sb}")
    result = sb.run_code(
        """
import urllib.request, socket
try:
    urllib.request.urlopen('http://example.com', timeout=5)
    print('outbound: reachable')
except Exception as e:
    print(f'outbound: blocked ({e})')
""",
        on_stdout=lambda m: print(" ", m.text, end=""),
    )
    if result.error:
        print(f"  error: {result.error.name}: {result.error.value}")
print("Sandbox destroyed.\n")

# ── 2. Sandbox with deny-all policy ──────────────────────────────────────────
print("=== deny-all ===")
with Sandbox.create(metadata={"network-policy": "deny-all"}) as sb:
    print(f"Created: {sb}")
    result = sb.run_code(
        """
import urllib.request
try:
    urllib.request.urlopen('http://example.com', timeout=5)
    print('outbound: reachable (unexpected)')
except Exception as e:
    print(f'outbound: blocked as expected ({type(e).__name__})')
""",
        on_stdout=lambda m: print(" ", m.text, end=""),
    )
    if result.error:
        print(f"  error: {result.error.name}: {result.error.value}")
print("Sandbox destroyed.\n")

# ── 3. Custom allow-list (only allow pypi.org) ────────────────────────────────
print("=== custom allow-list ===")
rules = json.dumps({"allow": ["pypi.org", "files.pythonhosted.org"]})
with Sandbox.create(
    metadata={"network-policy": "custom", "network-rules": rules}
) as sb:
    print(f"Created: {sb}")
    result = sb.run_code(
        """
import urllib.request
# Should succeed — pypi.org is allowed
try:
    urllib.request.urlopen('https://pypi.org/simple/', timeout=5)
    print('pypi.org: reachable')
except Exception as e:
    print(f'pypi.org: blocked ({e})')
# Should fail — example.com is not in allow-list
try:
    urllib.request.urlopen('http://example.com', timeout=5)
    print('example.com: reachable (unexpected)')
except Exception as e:
    print(f'example.com: blocked as expected ({type(e).__name__})')
""",
        on_stdout=lambda m: print(" ", m.text, end=""),
    )
    if result.error:
        print(f"  error: {result.error.name}: {result.error.value}")
print("Sandbox destroyed.")
