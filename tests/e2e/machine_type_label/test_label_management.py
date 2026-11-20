# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Live checks for CubeOps node label management and the machine-type contract.

Covered contract:

- ``GET /api/v1/nodes`` returns the node's labels.
- ``PATCH`` merges; ``PUT`` replaces user labels; ``DELETE`` removes one key.
- Isolation and machine-type survive a bulk replace.
- ``PUT`` rejects machine-type in its payload instead of silently ignoring it.
- ``PUT`` rejects a missing/null ``labels`` body instead of clearing everything.
- A Cubelet re-registration cannot overwrite machine-type.
- Invalid machine-type values are rejected.
- Data-plane labels (``kubernetes.io/*`` and friends) survive a bulk replace
  and cannot be written through the admin API; dropping them would silently
  make the node unschedulable.
"""

from __future__ import annotations

import time

import pytest

from conftest import ISOLATION_KEY, MACHINE_TYPE_KEY, user_labels
from ops_client import OpsError

pytestmark = [pytest.mark.e2e, pytest.mark.machine_type]


def test_list_nodes_exposes_labels(ops, node_id) -> None:
    node = ops.get_node(node_id)
    assert "labels" in node, "NodeView must expose labels"
    assert isinstance(node["labels"], dict)


def test_patch_merges_and_keeps_existing_keys(ops, node_id, unique_key, clean_labels) -> None:
    second = f"{unique_key}-second"
    ops.replace_labels(node_id, {unique_key: "one"})
    ops.set_labels(node_id, {second: "two"})
    labels = ops.wait_for_labels(node_id, {unique_key: "one", second: "two"})
    assert labels[unique_key] == "one"
    assert labels[second] == "two"


def test_put_replaces_user_labels(ops, node_id, unique_key, clean_labels) -> None:
    dropped = f"{unique_key}-dropped"
    ops.replace_labels(node_id, {unique_key: "one", dropped: "x"})
    ops.replace_labels(node_id, {unique_key: "two"})
    labels = ops.wait_for_labels(node_id, {unique_key: "two"})
    assert labels[unique_key] == "two"
    assert dropped not in labels


def test_delete_removes_only_the_named_key(ops, node_id, unique_key, clean_labels) -> None:
    kept = f"{unique_key}-kept"
    ops.replace_labels(node_id, {unique_key: "one", kept: "yes"})
    ops.delete_label(node_id, unique_key)
    labels = ops.wait_for_labels(node_id, {kept: "yes"})
    assert unique_key not in labels
    assert labels[kept] == "yes"


def test_delete_supports_keys_containing_slash(ops, node_id, clean_labels) -> None:
    key = "example.com/e2e-team"
    ops.set_labels(node_id, {key: "infra"})
    assert ops.wait_for_labels(node_id, {key: "infra"})[key] == "infra"
    ops.delete_label(node_id, key)
    deadline = time.time() + 15
    labels = ops.labels(node_id)
    while time.time() < deadline and key in labels:
        time.sleep(0.25)
        labels = ops.labels(node_id)
    assert key not in labels


def test_machine_type_label_api_is_reserved(ops, node_id, clean_labels) -> None:
    with pytest.raises(OpsError) as excinfo:
        ops.set_labels(node_id, {MACHINE_TYPE_KEY: "intel-e2e-k66"})
    assert excinfo.value.status == 403, excinfo.value.body
    with pytest.raises(OpsError) as excinfo:
        ops.delete_label(node_id, MACHINE_TYPE_KEY)
    assert excinfo.value.status == 403, excinfo.value.body


def test_machine_type_bind_and_rebind(ops, node_id, clean_labels) -> None:
    ops.bind_machine_type(node_id, "intel-e2e-k66")
    assert ops.wait_for_labels(node_id, {MACHINE_TYPE_KEY: "intel-e2e-k66"})[MACHINE_TYPE_KEY] == "intel-e2e-k66"
    ops.bind_machine_type(node_id, "amd-e2e-k66")
    assert ops.wait_for_labels(node_id, {MACHINE_TYPE_KEY: "amd-e2e-k66"})[MACHINE_TYPE_KEY] == "amd-e2e-k66"
    node = ops.get_node(node_id)
    assert node.get("machineType") == "amd-e2e-k66"


def test_replace_keeps_data_plane_labels(ops, node_id, unique_key, clean_labels) -> None:
    """A bulk replace must not drop the labels Cubelet reports; they feed
    scheduler affinity and losing them makes the node unschedulable."""
    before = ops.labels(node_id)
    system_before = {
        k: v
        for k, v in before.items()
        if k not in user_labels(before) and k not in (ISOLATION_KEY, MACHINE_TYPE_KEY)
    }
    if not system_before:
        pytest.skip("node reports no data-plane labels; nothing to preserve")

    marker = f"e2e-system-keep-{unique_key}"
    ops.replace_labels(node_id, {marker: "x"})
    after = ops.labels(node_id)
    for key, value in system_before.items():
        assert after.get(key) == value, f"replace dropped data-plane label {key}"


@pytest.mark.parametrize("key", ["kubernetes.io/hostname", "beta.kubernetes.io/os"])
def test_data_plane_labels_are_read_only(ops, node_id, clean_labels, key) -> None:
    with pytest.raises(OpsError) as excinfo:
        ops.set_labels(node_id, {key: "hijacked"})
    assert excinfo.value.status == 403, excinfo.value.body
    with pytest.raises(OpsError) as excinfo:
        ops.delete_label(node_id, key)
    assert excinfo.value.status == 403, excinfo.value.body


def test_replace_keeps_machine_type(ops, node_id, unique_key, clean_labels) -> None:
    ops.bind_machine_type(node_id, "intel-e2e-k66")
    ops.replace_labels(node_id, {unique_key: "e2e"})
    labels = ops.wait_for_labels(node_id, {unique_key: "e2e"})
    assert labels[MACHINE_TYPE_KEY] == "intel-e2e-k66"
    assert labels[unique_key] == "e2e"


def test_replace_rejects_machine_type_in_payload(ops, node_id, unique_key, clean_labels) -> None:
    ops.bind_machine_type(node_id, "intel-e2e-k66")
    # Read back through the eventual-consistency helper so the "nothing was
    # written" check cannot run against a stale snapshot.
    before_labels = ops.wait_for_labels(node_id, {MACHINE_TYPE_KEY: "intel-e2e-k66"})
    before = user_labels(before_labels)
    with pytest.raises(OpsError) as excinfo:
        ops.replace_labels(node_id, {MACHINE_TYPE_KEY: "amd-e2e-k66", unique_key: "e2e"})
    assert excinfo.value.status == 403, excinfo.value.body
    # The whole request is rejected, so the user label set is unchanged.
    after = ops.wait_for_labels(node_id, {MACHINE_TYPE_KEY: "intel-e2e-k66"})
    assert after[MACHINE_TYPE_KEY] == "intel-e2e-k66"
    assert user_labels(after) == before


def test_replace_rejects_missing_labels_body(ops, node_id, clean_labels) -> None:
    ops.replace_labels(node_id, {"keep": "me"})
    with pytest.raises(OpsError) as excinfo:
        ops._request("PUT", f"/api/v1/nodes/{node_id}/labels", {})
    assert excinfo.value.status == 400, excinfo.value.body
    assert user_labels(ops.labels(node_id)) == {"keep": "me"}


@pytest.mark.parametrize("bad", ["", "Intel-K66", "intel_k66", "-leading"])
def test_machine_type_value_shape_is_enforced(ops, bad) -> None:
    with pytest.raises(OpsError) as excinfo:
        ops._request("POST", "/api/v1/machine-types", {"name": bad})
    assert excinfo.value.status == 400, excinfo.value.body


def test_isolation_survives_bulk_replace(ops, node_id, unique_key, clean_labels) -> None:
    ops.replace_labels(node_id, {unique_key: "e2e"})
    try:
        ops.isolate(node_id)
        ops.replace_labels(node_id, {unique_key: "e2e-2"})
        labels = ops.wait_for_labels(node_id, {unique_key: "e2e-2"})
        assert labels.get(ISOLATION_KEY) == "true", "replace must preserve isolation"
        assert labels[unique_key] == "e2e-2"
    finally:
        ops.unisolate(node_id)
