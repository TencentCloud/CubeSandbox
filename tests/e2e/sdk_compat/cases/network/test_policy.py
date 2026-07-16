# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os

import pytest

from framework.assertions import assert_command_ok
from framework.capabilities import NETWORK_ALLOW_DENY, NETWORK_PUBLIC_ACCESS

TCP_TARGET_IP = os.environ.get("SDK_E2E_TCP_TARGET_IP", "8.8.8.8")
TCP_TARGET_PORT = int(os.environ.get("SDK_E2E_TCP_TARGET_PORT", "53"))
ALTERNATE_TCP_TARGET_IP = os.environ.get(
    "SDK_E2E_ALTERNATE_TCP_TARGET_IP",
    "1.1.1.1",
)

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.network,
    pytest.mark.p1,
    pytest.mark.requires_internet,
]


def _tcp_probe_command(
    host: str = TCP_TARGET_IP,
    port: int = TCP_TARGET_PORT,
    timeout: int = 5,
) -> str:
    return (
        "python3 - <<'PY'\n"
        "import socket\n"
        "try:\n"
        "    s = socket.socket()\n"
        f"    s.settimeout({timeout!r})\n"
        f"    rc = s.connect_ex(({host!r}, {port}))\n"
        "    print('OK' if rc == 0 else f'FAIL:{rc}')\n"
        "except Exception as exc:\n"
        "    print(f'ERROR:{type(exc).__name__}:{exc}')\n"
        "finally:\n"
        "    try:\n"
        "        s.close()\n"
        "    except Exception:\n"
        "        pass\n"
        "PY"
    )


def _assert_tcp_reachable(result, target: str) -> None:
    assert_command_ok(result)
    assert result.stdout.strip() == "OK", (
        f"TCP target {target}:{TCP_TARGET_PORT} should be reachable; "
        f"stdout={result.stdout!r} stderr={result.stderr!r}"
    )


def _assert_tcp_blocked(result, target: str) -> None:
    assert_command_ok(result)
    output = result.stdout.strip()
    assert output.startswith("FAIL:"), (
        f"TCP target {target}:{TCP_TARGET_PORT} should be blocked; "
        f"stdout={result.stdout!r} stderr={result.stderr!r}"
    )


@pytest.mark.requires_capability(NETWORK_ALLOW_DENY)
@pytest.mark.sandbox_create_options(
    network={
        "allow_out": [TCP_TARGET_IP],
        "deny_out": ["0.0.0.0/0"],
    },
)
def test_allow_out_can_punch_through_deny_all(sdk_sandbox, sdk_e2e_config):
    result = sdk_sandbox.run_command(
        _tcp_probe_command(timeout=sdk_e2e_config.network_probe_timeout),
        timeout=sdk_e2e_config.command_timeout,
    )

    assert_command_ok(result)
    assert result.stdout.strip() == "OK", (
        f"allow_out did not permit {TCP_TARGET_IP}:{TCP_TARGET_PORT}; "
        f"stdout={result.stdout!r} stderr={result.stderr!r}"
    )


@pytest.mark.requires_capability(NETWORK_ALLOW_DENY)
@pytest.mark.sandbox_create_options(
    network={
        "deny_out": ["0.0.0.0/0"],
    },
)
def test_deny_out_blocks_public_tcp(sdk_sandbox, sdk_e2e_config):
    result = sdk_sandbox.run_command(
        _tcp_probe_command(timeout=sdk_e2e_config.network_probe_timeout),
        timeout=sdk_e2e_config.command_timeout,
    )

    _assert_tcp_blocked(result, TCP_TARGET_IP)


@pytest.mark.requires_capability(NETWORK_PUBLIC_ACCESS)
@pytest.mark.sandbox_create_options(allow_internet_access=False)
def test_allow_internet_access_false_blocks_public_tcp(sdk_sandbox, sdk_e2e_config):
    result = sdk_sandbox.run_command(
        _tcp_probe_command(timeout=sdk_e2e_config.network_probe_timeout),
        timeout=sdk_e2e_config.command_timeout,
    )

    _assert_tcp_blocked(result, TCP_TARGET_IP)


@pytest.mark.requires_capability(NETWORK_ALLOW_DENY)
@pytest.mark.requires_capability(NETWORK_PUBLIC_ACCESS)
@pytest.mark.sandbox_create_options(
    allow_internet_access=False,
    network={"allow_out": [TCP_TARGET_IP]},
)
def test_allow_out_works_when_public_access_is_disabled(sdk_sandbox, sdk_e2e_config):
    allowed = sdk_sandbox.run_command(
        _tcp_probe_command(TCP_TARGET_IP, timeout=sdk_e2e_config.network_probe_timeout),
        timeout=sdk_e2e_config.command_timeout,
    )
    _assert_tcp_reachable(allowed, TCP_TARGET_IP)

    blocked = sdk_sandbox.run_command(
        _tcp_probe_command(
            ALTERNATE_TCP_TARGET_IP,
            timeout=sdk_e2e_config.network_probe_timeout,
        ),
        timeout=sdk_e2e_config.command_timeout,
    )
    _assert_tcp_blocked(blocked, ALTERNATE_TCP_TARGET_IP)


@pytest.mark.requires_capability(NETWORK_ALLOW_DENY)
@pytest.mark.requires_capability(NETWORK_PUBLIC_ACCESS)
@pytest.mark.sandbox_create_options(
    allow_internet_access=True,
    network={"deny_out": [TCP_TARGET_IP]},
)
def test_deny_out_blocks_selected_target_but_preserves_public_access(
    sdk_sandbox,
    sdk_e2e_config,
):
    blocked = sdk_sandbox.run_command(
        _tcp_probe_command(TCP_TARGET_IP, timeout=sdk_e2e_config.network_probe_timeout),
        timeout=sdk_e2e_config.command_timeout,
    )
    _assert_tcp_blocked(blocked, TCP_TARGET_IP)

    allowed = sdk_sandbox.run_command(
        _tcp_probe_command(
            ALTERNATE_TCP_TARGET_IP,
            timeout=sdk_e2e_config.network_probe_timeout,
        ),
        timeout=sdk_e2e_config.command_timeout,
    )
    _assert_tcp_reachable(allowed, ALTERNATE_TCP_TARGET_IP)


@pytest.mark.requires_capability(NETWORK_PUBLIC_ACCESS)
@pytest.mark.sandbox_create_options(allow_internet_access=False)
def test_no_internet_still_allows_internal_commands(sdk_sandbox, sdk_e2e_config):
    result = sdk_sandbox.run_command(
        "printf isolated-execution-ok",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    assert result.stdout == "isolated-execution-ok"
