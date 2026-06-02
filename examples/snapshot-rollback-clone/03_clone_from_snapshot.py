# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Demo 03: Clone a new sandbox from a snapshot.
#
# Pass snapshot_id as the `template` argument to Sandbox.create() — the new
# sandbox starts from the exact memory + filesystem state captured in the
# snapshot.
#
# (The SDK also exposes sb.clone(n=...) as a one-shot helper that wraps
# create_snapshot + create + delete_snapshot — see demo 06.)

from cubesandbox import Sandbox
from env import TEMPLATE_ID

with Sandbox.create(template=TEMPLATE_ID) as src:
    snapshot = src.create_snapshot()
    snapshot_id = snapshot.snapshot_id
    print(f"snapshot created: {snapshot_id}")

with Sandbox.create(template=snapshot_id) as cloned:
    print(f"cloned sandbox: {cloned.sandbox_id}")

# Cleanup
Sandbox.delete_snapshot(snapshot_id)
print("snapshot deleted")
