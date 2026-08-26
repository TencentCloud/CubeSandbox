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
from framework.build_throttle import template_build_slot
from framework.parallel import scale_timeout_for_xdist

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


def _wait_for_ready(template_id, config, timeout=None):
    # Widen the serial-run budget for parallel (xdist) runs: every alias case
    # builds a fresh template from the same image, so under xdist all workers
    # submit near-identical builds that serialize on CubeMaster's per-artifactID
    # lock. The last worker's build can then take well past the serial budget.
    if timeout is None:
        timeout = scale_timeout_for_xdist(120)
    deadline = time.time() + timeout
    last_info = None
    while time.time() < deadline:
        try:
            last_info = Template.get(template_id, config=config)
            if last_info.status == "READY":
                return last_info
            if last_info.status == "FAILED":
                pytest.fail(
                    f"template {template_id} build failed; "
                    f"last_error={last_info.last_error!r}"
                )
        except TemplateNotFoundError:
            pass
        time.sleep(2)
    if last_info is None:
        pytest.fail(
            f"template {template_id} did not reach READY within {timeout}s; "
            "template was never observed"
        )
    pytest.fail(
        f"template {template_id} did not reach READY within {timeout}s; "
        f"last_status={last_info.status!r} last_error={last_info.last_error!r}"
    )


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
        with template_build_slot(label="alias_create_from_image"):
            job = Template.build(
                image=DEFAULT_IMAGE,
                writable_layer_size=DEFAULT_WRITABLE_LAYER_SIZE,
                config=cfg,
            )
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
        with template_build_slot(label="alias_advanced_options"):
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
            _wait_for_ready(created_id, cfg)
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
        http_get = ((container.get("probe") or {}).get("probe_handler") or {}).get(
            "http_get"
        ) or {}
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
        with template_build_slot(label="alias_create_get_delete"):
            job = Template.build(
                name=alias,
                image=DEFAULT_IMAGE,
                writable_layer_size=DEFAULT_WRITABLE_LAYER_SIZE,
                config=cfg,
            )
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
        with template_build_slot(label="alias_dedicated_lookup"):
            job = Template.build(
                name=alias,
                image=DEFAULT_IMAGE,
                writable_layer_size=DEFAULT_WRITABLE_LAYER_SIZE,
                config=cfg,
            )
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
        with template_build_slot(label="alias_rebuild_a"):
            job_a = Template.build(
                name=alias,
                image=DEFAULT_IMAGE,
                writable_layer_size=DEFAULT_WRITABLE_LAYER_SIZE,
                config=cfg,
            )
            template_ids.append(job_a.template_id)
            _wait_for_ready(job_a.template_id, cfg)
        assert Template.get(alias, config=cfg).template_id == job_a.template_id

        with template_build_slot(label="alias_rebuild_b"):
            job_b = Template.build(
                name=alias,
                image=DEFAULT_IMAGE,
                writable_layer_size=DEFAULT_WRITABLE_LAYER_SIZE,
                config=cfg,
            )
            template_ids.append(job_b.template_id)
            _wait_for_ready(job_b.template_id, cfg)
        assert Template.get(alias, config=cfg).template_id == job_b.template_id
        assert (
            Template.get(job_a.template_id, config=cfg).template_id == job_a.template_id
        )
    finally:
        time.sleep(1)
        for tid in template_ids:
            try:
                Template.delete(tid, config=cfg)
            except (ApiError, TemplateNotFoundError):
                pass


def test_template_alias_set_on_existing_template(sdk_backend, sdk_e2e_config):
    """Set an alias on an already-existing template, then clear it (PUT /templates/:id/alias)."""
    _require_cubesandbox(sdk_backend)
    cfg = _cfg(sdk_e2e_config)
    alias = f"e2e-set-alias-{uuid.uuid4().hex[:8]}"
    created_id = None
    try:
        # 1. Create WITHOUT an alias.
        with template_build_slot(label="alias_set_existing"):
            job = Template.build(
                image=DEFAULT_IMAGE,
                writable_layer_size=DEFAULT_WRITABLE_LAYER_SIZE,
                config=cfg,
            )
            created_id = job.template_id
            _wait_for_ready(created_id, cfg)

        with pytest.raises(ApiError) as exc:
            Template.set_alias(created_id, "My-Alias", config=cfg)
        assert exc.value.status_code == 400

        # 2. Set the alias on the existing template.
        updated = Template.set_alias(created_id, alias, config=cfg)
        assert updated.template_id == created_id
        assert updated.name == alias

        # 3. GET by alias resolves to the template.
        assert Template.get(alias, config=cfg).template_id == created_id

        # 4. Clear the alias.
        cleared = Template.set_alias(created_id, None, config=cfg)
        assert cleared.template_id == created_id

        # 5. Alias should no longer resolve.
        with pytest.raises(TemplateNotFoundError):
            Template.get(alias, config=cfg)
    finally:
        time.sleep(1)
        if created_id:
            try:
                Template.delete(created_id, config=cfg)
            except (ApiError, TemplateNotFoundError):
                pass


def test_template_alias_set_reassign_between_templates(sdk_backend, sdk_e2e_config):
    """Setting an alias held by another template steals it (PUT /templates/:id/alias)."""
    _require_cubesandbox(sdk_backend)
    cfg = _cfg(sdk_e2e_config)
    alias = f"e2e-set-reassign-{uuid.uuid4().hex[:8]}"
    template_ids = []
    try:
        # 1. Create T1 WITH alias A.
        with template_build_slot(label="alias_set_reassign_a"):
            job_a = Template.build(
                name=alias,
                image=DEFAULT_IMAGE,
                writable_layer_size=DEFAULT_WRITABLE_LAYER_SIZE,
                config=cfg,
            )
            template_ids.append(job_a.template_id)
            _wait_for_ready(job_a.template_id, cfg)

        # 2. Create T2 WITHOUT alias.
        with template_build_slot(label="alias_set_reassign_b"):
            job_b = Template.build(
                image=DEFAULT_IMAGE,
                writable_layer_size=DEFAULT_WRITABLE_LAYER_SIZE,
                config=cfg,
            )
            template_ids.append(job_b.template_id)
            _wait_for_ready(job_b.template_id, cfg)

        # 3. set_alias(T2, A) should release A from T1 and claim for T2.
        Template.set_alias(job_b.template_id, alias, config=cfg)

        # 4. Alias A now resolves to T2.
        assert Template.get(alias, config=cfg).template_id == job_b.template_id

        # 5. T1 no longer holds A (name derives from aliases[0]; empty once released).
        t1_detail = Template.get(job_a.template_id, config=cfg)
        assert not t1_detail.name
    finally:
        time.sleep(1)
        for tid in template_ids:
            try:
                Template.delete(tid, config=cfg)
            except (ApiError, TemplateNotFoundError):
                pass
