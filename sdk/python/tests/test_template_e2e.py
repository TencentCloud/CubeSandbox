# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""End-to-end tests for CubeAPI template endpoints.

These tests intentionally hit a real CubeAPI instance. They are skipped by
default and can be enabled with ``--run-e2e`` or ``CUBE_E2E=1``.
"""

from __future__ import annotations

import os
import time
import uuid
import requests
import pytest

from cubesandbox import Config, Template
from cubesandbox._exceptions import ApiError, TemplateNotFoundError


pytestmark = pytest.mark.e2e


def _option(pytestconfig: pytest.Config, option: str, env: str, default: str | None = None) -> str | None:
    return pytestconfig.getoption(option) or os.environ.get(env) or default


def _require_e2e(pytestconfig: pytest.Config) -> None:
    if not pytestconfig.getoption("--run-e2e") and os.environ.get("CUBE_E2E") != "1":
        pytest.skip("use --run-e2e or set CUBE_E2E=1 to run live CubeAPI e2e tests")


def _config(pytestconfig: pytest.Config) -> Config:
    api_url = _option(pytestconfig, "--cube-api-url", "CUBE_API_URL", "http://127.0.0.1:3000")
    return Config(api_url=api_url)


def _env_int(name: str) -> int | None:
    value = os.environ.get(name)
    return int(value) if value else None


def _env_ports(name: str) -> list[int] | None:
    value = os.environ.get(name)
    if not value:
        return None
    return [int(port.strip()) for port in value.split(",") if port.strip()]


def _env_bool(name: str) -> bool | None:
    value = os.environ.get(name)
    if value is None:
        return None
    return value == "1"


def test_template_list_and_get_existing_template(pytestconfig: pytest.Config) -> None:
    _require_e2e(pytestconfig)
    config = _config(pytestconfig)

    templates = Template.list(config=config)
    assert isinstance(templates, list)

    template_id = _option(pytestconfig, "--cube-template-id", "CUBE_TEMPLATE_ID")
    if template_id is None:
        if not templates:
            pytest.skip("no templates available and CUBE_TEMPLATE_ID is not set")
        template_id = templates[0].template_id

    detail = Template.get(template_id, config=config)
    assert detail.template_id == template_id
    assert detail.status


def test_template_create_from_image_and_cleanup(pytestconfig: pytest.Config) -> None:
    _require_e2e(pytestconfig)
    image = _option(pytestconfig, "--cube-template-image", "CUBE_TEMPLATE_E2E_IMAGE")
    if not image:
        pytest.skip("use --cube-template-image or set CUBE_TEMPLATE_E2E_IMAGE to create a template")

    config = _config(pytestconfig)
    requested_template_id = os.environ.get("CUBE_TEMPLATE_E2E_ID") or f"e2e-python-{uuid.uuid4().hex[:8]}"
    created_template_id: str | None = None

    try:
        job = Template.build(
            template_id=requested_template_id,
            image=image,
            instance_type=os.environ.get("CUBE_TEMPLATE_E2E_INSTANCE_TYPE"),
            writable_layer_size=os.environ.get("CUBE_TEMPLATE_E2E_WRITABLE_LAYER_SIZE"),
            exposed_ports=_env_ports("CUBE_TEMPLATE_E2E_EXPOSED_PORTS"),
            probe_port=_env_int("CUBE_TEMPLATE_E2E_PROBE_PORT"),
            probe_path=os.environ.get("CUBE_TEMPLATE_E2E_PROBE_PATH"),
            cpu_count=_env_int("CUBE_TEMPLATE_E2E_CPU"),
            memory_mb=_env_int("CUBE_TEMPLATE_E2E_MEMORY"),
            allow_internet_access=_env_bool("CUBE_TEMPLATE_E2E_ALLOW_INTERNET"),
            config=config,
        )

        assert job.job_id
        assert job.template_id.startswith("tpl-")
        assert job.template_id != requested_template_id
        assert job.status
        created_template_id = job.template_id

        # Template creation is async — poll until the definition is persisted.
        deadline = time.time() + 120
        detail = None
        while time.time() < deadline:
            try:
                detail = Template.get(created_template_id, config=config)
                break
            except TemplateNotFoundError:
                time.sleep(2)
        assert detail is not None, f"template {created_template_id} not persisted within 120s"
        assert detail.template_id == created_template_id

        if job.job_id:
            status = Template.get_build_status(created_template_id, job.job_id, config=config)
            assert status.build_id == job.job_id
            assert status.template_id == created_template_id
    finally:
        # Give CubeAPI a short moment to persist the new template before cleanup.
        time.sleep(1)
        if created_template_id is not None:
            try:
                Template.delete(created_template_id, config=config)
            except (ApiError, TemplateNotFoundError):
                pass


def test_template_alias_create_get_and_delete(pytestconfig: pytest.Config) -> None:
    """Create a template with a stable alias, then look up and delete it by alias.

    Exercises the full alias lifecycle:
      1. Template.build(name=<alias>) → CubeAPI derives alias from the E2B name.
      2. Template.get(<alias>) → CubeMaster resolves alias to the template ID.
      3. Template.list() includes the alias in the response.
      4. Template.delete(<alias>) → delete handler resolves alias before cleanup.
      5. Template.get(<alias>) → 404 after deletion.
    """
    _require_e2e(pytestconfig)
    image = _option(pytestconfig, "--cube-template-image", "CUBE_TEMPLATE_E2E_IMAGE")
    if not image:
        pytest.skip("use --cube-template-image or set CUBE_TEMPLATE_E2E_IMAGE to create a template")

    config = _config(pytestconfig)
    alias = f"e2e-alias-{uuid.uuid4().hex[:8]}"
    created_template_id: str | None = None

    try:
        # 1. Create with alias.
        job = Template.build(
            name=alias,
            image=image,
            instance_type=os.environ.get("CUBE_TEMPLATE_E2E_INSTANCE_TYPE"),
            writable_layer_size=os.environ.get("CUBE_TEMPLATE_E2E_WRITABLE_LAYER_SIZE"),
            exposed_ports=_env_ports("CUBE_TEMPLATE_E2E_EXPOSED_PORTS"),
            config=config,
        )
        assert job.job_id
        assert job.template_id.startswith("tpl-")
        created_template_id = job.template_id

        # Poll until the build finishes (READY or FAILED), since the alias
        # is claimed only after finalizeTemplateReplicas succeeds.
        deadline = time.time() + 120
        while time.time() < deadline:
            try:
                built = Template.get(created_template_id, config=config)
                if built.status in ("READY", "FAILED"):
                    break
            except TemplateNotFoundError:
                pass
            time.sleep(2)

        # 2. Look up by alias — CubeMaster resolves alias → template ID.
        detail = Template.get(alias, config=config)
        assert detail.template_id == created_template_id

        # 3. List should include the template (verify it's reachable).
        templates = Template.list(config=config)
        assert any(t.template_id == created_template_id for t in templates)

        # 4. Delete by alias — delete handler resolves alias before cleanup.
        #    Retry until the build job settles (deletion is blocked while a
        #    build is still active).
        deadline = time.time() + 180
        while time.time() < deadline:
            try:
                Template.delete(alias, config=config)
                break
            except ApiError as e:
                if "attempt is already in progress" in str(e):
                    time.sleep(5)
                    continue
                raise
        created_template_id = None  # already deleted

        # 5. Alias should no longer resolve.
        try:
            Template.get(alias, config=config)
            assert False, f"alias {alias} should not resolve after deletion"
        except TemplateNotFoundError:
            pass  # expected
    finally:
        time.sleep(1)
        if created_template_id is not None:
            try:
                Template.delete(created_template_id, config=config)
            except (ApiError, TemplateNotFoundError):
                pass


def test_template_alias_validation_rejects_invalid_formats(pytestconfig: pytest.Config) -> None:
    """Invalid alias formats are rejected with 400 before the build starts.

    Exercises CubeMaster's validateTemplateAlias regex via CubeAPI:
      - Uppercase letters are not allowed.
      - Aliases starting with tpl-/snap- are rejected (namespace collision).
      - Aliases exceeding 64 characters are rejected.
    """
    _require_e2e(pytestconfig)
    image = _option(pytestconfig, "--cube-template-image", "CUBE_TEMPLATE_E2E_IMAGE")
    if not image:
        pytest.skip("use --cube-template-image or set CUBE_TEMPLATE_E2E_IMAGE to create a template")

    config = _config(pytestconfig)
    invalid_aliases = [
        "UPPER",            # uppercase not allowed
        "tpl-hijack",       # collides with template ID prefix
        "snap-hijack",      # collides with snapshot ID prefix
        "a" * 65,           # exceeds 64-char limit
    ]
    for invalid in invalid_aliases:
        with pytest.raises(ApiError):
            Template.build(name=invalid, image=image, config=config)


def test_template_alias_dedicated_lookup_endpoint(pytestconfig: pytest.Config) -> None:
    """GET /templates/aliases/:alias (E2B compat endpoint) returns templateID.

    Unlike GET /templates/:templateID which transparently resolves aliases via
    CubeMaster, this dedicated endpoint is the E2B-compatible alias lookup that
    returns a minimal {templateID, public} response.
    """
    _require_e2e(pytestconfig)
    image = _option(pytestconfig, "--cube-template-image", "CUBE_TEMPLATE_E2E_IMAGE")
    if not image:
        pytest.skip("use --cube-template-image or set CUBE_TEMPLATE_E2E_IMAGE to create a template")

    config = _config(pytestconfig)
    alias = f"e2e-alias-ep-{uuid.uuid4().hex[:8]}"
    created_template_id: str | None = None

    try:
        job = Template.build(
            name=alias,
            image=image,
            instance_type=os.environ.get("CUBE_TEMPLATE_E2E_INSTANCE_TYPE"),
            writable_layer_size=os.environ.get("CUBE_TEMPLATE_E2E_WRITABLE_LAYER_SIZE"),
            exposed_ports=_env_ports("CUBE_TEMPLATE_E2E_EXPOSED_PORTS"),
            config=config,
        )
        created_template_id = job.template_id

        # Poll until the build finishes (alias claimed after READY).
        deadline = time.time() + 120
        while time.time() < deadline:
            try:
                built = Template.get(created_template_id, config=config)
                if built.status in ("READY", "FAILED"):
                    break
            except TemplateNotFoundError:
                pass
            time.sleep(2)

        # Hit the dedicated alias endpoint.
        resp = requests.get(f"{config.api_url}/templates/aliases/{alias}")
        assert resp.status_code == 200, f"alias lookup failed: {resp.status_code} {resp.text}"
        data = resp.json()
        assert data["templateID"] == created_template_id
        assert "public" in data
    finally:
        time.sleep(1)
        if created_template_id is not None:
            try:
                Template.delete(created_template_id, config=config)
            except (ApiError, TemplateNotFoundError):
                pass


def test_template_alias_rebuild_reassignment(pytestconfig: pytest.Config) -> None:
    """Rebuilding with the same alias moves it to the newest template.

    The core feature of template aliases: creating a second template with an
    alias already held by a non-deleting template releases the alias from the
    old one and claims it for the new one. This is what makes aliases survive
    image rebuilds without manual cleanup.

      1. Create template A with alias.
      2. Create template B with the same alias.
      3. Verify alias now resolves to B, not A.
      4. Verify A no longer has the alias.
    """
    _require_e2e(pytestconfig)
    image = _option(pytestconfig, "--cube-template-image", "CUBE_TEMPLATE_E2E_IMAGE")
    if not image:
        pytest.skip("use --cube-template-image or set CUBE_TEMPLATE_E2E_IMAGE to create a template")

    config = _config(pytestconfig)
    alias = f"e2e-alias-rebuild-{uuid.uuid4().hex[:8]}"
    template_ids: list[str] = []

    try:
        common_kwargs = dict(
            image=image,
            instance_type=os.environ.get("CUBE_TEMPLATE_E2E_INSTANCE_TYPE"),
            writable_layer_size=os.environ.get("CUBE_TEMPLATE_E2E_WRITABLE_LAYER_SIZE"),
            exposed_ports=_env_ports("CUBE_TEMPLATE_E2E_EXPOSED_PORTS"),
            config=config,
        )

        # 1. Create template A with the alias.
        job_a = Template.build(name=alias, **common_kwargs)
        template_ids.append(job_a.template_id)

        # Poll until A is READY (alias claimed after finalize).
        deadline = time.time() + 120
        while time.time() < deadline:
            try:
                built_a = Template.get(job_a.template_id, config=config)
                if built_a.status in ("READY", "FAILED"):
                    break
            except TemplateNotFoundError:
                pass
            time.sleep(2)

        # Verify alias resolves to A initially.
        detail_a = Template.get(alias, config=config)
        assert detail_a.template_id == job_a.template_id

        # 2. Create template B with the same alias — should steal it from A.
        job_b = Template.build(name=alias, **common_kwargs)
        template_ids.append(job_b.template_id)

        # Poll until B is READY (alias claim steals from A).
        deadline = time.time() + 120
        while time.time() < deadline:
            try:
                built_b = Template.get(job_b.template_id, config=config)
                if built_b.status in ("READY", "FAILED"):
                    break
            except TemplateNotFoundError:
                pass
            time.sleep(2)

        # 3. Alias should now resolve to B, not A.
        detail_b = Template.get(alias, config=config)
        assert detail_b.template_id == job_b.template_id

        # 4. A should still exist but alias should no longer point to it.
        detail_a_direct = Template.get(job_a.template_id, config=config)
        assert detail_a_direct.template_id == job_a.template_id
    finally:
        time.sleep(1)
        for tid in template_ids:
            try:
                Template.delete(tid, config=config)
            except (ApiError, TemplateNotFoundError):
                pass