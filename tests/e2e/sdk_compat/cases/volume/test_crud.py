# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Volume CRUD cases (create / list / get / delete).

Prerequisites (manual; not provisioned by this suite):
- Deploy and configure a Volume Plugin on CubeMaster (Controller) and Cubelet
  (Node), e.g. COS binary/rpc under ``volume_plugins``, with credentials.
  Guide: https://github.com/TencentCloud/CubeSandbox/blob/master/examples/volume/cos/README.md
- Platform Volume API available (CubeAPI / CubeMaster / Cubelet >= 0.6.0).
- Python SDK ``cubesandbox`` >= 0.6.0 (Volume / volume_mounts support).
- Opt-in: ``SDK_E2E_VOLUME_PLUGIN=true`` (and usually ``SDK_E2E_VOLUME_DRIVER``).
"""

from __future__ import annotations

import uuid

import pytest

from adapters.api_adapter import ApiClient
from framework.capabilities import VOLUME_PLUGIN
from framework.volume import (
    volume_id_from_payload,
    volume_in_list,
    volume_plugin_misconfigured_hint,
)

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.volume,
    pytest.mark.p1,
    pytest.mark.requires_capability(VOLUME_PLUGIN),
]


def test_volume_create_list_get_delete(sdk_backend, sdk_e2e_config):
    # Verify Volume REST CRUD: create → list → get (token) → delete → get 404.
    if sdk_backend != "cubesandbox":
        pytest.skip(
            f"volume plugin e2e currently requires cubesandbox backend, got {sdk_backend!r}"
        )
    name = f"vol-crud-{uuid.uuid4().hex[:12]}"
    api = ApiClient(sdk_e2e_config)
    volume_id: str | None = None
    try:
        created = api.create_volume(name, driver=sdk_e2e_config.volume_driver)
        volume_id = volume_id_from_payload(created)
        assert "token" in created, f"create response missing token: {created!r}"

        listed = api.list_volumes()
        assert volume_in_list(listed, volume_id), (
            f"volume {volume_id} not in list; count={len(listed)}"
        )

        status, got = api.get_volume(volume_id)
        assert status == 200, (
            f"GET /volumes/{volume_id} expected 200, got {status}. "
            f"{volume_plugin_misconfigured_hint()}"
        )
        assert volume_id_from_payload(got) == volume_id
        assert "token" in got, f"GET /volumes/{{id}} missing token: {got!r}"

        delete_status = api.delete_volume(volume_id)
        assert delete_status == 204, (
            f"DELETE /volumes/{volume_id} expected 204, got {delete_status}. "
            f"{volume_plugin_misconfigured_hint()}"
        )
        deleted_id = volume_id
        volume_id = None

        missing_status, missing_body = api.get_volume(deleted_id)
        assert missing_status == 404, (
            f"GET after delete expected 404, got {missing_status} body={missing_body!r}"
        )
    finally:
        if volume_id is not None:
            api.delete_volume(volume_id)
        api.close()
