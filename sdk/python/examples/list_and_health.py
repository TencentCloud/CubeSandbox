# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
Example: list sandboxes and check API health.

Tests:
  1. Sandbox.health()       — GET /health
  2. Sandbox.create()       — POST /sandboxes
  3. Sandbox.list()         — GET /sandboxes      (v1)
  4. Sandbox.list_v2()      — GET /v2/sandboxes   (v2)
  5. sb.kill()              — DELETE /sandboxes/{id}
  6. Sandbox.list() after kill   — sandbox absent from v1 list
  7. Sandbox.list_v2() after kill — sandbox absent from v2 list

Usage:
    export CUBE_API_URL=http://<YOUR_NODE_IP>:3000
    export CUBE_TEMPLATE_ID=tpl-6265796cee124256b4dcd6a1
    export CUBE_PROXY_NODE_IP=<YOUR_NODE_IP>
    python examples/list_and_health.py
"""
from __future__ import annotations

import sys
import os

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


def _ids(sandboxes: list[dict]) -> set[str]:
    return {s.get("sandboxID") or s.get("sandbox_id") or s.get("id", "") for s in sandboxes}


# ── 1. health ─────────────────────────────────────────────────────────────────
print("=== Sandbox.health() ===")
try:
    h = Sandbox.health()
    print(f"  result: {h}")
    check("health() returns dict",    isinstance(h, dict))
    check("health() has 'status' key", "status" in h, f"keys={list(h.keys())}")
except Exception as exc:
    failures.append(f"health() raised: {exc}")
    print(f"  ❌ FAIL: {exc}")

# ── 2. create ─────────────────────────────────────────────────────────────────
print("\n=== Sandbox.create() ===")
sb: Sandbox | None = None
sandbox_id: str | None = None
try:
    sb = Sandbox.create()
    sandbox_id = sb.sandbox_id
    print(f"  created sandbox_id={sandbox_id!r}")
    check("create() sandbox_id not empty", bool(sandbox_id))
except Exception as exc:
    failures.append(f"Sandbox.create() raised: {exc}")
    print(f"  ❌ FAIL: {exc}")

if sandbox_id and sb:
    # ── 3. list() v1 ──────────────────────────────────────────────────────────
    print("\n=== Sandbox.list() [v1] ===")
    try:
        v1 = Sandbox.list()
        print(f"  returned {len(v1)} sandbox(es)")
        check("list() is a list", isinstance(v1, list))
        check(f"list() contains {sandbox_id!r}", sandbox_id in _ids(v1),
              f"ids={_ids(v1)}")
    except Exception as exc:
        failures.append(f"list() raised: {exc}")
        print(f"  ❌ FAIL: {exc}")

    # ── 4. list_v2() ──────────────────────────────────────────────────────────
    print("\n=== Sandbox.list_v2() [v2] ===")
    try:
        v2 = Sandbox.list_v2()
        print(f"  returned {len(v2)} sandbox(es)")
        check("list_v2() is a list", isinstance(v2, list))
        check(f"list_v2() contains {sandbox_id!r}", sandbox_id in _ids(v2),
              f"ids={_ids(v2)}")
    except Exception as exc:
        failures.append(f"list_v2() raised: {exc}")
        print(f"  ❌ FAIL: {exc}")

    # ── 5. kill ────────────────────────────────────────────────────────────────
    print("\n=== sb.kill() ===")
    try:
        sb.kill()
        print(f"  destroyed sandbox_id={sandbox_id!r}")
        check("kill() succeeded", True)
    except Exception as exc:
        failures.append(f"kill() raised: {exc}")
        print(f"  ❌ FAIL: {exc}")

    # ── 6. list() after kill ───────────────────────────────────────────────────
    print("\n=== Sandbox.list() after kill [v1] ===")
    try:
        v1_after = Sandbox.list()
        absent = sandbox_id not in _ids(v1_after)
        check(f"list(): {sandbox_id!r} absent after kill", absent,
              f"still present in {_ids(v1_after)}")
    except Exception as exc:
        failures.append(f"list() after kill raised: {exc}")
        print(f"  ❌ FAIL: {exc}")

    # ── 7. list_v2() after kill ────────────────────────────────────────────────
    print("\n=== Sandbox.list_v2() after kill [v2] ===")
    try:
        v2_after = Sandbox.list_v2()
        absent = sandbox_id not in _ids(v2_after)
        check(f"list_v2(): {sandbox_id!r} absent after kill", absent,
              f"still present in {_ids(v2_after)}")
    except Exception as exc:
        failures.append(f"list_v2() after kill raised: {exc}")
        print(f"  ❌ FAIL: {exc}")

# ── summary ───────────────────────────────────────────────────────────────────
print("\n" + "=" * 40)
if failures:
    print("FAIL")
    for f in failures:
        print(f"  - {f}")
    sys.exit(1)
else:
    print("PASS")
