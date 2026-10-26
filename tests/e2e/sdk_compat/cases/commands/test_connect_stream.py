# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""E2E regression tests for the Connect stream gzip fix.

Without ``Accept-Encoding: identity``, nginx gzip-compresses the Connect
stream response (gzip_min_length=1000).  ``httpx.iter_raw()`` does not
decompress Content-Encoding, so gzip magic bytes (1f 8b …) are misread as
a Connect frame header whose size field decodes to 2.17 GiB, triggering::

    RuntimeError: Connect stream message too large: 2332557312 bytes

The fix sets ``Accept-Encoding: identity`` on every httpx client built by
``build_client()``, preventing nginx from applying transparent compression.
"""

from __future__ import annotations

import pytest

from framework.assertions import assert_command_ok
from framework.capabilities import COMMANDS

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.commands,
    pytest.mark.p0,
    pytest.mark.requires_capability(COMMANDS),
]


# nginx gzip_min_length is 1000 bytes; produce output well above that to
# reliably trigger compression when Accept-Encoding is missing.
LARGE_STDOUT_SIZE = 2400


def test_large_stdout_no_gzip_error(sdk_sandbox, sdk_e2e_config):
    """>1000-byte stdout must succeed without 'message too large' crash.

    This is the core regression case: nginx ``gzip_min_length 1000`` would
    compress the response, and ``iter_raw()`` cannot decompress gzip.
    """
    padding = "x" * LARGE_STDOUT_SIZE
    result = sdk_sandbox.run_command(
        f"printf '%s' '{padding}'",
        timeout=sdk_e2e_config.command_timeout,
    )

    assert_command_ok(result)
    assert len(result.stdout) == LARGE_STDOUT_SIZE
    assert result.stdout == padding


def test_large_stderr_only(sdk_sandbox, sdk_e2e_config):
    """>1000-byte stderr alone must survive without crash."""
    result = sdk_sandbox.run_command(
        f"python3 -c \"import sys; sys.stderr.write('E' * {LARGE_STDOUT_SIZE})\"",
        timeout=sdk_e2e_config.command_timeout,
    )

    assert_command_ok(result)
    assert result.stdout == ""
    assert len(result.stderr) == LARGE_STDOUT_SIZE
    assert result.stderr == "E" * LARGE_STDOUT_SIZE


def test_mixed_stdout_stderr_large(sdk_sandbox, sdk_e2e_config):
    """Concurrent stdout and stderr, each >1000 bytes, must both arrive intact."""
    result = sdk_sandbox.run_command(
        f"python3 -c \"import sys; sys.stdout.write('A' * {LARGE_STDOUT_SIZE}); sys.stderr.write('B' * {LARGE_STDOUT_SIZE})\"",
        timeout=sdk_e2e_config.command_timeout,
    )

    assert_command_ok(result)
    assert len(result.stdout) == LARGE_STDOUT_SIZE
    assert len(result.stderr) == LARGE_STDOUT_SIZE
    assert result.stdout == "A" * LARGE_STDOUT_SIZE
    assert result.stderr == "B" * LARGE_STDOUT_SIZE


def test_large_output_exit_nonzero(sdk_sandbox, sdk_e2e_config):
    """Large stdout + non-zero exit code: exit code and output must be correct."""
    padding = "Z" * LARGE_STDOUT_SIZE
    result = sdk_sandbox.run_command(
        f"printf '%s' '{padding}'; exit 42",
        timeout=sdk_e2e_config.command_timeout,
    )

    assert result.exit_code == 42
    assert result.stdout == padding


def test_repeated_large_commands(sdk_sandbox, sdk_e2e_config):
    """Multiple consecutive large-output commands must all succeed.

    Verifies the fix is persistent across several Connect-stream round-trips
    within the same sandbox lifecycle.
    """
    for i in range(5):
        marker = chr(ord("a") + i)
        result = sdk_sandbox.run_command(
            f"printf '%s' '{marker * LARGE_STDOUT_SIZE}'",
            timeout=sdk_e2e_config.command_timeout,
        )
        assert_command_ok(result)
        assert result.stdout == marker * LARGE_STDOUT_SIZE, f"iteration {i} failed"
