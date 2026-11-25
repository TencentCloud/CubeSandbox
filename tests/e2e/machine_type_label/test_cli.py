# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Live checks for ``cubeopscli node label``.

The CLI is exercised as a subprocess against the same CubeOps the fixtures use,
so the test covers argument parsing, HTTP wiring, batch result reporting and the
exit-code contract, not just the service layer.
"""

from __future__ import annotations

import json
import shutil
import subprocess
import time

import pytest

from ops_client import MACHINE_TYPE_KEY

pytestmark = [pytest.mark.e2e, pytest.mark.machine_type]


def _cli(ops, *args: str) -> subprocess.CompletedProcess[str]:
    binary = shutil.which("cubeopscli")
    if not binary:
        pytest.skip("cubeopscli not found on PATH")
    host = ops.base_url.split("://", 1)[-1]
    address, _, port = host.partition(":")
    cmd = [binary, "--address", address, "--port", port or "3010", "node", "label", *args]
    return subprocess.run(cmd, capture_output=True, text=True, check=False)


def test_cli_list_shows_labels(ops, node_id, unique_key, clean_labels) -> None:
    ops.replace_labels(node_id, {unique_key: "cli-value"})
    result = _cli(ops, "list", node_id)
    assert result.returncode == 0, result.stderr
    assert unique_key in result.stdout
    assert "cli-value" in result.stdout


def test_cli_set_merges(ops, node_id, unique_key, clean_labels) -> None:
    added = f"{unique_key}-cli"
    ops.replace_labels(node_id, {unique_key: "one"})
    result = _cli(ops, "set", node_id, f"{added}=two")
    assert result.returncode == 0, result.stderr
    labels = ops.wait_for_labels(node_id, {unique_key: "one", added: "two"})
    assert labels[unique_key] == "one"
    assert labels[added] == "two"


def test_cli_replace_keeps_machine_type(ops, node_id, unique_key, clean_labels) -> None:
    ops.bind_machine_type(node_id, "intel-e2e-k66")
    result = _cli(ops, "replace", node_id, f"{unique_key}=three")
    assert result.returncode == 0, result.stderr
    labels = ops.wait_for_labels(
        node_id, {unique_key: "three", MACHINE_TYPE_KEY: "intel-e2e-k66"}
    )
    assert labels[unique_key] == "three"
    assert labels[MACHINE_TYPE_KEY] == "intel-e2e-k66"


def test_cli_replace_refuses_machine_type(ops, node_id, clean_labels) -> None:
    result = _cli(ops, "replace", node_id, f"{MACHINE_TYPE_KEY}=amd-e2e-k66")
    assert result.returncode != 0
    assert "machine-type" in result.stderr


def test_cli_delete_supports_slash_key(ops, node_id, clean_labels) -> None:
    key = "example.com/e2e-team"
    ops.set_labels(node_id, {key: "infra"})
    result = _cli(ops, "delete", node_id, key)
    assert result.returncode == 0, result.stderr
    deadline = time.time() + 15
    while time.time() < deadline:
        if key not in ops.labels(node_id):
            break
        time.sleep(0.25)
    assert key not in ops.labels(node_id)


def test_cli_refuses_machine_type_delete(ops, node_id, clean_labels) -> None:
    result = _cli(ops, "delete", node_id, MACHINE_TYPE_KEY)
    assert result.returncode != 0
    assert "machine-type" in result.stderr


def test_cli_replace_clear_is_explicit(ops, node_id, unique_key, clean_labels) -> None:
    """`replace` with no key=value clears user labels on purpose; machine-type
    and data-plane labels stay."""
    ops.replace_labels(node_id, {unique_key: "gone"})
    machine_type_before = ops.labels(node_id).get(MACHINE_TYPE_KEY)
    result = _cli(ops, "replace", node_id)
    assert result.returncode == 0, result.stderr
    deadline = time.time() + 15
    labels = ops.labels(node_id)
    while time.time() < deadline and unique_key in labels:
        time.sleep(0.25)
        labels = ops.labels(node_id)
    assert unique_key not in labels
    assert labels.get(MACHINE_TYPE_KEY) == machine_type_before


def test_cli_operator_reaches_the_audit_trail(ops, node_id, unique_key, clean_labels) -> None:
    """`--operator` must be recorded in the audit trail, not just accepted."""
    result = _cli(ops, "set", node_id, unique_key + "=audited", "--operator=e2e-operator")
    assert result.returncode == 0, result.stderr
    recorded = [
        op for op in ops.operations(node_id, limit=10) if op.get("operator") == "e2e-operator"
    ]
    assert recorded, "no operation recorded for --operator=e2e-operator"
    detail = recorded[0].get("detail") or ""
    assert "added=" in detail and unique_key in detail, detail


def test_cli_delete_is_recorded_as_a_removal(ops, node_id, unique_key, clean_labels) -> None:
    ops.set_labels(node_id, {unique_key: "to-delete"})
    result = _cli(ops, "delete", node_id, unique_key)
    assert result.returncode == 0, result.stderr
    newest = ops.operations(node_id, limit=5)[0]
    assert newest.get("type") == "delete-label", newest
    detail = newest.get("detail") or ""
    assert "removed=1" in detail and f"-{unique_key}" in detail, detail


def test_cli_json_output_is_structured(ops, node_id, unique_key, clean_labels) -> None:
    ops.replace_labels(node_id, {unique_key: "json"})
    result = _cli(ops, "list", node_id, "--json")
    assert result.returncode == 0, result.stderr
    payload = json.loads(result.stdout)
    assert any(item["node_id"] == node_id for item in payload)


def test_cli_batch_reports_each_node(ops, node_id, unique_key, clean_labels) -> None:
    nodes = ops.list_nodes()
    if len(nodes) < 2:
        pytest.skip("needs two nodes to exercise the batch path")
    second = nodes[1]["nodeID"]
    result = _cli(ops, "set", node_id, second, f"{unique_key}=batch")
    assert result.returncode == 0, result.stderr
    assert ops.wait_for_labels(node_id, {unique_key: "batch"})[unique_key] == "batch"
    assert ops.wait_for_labels(second, {unique_key: "batch"})[unique_key] == "batch"
    # Clean the second node separately: the fixture only restores the first.
    ops.delete_label(second, unique_key)
