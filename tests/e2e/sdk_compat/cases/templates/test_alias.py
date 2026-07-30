# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""E2E tests for template alias lifecycle.

Template operations only support the cubesandbox backend. The E2B SDK's
template API uses a different endpoint format (/v3/templates with Dockerfile)
that CubeAPI does not implement. When running with --sdk-e2e-backends
cubesandbox,e2b, the [e2b] variants are skipped with an explanation.
"""

from __future__ import annotations

import sys
import time
import uuid
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[5] / "sdk" / "python"))

import pytest
import requests
from cubesandbox import Config, Template
from cubesandbox._exceptions import ApiError, TemplateNotFoundError
from framework.auth import auth_headers

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.templates,
    pytest.mark.p1,
]

DEFAULT_IMAGE = "cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest"
DEFAULT_WRITABLE_LAYER_SIZE = "1G"

_SKIP_REASON = (
    "template operations only support cubesandbox backend "
    "(E2B SDK template API is incompatible with CubeAPI)"
)


def _require_cubesandbox(sdk_backend):
    if sdk_backend != "cubesandbox":
        pytest.skip(_SKIP_REASON)


def _cfg(sdk_e2e_config):
    return Config(api_url=sdk_e2e_config.cube_api_url)


def _wait_for_ready(template_id, config, timeout=120):
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            info = Template.get(template_id, config=config)
            if info.status in ("READY", "FAILED"):
                return info
        except TemplateNotFoundError:
            pass
        time.sleep(2)
    pytest.fail(f"template {template_id} did not reach READY within {timeout}s")


def _delete_with_retry(identifier, cfg, timeout=180):
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            Template.delete(identifier, config=cfg)
            return
        except ApiError as e:
            if "attempt is already in progress" in str(e):
                time.sleep(5)
                continue
            raise


def test_template_list_and_get_existing(sdk_backend, sdk_e2e_config):
    """List templates and GET one by ID."""
    _require_cubesandbox(sdk_backend)
    if not sdk_e2e_config.cube_template_id:
        pytest.skip("set CUBE_TEMPLATE_ID for read-only template tests")
    cfg = _cfg(sdk_e2e_config)
    templates = Template.list(config=cfg)
    assert templates
    assert any(t.template_id == sdk_e2e_config.cube_template_id for t in templates)
    detail = Template.get(sdk_e2e_config.cube_template_id, config=cfg)
    assert detail.template_id == sdk_e2e_config.cube_template_id
    assert detail.status


def test_template_create_from_image_and_cleanup(sdk_backend, sdk_e2e_config):
    """Create a template from an image and clean up."""
    _require_cubesandbox(sdk_backend)
    cfg = _cfg(sdk_e2e_config)
    created_id = None
    try:
        job = Template.build(image=DEFAULT_IMAGE, writable_layer_size=DEFAULT_WRITABLE_LAYER_SIZE, config=cfg)
        assert job.template_id.startswith("tpl-")
        created_id = job.template_id
        _wait_for_ready(created_id, cfg)
        assert Template.get(created_id, config=cfg).status == "READY"
    finally:
        if created_id:
            try:
                Template.delete(created_id, config=cfg)
            except (ApiError, TemplateNotFoundError):
                pass


def test_template_build_preserves_advanced_create_options(
    sdk_backend,
    sdk_e2e_config,
):
    _require_cubesandbox(sdk_backend)
    cfg = _cfg(sdk_e2e_config)
    created_id = None
    try:
        job = Template.build(
            image=DEFAULT_IMAGE,
            writable_layer_size=DEFAULT_WRITABLE_LAYER_SIZE,
            exposed_ports=[49983, 49999],
            probe_port=49999,
            probe_path="/",
            envs={"SDK_TEMPLATE_E2E": "advanced"},
            config=cfg,
        )
        created_id = job.template_id
        info = _wait_for_ready(created_id, cfg)
        assert info.status == "READY", (
            f"advanced template build failed: id={created_id} status={info.status!r}"
        )
        detail = Template.get(created_id, config=cfg)
        request = detail.create_request or {}
        annotations = request.get("annotations") or {}
        assert annotations.get("com.exposed_ports") == "49983:49999", request
        assert (
            annotations.get("cube.master.rootfs.writable_layer_size")
            == DEFAULT_WRITABLE_LAYER_SIZE
        ), request

        containers = request.get("containers") or []
        assert len(containers) == 1, request
        container = containers[0]
        assert container.get("envs") == [
            {"key": "SDK_TEMPLATE_E2E", "value": "advanced"}
        ], request
        http_get = (
            (container.get("probe") or {}).get("probe_handler") or {}
        ).get("http_get") or {}
        assert http_get.get("port") == 49999, request
        assert http_get.get("path") == "/", request

        volumes = request.get("volumes") or []
        assert any(
            ((volume.get("volume_source") or {}).get("empty_dir") or {}).get(
                "size_limit"
            )
            == DEFAULT_WRITABLE_LAYER_SIZE
            for volume in volumes
        ), request
    finally:
        if created_id:
            try:
                _delete_with_retry(created_id, cfg)
            except (ApiError, TemplateNotFoundError):
                pass


def test_template_alias_dedicated_endpoint_rejects_invalid(sdk_e2e_config):
    """GET /templates/aliases/:alias returns 400 for invalid formats."""
    headers = auth_headers()
    for invalid in ["UPPER", "tpl-hijack", "snap-hijack", "a" * 65]:
        resp = requests.get(
            f"{sdk_e2e_config.cube_api_url}/templates/aliases/{invalid}",
            headers=headers,
        )
        assert resp.status_code == 400


def test_template_alias_create_get_and_delete(sdk_backend, sdk_e2e_config):
    """Full alias lifecycle: create -> get by alias -> delete by alias -> 404."""
    _require_cubesandbox(sdk_backend)
    cfg = _cfg(sdk_e2e_config)
    alias = f"e2e-alias-{uuid.uuid4().hex[:8]}"
    created_id = None
    try:
        job = Template.build(name=alias, image=DEFAULT_IMAGE, writable_layer_size=DEFAULT_WRITABLE_LAYER_SIZE, config=cfg)
        created_id = job.template_id
        _wait_for_ready(created_id, cfg)
        assert Template.get(alias, config=cfg).template_id == created_id
        assert any(t.template_id == created_id for t in Template.list(config=cfg))
        _delete_with_retry(alias, cfg)
        created_id = None
        with pytest.raises(TemplateNotFoundError):
            Template.get(alias, config=cfg)
    finally:
        time.sleep(1)
        if created_id:
            try:
                Template.delete(created_id, config=cfg)
            except (ApiError, TemplateNotFoundError):
                pass


def test_template_alias_dedicated_lookup_endpoint(sdk_backend, sdk_e2e_config):
    """GET /templates/aliases/:alias (E2B compat) returns {templateID, public}."""
    _require_cubesandbox(sdk_backend)
    cfg = _cfg(sdk_e2e_config)
    alias = f"e2e-alias-ep-{uuid.uuid4().hex[:8]}"
    created_id = None
    try:
        job = Template.build(name=alias, image=DEFAULT_IMAGE, writable_layer_size=DEFAULT_WRITABLE_LAYER_SIZE, config=cfg)
        created_id = job.template_id
        _wait_for_ready(created_id, cfg)
        resp = requests.get(
            f"{sdk_e2e_config.cube_api_url}/templates/aliases/{alias}",
            headers=auth_headers(),
        )
        assert resp.status_code == 200
        assert resp.json()["templateID"] == created_id
    finally:
        time.sleep(1)
        if created_id:
            try:
                Template.delete(created_id, config=cfg)
            except (ApiError, TemplateNotFoundError):
                pass


def test_template_alias_rebuild_reassignment(sdk_backend, sdk_e2e_config):
    """Rebuild with same alias moves it to the newly READY template."""
    _require_cubesandbox(sdk_backend)
    cfg = _cfg(sdk_e2e_config)
    alias = f"e2e-alias-rebuild-{uuid.uuid4().hex[:8]}"
    template_ids = []
    try:
        job_a = Template.build(name=alias, image=DEFAULT_IMAGE, writable_layer_size=DEFAULT_WRITABLE_LAYER_SIZE, config=cfg)
        template_ids.append(job_a.template_id)
        _wait_for_ready(job_a.template_id, cfg)
        assert Template.get(alias, config=cfg).template_id == job_a.template_id

        job_b = Template.build(name=alias, image=DEFAULT_IMAGE, writable_layer_size=DEFAULT_WRITABLE_LAYER_SIZE, config=cfg)
        template_ids.append(job_b.template_id)
        _wait_for_ready(job_b.template_id, cfg)
        assert Template.get(alias, config=cfg).template_id == job_b.template_id
        assert Template.get(job_a.template_id, config=cfg).template_id == job_a.template_id
    finally:
        time.sleep(1)
        for tid in template_ids:
            try:
                Template.delete(tid, config=cfg)
            except (ApiError, TemplateNotFoundError):
                pass
