# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""
tool_allowlist_fanout.py — Same host allowlist across parallel sandboxes.

Differentiated scenario (#645 soft bar: multi-sandbox): fan out N MicroVMs,
each running only allowlisted tools. Not multi-agent orchestration.
"""

from __future__ import annotations

import os
from concurrent.futures import ThreadPoolExecutor, as_completed

from e2b_code_interpreter import Sandbox

from env_utils import load_local_dotenv
from tool_allowlist import AllowlistDenied, assert_allowlisted

load_local_dotenv()

template_id = os.environ["CUBE_TEMPLATE_ID"]
N = int(os.environ.get("ALLOWLIST_FANOUT_N", "2"))
if N < 2 or N > 4:
    raise SystemExit("ALLOWLIST_FANOUT_N must be 2..4 for this demo")


def worker(idx: int) -> tuple[int, str, str]:
    cmd = f"echo fanout-{idx}"
    assert_allowlisted(cmd)
    with Sandbox.create(
        template=template_id,
        allow_internet_access=False,
        timeout=60,
    ) as sandbox:
        sid = getattr(sandbox, "sandbox_id", None) or getattr(sandbox, "id", "?")
        out = sandbox.commands.run(cmd).stdout.strip()
        return idx, sid, out


# Host deny once before fan-out (shared policy, no sandbox).
try:
    assert_allowlisted("bash -c id")
except AllowlistDenied as exc:
    print(f"host_deny (shared): {exc}")
else:
    raise SystemExit("expected shared host deny before fan-out")

print(f"fanout: N={N} parallel Sandbox.create under same allowlist")
results: list[tuple[int, str, str]] = []
with ThreadPoolExecutor(max_workers=N) as pool:
    futures = [pool.submit(worker, i) for i in range(N)]
    for fut in as_completed(futures):
        results.append(fut.result())

results.sort(key=lambda row: row[0])
for idx, sid, out in results:
    expected = f"fanout-{idx}"
    print(f"worker[{idx}] sandbox_id={sid} out={out!r}")
    if out != expected:
        raise SystemExit(f"worker {idx} expected {expected!r}, got {out!r}")

ids = {sid for _, sid, _ in results}
if len(ids) != N:
    raise SystemExit(f"expected {N} distinct sandbox ids, got {ids!r}")

print("FANOUT_OK")
