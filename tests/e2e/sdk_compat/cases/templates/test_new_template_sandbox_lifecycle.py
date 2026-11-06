# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""E2E coverage for E01 in docs/dev/template-reliability-test-guide.md §11.2:

    build a brand-new template -> create a real sandbox from that *exact*
    template_id (not the pre-provisioned default) -> run a real command and
    a file write/read roundtrip inside it -> delete the sandbox and the
    template -> confirm both converge.

``test_alias.py`` already covers build -> READY and alias semantics but
never uses the resulting ``template_id`` to create a sandbox.
``cases/commands`` and ``cases/filesystem`` only ever run against the
pre-provisioned default template (``CUBE_TEMPLATE_ID``). Neither proves that
the artifact *this* build just produced is the one Cubelet actually
downloads and boots. This file closes exactly that gap: the sandbox below is
created with ``cube_template_id`` pinned to the freshly built ID, and every
assertion (command stdout, file content) embeds a per-run random marker so a
stale/cached/wrong template would fail the test instead of passing by
coincidence.

What this intentionally does NOT cover:

- Byte-path provenance (cold S3 GET vs. TC node-direct GET, size/SHA256 at
  the wire level) -- that needs packet capture or node-side instrumentation
  outside the CubeAPI/SDK surface this suite talks to; see guide §6 D01/D02.
- Ticket-protocol authorization edge cases (E02 in the same guide section:
  forged/foreign-node tickets, revoked replicas, component-secret-is-not-
  enough). That needs direct access to CubeMaster/CubeTemplateCenter
  internal endpoints and component secrets, which are not reachable through
  CubeAPI and would violate the test guide's "credentials only in memory,
  never in command args/scripts" rule (§2.2) if hardcoded here. That is
  covered by a same-process Go integration test instead (real CubeMaster
  ticket issuance + real CubeTemplateCenter verification against a
  throwaway SQLite store, not a real cross-service hop, so it is not a
  substitute for this file):
  CubeTemplateCenter/pkg/httpservice/artifact_download_ticket_integration_test.go
  (TestTicketDownloadIntegration_*).
"""

from __future__ import annotations

import uuid
from dataclasses import replace

import pytest

from adapters import create_adapter_with_capacity_retry
from framework.assertions import assert_command_ok
from framework.capabilities import COMMANDS, FILESYSTEM, capabilities_for_backend
from framework.cleanup import safe_kill

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.templates,
    pytest.mark.p2,
    pytest.mark.slow,
]


def _require_cubesandbox(sdk_backend: str) -> None:
    if sdk_backend != "cubesandbox":
        pytest.skip(
            "template build + direct template_id sandbox creation only "
            "applies to the cubesandbox backend"
        )


def test_new_template_sandbox_runs_real_command_and_file_roundtrip(
    sdk_backend,
    sdk_e2e_config,
):
    """Build a fresh template, boot a sandbox pinned to it, prove it is live.

    Covers guide §11.2 E01's first-layer assertion: a sandbox created with
    this run's own freshly built ``template_id`` executes a real command and
    round-trips a real file, i.e. the artifact this build just produced is
    actually servable and bootable end to end, not just reported READY.
    """
    _require_cubesandbox(sdk_backend)
    if COMMANDS not in capabilities_for_backend(sdk_backend) or (
        FILESYSTEM not in capabilities_for_backend(sdk_backend)
    ):
        pytest.skip(f"backend {sdk_backend!r} does not support commands+filesystem")

    from cases.templates.test_alias import (
        DEFAULT_IMAGE,
        DEFAULT_WRITABLE_LAYER_SIZE,
        _delete_with_retry,
        _wait_for_ready,
    )
    from cubesandbox import Config, Template
    from cubesandbox._exceptions import ApiError, TemplateNotFoundError
    from framework.build_throttle import template_build_slot

    cfg = Config(api_url=sdk_e2e_config.cube_api_url)
    alias = f"e2e-new-template-{uuid.uuid4().hex[:8]}"
    created_id = None
    adapter = None
    try:
        with template_build_slot(label="new_template_sandbox_lifecycle"):
            job = Template.build(
                name=alias,
                image=DEFAULT_IMAGE,
                writable_layer_size=DEFAULT_WRITABLE_LAYER_SIZE,
                exposed_ports=[49983, 49999],
                probe_port=49999,
                probe_path="/",
                config=cfg,
            )
            created_id = job.template_id
            _wait_for_ready(created_id, cfg)
        assert Template.get(created_id, config=cfg).status == "READY"

        node_config = replace(sdk_e2e_config, cube_template_id=created_id)
        adapter = create_adapter_with_capacity_retry(
            sdk_backend,
            node_config,
            metadata={
                "test_suite": "sdk_compat",
                "test_nodeid": "templates/test_new_template_sandbox_lifecycle",
                "test_run_id": uuid.uuid4().hex,
            },
        )

        command_marker = f"e2e-new-template-cmd-{uuid.uuid4().hex[:8]}"
        result = adapter.run_command(
            f"echo {command_marker}",
            timeout=sdk_e2e_config.command_timeout,
        )
        assert_command_ok(result)
        assert command_marker in result.stdout, (
            f"sandbox booted from freshly built template {created_id!r} did "
            f"not execute the real command; stdout={result.stdout!r}"
        )

        path = "/tmp/sdk-compat-new-template.txt"
        file_marker = f"e2e-new-template-file-{uuid.uuid4().hex[:8]}"
        adapter.write_file(path, file_marker)
        assert adapter.read_file(path) == file_marker, (
            f"file roundtrip on sandbox from template {created_id!r} did not "
            "return the exact content just written"
        )
        cat_result = adapter.run_command(
            f"cat {path}", timeout=sdk_e2e_config.command_timeout
        )
        assert_command_ok(cat_result)
        assert cat_result.stdout == file_marker, (
            "a real command inside the sandbox could not see the file "
            "written through the files API; writable-layer/rootfs mismatch?"
        )
    finally:
        if adapter is not None:
            errors = safe_kill(adapter, sdk_e2e_config)
            assert not errors, f"sandbox cleanup errors: {errors}"
        if created_id:
            try:
                _delete_with_retry(created_id, cfg)
            except (ApiError, TemplateNotFoundError):
                pass

    with pytest.raises(TemplateNotFoundError):
        Template.get(created_id, config=cfg)
