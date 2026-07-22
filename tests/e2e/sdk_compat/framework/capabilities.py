# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

LIFECYCLE = "lifecycle"
COMMANDS = "commands"
FILESYSTEM = "filesystem"
RUN_CODE = "run_code"
CODE_INTERPRETER = "code_interpreter"
PAUSE_RESUME = "pause_resume"
NETWORK_ALLOW_DENY = "network_allow_deny"
NETWORK_PUBLIC_ACCESS = "network_public_access"
NETWORK_MASK_REQUEST_HOST = "network_mask_request_host"
PLATFORM_LIFECYCLE = "platform_lifecycle"
HOST_MOUNT = "host_mount"

COMMON_CAPABILITIES = frozenset({LIFECYCLE, COMMANDS, FILESYSTEM, RUN_CODE})

E2B_CAPABILITIES = frozenset(
    {
        *COMMON_CAPABILITIES,
        CODE_INTERPRETER,
        PAUSE_RESUME,
        NETWORK_ALLOW_DENY,
        NETWORK_PUBLIC_ACCESS,
        NETWORK_MASK_REQUEST_HOST,
    }
)

CUBESANDBOX_CAPABILITIES = frozenset(
    {
        *COMMON_CAPABILITIES,
        CODE_INTERPRETER,
        PAUSE_RESUME,
        NETWORK_ALLOW_DENY,
        NETWORK_PUBLIC_ACCESS,
        NETWORK_MASK_REQUEST_HOST,
        PLATFORM_LIFECYCLE,
        HOST_MOUNT,
    }
)

# Canonical backend -> capability-set map. Single source of truth for both the
# sdk_sandbox fixture gate (conftest._capabilities_for_backend) and the helpers
# that drive create_adapter directly (framework.host_mount). Unknown backends
# resolve to the empty set.
BACKEND_CAPABILITIES = {
    "cubesandbox": CUBESANDBOX_CAPABILITIES,
    "e2b": E2B_CAPABILITIES,
}


def capabilities_for_backend(backend: str) -> frozenset[str]:
    """Return the capability set a backend supports (empty for unknown)."""
    return BACKEND_CAPABILITIES.get(backend, frozenset())
