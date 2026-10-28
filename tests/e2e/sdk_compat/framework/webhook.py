# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""P6 receiver evidence checks, shared by real-entrypoint acceptance cases."""
import base64
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import subprocess
from uuid import UUID

ENVELOPE = {
    "id", "version", "type", "timestamp", "event_category", "event_label",
    "sandbox_id", "sandbox_execution_id", "sandbox_template_id", "sandbox_build_id",
    "sandbox_team_id", "event_data",
}
FIELDS = {
    "created": set(), "resumed": {"resume_mode"}, "paused": {"pause_mode", "execution"},
    "checkpointed": set(), "updated": {"set_timeout", "network_egress"},
    "killed": {"kill_reason", "execution"},
}


def utc_time(value):
    assert isinstance(value, str) and value.endswith("Z"), value
    # Python 3.9/3.10 accept only millisecond/microsecond fractions, whereas
    # Go's RFC3339Nano emits 1–9 digits. datetime retains microsecond precision.
    whole, dot, fraction = value[:-1].partition(".")
    if dot:
        assert fraction.isdigit() and 1 <= len(fraction) <= 9, value
        whole += "." + fraction[:6].ljust(6, "0")
    parsed = datetime.fromisoformat(whole + "+00:00")
    assert parsed.tzinfo == timezone.utc
    return parsed


def records(path, sandbox_id, subscription):
    """Check all recorded attempts; ignore only an incomplete final JSONL line."""
    result, bodies, deliveries = [], {}, set()
    for line in Path(path).read_text().splitlines(keepends=True):
        if not line.endswith("\n"):
            continue
        row = json.loads(line)
        if row["sandbox_id"] != sandbox_id or row["webhook_id"] != subscription:
            continue
        body = base64.b64decode(row["body_base64"], validate=True)
        event = json.loads(body)
        assert event["sandbox_id"] == sandbox_id
        assert row["event_id"] == event["id"] and row["type"] == event["type"]
        UUID(event["id"])
        UUID(row["delivery_id"])
        assert row["content_type"] == "application/json"
        assert not row.get("signature_present", False)
        assert not row.get("signature_version_present", False)
        assert row["delivery_id"] not in deliveries, "delivery ID reused"
        deliveries.add(row["delivery_id"])
        assert bodies.setdefault(event["id"], body) == body, "retry changed stored bytes"
        result.append((row, event))
    return result


def delivery_records(container, password):
    """Read-only P6 evidence from an already inventoried dedicated Redis."""
    env = {**os.environ, "REDISCLI_AUTH": password}
    raw = subprocess.check_output([
        "docker", "exec", "-e", "REDISCLI_AUTH", container,
        "redis-cli", "--json", "-n", os.environ.get("WEBHOOK_P6_REDIS_DB", "6"), "HGETALL",
        "cube:v1:master:webhook:tasks:{delivery}"
    ], env=env, text=True)
    data = json.loads(raw)
    if isinstance(data, list):
        data = dict(zip(data[::2], data[1::2]))
    result = []
    for key, value in data.items():
        record = json.loads(value)
        immutable = json.loads(record["input"])
        encoded = immutable["Body"]
        body = base64.urlsafe_b64decode(encoded + "=" * (-len(encoded) % 4))
        result.append({"key": key, "record": record, "event": json.loads(body)})
    return result


def new_events(rows, label, before=(), field=None):
    """Independent successful operations may have different IDs of the same type."""
    found = {}
    for _, event in rows:
        if event["type"] != "sandbox.lifecycle." + label or event["id"] in before:
            continue
        if field is not None and field not in event.get("event_data", {}):
            continue
        found[event["id"]] = event
    return list(found.values())


def assert_payload(event, label, *, template=None, metadata=None, execution=None,
                   network=None, deadline=None, reason=None):
    assert not set(event) - ENVELOPE, "unexpected envelope fields"
    assert event["version"] == "v2"
    assert event["type"] == "sandbox.lifecycle." + label
    utc_time(event["timestamp"])
    for key in ("sandbox_execution_id", "sandbox_build_id", "sandbox_team_id", "event_category", "event_label"):
        assert event[key] == "", f"invented {key}"
    if template is not None:
        assert event["sandbox_template_id"] == template
    data = event.get("event_data", {})
    assert not set(data) - FIELDS[label] - {"sandbox_metadata"}, "unexpected event fields"
    if metadata is not None:
        actual = data["sandbox_metadata"]
        for key, value in metadata.items():
            assert actual[key] == value
        assert not any(key in {"host-mount", "X-Caller"} or key.startswith("cube.master.") for key in actual)
    if label == "paused":
        assert data["pause_mode"] == "full"
    if label == "resumed":
        assert data["resume_mode"] == "restore"
    if "execution" in data:
        actual = data["execution"]
        assert not set(actual) - {"started_at", "vcpu_count", "memory_mb"}
        assert actual["started_at"] == ""
        for key in ("vcpu_count", "memory_mb"):
            if key in actual:
                assert type(actual[key]) is int and actual[key] > 0
    if execution is not None:
        for key, value in execution.items():
            assert data["execution"][key] == value
    if label == "killed":
        assert isinstance(data["kill_reason"], str) and data["kill_reason"]
        if reason is not None:
            assert data["kill_reason"] == reason
    if "set_timeout" in data:
        utc_time(data["set_timeout"])
    if deadline is not None:
        assert data["set_timeout"] == deadline
    if "network_egress" in data:
        actual = data["network_egress"]
        assert set(actual) == {"allowed_cidrs", "denied_cidrs", "allowed_domains"}
        for values in actual.values():
            assert values is None or isinstance(values, list)
        if network is not None:
            assert actual == network
