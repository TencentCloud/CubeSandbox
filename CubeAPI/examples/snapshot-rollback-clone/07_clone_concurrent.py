# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Demo 07: Concurrent clone — sb.clone(n=N, concurrency=C).
#
# The default sb.clone(n=N) creates the N child sandboxes serially. For
# fan-out workloads (e.g. parallel agent rollouts) you can set
# `concurrency=C` to fan the per-child Sandbox.create() out across a thread
# pool of size min(N, C). The snapshot is taken once and deleted once;
# only the create-N step runs in parallel.
#
# Semantics:
#   - concurrency=1 (default) is byte-identical to the serial loop.
#   - On any failure, every successfully-created clone is killed before
#     the exception propagates. Caller gets exactly N sandboxes or an
#     exception with no orphaned resources.

import os

from cubesandbox import Sandbox
from env import TEMPLATE_ID

N = int(os.environ.get("FORK_N", "10"))
CONCURRENCY = int(os.environ.get("FORK_CONCURRENCY", "5"))

src = Sandbox.create(template=TEMPLATE_ID)
src.run_code("open('/tmp/origin.txt','w').write('I am from sandbox a')")
print(f"src sandbox: {src.sandbox_id}")

# ★ Concurrent clone — SDK fans Sandbox.create out internally
clones = src.clone(n=N, concurrency=CONCURRENCY)
print(f"cloned {len(clones)} sandboxes (concurrency={CONCURRENCY})")

# Verify every clone inherited the origin marker
expect = "I am from sandbox a"
ok = 0
for i, sb in enumerate(clones):
    r = sb.run_code("print(open('/tmp/origin.txt').read())")
    marker = r.logs.stdout[0].strip() if r.logs.stdout else ""
    if marker == expect:
        ok += 1
    print(f"  clone[{i:>2}] {sb.sandbox_id}  marker={marker!r}")

print(f"\n{ok}/{N} clones inherited the origin marker")
assert ok == N, "some clones failed to inherit state"

# Cleanup
src.kill()
for sb in clones:
    sb.kill()
print("all sandboxes killed")
