# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import importlib
import os
from typing import Any

from adapters.api_adapter import ApiClient
from framework.config import SdkE2EConfig
from framework.models import first_present
from framework.platform_lifecycle import probe_platform_lifecycle
from framework.reporting import JsonlReporter


def run_preflight(
    config: SdkE2EConfig,
    reporter: JsonlReporter,
    *,
    template_ids: set[str] | None = None,
    require_default_template: bool = True,
) -> None:
    errors: list[str] = []
    details: dict[str, Any] = {"backends": config.backends}
    effective_template_ids = set(template_ids or set())
    if config.cube_template_id:
        effective_template_ids.add(config.cube_template_id)

    if require_default_template and not config.cube_template_id:
        errors.append("CUBE_TEMPLATE_ID or --cube-template-id is required")
    if not effective_template_ids:
        errors.append("at least one template ID is required")

    _check_backend_dependencies(config.backends, errors)

    api = ApiClient(config)
    try:
        try:
            health = api.health()
            details["health"] = health
            if health.get("status") not in ("ok", "healthy"):
                errors.append(f"CubeAPI health returned unexpected status: {health!r}")
        except Exception as exc:  # noqa: BLE001 - preflight should aggregate diagnostics
            errors.append(f"CubeAPI {config.cube_api_url}/health is not reachable: {exc}")

        if effective_template_ids:
            template_summaries = []
            try:
                for template_id in sorted(effective_template_ids):
                    template = api.get_template(template_id)
                    template_summaries.append(_template_summary(template_id, template))
                    if not template:
                        errors.append(f"template {template_id!r} was not found")
                    else:
                        _check_template_ready(template_id, template, errors)
                details["templates"] = template_summaries
            except Exception as exc:  # noqa: BLE001
                errors.append(f"failed to read template metadata: {exc}")
    finally:
        api.close()

    if config.platform_lifecycle_enabled:
        ready, reason, probe_details = probe_platform_lifecycle(config)
        details["platform_lifecycle_probe"] = {
            "ready": ready,
            "reason": reason,
            **probe_details,
        }
        if not ready:
            details["platform_lifecycle_warning"] = reason

    if errors:
        reporter.record("preflight_failed", errors=errors, **details)
        raise RuntimeError("SDK E2E preflight failed:\n- " + "\n- ".join(errors))

    reporter.record("preflight_passed", **details)


def _check_backend_dependencies(backends: tuple[str, ...], errors: list[str]) -> None:
    if "cubesandbox" in backends:
        try:
            importlib.import_module("cubesandbox")
        except ImportError as exc:
            errors.append(f"cubesandbox backend requires the CubeSandbox Python SDK: {exc}")

    if "e2b" in backends:
        if _can_import("e2b_code_interpreter") or _can_import("e2b"):
            if not os.environ.get("E2B_API_KEY"):
                errors.append("e2b backend requires E2B_API_KEY")
        else:
            errors.append(
                "e2b backend requires e2b-code-interpreter or e2b. "
                "Install tests/e2e/sdk_compat/requirements.txt."
            )


def _can_import(module: str) -> bool:
    try:
        importlib.import_module(module)
    except ImportError:
        return False
    return True


def _check_template_ready(template_id: str, template: dict[str, Any], errors: list[str]) -> None:
    status = first_present(
        template,
        "status",
        "state",
        "template_status",
        "templateStatus",
    )
    if status is None:
        return
    if str(status).lower() not in {"ready", "active", "available"}:
        errors.append(f"template {template_id!r} is not ready: status={status!r}")
def _template_summary(template_id: str, template: dict[str, Any]) -> dict[str, Any]:
    return {
        "template_id": template_id,
        "name": first_present(template, "name", "templateName", "template_name"),
        "status": first_present(
            template,
            "status",
            "state",
            "template_status",
            "templateStatus",
        ),
        "response_keys": sorted(template),
    }
