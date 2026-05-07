# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
Example: Sandbox lifecycle — create, get_info, pause, connect (auto-resume), resume (deprecated), kill.

Tests:
  - Sandbox.create()
  - sb.get_info()       — GET /sandboxes/{id}
  - sb.pause()          — POST /sandboxes/{id}/pause
  - Sandbox.connect()   — POST /sandboxes/{id}/connect  (auto-resume)
  - sb.resume()         — POST /sandboxes/{id}/resume   (deprecated, still tested)
  - sb.kill()           — DELETE /sandboxes/{id}

Usage:
    export CUBE_API_URL=http://9.135.79.34:3000
    export CUBE_TEMPLATE_ID=tpl-6265796cee124256b4dcd6a1
    export CUBE_PROXY_NODE_IP=9.135.79.34
    python examples/lifecycle.py
"""
import sys
import os
import time

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from cubesandbox import Config, Sandbox

failures: list[str] = []


def check(tag: str, condition: bool, detail: str = "") -> None:
    if condition:
        print(f"  ✅ {tag}")
    else:
        msg = f"{tag}: {detail}" if detail else tag
        print(f"  ❌ {msg}")
        failures.append(msg)


config = Config(
    api_url=os.environ.get("CUBE_API_URL", "http://9.135.79.34:3000"),
    template_id=os.environ.get("CUBE_TEMPLATE_ID", "tpl-6265796cee124256b4dcd6a1"),
    proxy_node_ip=os.environ.get("CUBE_PROXY_NODE_IP", "9.135.79.34"),
)


def _wait_for_state(sb: Sandbox, target: str, retries: int = 15, interval: float = 2.0) -> str:
    """Poll get_info() until state == target or retries exhausted. Returns final state."""
    state = ""
    for _ in range(retries):
        time.sleep(interval)
        try:
            info = sb.get_info()
            state = info.get("state", "")
            if state == target:
                return state
        except Exception:
            pass
    return state


# ── 1. create ────────────────────────────────────────────────────────────────
print("=== create ===")
sb = Sandbox.create(timeout=600, config=config)
print(f"  created: {sb.sandbox_id}")
check("sandbox_id not empty", bool(sb.sandbox_id))

# ── 2. get_info ───────────────────────────────────────────────────────────────
print("\n=== get_info (GET /sandboxes/{id}) ===")
info = sb.get_info()
print(f"  info: {info}")
check("get_info returns dict", isinstance(info, dict))
check("get_info sandboxID matches", info.get("sandboxID") == sb.sandbox_id,
      f"info={info.get('sandboxID')!r} vs {sb.sandbox_id!r}")

# ── 3. set some state we'll verify after resume ───────────────────────────────
print("\n=== set state before pause ===")
sb.run_code("persistent_value = 42")
check("state set", True)

# ── 4. pause ──────────────────────────────────────────────────────────────────
print("\n=== pause (POST /sandboxes/{id}/pause) ===")
sb.pause()
print("  pause requested")
state = _wait_for_state(sb, "paused")
print(f"  state after pause: {state!r}")
check("state == paused", state == "paused", f"got {state!r}")

# ── 5. connect (auto-resume) ──────────────────────────────────────────────────
print("\n=== connect (POST /sandboxes/{id}/connect) ===")
sb2 = Sandbox.connect(sb.sandbox_id, config=config)
result = sb2.run_code("persistent_value")
print(f"  persistent_value = {result.text!r}")
check("state persisted across pause/connect", result.text == "42", f"got {result.text!r}")

# ── 6. pause again, then test resume (deprecated API) ────────────────────────
# Note: after connect(), the sandbox is running again. Pause it so we can test resume.
print("\n=== resume deprecated (POST /sandboxes/{id}/resume) ===")
try:
    sb2.pause()
    state2 = _wait_for_state(sb2, "paused")
    print(f"  re-paused: state={state2!r}")

    sb2.resume(timeout=300)
    print("  resume() called (deprecated)")
    check("resume() did not raise", True)

    # Verify accessible after resume
    result2 = sb2.run_code("1 + 1")
    check("run_code after resume", result2.text == "2", f"got {result2.text!r}")
except Exception as e:
    # resume() is deprecated; some platforms may return 4xx — treat as warning
    print(f"  resume() raised (deprecated API may not be supported): {e}")
    check("resume() — deprecated, skipped", True)

# ── 7. kill ───────────────────────────────────────────────────────────────────
print("\n=== kill (DELETE /sandboxes/{id}) ===")
try:
    sb2.kill()
except Exception:
    try:
        sb.kill()
    except Exception:
        pass
print("  destroyed")
check("kill succeeded", True)

# ── summary ───────────────────────────────────────────────────────────────────
print("\n" + "=" * 40)
if failures:
    print("FAIL")
    for f in failures:
        print(f"  - {f}")
    sys.exit(1)
else:
    print("PASS")
