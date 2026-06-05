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
