# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""E2E tests for the template create-from-image ``backend`` option.

Verifies that the CoW backend passed to ``Template.build(backend=...)`` is
forwarded by CubeAPI and persisted by CubeMaster into the template's stored
create request. ``xfs`` is used because it is the historical default and
builds successfully on every cluster; ``s3`` requires an S3-backed deployment
and is therefore not exercised here.
"""

from __future__ import annotations

import time

import pytest
from cubesandbox import Config, Template
from cubesandbox._exceptions import ApiError, TemplateNotFoundError

from framework.build_throttle import template_build_slot

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.templates,
    pytest.mark.p1,
]

DEFAULT_IMAGE = "cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest"
DEFAULT_WRITABLE_LAYER_SIZE = "1G"


def _require_cubesandbox(sdk_backend):
    if sdk_backend != "cubesandbox":
        pytest.skip("template operations only support cubesandbox backend")


def _cfg(sdk_e2e_config):
    return Config(api_url=sdk_e2e_config.cube_api_url)


def _wait_for_ready(template_id, config, timeout=180):
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            info = Template.get(template_id, config=config)
            if info.status == "READY":
                return info
            if info.status == "FAILED":
                pytest.fail(f"template {template_id} build failed: {info.last_error!r}")
        except TemplateNotFoundError:
            pass
        time.sleep(2)
    pytest.fail(f"template {template_id} did not reach READY within {timeout}s")


def test_template_build_forwards_backend_to_create_request(
    sdk_backend,
    sdk_e2e_config,
):
    """backend=xfs is accepted by CubeAPI and persisted in create_request."""
    _require_cubesandbox(sdk_backend)
    cfg = _cfg(sdk_e2e_config)
    created_id = None
    try:
        with template_build_slot(label="backend_xfs_forward"):
            job = Template.build(
                image=DEFAULT_IMAGE,
                writable_layer_size=DEFAULT_WRITABLE_LAYER_SIZE,
                backend="xfs",
                config=cfg,
            )
            created_id = job.template_id
            _wait_for_ready(created_id, cfg)

        detail = Template.get(created_id, config=cfg)
        request = detail.create_request or {}
        assert request.get("backend") == "xfs", request
    finally:
        if created_id:
            try:
                Template.delete(created_id, config=cfg)
            except (ApiError, TemplateNotFoundError):
                pass
