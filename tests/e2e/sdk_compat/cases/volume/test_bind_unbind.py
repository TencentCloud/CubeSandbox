# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Volume bind / unbind and delete-while-bound (HTTP 409) cases.

Prerequisites (manual; not provisioned by this suite):
- Deploy and configure a Volume Plugin on CubeMaster (Controller) and Cubelet
  (Node), e.g. COS binary/rpc under ``volume_plugins``, with credentials.
  Guide: https://github.com/TencentCloud/CubeSandbox/blob/master/examples/volume/cos/README.md
- Platform Volume API available (CubeAPI / CubeMaster / Cubelet >= 0.6.0).
- Python SDK ``cubesandbox`` >= 0.6.0 (Volume / volume_mounts support).
- A READY template (``CUBE_TEMPLATE_ID``) for sandbox create with mounts.
- Opt-in: ``SDK_E2E_VOLUME_PLUGIN=true`` (and usually ``SDK_E2E_VOLUME_DRIVER``).
"""

from __future__ import annotations

import pytest

from adapters import create_adapter
from framework.capabilities import VOLUME_PLUGIN
from framework.cleanup import safe_kill
from framework.volume import (
    VOLUME_MOUNT_PATH,
    managed_volume,
    wait_delete_volume_status,
)

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.volume,
    pytest.mark.p1,
    pytest.mark.requires_capability(VOLUME_PLUGIN),
]


def test_volume_bind_delete_conflict_then_unbind(sdk_backend, sdk_e2e_config):
    # Bind via sandbox create with volumeMounts, assert delete-while-bound → 409,
    # then kill sandbox (unbind) and delete volume → 204.
    if not sdk_e2e_config.cube_template_id:
        pytest.skip("CUBE_TEMPLATE_ID or --cube-template-id is required for volume bind")

    with managed_volume(sdk_e2e_config) as (volume_id, api):
        adapter = None
        try:
            adapter = create_adapter(
                sdk_backend,
                sdk_e2e_config,
                metadata={
                    "test_suite": "sdk_compat",
                    "test_role": "volume_bind",
                    "test_backend": sdk_backend,
                },
                create_options={
                    # Sandbox.create merges **kwargs into the JSON body as-is.
                    "volumeMounts": [
                        {"name": volume_id, "path": VOLUME_MOUNT_PATH},
                    ],
                },
            )
            assert adapter.sandbox_id

            conflict = wait_delete_volume_status(
                api,
                volume_id,
                409,
                timeout=sdk_e2e_config.volume_refcount_wait,
            )
            assert conflict == 409

            errors = safe_kill(adapter, sdk_e2e_config)
            adapter = None
            assert not errors, f"sandbox cleanup errors: {errors}"

            deleted = wait_delete_volume_status(
                api,
                volume_id,
                204,
                timeout=sdk_e2e_config.volume_refcount_wait,
            )
            assert deleted == 204
        finally:
            if adapter is not None:
                safe_kill(adapter, sdk_e2e_config)
