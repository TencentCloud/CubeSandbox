# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

LIFECYCLE = "lifecycle"
COMMANDS = "commands"
FILESYSTEM = "filesystem"
FILESYSTEM_EXTENDED = "filesystem_extended"
RUN_CODE = "run_code"
CODE_INTERPRETER = "code_interpreter"
PAUSE_RESUME = "pause_resume"
SET_TIMEOUT = "set_timeout"
ROLLBACK_CLONE = "rollback_clone"
NETWORK_ALLOW_DENY = "network_allow_deny"
# In-place egress policy replacement on a running sandbox, including
# re-evaluation of already-established connections.
NETWORK_DYNAMIC_UPDATE = "network_dynamic_update"
NETWORK_PUBLIC_ACCESS = "network_public_access"
NETWORK_MASK_REQUEST_HOST = "network_mask_request_host"
NETWORK_L7_CUSTOM_PORT = "network_l7_custom_port"
# CubeVS domain allow_out + DNS A learning (exact / leading "*.").
NETWORK_DNS_ALLOW = "network_dns_allow"
# Built-in deny of sandbox-private / link-local CIDRs when public egress is on.
NETWORK_ALWAYS_DENIED = "network_always_denied"
# CubeEgress L7 rules: inject, first-match, deny, TLS MITM, SNI/host.
NETWORK_L7_EGRESS = "network_l7_egress"
# Template CubeNetworkConfig merged with per-create network options.
NETWORK_TEMPLATE_MERGE = "network_template_merge"
PLATFORM_LIFECYCLE = "platform_lifecycle"
HOST_MOUNT = "host_mount"
VOLUME_PLUGIN = "volume_plugin"
AUTH_SIMPLE_KEY = "auth_simple_key"

COMMON_CAPABILITIES = frozenset(
    {LIFECYCLE, COMMANDS, FILESYSTEM, FILESYSTEM_EXTENDED, RUN_CODE}
)

E2B_CAPABILITIES = frozenset(
    {
        *COMMON_CAPABILITIES,
        CODE_INTERPRETER,
        PAUSE_RESUME,
        SET_TIMEOUT,
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
        SET_TIMEOUT,
        ROLLBACK_CLONE,
        NETWORK_ALLOW_DENY,
        NETWORK_PUBLIC_ACCESS,
        NETWORK_MASK_REQUEST_HOST,
        NETWORK_DNS_ALLOW,
        NETWORK_ALWAYS_DENIED,
        NETWORK_L7_EGRESS,
        NETWORK_TEMPLATE_MERGE,
        NETWORK_DYNAMIC_UPDATE,
        PLATFORM_LIFECYCLE,
        HOST_MOUNT,
        VOLUME_PLUGIN,
        AUTH_SIMPLE_KEY,
        NETWORK_L7_CUSTOM_PORT,
    }
)

# Canonical backend -> capability-set map. Single source of truth for the
# sdk_sandbox fixture gate and helpers that drive create_adapter directly
# (framework.host_mount / cases/volume). Unknown backends resolve to empty.
BACKEND_CAPABILITIES = {
    "cubesandbox": CUBESANDBOX_CAPABILITIES,
    "e2b": E2B_CAPABILITIES,
}


def capabilities_for_backend(backend: str) -> frozenset[str]:
    """Return the capability set a backend supports (empty for unknown)."""
    return BACKEND_CAPABILITIES.get(backend, frozenset())
