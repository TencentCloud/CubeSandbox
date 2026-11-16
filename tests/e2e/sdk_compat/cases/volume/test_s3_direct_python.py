# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os
import uuid

import pytest

from cubesandbox import Config, Volume
from framework.capabilities import VOLUME_PLUGIN

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.volume,
    pytest.mark.p1,
    pytest.mark.requires_capability(VOLUME_PLUGIN),
]


def test_python_volume_reads_and_writes_s3_directly(sdk_backend, sdk_e2e_config):
    if sdk_backend != "cubesandbox":
        pytest.skip("direct S3 Volume credentials are a CubeSandbox Volume Plugin extension")
    volume_id = os.environ.get("SDK_E2E_DIRECT_S3_VOLUME_ID") or os.environ.get("CUBE_VOLUME_ID")
    if not volume_id:
        pytest.skip("set SDK_E2E_DIRECT_S3_VOLUME_ID to an existing volume with a credentials hook")

    volume = Volume.connect(volume_id, config=Config(api_url=sdk_e2e_config.cube_api_url))
    path = f"/sdk-e2e-s3-{uuid.uuid4().hex}.bin"
    expected = b"direct S3 volume\x00\xff"
    created = False
    try:
        written = volume.write_file(path, expected, force=True)
        created = True
        assert written.size == len(expected)
        assert volume.read_file(path, format="bytes") == expected
        assert b"".join(volume.read_file(path, format="stream")) == expected
    finally:
        if created:
            volume.remove(path)
