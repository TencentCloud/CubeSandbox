# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Host-mount happy-path tests, equivalent to examples/host-mount.

Mirrors examples/host-mount/create_with_mount.py: create a sandbox with a
read-write and a read-only host directory mounted, verify both are visible
inside the VM, that the read-only mount rejects writes, and that a hostPath
outside the allowed prefix is rejected at create time (README section 1).

A host-mount ``hostPath`` must already exist on the Cubelet node: Cubelet
bind-mounts it directly and does not create it. Only the allowed-prefix root is
guaranteed to exist, so the ``_provision_host_dirs`` autouse fixture creates the
``rw`` and ``ro`` subdirectories before the mount sandbox is created.
"""

from __future__ import annotations

import pytest

from framework.assertions import assert_command_ok
from framework.capabilities import HOST_MOUNT
from framework.host_mount import (
    expect_create_rejected,
    host_mount_metadata,
    mount_option,
    provision_host_dirs,
    skip_if_host_mount_unavailable,
    under_prefix,
)

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.host_mount,
    pytest.mark.p1,
    pytest.mark.requires_capability(HOST_MOUNT),
]

_RW_MOUNT = "/mnt/rw"
_RO_MOUNT = "/mnt/ro"

# Equivalent to the example's metadata: one RW mount and one RO mount.
_RW_RO_METADATA = host_mount_metadata(
    [
        mount_option(under_prefix("rw"), _RW_MOUNT, read_only=False),
        mount_option(under_prefix("ro"), _RO_MOUNT, read_only=True),
    ]
)


# Backends whose rw/ro host dirs have already been provisioned this session.
# The dirs are created idempotently and persist on the host after the throwaway
# provisioning sandbox is killed, so one provisioning boot per backend suffices
# for the whole module instead of one per test.
_provisioned_backends: set[str] = set()


@pytest.fixture(autouse=True)
def _provision_host_dirs(sdk_backend, sdk_e2e_config):
    """Ensure the rw/ro host dirs exist before the mount sandbox is created.

    Autouse function-scoped fixtures run before an explicitly requested fixture
    such as ``sdk_sandbox``, so the directories are in place by the time the
    marker-driven create happens. Delegates the skip decision to the shared
    ``skip_if_host_mount_unavailable`` helper (the same two conditions
    ``sdk_sandbox`` applies), so a third gating condition added there is picked
    up here automatically instead of drifting. Provisioning is memoized per
    backend: only the first test in the module pays the throwaway-sandbox boot;
    the rest reuse the persisted host dirs.
    """
    skip_if_host_mount_unavailable(sdk_backend, sdk_e2e_config)
    if sdk_backend in _provisioned_backends:
        return
    provision_host_dirs(sdk_backend, sdk_e2e_config, ["rw", "ro"])
    _provisioned_backends.add(sdk_backend)


@pytest.mark.sandbox_create_options(metadata=_RW_RO_METADATA)
def test_rw_and_ro_mounts_visible(sdk_sandbox, sdk_e2e_config):
    """Both mounts appear inside the sandbox (example's `ls /mnt/rw /mnt/ro`)."""
    result = sdk_sandbox.run_command(
        f"test -d {_RW_MOUNT} && test -d {_RO_MOUNT} && echo both-present",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    assert "both-present" in result.stdout, (
        f"expected both {_RW_MOUNT} and {_RO_MOUNT} to be mounted directories; "
        f"stdout={result.stdout!r} stderr={result.stderr!r}"
    )


@pytest.mark.sandbox_create_options(metadata=_RW_RO_METADATA)
def test_rw_mount_accepts_writes(sdk_sandbox, sdk_e2e_config):
    """The read-write mount is writable from inside the sandbox."""
    result = sdk_sandbox.run_command(
        f"echo cube > {_RW_MOUNT}/sdk-compat-rw.txt && cat {_RW_MOUNT}/sdk-compat-rw.txt",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    assert "cube" in result.stdout, (
        f"expected write to {_RW_MOUNT} to succeed; "
        f"stdout={result.stdout!r} stderr={result.stderr!r}"
    )


@pytest.mark.sandbox_create_options(metadata=_RW_RO_METADATA)
def test_ro_mount_rejects_writes(sdk_sandbox, sdk_e2e_config):
    """Writes to the read-only mount fail (README: kernel enforces MS_RDONLY)."""
    result = sdk_sandbox.run_command(
        f"echo denied > {_RO_MOUNT}/sdk-compat-ro.txt",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert result.exit_code != 0, (
        f"expected write to read-only mount {_RO_MOUNT} to fail; "
        f"stdout={result.stdout!r} stderr={result.stderr!r}"
    )
    assert "read-only" in result.stderr.lower() or "read only" in result.stderr.lower(), (
        f"expected a read-only filesystem error, got stderr={result.stderr!r}"
    )


def test_disallowed_hostpath_rejected_at_create(sdk_backend, sdk_e2e_config):
    """A hostPath outside the allowed prefix is rejected at create time.

    README section 1: mounting e.g. /etc/passwd raises ApiError with the
    message "not within an allowed mount prefix". Gated by the same host-mount
    availability check ``sdk_sandbox`` would apply, without booting a sandbox we
    never use.
    """
    skip_if_host_mount_unavailable(sdk_backend, sdk_e2e_config)
    metadata = host_mount_metadata([mount_option("/etc/passwd", "/mnt/passwd")])
    message = expect_create_rejected(
        sdk_backend, sdk_e2e_config, metadata, role="host_mount_disallowed"
    )
    assert "is not within an allowed mount prefix" in message.lower(), (
        f"expected an allowed-prefix rejection message, got {message!r}"
    )
