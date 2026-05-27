# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Demo 01: Create a snapshot from a running sandbox.
#
# sb.create_snapshot() pauses the sandbox, captures its full state
# (memory + filesystem) and resumes it. The returned SnapshotInfo has a
# `snapshot_id` that can be passed as `template=` to Sandbox.create().

from cubesandbox import Sandbox
from env import TEMPLATE_ID

with Sandbox.create(template=TEMPLATE_ID) as sb:
    print(f"sandbox: {sb.sandbox_id}")

    snapshot = sb.create_snapshot()
    print(f"snapshot created: {snapshot.snapshot_id}")
    print(f"use as template:  Sandbox.create(template='{snapshot.snapshot_id}')")

    # Cleanup
    Sandbox.delete_snapshot(snapshot.snapshot_id)
    print("snapshot deleted")
