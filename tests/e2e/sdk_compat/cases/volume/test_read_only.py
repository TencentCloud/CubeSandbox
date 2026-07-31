"""Per-sandbox read-only Volume attachment cases.

Prerequisites (manual; not provisioned by this suite):
- Deploy and configure a Volume Plugin on CubeMaster (Controller) and Cubelet
  (Node), e.g. COS binary/rpc under ``volume_plugins``, with credentials.
  Guide: https://github.com/TencentCloud/CubeSandbox/blob/master/examples/volume/cos/README.md
- Platform Volume API available (CubeAPI / CubeMaster / Cubelet >= 0.6.0).
- Python SDK ``cubesandbox`` with ``VolumeMount`` support.
- A READY template (``CUBE_TEMPLATE_ID``) for sandbox create with mounts.
- Opt-in: ``SDK_E2E_VOLUME_PLUGIN=true`` (and usually ``SDK_E2E_VOLUME_DRIVER``).
"""

from __future__ import annotations

import shlex
import sys
import uuid

import pytest
from adapters import create_adapter
from framework.capabilities import VOLUME_PLUGIN
from framework.cleanup import safe_kill
from framework.volume import managed_volume

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.volume,
    pytest.mark.p1,
    pytest.mark.requires_capability(VOLUME_PLUGIN),
]

MOUNT_PATH = "/mnt/readonly-volume"


def test_read_only_volume_attachment_rejects_mutations(
    sdk_backend,
    sdk_e2e_config,
):
    if not sdk_e2e_config.cube_template_id:
        pytest.skip("CUBE_TEMPLATE_ID or --cube-template-id is required for volume bind")

    # Import after sdk_e2e_config prepends the source checkout's Python SDK.
    from cubesandbox import VolumeMount

    seed_path = f"{MOUNT_PATH}/seed.txt"
    renamed_path = f"{MOUNT_PATH}/renamed.txt"
    new_path = f"{MOUNT_PATH}/new.txt"
    sdk_write_path = f"{MOUNT_PATH}/sdk-write.txt"
    concurrent_write_path = f"{MOUNT_PATH}/rw-during-ro.txt"
    payload = f"readonly-e2e-{uuid.uuid4().hex}"

    with managed_volume(sdk_e2e_config) as (volume_id, _api):
        read_write = None
        read_only = None
        try:
            read_write = create_adapter(
                sdk_backend,
                sdk_e2e_config,
                metadata={
                    "test_suite": "sdk_compat",
                    "test_role": "volume_read_write",
                    "test_backend": sdk_backend,
                },
                create_options={
                    "volume_mounts": {MOUNT_PATH: volume_id},
                },
            )
            read_write.write_file(seed_path, payload)
            assert read_write.read_file(seed_path) == payload

            read_only = create_adapter(
                sdk_backend,
                sdk_e2e_config,
                metadata={
                    "test_suite": "sdk_compat",
                    "test_role": "volume_read_only",
                    "test_backend": sdk_backend,
                },
                create_options={
                    "volume_mounts": {
                        MOUNT_PATH: VolumeMount(volume_id, read_only=True),
                    },
                },
            )
            assert read_only.read_file(seed_path) == payload

            with pytest.raises(IOError):
                read_only.write_file(sdk_write_path, "blocked")

            mutations = {
                "create": f"touch {shlex.quote(new_path)}",
                "write": f"printf changed > {shlex.quote(seed_path)}",
                "rename": f"mv {shlex.quote(seed_path)} {shlex.quote(renamed_path)}",
                "delete": f"rm {shlex.quote(seed_path)}",
            }
            for operation, command in mutations.items():
                result = read_only.run_command(command)
                assert result.exit_code != 0, (
                    f"{operation} unexpectedly succeeded on read-only volume attachment"
                )

            assert read_only.read_file(seed_path) == payload

            read_write.write_file(concurrent_write_path, "writable")
            assert read_write.read_file(concurrent_write_path) == "writable"
        finally:
            active_failure = sys.exc_info()[0] is not None
            cleanup_errors: list[str] = []
            for adapter in (read_only, read_write):
                if adapter is not None:
                    cleanup_errors.extend(safe_kill(adapter, sdk_e2e_config))
            if cleanup_errors and not active_failure:
                pytest.fail("sandbox cleanup failed: " + "; ".join(cleanup_errors))
