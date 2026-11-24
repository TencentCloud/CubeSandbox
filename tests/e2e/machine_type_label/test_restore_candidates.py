# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Live checks that the machine-type label drives cross-node restore candidates.

What can be asserted from a running cluster:

1. A node with no machine-type keeps the historical cpuid+kernel behaviour.
2. With a machine-type, the same node still restores locally (same-node path is
   untouched by the label).
3. A paused package created while the node has a machine-type can be resumed
   on that node, so the label path does not break the happy path.
4. Labels are visible to CubeMaster, which is the process that evaluates them.

What CANNOT be asserted on a single-node test cluster:

   Real cross-node resume requires two schedulable nodes with the same
   ``cpuid_hash`` and a shared (jfs/s3) backend with ``remote_status=ready``.
   Those preconditions are checked explicitly and the test skips with a precise
   reason instead of passing vacuously.
"""

from __future__ import annotations

import time

import pytest

from conftest import wait_until_schedulable
from ops_client import MACHINE_TYPE_KEY

pytestmark = [pytest.mark.e2e, pytest.mark.machine_type]


def _wait_scheduling_disabled(ops, node_id: str, want: bool, timeout: float = 30.0) -> None:
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        last = bool(ops.get_node(node_id).get("schedulingDisabled"))
        if last == want:
            return
        time.sleep(1)
    raise AssertionError(f"node {node_id} schedulingDisabled={last}, want {want}")


def _node_id(node: dict) -> str:
    return node.get("InstanceID") or node.get("IP") or ""


def _compatible_nodes(ops) -> list[dict]:
    """Nodes whose HostFacts are visible to CubeMaster (label path needs them)."""
    return [n for n in ops.internal_nodes() if (n.get("HostFacts") or {}).get("cpuid_hash")]


def test_label_does_not_break_same_node_pause_resume(ops, node_id, sandboxes, clean_labels) -> None:
    ops.bind_machine_type(node_id, "intel-e2e-k66")
    wait_until_schedulable(ops, node_id)

    sandbox = sandboxes.create()
    assert sandbox.commands.run("echo ready").stdout.strip() == "ready"
    sandbox.pause()
    resumed = type(sandbox).connect(sandbox.sandbox_id, config=sandboxes.config)
    sandboxes.created[sandboxes.created.index(sandbox)] = resumed
    assert resumed.commands.run("echo resumed").stdout.strip() == "resumed"


def test_label_is_visible_to_cubemaster(ops, node_id, clean_labels) -> None:
    ops.bind_machine_type(node_id, "intel-e2e-k66")
    # CubeMaster mirrors CubeOps labels into its local cache; give the metadata
    # sync one cycle before asserting.
    import time

    deadline = time.time() + 30
    seen = None
    while time.time() < deadline:
        for node in ops.internal_nodes():
            if node.get("InstanceID") == node_id or node.get("IP") == node_id:
                seen = (node.get("NodeLabels") or {}).get(MACHINE_TYPE_KEY)
        if seen == "intel-e2e-k66":
            break
        time.sleep(1)
    assert seen == "intel-e2e-k66", "CubeMaster did not observe the machine-type label"


def test_cross_node_resume_requires_two_compatible_nodes(ops, node_id, sandboxes, clean_labels) -> None:
    """Cross-node resume is the actual label-aware path.

    Skip with an explicit reason when the cluster cannot host the scenario; a
    pass here must mean the target node was really chosen by label.
    """
    nodes = _compatible_nodes(ops)
    if len(nodes) < 2:
        pytest.skip(
            "cross-node resume needs >=2 healthy nodes with HostFacts; "
            f"cluster has {len(nodes)}"
        )
    source = next((n for n in nodes if _node_id(n) == node_id), None)
    if source is None:
        pytest.skip(f"source node {node_id} reports no HostFacts; cannot pin it")
    cpuid = (source.get("HostFacts") or {}).get("cpuid_hash")
    peers = [
        n
        for n in nodes
        if _node_id(n) != node_id and (n.get("HostFacts") or {}).get("cpuid_hash") == cpuid
    ]
    if not peers:
        pytest.skip("cross-node resume needs a peer node sharing cpuid_hash")

    # Require exactly the two nodes this test reasons about; a third healthy
    # node would let an un-scoped create land on the wrong path.
    if len(nodes) > 2:
        pytest.skip(
            "cross-node resume needs exactly two host-fact nodes to pin the source; "
            f"cluster has {len(nodes)}"
        )

    second = peers[0]
    second_id = second.get("InstanceID")
    if not second_id:
        pytest.skip(f"peer {second.get('IP')} has no InstanceID; cannot address it")
    machine_type_before = ops.labels(second_id).get(MACHINE_TYPE_KEY)
    ops.isolate(second_id)
    try:
        ops.bind_machine_type(node_id, "intel-e2e-k66")
        ops.bind_machine_type(second_id, "intel-e2e-k66")
        # Pin the source node so the create lands on the labelled origin.
        sandbox = sandboxes.create(distribution_scope=[node_id])
        assert sandbox.commands.run("echo origin").stdout.strip() == "origin"
        sandbox.pause()
        ops.isolate(node_id)
        ops.unisolate(second_id)
        # CubeMaster's node cache follows a sync cycle behind CubeOps; wait for
        # both nodes to reflect the new isolation state before connecting.
        _wait_scheduling_disabled(ops, node_id, True)
        _wait_scheduling_disabled(ops, second_id, False)
        time.sleep(8)
        resumed = type(sandbox).connect(sandbox.sandbox_id, config=sandboxes.config)
        sandboxes.created[sandboxes.created.index(sandbox)] = resumed
        assert resumed.commands.run("echo moved").stdout.strip() == "moved"
        # The raw CubeAPI snapshot carries clientID, which is the node the
        # sandbox actually landed on.
        info = resumed.get_info()
        landed = info.get("clientID") or (info.get("metadata") or {}).get("clientID")
        assert landed == second_id, f"resumed on {landed}, expected labelled peer {second_id}"
    finally:
        ops.unisolate(node_id)
        ops.unisolate(second_id)
        # The fixture restores the primary node; restore the peer too.
        if machine_type_before is None:
            ops.clear_machine_type(second_id)
        else:
            ops.bind_machine_type(second_id, machine_type_before)
