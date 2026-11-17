# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""E2E tests for the template create-from-image ``user`` option.

Each template runs a plain shell loop that prints its own uid/gid to stdout,
so no probe or envd is needed. The printed ids are read back with
``cubecli logs`` from both the template build and a sandbox created from the
template. That makes this module single-node only, like the other cubecli
log checks.

The default image is ``redis:7-alpine``. It runs fine as a non-root user and
ships ``redis:x:999:1000`` in ``/etc/passwd``, which is enough to check name
lookup, the primary group from passwd, and an explicit numeric group.
"""

from __future__ import annotations

import os
import re
import time

import pytest
from cubesandbox import Config, Sandbox, Template
from cubesandbox._exceptions import ApiError, TemplateNotFoundError

from framework.build_throttle import template_build_slot
from framework.parallel import scale_timeout_for_xdist
from framework.sandbox_logs import read_cubecli_logs, skip_unless_single_node_cubecli

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.templates,
    pytest.mark.p1,
]

DEFAULT_IMAGE = "cube-sandbox-image.tencentcloudcr.com/opensource/redis:7-alpine"
DEFAULT_WRITABLE_LAYER_SIZE = "1G"
MARKER = "cube-e2e-user"
START_CMD = ["sh"]
START_ARGS = [
    "-c",
    f'while :; do echo "{MARKER} uid=$(id -u) gid=$(id -g)"; sleep 2; done',
]
ID_RE = re.compile(rf"{MARKER} uid=(\d+) gid=(\d+)")


def _image() -> str:
    return os.environ.get("SDK_E2E_TEMPLATE_USER_IMAGE") or DEFAULT_IMAGE


@pytest.fixture
def cubecli(sdk_backend: str) -> tuple[str, str]:
    if sdk_backend != "cubesandbox":
        pytest.skip("template operations only support cubesandbox backend")
    return skip_unless_single_node_cubecli()


def _cfg(sdk_e2e_config) -> Config:
    return Config(api_url=sdk_e2e_config.cube_api_url)


def _wait_for_terminal(template_id: str, cfg: Config):
    deadline = time.time() + scale_timeout_for_xdist(300)
    info = None
    while time.time() < deadline:
        try:
            info = Template.get(template_id, config=cfg)
            if info.status in ("READY", "FAILED"):
                return info
        except TemplateNotFoundError:
            pass
        time.sleep(2)
    pytest.fail(
        f"template {template_id} did not finish; "
        f"last_status={getattr(info, 'status', None)!r}"
    )


def _delete_template(template_id: str, cfg: Config) -> None:
    deadline = time.time() + 180
    while time.time() < deadline:
        try:
            Template.delete(template_id, config=cfg)
            return
        except TemplateNotFoundError:
            return
        except ApiError as exc:
            if "attempt is already in progress" in str(exc):
                time.sleep(5)
                continue
            raise


def _wait_for_ids(
    cubecli: tuple[str, str], target_id: str, *, template: bool
) -> tuple[int, int]:
    binary, address = cubecli
    deadline = time.time() + 60
    logs = ""
    while time.time() < deadline:
        try:
            logs = read_cubecli_logs(
                binary, target_id, address=address, template=template
            )
        except AssertionError:
            logs = ""
        found = ID_RE.findall(logs)
        if found:
            uid, gid = found[-1]
            return int(uid), int(gid)
        time.sleep(2)
    pytest.fail(f"no {MARKER!r} line in cubecli logs of {target_id}: {logs[-500:]!r}")


def _build(cfg: Config, user: str | None):
    with template_build_slot(label=f"template_user_{user or 'default'}"):
        job = Template.build(
            image=_image(),
            writable_layer_size=DEFAULT_WRITABLE_LAYER_SIZE,
            command=START_CMD,
            args=START_ARGS,
            user=user,
            config=cfg,
        )
        return job.template_id, _wait_for_terminal(job.template_id, cfg)


@pytest.mark.parametrize(
    ("user", "expected"),
    [
        pytest.param("redis", (999, 1000), id="username"),
        pytest.param("redis:2000", (999, 2000), id="username-gid"),
        pytest.param("1000:1000", (1000, 1000), id="uid-gid-not-in-passwd"),
        pytest.param(None, (0, 0), id="omitted-stays-root"),
    ],
)
def test_template_user_applies_to_build_and_sandbox(
    cubecli, sdk_e2e_config, user, expected
):
    cfg = _cfg(sdk_e2e_config)
    template_id = None
    sandbox = None
    try:
        template_id, info = _build(cfg, user)
        assert info.status == "READY", f"build failed: {info.last_error!r}"
        assert _wait_for_ids(cubecli, template_id, template=True) == expected

        sandbox = Sandbox.create(template=template_id, timeout=120, config=cfg)
        assert _wait_for_ids(cubecli, sandbox.sandbox_id, template=False) == expected
    finally:
        if sandbox is not None:
            try:
                sandbox.kill()
            except Exception:
                pass
        if template_id:
            _delete_template(template_id, cfg)


def test_template_user_unknown_name_fails_build(cubecli, sdk_e2e_config):
    # A failed first build drops the template record, so follow the build job
    # instead of GET /templates/:id.
    cfg = _cfg(sdk_e2e_config)
    template_id = None
    try:
        with template_build_slot(label="template_user_unknown"):
            job = Template.build(
                image=_image(),
                writable_layer_size=DEFAULT_WRITABLE_LAYER_SIZE,
                command=START_CMD,
                args=START_ARGS,
                user="cube-e2e-no-such-user",
                config=cfg,
            )
            template_id = job.template_id
            deadline = time.time() + scale_timeout_for_xdist(300)
            status = job
            # Build status uses E2B-style lowercase states (ready/error).
            while time.time() < deadline and status.status.lower() not in (
                "ready",
                "error",
                "failed",
            ):
                time.sleep(2)
                status = Template.get_build_status(
                    template_id, job.job_id, config=cfg
                )
        assert status.status.lower() in ("error", "failed"), status
        error = f"{status.error_message} {status.message}"
        assert "not defined in /etc/passwd" in error, error
    finally:
        if template_id:
            _delete_template(template_id, cfg)
