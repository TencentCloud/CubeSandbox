"""Example: list sandboxes and check API health.

Tests:
  1. Sandbox.health()      - GET /health
  2. Sandbox.list()        - GET /sandboxes      (v1)
  3. Sandbox.list_v2()     - GET /v2/sandboxes   (v2)

Environment variables expected:
  CUBE_API_URL        e.g. http://9.135.79.34:3000
  CUBE_TEMPLATE_ID    e.g. tpl-6265796cee124256b4dcd6a1
  CUBE_PROXY_NODE_IP  e.g. 9.135.79.34
"""

from __future__ import annotations

import sys
import os

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from cube_sandbox import Sandbox


def _ids(sandboxes: list[dict]) -> set[str]:
    """Extract sandbox IDs from a list of sandbox dicts."""
    return {s.get("sandboxID") or s.get("sandbox_id") or s.get("id", "") for s in sandboxes}


def main() -> None:
    failures: list[str] = []

    # ── 1. health ────────────────────────────────────────────────────
    print("=== Sandbox.health() ===")
    try:
        h = Sandbox.health()
        print(f"  result: {h}")
        if not isinstance(h, dict):
            raise AssertionError(f"expected dict, got {type(h).__name__}")
        if "status" not in h:
            raise AssertionError(f"'status' key missing from health response: {h}")
        print("  health() OK")
    except Exception as exc:
        failures.append(f"health() failed: {exc}")
        print(f"  FAIL: {exc}")

    # ── 2. create a sandbox ─────────────────────────────────────────
    print("\n=== Sandbox.create() ===")
    sb: Sandbox | None = None
    sandbox_id: str | None = None
    try:
        sb = Sandbox.create()
        sandbox_id = sb.sandbox_id
        print(f"  created sandbox_id={sandbox_id!r}")
    except Exception as exc:
        failures.append(f"Sandbox.create() failed: {exc}")
        print(f"  FAIL: {exc}")

    if sandbox_id:
        # ── 3. list() (v1) ──────────────────────────────────────────
        print("\n=== Sandbox.list() [v1] ===")
        try:
            v1 = Sandbox.list()
            print(f"  returned {len(v1)} sandbox(es)")
            ids_v1 = _ids(v1)
            if sandbox_id not in ids_v1:
                raise AssertionError(
                    f"created sandbox {sandbox_id!r} not found in list(). ids={ids_v1}"
                )
            print(f"  sandbox_id {sandbox_id!r} present  OK")
        except Exception as exc:
            failures.append(f"list() failed: {exc}")
            print(f"  FAIL: {exc}")

        # ── 4. list_v2() ────────────────────────────────────────────
        print("\n=== Sandbox.list_v2() [v2] ===")
        try:
            v2 = Sandbox.list_v2()
            print(f"  returned {len(v2)} sandbox(es)")
            ids_v2 = _ids(v2)
            if sandbox_id not in ids_v2:
                raise AssertionError(
                    f"created sandbox {sandbox_id!r} not found in list_v2(). ids={ids_v2}"
                )
            print(f"  sandbox_id {sandbox_id!r} present  OK")
        except Exception as exc:
            failures.append(f"list_v2() failed: {exc}")
            print(f"  FAIL: {exc}")

        # ── 5. destroy the sandbox ───────────────────────────────────
        print("\n=== Sandbox.kill() ===")
        try:
            assert sb is not None
            sb.kill()
            print(f"  destroyed sandbox_id={sandbox_id!r}")
        except Exception as exc:
            failures.append(f"kill() failed: {exc}")
            print(f"  FAIL: {exc}")

        # ── 6. verify sandbox is gone from list() ───────────────────
        print("\n=== Sandbox.list() after kill [v1] ===")
        try:
            v1_after = Sandbox.list()
            ids_after_v1 = _ids(v1_after)
            if sandbox_id in ids_after_v1:
                raise AssertionError(
                    f"destroyed sandbox {sandbox_id!r} still present in list(). ids={ids_after_v1}"
                )
            print(f"  sandbox_id {sandbox_id!r} absent  OK")
        except Exception as exc:
            failures.append(f"list() after kill failed: {exc}")
            print(f"  FAIL: {exc}")

        # ── 7. verify sandbox is gone from list_v2() ────────────────
        print("\n=== Sandbox.list_v2() after kill [v2] ===")
        try:
            v2_after = Sandbox.list_v2()
            ids_after_v2 = _ids(v2_after)
            if sandbox_id in ids_after_v2:
                raise AssertionError(
                    f"destroyed sandbox {sandbox_id!r} still present in list_v2(). ids={ids_after_v2}"
                )
            print(f"  sandbox_id {sandbox_id!r} absent  OK")
        except Exception as exc:
            failures.append(f"list_v2() after kill failed: {exc}")
            print(f"  FAIL: {exc}")

    # ── summary ──────────────────────────────────────────────────────
    print("\n" + "=" * 40)
    if failures:
        print("FAIL")
        for f in failures:
            print(f"  - {f}")
        sys.exit(1)
    else:
        print("PASS")


if __name__ == "__main__":
    main()
