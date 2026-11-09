# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""V2 volume input checks and opt-in S3 binding data paths."""

from __future__ import annotations

import json
import sys
import uuid

import pytest
import requests

from adapters import create_adapter_with_capacity_retry
from adapters.api_adapter import ApiClient
from framework.capabilities import VOLUME_PLUGIN
from framework.cleanup import safe_kill
from framework.volume import (
    VOLUME_MOUNT_PATH,
    safe_delete_volume,
    volume_id_from_payload,
    wait_delete_volume_status,
)

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.volume,
    pytest.mark.p1,
    pytest.mark.requires_capability(VOLUME_PLUGIN),
]


@pytest.fixture
def dynamic_volume_cred_ref(sdk_e2e_config):
    cred_ref = (sdk_e2e_config.volume_cred_ref or "").strip()
    if not cred_ref:
        pytest.skip("SDK_E2E_VOLUME_CRED_REF is required for dynamic volume mounts")
    return cred_ref


def _post_volume(api, config, name, private_data, *, prefix=None):
    try:
        return api.create_volume_raw(
            name, driver=config.volume_driver, private_data=private_data
        )
    except requests.RequestException as exc:
        exc.add_note(
            f"Volume create outcome is unknown: name={name!r}, prefix={prefix!r}. "
            "No DELETE was issued for this request; inspect its result before cleanup."
        )
        raise


def _remember_created_volume(response, name, volumes):
    # Named creates use name as volumeID. Record it before parsing a known 201,
    # so a malformed success response cannot prevent best-effort cleanup.
    volumes.append(name)
    created = response.json()
    assert isinstance(created, dict), "create volume returned a non-object body"
    volume_id = volume_id_from_payload(created)
    assert volume_id == name, "named create returned a different volume ID"
    return created


def _create_volume(api, config, volumes, name, prefix, cred_ref):
    private_data = json.dumps({"prefix": prefix, "cred_ref": cred_ref})
    response = _post_volume(api, config, name, private_data, prefix=prefix)
    assert response.status_code == 201, (
        f"POST /v2/volumes name={name!r} expected 201, got {response.status_code}"
    )
    return _remember_created_volume(response, name, volumes)


def _create_sandbox(backend, config, sandboxes, mounts):
    adapter = create_adapter_with_capacity_retry(
        backend,
        config,
        metadata={
            "test_suite": "sdk_compat",
            "test_role": "volume_private_data",
            "test_backend": backend,
        },
        create_options={"volume_mounts": mounts},
    )
    sandboxes.append(adapter)
    assert adapter.sandbox_id, "volume sandbox create returned an empty ID"
    return adapter


def _kill_sandbox(adapter, config, sandboxes):
    errors = safe_kill(adapter, config)
    assert not errors, f"sandbox cleanup errors: {errors}"
    sandboxes.remove(adapter)


def _delete_volume(api, config, volumes, volume_id):
    status = wait_delete_volume_status(
        api, volume_id, 204, timeout=config.volume_refcount_wait
    )
    assert status == 204
    volumes.remove(volume_id)
    missing_status, _ = api.get_volume(volume_id)
    assert missing_status == 404, f"GET /volumes/{volume_id} after DELETE expected 404"


def _cleanup(api, config, sandboxes, volumes, failure):
    errors = []
    for adapter in reversed(sandboxes):
        try:
            errors.extend(safe_kill(adapter, config))
        except Exception as exc:  # noqa: BLE001 - finish the remaining cleanup
            errors.append(f"sandbox cleanup raised: {exc}")
    for volume_id in reversed(volumes):
        try:
            errors.extend(safe_delete_volume(api, volume_id))
        except Exception as exc:  # noqa: BLE001 - finish the remaining cleanup
            errors.append(f"volume {volume_id} cleanup raised: {exc}")
    try:
        api.close()
    except Exception as exc:  # noqa: BLE001 - report close along with other errors
        errors.append(f"volume API client close failed: {exc}")
    if errors:
        message = "volume cleanup failed: " + "; ".join(errors)
        if failure is not None:
            failure.add_note(message)
        else:
            pytest.fail(message)


def test_v2_volume_data_survives_sandbox_recreation(
    sdk_backend, sdk_e2e_config, dynamic_volume_cred_ref
):
    config = sdk_e2e_config
    run_id = uuid.uuid4().hex
    prefix = f"{config.volume_prefix.rstrip('/')}/{run_id}/a/"
    name = f"vol-v2-{run_id}-a"
    filename = f"data-{uuid.uuid4().hex}.txt"
    path = f"{VOLUME_MOUNT_PATH}/{filename}"
    content = f"same-volume-{uuid.uuid4().hex}"
    api = ApiClient(config)
    volumes = []
    sandboxes = []
    try:
        created = _create_volume(
            api, config, volumes, name, prefix, dynamic_volume_cred_ref
        )
        volume_id = volume_id_from_payload(created)
        assert "private_data" not in created, "create response exposed private_data"
        status, got = api.get_volume(volume_id)
        assert status == 200, f"GET /volumes/{volume_id} expected 200, got {status}"
        assert volume_id_from_payload(got) == volume_id
        assert "private_data" not in got, "GET response exposed private_data"
        entries = [
            entry for entry in api.list_volumes()
            if isinstance(entry, dict)
            and (entry.get("volumeID") or entry.get("volume_id")) == volume_id
        ]
        assert entries, f"volume {volume_id} missing from list"
        assert all("private_data" not in entry for entry in entries), (
            "list response exposed private_data"
        )

        writer = _create_sandbox(
            sdk_backend, config, sandboxes, {VOLUME_MOUNT_PATH: volume_id}
        )
        writer_id = writer.sandbox_id
        writer.write_file(path, content)
        assert writer.read_file(path) == content
        synced = writer.run_command("sync", timeout=config.command_timeout)
        assert synced.exit_code == 0, f"sync failed: {synced.stderr}"
        _kill_sandbox(writer, config, sandboxes)

        reader = _create_sandbox(
            sdk_backend, config, sandboxes, {VOLUME_MOUNT_PATH: volume_id}
        )
        assert reader.sandbox_id != writer_id, "readback reused the writer sandbox"
        assert reader.read_file(path) == content
        _kill_sandbox(reader, config, sandboxes)
        _delete_volume(api, config, volumes, volume_id)
    finally:
        _cleanup(api, config, sandboxes, volumes, sys.exc_info()[1])


def test_v2_volume_delete_preserves_sibling_prefix(
    sdk_backend, sdk_e2e_config, dynamic_volume_cred_ref
):
    config = sdk_e2e_config
    run_id = uuid.uuid4().hex
    root = f"{config.volume_prefix.rstrip('/')}/{run_id}/"
    filename = f"data-{uuid.uuid4().hex}.txt"
    path_a = f"{VOLUME_MOUNT_PATH}/{filename}"
    mount_b = f"{VOLUME_MOUNT_PATH}-b"
    path_b = f"{mount_b}/{filename}"
    content_a = f"sibling-a-{uuid.uuid4().hex}"
    content_b = f"sibling-b-{uuid.uuid4().hex}"
    api = ApiClient(config)
    volumes = []
    sandboxes = []
    try:
        created_a = _create_volume(
            api, config, volumes, f"vol-v2-{run_id}-a", root + "a/",
            dynamic_volume_cred_ref,
        )
        volume_a = volume_id_from_payload(created_a)
        created_b = _create_volume(
            api, config, volumes, f"vol-v2-{run_id}-b", root + "b/",
            dynamic_volume_cred_ref,
        )
        volume_b = volume_id_from_payload(created_b)
        assert volume_a != volume_b, "sibling creates returned the same volume ID"
        writer = _create_sandbox(
            sdk_backend, config, sandboxes,
            {VOLUME_MOUNT_PATH: volume_a, mount_b: volume_b},
        )
        writer_id = writer.sandbox_id
        writer.write_file(path_a, content_a)
        writer.write_file(path_b, content_b)
        assert writer.read_file(path_a) == content_a
        assert writer.read_file(path_b) == content_b
        synced = writer.run_command("sync", timeout=config.command_timeout)
        assert synced.exit_code == 0, f"sync failed: {synced.stderr}"
        _kill_sandbox(writer, config, sandboxes)
        _delete_volume(api, config, volumes, volume_a)

        reader = _create_sandbox(
            sdk_backend, config, sandboxes, {mount_b: volume_b}
        )
        assert reader.sandbox_id != writer_id, "readback reused the writer sandbox"
        assert reader.read_file(path_b) == content_b
        _kill_sandbox(reader, config, sandboxes)
        _delete_volume(api, config, volumes, volume_b)
    finally:
        _cleanup(api, config, sandboxes, volumes, sys.exc_info()[1])


def _response_strings(value):
    if isinstance(value, str):
        yield value
    elif isinstance(value, dict):
        for nested in value.values():
            yield from _response_strings(nested)
    elif isinstance(value, list):
        for nested in value:
            yield from _response_strings(nested)


@pytest.mark.parametrize(
    "private_data",
    [
        "a" * 1025,
        "\u754c" * 341 + "ab",
        "invalid-\x00-private-data",
        "invalid-\U0001f600-private-data",
    ],
    ids=["ascii-over-1024-bytes", "utf8-over-1024-bytes", "nul", "non-bmp"],
)
def test_v2_volume_rejects_invalid_private_data(sdk_backend, sdk_e2e_config, private_data):
    config = sdk_e2e_config
    name = f"vol-v2-reject-{uuid.uuid4().hex}"
    api = ApiClient(config)
    volumes = []
    try:
        response = _post_volume(api, config, name, private_data)
        if response.status_code == 201:
            _remember_created_volume(response, name, volumes)
        assert response.status_code == 400, (
            f"POST /v2/volumes name={name!r} expected 400, got {response.status_code}"
        )
        status, _ = api.get_volume(name)
        if status == 200:
            volumes.append(name)
        assert status == 404, f"rejected create left a volume record: name={name!r}"
        assert private_data not in response.text, "error response echoed private_data"
        try:
            error_body = response.json()
        except ValueError:
            error_body = None
        assert not any(private_data in value for value in _response_strings(error_body)), (
            "error response echoed private_data"
        )
    finally:
        _cleanup(api, config, [], volumes, sys.exc_info()[1])
