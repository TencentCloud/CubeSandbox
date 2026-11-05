# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Template wait and cleanup shared by public SDK cases."""
import time

import pytest
from cubesandbox import Template
from cubesandbox._exceptions import ApiError, TemplateNotFoundError
from framework.parallel import scale_timeout_for_xdist


def wait_for_ready(template_id, config, timeout=None, build_id=None):
    # Widen the serial-run budget for parallel (xdist) runs: every alias case
    # builds a fresh template from the same image, so under xdist all workers
    # submit near-identical builds that serialize on CubeMaster's per-artifactID
    # lock. The last worker's build can then take well past the serial budget.
    if timeout is None:
        timeout = scale_timeout_for_xdist(120)
    deadline = time.monotonic() + timeout
    last_info = None
    while time.monotonic() < deadline:
        if build_id:
            build = Template.get_build_status(template_id, build_id, config=config)
            if build.status.lower() in {"error", "failed"}:
                pytest.fail(f"template {template_id} build {build_id} failed: {build.error_message or build.message}")
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


def delete_with_retry(identifier, cfg, timeout=180):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            Template.delete(identifier, config=cfg)
            return
        except TemplateNotFoundError:
            return
        except ApiError as e:
            if "attempt is already in progress" in str(e):
                time.sleep(5)
                continue
            raise

    raise TimeoutError(f"template {identifier} deletion remained busy after {timeout}s")
