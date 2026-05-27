# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Demo 02: List snapshots — global and filtered by sandbox_id.
#
# Sandbox.list_snapshots() returns (list[SnapshotInfo], next_token).
# Pass next_token back to retrieve the next page; None means no more pages.

from cubesandbox import Sandbox
from env import TEMPLATE_ID

# 2a. List all snapshots
print("── all snapshots ──")
items, token = Sandbox.list_snapshots()
while True:
    for snap in items:
        print(f"  {snap.snapshot_id}")
    if not token:
        break
    items, token = Sandbox.list_snapshots(next_token=token)

# 2b. Filter by sandbox_id
with Sandbox.create(template=TEMPLATE_ID) as sb:
    snap = sb.create_snapshot()
    sandbox_id = sb.sandbox_id
    snapshot_id = snap.snapshot_id

print(f"\n── filtered by sandbox_id={sandbox_id} ──")
items, _ = Sandbox.list_snapshots(sandbox_id=sandbox_id)
for snap in items:
    print(f"  {snap.snapshot_id}")

# Cleanup
Sandbox.delete_snapshot(snapshot_id)
print("snapshot deleted")
