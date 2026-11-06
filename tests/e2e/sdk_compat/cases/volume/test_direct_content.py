# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Direct volume access: read and write a volume's files without a sandbox.

The SDK talks to Cubelet's ``/volumecontent`` service with a volume-scoped token
that CubeMaster mints on create/get. A request takes a reference on the volume
for as long as it runs, the same ``t_cube_volume.refcount`` that sandbox mounts
use, so ``DELETE /volumes/{id}`` is refused (409) while a request or a sandbox
uses the volume and a deleted volume can never be attached again.

Prerequisites (in addition to the Volume Plugin setup used by the other cases):
- ``CUBE_VOLUME_CONTENT_TOKEN_SECRET`` and ``CUBE_VOLUME_CONTENT_AUDIENCE`` on
  CubeMaster and Cubelet (same values), and ``CUBE_VOLUME_CONTENT_ADDR`` on
  Cubelet. Without them ``POST /volumes`` returns no ``endpoint`` and these cases
  skip. See ``sdk/python/docs/volume.md``.
- The content service must be reachable from the runner. If the advertised
  audience is not, set ``SDK_E2E_VOLUME_CONTENT_URL`` to a reachable origin.
- The sandbox-sharing case also needs a READY template (``CUBE_TEMPLATE_ID``).
"""

from __future__ import annotations

import threading
import time
import uuid

import pytest

from adapters import create_adapter_with_capacity_retry
from adapters.api_adapter import ApiClient
from framework.capabilities import VOLUME_PLUGIN
from framework.cleanup import safe_kill
from framework.volume import (
    VOLUME_MOUNT_PATH,
    content_request,
    direct_content_endpoint,
    managed_volume,
    safe_delete_volume,
    volume_id_from_payload,
    volume_in_list,
    volume_content_url_override,
    wait_delete_volume_status,
)

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.volume,
    pytest.mark.p1,
    pytest.mark.requires_capability(VOLUME_PLUGIN),
]

# Seconds the content service needs to have taken its reference for a request
# that is already blocked mid-upload. There is no API to observe the count, so
# this is a settle time, not a poll.
REQUEST_SETTLE_SECONDS = 2.0


def _sdk_config(sdk_e2e_config):
    from cubesandbox import Config

    # request_timeout is generous: the first request to a volume mounts it.
    return Config(
        api_url=sdk_e2e_config.cube_api_url,
        volume_api_url=volume_content_url_override(),
        request_timeout=60,
    )


@pytest.fixture
def direct_volume(sdk_backend, sdk_e2e_config):
    """A fresh volume plus its create response; skips when the content service is off."""
    if sdk_backend != "cubesandbox":
        pytest.skip(f"direct volume access requires the cubesandbox backend, got {sdk_backend!r}")
    api = ApiClient(sdk_e2e_config)
    name = f"vol-direct-{uuid.uuid4().hex[:12]}"
    volume_id = None
    try:
        payload = api.create_volume(name, driver=sdk_e2e_config.volume_driver)
        volume_id = volume_id_from_payload(payload)
        endpoint = direct_content_endpoint(payload)
        if endpoint is None:
            pytest.skip(
                "POST /volumes returned no content endpoint: the volume content service "
                "(CUBE_VOLUME_CONTENT_*) is not enabled on this deployment"
            )
        yield {
            "api": api,
            "id": volume_id,
            "name": name,
            "payload": payload,
            "token": payload["token"],
            "endpoint": endpoint,
            "config": _sdk_config(sdk_e2e_config),
            "e2e_config": sdk_e2e_config,
        }
    finally:
        if volume_id is not None:
            errors = safe_delete_volume(api, volume_id)
            assert not errors, f"volume cleanup errors: {errors}"
        api.close()


def _volume(handle):
    from cubesandbox import Volume

    return Volume(
        handle["id"],
        handle["name"],
        handle["token"],
        config=handle["config"],
        endpoint=handle["endpoint"],
    )


def test_read_write_without_a_sandbox(direct_volume):
    # Create, write, read, list, stat, chmod and remove files through the
    # content service only; no sandbox exists at any point.
    from cubesandbox import VolumePathNotFoundException

    vol = _volume(direct_volume)
    blob = bytes(range(256)) * 4

    vol.make_dir("/docs", force=True)
    vol.write_file("/docs/hello.txt", "hello", force=True)
    vol.write_file("/docs/blob.bin", blob, force=True)

    assert vol.read_file("/docs/hello.txt") == "hello"
    assert vol.read_file("/docs/blob.bin", format="bytes") == blob
    streamed = b"".join(vol.read_file("/docs/blob.bin", format="stream"))
    assert streamed == blob

    names = {entry.name for entry in vol.list("/docs")}
    assert names == {"hello.txt", "blob.bin"}
    info = vol.get_info("/docs/blob.bin")
    assert info.size == len(blob)
    assert vol.exists("/docs/hello.txt")
    assert not vol.exists("/docs/absent.txt")

    updated = vol.update_metadata("/docs/hello.txt", mode=0o600)
    assert updated.mode & 0o777 == 0o600

    vol.remove("/docs/hello.txt")
    assert not vol.exists("/docs/hello.txt")
    with pytest.raises(VolumePathNotFoundException):
        vol.read_file("/docs/hello.txt")


def test_nothing_stays_pinned_after_requests(direct_volume):
    # A request takes its reference only while it runs. Once the last request
    # is done the volume must be deletable immediately, not after an idle timer.
    vol = _volume(direct_volume)
    vol.write_file("/pin.txt", "x", force=True)
    assert vol.read_file("/pin.txt") == "x"

    status = direct_volume["api"].delete_volume(direct_volume["id"])
    assert status == 204, f"DELETE right after the last request expected 204, got {status}"


def test_rejected_tokens_never_reach_the_volume(direct_volume):
    # Authentication is checked before the volume is touched.
    endpoint, vol_id = direct_volume["endpoint"], direct_volume["id"]

    missing = content_request(endpoint, vol_id, "", "GET", "path", "/")
    assert missing.status_code == 401, missing.text
    garbage = content_request(endpoint, vol_id, "not.a.jwt", "GET", "path", "/")
    assert garbage.status_code == 401, garbage.text

    with managed_volume(direct_volume["e2e_config"]) as (other_id, other_api):
        other_token = other_api.get_volume(other_id)[1]["token"]
        wrong_volume = content_request(endpoint, vol_id, other_token, "GET", "path", "/")
        assert wrong_volume.status_code == 403, (
            f"a token for another volume must be forbidden, got {wrong_volume.status_code}"
        )


def test_running_request_blocks_delete_until_it_finishes(direct_volume):
    # A request that is still running holds the volume: DELETE /volumes is
    # refused with 409 and succeeds once the request is done.
    vol = _volume(direct_volume)
    api, vol_id = direct_volume["api"], direct_volume["id"]

    class SlowBody:
        """File-like body: one chunk, then blocks until released."""

        def __init__(self) -> None:
            self.sent_first = False
            self.blocked = threading.Event()
            self.release = threading.Event()

        def read(self, size: int = -1) -> bytes:
            if not self.sent_first:
                self.sent_first = True
                return b"a" * 1024
            self.blocked.set()
            self.release.wait(timeout=60)
            return b""

    body = SlowBody()
    outcome: dict = {}

    def upload() -> None:
        try:
            vol.write_file("/slow.bin", body, force=True)
        except Exception as exc:  # noqa: BLE001 - reported by the main thread
            outcome["error"] = exc

    worker = threading.Thread(target=upload, daemon=True)
    worker.start()
    try:
        assert body.blocked.wait(timeout=30), "upload never started"
        time.sleep(REQUEST_SETTLE_SECONDS)

        status = api.delete_volume(vol_id)
        assert status == 409, (
            f"DELETE while a content request is running expected 409, got {status}"
        )
    finally:
        body.release.set()
        worker.join(timeout=60)
    assert not worker.is_alive(), "upload never finished"
    assert "error" not in outcome, f"upload failed: {outcome.get('error')!r}"

    wait_delete_volume_status(api, vol_id, 204, timeout=30)


def test_deleted_volume_token_gets_404_and_nothing_is_recreated(direct_volume):
    # A token outlives its volume. After the volume is deleted every route
    # answers 404 and no request brings the volume (or its backend) back.
    api, vol_id = direct_volume["api"], direct_volume["id"]
    endpoint, token = direct_volume["endpoint"], direct_volume["token"]

    vol = _volume(direct_volume)
    vol.write_file("/keep.txt", "data", force=True)
    assert api.delete_volume(vol_id) == 204

    for method, route in [
        ("GET", "file"), ("PUT", "file"), ("GET", "dir"), ("POST", "dir"),
        ("GET", "path"), ("PATCH", "path"), ("DELETE", "path"),
    ]:
        resp = content_request(endpoint, vol_id, token, method, route, "/keep.txt", data=b"{}")
        assert resp.status_code == 404, (
            f"{method} {route} on a deleted volume expected 404, got {resp.status_code} {resp.text}"
        )

    status, _ = api.get_volume(vol_id)
    assert status == 404, "a content request must not recreate the volume"
    assert not volume_in_list(api.list_volumes(), vol_id)


def test_direct_access_and_sandboxes_share_the_volume(sdk_backend, sdk_e2e_config):
    # Sandboxes and direct access use one reference count. Two sandboxes mount
    # the volume and direct access works alongside them; DELETE stays refused
    # until the last user is gone, whichever kind it is.
    if sdk_backend != "cubesandbox":
        pytest.skip(f"direct volume access requires the cubesandbox backend, got {sdk_backend!r}")
    if not sdk_e2e_config.cube_template_id:
        pytest.skip("CUBE_TEMPLATE_ID or --cube-template-id is required to mount a volume")

    with managed_volume(sdk_e2e_config) as (volume_id, api):
        payload = api.get_volume(volume_id)[1]
        endpoint = direct_content_endpoint(payload)
        if endpoint is None:
            pytest.skip("volume content service (CUBE_VOLUME_CONTENT_*) is not enabled")
        handle = {
            "id": volume_id,
            "name": payload.get("name", volume_id),
            "token": payload["token"],
            "endpoint": endpoint,
            "config": _sdk_config(sdk_e2e_config),
        }
        vol = _volume(handle)
        mounts = {"volumeMounts": [{"name": volume_id, "path": VOLUME_MOUNT_PATH}]}
        meta = {"test_suite": "sdk_compat", "test_role": "volume_direct", "test_backend": sdk_backend}
        first = second = None
        try:
            first = create_adapter_with_capacity_retry(
                sdk_backend, sdk_e2e_config, metadata=meta, create_options=mounts)
            second = create_adapter_with_capacity_retry(
                sdk_backend, sdk_e2e_config, metadata=meta, create_options=mounts)

            # Written directly, visible in both sandboxes.
            vol.write_file("/shared.txt", "from-direct", force=True)
            for adapter in (first, second):
                deadline = time.monotonic() + 30
                seen = ""
                while time.monotonic() < deadline:
                    seen = adapter.run_command(f"cat {VOLUME_MOUNT_PATH}/shared.txt || true").stdout.strip()
                    if seen == "from-direct":
                        break
                    time.sleep(1)
                assert seen == "from-direct", f"sandbox did not see the direct write: {seen!r}"

            # Written in a sandbox, readable directly.
            first.run_command(f"echo -n from-sandbox > {VOLUME_MOUNT_PATH}/from-sandbox.txt")
            deadline = time.monotonic() + 30
            while time.monotonic() < deadline and not vol.exists("/from-sandbox.txt"):
                time.sleep(1)
            assert vol.read_file("/from-sandbox.txt") == "from-sandbox"

            # A finished direct request does not release a sandbox's hold.
            assert wait_delete_volume_status(
                api, volume_id, 409, timeout=sdk_e2e_config.volume_refcount_wait) == 409

            errors = safe_kill(first, sdk_e2e_config)
            first = None
            assert not errors, f"sandbox cleanup errors: {errors}"
            # One sandbox is still using it (and direct access still works).
            assert vol.read_file("/shared.txt") == "from-direct"
            assert api.delete_volume(volume_id) == 409

            errors = safe_kill(second, sdk_e2e_config)
            second = None
            assert not errors, f"sandbox cleanup errors: {errors}"
            assert wait_delete_volume_status(
                api, volume_id, 204, timeout=sdk_e2e_config.volume_refcount_wait) == 204
        finally:
            for adapter in (first, second):
                if adapter is not None:
                    safe_kill(adapter, sdk_e2e_config)
