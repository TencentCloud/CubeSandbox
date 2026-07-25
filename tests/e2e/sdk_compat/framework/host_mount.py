# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Helpers for the host-mount extension.

Host mounts are a Cube-specific extension to the E2B API: they are requested
through ``metadata["host-mount"]`` as a JSON-encoded list of descriptors, each
with ``hostPath`` / ``mountPath`` / ``readOnly``. CubeAPI lifts the value onto
the sandbox annotation of the same name; CubeMaster validates ``hostPath``
against the allowed-prefix whitelist (default ``/data/shared/``) and Cubelet
performs the bind-mount before the VM boots.
"""

from __future__ import annotations

import json
import os

import pytest

from adapters import create_adapter
from framework.capabilities import HOST_MOUNT, capabilities_for_backend
from framework.cleanup import safe_kill

HOST_MOUNT_KEY = "host-mount"

# Internal mount point used only for provisioning host subdirectories.
_PROVISION_MOUNT = "/mnt/.host-mount-provision"

# Allowed prefix on the target environment. Must match CubeMaster's
# ``allowed_host_mount_prefixes`` (default ``/data/shared/``). Trailing slash is
# stripped so it can be composed with child paths.
ALLOWED_PREFIX = os.environ.get("SDK_E2E_HOST_MOUNT_PREFIX", "/data/shared").rstrip("/")


def backend_supports_host_mount(backend: str) -> bool:
    """Whether ``backend`` implements the host-mount extension.

    Boundary tests drive ``create_adapter`` directly and never pass through the
    ``sdk_sandbox`` fixture's ``requires_capability`` check, so they consult this
    against the same canonical backend->capability map the fixture uses.
    """
    return HOST_MOUNT in capabilities_for_backend(backend)


def skip_if_host_mount_unavailable(backend: str, config) -> None:
    """Skip the current test unless a real host-mount create is possible.

    Boundary tests drive ``create_adapter`` directly instead of requesting the
    ``sdk_sandbox`` fixture, so they never pass through the fixture's
    ``requires_capability`` / template gates. Apply the same two conditions here
    so those tests skip exactly the cases ``sdk_sandbox`` would.
    """
    if not backend_supports_host_mount(backend):
        pytest.skip(f"backend {backend!r} does not support host-mount")
    if not config.cube_template_id:
        pytest.skip("CUBE_TEMPLATE_ID or --cube-template-id is required for SDK E2E")


def expect_create_rejected(
    backend: str,
    config,
    metadata: dict[str, str],
    *,
    role: str,
) -> str:
    """Attempt a create expected to fail; return the error text.

    If the create unexpectedly succeeds, clean up the sandbox and fail the test
    so it never leaks.
    """
    try:
        adapter = create_adapter(
            backend,
            config,
            metadata={"test_role": role, **metadata},
        )
    except Exception as exc:  # noqa: BLE001 - the rejection is the assertion
        return str(exc)
    safe_kill(adapter, config)
    pytest.fail(f"expected create to be rejected ({role}), but the sandbox was created")
    raise AssertionError("unreachable")  # pytest.fail raises; keeps type checkers happy


def mount_option(host_path: str, mount_path: str, *, read_only: bool = False) -> dict:
    """One entry of the host-mount list.

    Field names match CubeMaster's ``HostDirMountOption`` json tags
    (hostPath / mountPath / readOnly).
    """
    return {"hostPath": host_path, "mountPath": mount_path, "readOnly": read_only}


def host_mount_metadata(options: list[dict]) -> dict[str, str]:
    """Wrap descriptors into the ``metadata`` dict the SDK expects."""
    return {HOST_MOUNT_KEY: json.dumps(options)}


def under_prefix(*parts: str) -> str:
    """Join path components under the allowed prefix."""
    return "/".join([ALLOWED_PREFIX, *(p.strip("/") for p in parts)])


def provision_host_dirs(backend: str, config, subpaths: list[str]) -> None:
    """Create subdirectories under the allowed prefix on the Cubelet host node.

    A host-mount ``hostPath`` must already exist on the node: Cubelet's
    ``prepareHostDirVolume`` bind-mounts it directly and does NOT create it, so
    a missing source dir fails the create at bind-mount time. Only the
    allowed-prefix root itself is guaranteed to exist.

    This helper mounts the prefix root read-write into a throwaway sandbox and
    runs ``mkdir -p`` for each subpath. The directories persist on the host
    after the sandbox is killed, so later tests can mount them.
    """
    rel = [p.strip("/") for p in subpaths if p.strip("/")]
    if not rel:
        return
    metadata = host_mount_metadata(
        [mount_option(ALLOWED_PREFIX, _PROVISION_MOUNT, read_only=False)]
    )
    targets = " ".join(f"{_PROVISION_MOUNT}/{r}" for r in rel)
    adapter = create_adapter(
        backend, config, metadata={"test_role": "host_mount_provision", **metadata}
    )
    try:
        result = adapter.run_command(
            f"mkdir -p {targets}", timeout=config.command_timeout
        )
        if result.exit_code != 0:
            raise RuntimeError(
                f"failed to provision host dirs {rel!r}: "
                f"exit={result.exit_code} stderr={result.stderr!r}"
            )
    finally:
        safe_kill(adapter, config)
