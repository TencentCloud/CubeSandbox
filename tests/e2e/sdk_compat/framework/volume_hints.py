# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Shared volume-plugin skip/hint strings (no ApiClient dependency)."""

from __future__ import annotations

VOLUME_MOUNT_PATH = "/mnt/vol-data"
# COS reference plugin install / config guide (manual deploy).
VOLUME_PLUGIN_GUIDE_URL = (
    "https://github.com/TencentCloud/CubeSandbox/blob/master/examples/volume/cos/README.md"
)
VOLUME_PLUGIN_SKIP_REASON = (
    "volume plugin tests require SDK_E2E_VOLUME_PLUGIN=true "
    "(CubeAPI + CubeMaster + Cubelet + a configured volume plugin). "
    f"See {VOLUME_PLUGIN_GUIDE_URL}"
)


def volume_plugin_misconfigured_hint(detail: str = "") -> str:
    """Hint appended when Volume API failures look like a missing/misconfigured plugin."""
    suffix = f": {detail}" if detail else ""
    return (
        f"Volume plugin may be missing or misconfigured{suffix}. "
        f"Deploy and configure the plugin first: {VOLUME_PLUGIN_GUIDE_URL}"
    )
