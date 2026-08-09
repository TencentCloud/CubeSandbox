#!/usr/bin/env python3
"""Regression tests for the rollback state helpers (no network/backend).

Covers:
  - find_snapshot_index: 'last', decimal index, snapshot-id lookup
  - prune_after: keeps up to idx, drops the rest
  - evict_auto: ring eviction of 'auto' snapshots only (baseline and
    checkpoint are NEVER evicted), backward-compat missing 'kind' → auto
  - get_max_auto_snapshots: env parsing + clamp to >= 1
  - load_hook_env: KEY=VALUE parsing, no clobber of existing env vars
  - delete_snapshot_backend: backend errors are swallowed (no network:
    the classmethod is patched)

Run:  python3 test_lib.py
"""

import os
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import cubesandbox_lib  # noqa: E402
from cubesandbox_lib import (  # noqa: E402
    delete_snapshot_backend,
    evict_auto,
    find_snapshot_index,
    get_max_auto_snapshots,
    load_hook_env,
    prune_after,
)

PASS = 0
FAIL = 0
FAILURES = []


def check(desc: str, cond: bool, note: str = ""):
    global PASS, FAIL
    if cond:
        PASS += 1
    else:
        FAIL += 1
        FAILURES.append(f"  FAIL {desc} {note}")


def snap(sid: str, kind=None):
    """Build a minimal snapshot dict; omit 'kind' to emulate old state."""
    s = {"snapshot_id": sid}
    if kind is not None:
        s["kind"] = kind
    return s


# ---------------------------------------------------------------------------
# find_snapshot_index
# ---------------------------------------------------------------------------
print("== find_snapshot_index ==")
SNAPS = [
    snap("snap-b", "baseline"),
    snap("snap-a1"),
    snap("snap-a2", "checkpoint"),
    snap("snap-a3"),
]
check("find 'last' → len-1", find_snapshot_index(SNAPS, "last") == 3)
check("find '0' → index 0", find_snapshot_index(SNAPS, "0") == 0)
check("find '2' → index 2", find_snapshot_index(SNAPS, "2") == 2)
check("find snapshot-id present",
      find_snapshot_index(SNAPS, "snap-a2") == 2)
check("find snapshot-id absent",
      find_snapshot_index(SNAPS, "snap-zzz") is None)
check("find 'abc' → None", find_snapshot_index(SNAPS, "abc") is None)
check("find '' → None", find_snapshot_index(SNAPS, "") is None)
check("find out-of-range digit → None",
      find_snapshot_index(SNAPS, "9") is None)
check("find negative → None", find_snapshot_index(SNAPS, "-1") is None)
check("find 'last' empty list → None",
      find_snapshot_index([], "last") is None)
check("find '0' empty list → None", find_snapshot_index([], "0") is None)

# ---------------------------------------------------------------------------
# prune_after
# ---------------------------------------------------------------------------
print("== prune_after ==")
kept, dropped = prune_after(SNAPS, 1)
check("prune idx1 kept = first 2", kept == SNAPS[:2])
check("prune idx1 dropped = rest", dropped == SNAPS[2:])
kept, dropped = prune_after(SNAPS, 0)
check("prune idx0 kept = first 1", kept == SNAPS[:1])
check("prune idx0 dropped = rest", dropped == SNAPS[1:])
kept, dropped = prune_after(SNAPS, len(SNAPS) - 1)
check("prune last kept = all", kept == SNAPS)
check("prune last dropped = none", dropped == [])
check("prune preserves order", kept + dropped == SNAPS)

# ---------------------------------------------------------------------------
# evict_auto
# ---------------------------------------------------------------------------
print("== evict_auto ==")

# baseline + checkpoint NEVER evicted even when max_auto=1
S = [snap("snap-b", "baseline"), snap("snap-a1"),
     snap("snap-c", "checkpoint"), snap("snap-a2")]
new, ev = evict_auto(S, 1)
check("max=1 baseline+checkpoint kept",
      [s["snapshot_id"] for s in new] == ["snap-b", "snap-c", "snap-a2"])
check("max=1 evicted oldest auto only",
      [s["snapshot_id"] for s in ev] == ["snap-a1"])

# oldest auto evicted even when it sits after a baseline at index 0
# (3 autos, max=1 → evict the two oldest autos down to 1)
S = [snap("snap-b", "baseline"), snap("snap-a1"), snap("snap-a2"),
     snap("snap-a3")]
new, ev = evict_auto(S, 1)
check("oldest autos after baseline evicted",
      [s["snapshot_id"] for s in new] == ["snap-b", "snap-a3"])
check("evicted = two oldest autos",
      [s["snapshot_id"] for s in ev] == ["snap-a1", "snap-a2"])

# multiple evictions when max exceeded by > 1
S = [snap("snap-a1"), snap("snap-a2"), snap("snap-a3"), snap("snap-a4")]
new, ev = evict_auto(S, 2)
check("evict multiple down to max",
      [s["snapshot_id"] for s in new] == ["snap-a3", "snap-a4"])
check("evicted two oldest",
      [s["snapshot_id"] for s in ev] == ["snap-a1", "snap-a2"])

# empty list
new, ev = evict_auto([], 5)
check("evict empty list → empty", new == [] and ev == [])

# at/below limit → nothing evicted
new, ev = evict_auto(SNAPS, 10)
check("at limit nothing evicted",
      ev == [] and len(new) == len(SNAPS) and new == SNAPS)

# max clamp: max_auto=0 behaves as 1 (baseline never evicted)
S = [snap("snap-b", "baseline"), snap("snap-a1")]
new, ev = evict_auto(S, 0)
check("evict clamp 0 → 1, nothing evicted",
      new == S and ev == [])

# backward compat: snapshots missing 'kind' are treated as 'auto'
S = [snap("snap-old1"), snap("snap-old2")]
new, ev = evict_auto(S, 1)
check("missing-kind treated as auto (evicted)",
      [s["snapshot_id"] for s in ev] == ["snap-old1"])
check("missing-kind treated as auto (kept)",
      [s["snapshot_id"] for s in new] == ["snap-old2"])

# mixed: missing-kind autos around baseline/checkpoint
S = [snap("snap-b", "baseline"), snap("snap-old1"),
     snap("snap-c", "checkpoint"), snap("snap-old2")]
new, ev = evict_auto(S, 1)
check("missing-kind autos evicted around baseline/checkpoint",
      [s["snapshot_id"] for s in new] == ["snap-b", "snap-c", "snap-old2"])
check("missing-kind autos evicted = oldest",
      [s["snapshot_id"] for s in ev] == ["snap-old1"])

# ---------------------------------------------------------------------------
# get_max_auto_snapshots (env parsing + clamp)
# ---------------------------------------------------------------------------
print("== get_max_auto_snapshots ==")
os.environ["CUBE_ROLLBACK_MAX_AUTO_SNAPSHOTS"] = "0"
check("clamp 0 → 1", get_max_auto_snapshots() == 1)
os.environ["CUBE_ROLLBACK_MAX_AUTO_SNAPSHOTS"] = "-5"
check("clamp -5 → 1", get_max_auto_snapshots() == 1)
os.environ["CUBE_ROLLBACK_MAX_AUTO_SNAPSHOTS"] = "abc"
check("garbage → default 30", get_max_auto_snapshots() == 30)
os.environ["CUBE_ROLLBACK_MAX_AUTO_SNAPSHOTS"] = "7"
check("parse 7 → 7", get_max_auto_snapshots() == 7)
os.environ["CUBE_ROLLBACK_MAX_AUTO_SNAPSHOTS"] = " 42 "
check("parse whitespace-padded 42 → 42", get_max_auto_snapshots() == 42)
del os.environ["CUBE_ROLLBACK_MAX_AUTO_SNAPSHOTS"]
check("unset → default 30", get_max_auto_snapshots() == 30)

# ---------------------------------------------------------------------------
# load_hook_env (filesystem-only; patches _HOOK_ENV_PATH to a temp file)
# ---------------------------------------------------------------------------
print("== load_hook_env ==")
with tempfile.TemporaryDirectory() as tmp:
    envf = Path(tmp) / "cubesandbox.env"
    envf.write_text(
        "# comment line\n\n"
        "CUBE_TEST_VAR=hello\n"
        "CUBE_ROLLBACK_SAFE=echo,cat\n"
        "   CUBE_PADDED = spaced\n"
        "NOT_A_KEY_VALUE_LINE\n"
        "CUBE_QUOTED_SINGLE='http://127.0.0.1:3000'\n"
        'CUBE_QUOTED_DOUBLE="tpl-abc"\n'
    )
    orig_path = cubesandbox_lib._HOOK_ENV_PATH
    cubesandbox_lib._HOOK_ENV_PATH = envf
    try:
        os.environ.pop("CUBE_TEST_VAR", None)
        os.environ["CUBE_ROLLBACK_SAFE"] = "already-set"
        os.environ.pop("CUBE_QUOTED_SINGLE", None)
        os.environ.pop("CUBE_QUOTED_DOUBLE", None)
        load_hook_env()
        check("sets missing var", os.environ.get("CUBE_TEST_VAR") == "hello")
        check("does not clobber existing var",
              os.environ.get("CUBE_ROLLBACK_SAFE") == "already-set")
        check("parses padded key/value",
              os.environ.get("CUBE_PADDED") == "spaced")
        check("skips comment/blank/no-'=' lines",
              os.environ.get("NOT_A_KEY_VALUE_LINE") is None)
        check("strips single quotes (dotenv compat)",
              os.environ.get("CUBE_QUOTED_SINGLE") == "http://127.0.0.1:3000")
        check("strips double quotes (dotenv compat)",
              os.environ.get("CUBE_QUOTED_DOUBLE") == "tpl-abc")
    finally:
        cubesandbox_lib._HOOK_ENV_PATH = orig_path
        os.environ.pop("CUBE_TEST_VAR", None)
        os.environ.pop("CUBE_ROLLBACK_SAFE", None)
        os.environ.pop("CUBE_PADDED", None)
        os.environ.pop("CUBE_QUOTED_SINGLE", None)
        os.environ.pop("CUBE_QUOTED_DOUBLE", None)

# missing env file → no-op, no crash
orig_path = cubesandbox_lib._HOOK_ENV_PATH
cubesandbox_lib._HOOK_ENV_PATH = Path("/nonexistent/definitely/missing.env")
try:
    load_hook_env()
    check("missing env file → no-op", True)
finally:
    cubesandbox_lib._HOOK_ENV_PATH = orig_path

# ---------------------------------------------------------------------------
# delete_snapshot_backend (backend errors swallowed; classmethod patched —
# no network)
# ---------------------------------------------------------------------------
print("== delete_snapshot_backend ==")
try:
    import cubesandbox
except ImportError:  # pragma: no cover - only if package absent
    cubesandbox = None

if cubesandbox is not None:
    _orig = cubesandbox.Sandbox.delete_snapshot

    def _boom(snapshot_id, **kwargs):
        raise RuntimeError("backend down")

    cubesandbox.Sandbox.delete_snapshot = _boom
    try:
        delete_snapshot_backend("snap-x")  # must not raise
        check("backend error swallowed", True)
        delete_snapshot_backend("")  # empty → early return, no call
        check("empty id → early return", True)
    finally:
        cubesandbox.Sandbox.delete_snapshot = _orig
else:
    print("  (cubesandbox not installed — skipping backend-swallow test)")

# ---------------------------------------------------------------------------
print(f"\n{'-' * 60}")
print(f"PASS: {PASS}  FAIL: {FAIL}")
if FAILURES:
    print("Failures:")
    print("\n".join(FAILURES))
    sys.exit(1)
print("All tests passed.")
