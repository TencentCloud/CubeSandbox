# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import pytest

from framework.assertions import assert_code_ok
from framework.capabilities import RUN_CODE

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.run_code,
    pytest.mark.p0,
    pytest.mark.requires_capability(RUN_CODE),
    pytest.mark.requires_code_interpreter,
]


def test_run_code_returns_expression_text(sdk_sandbox, sdk_e2e_config):
    result = sdk_sandbox.run_code("1 + 2", timeout=sdk_e2e_config.run_code_timeout)

    assert_code_ok(result)
    assert result.text == "3"


def test_run_code_captures_stdout(sdk_sandbox, sdk_e2e_config):
    result = sdk_sandbox.run_code(
        "print('hello from python')",
        timeout=sdk_e2e_config.run_code_timeout,
    )

    assert_code_ok(result)
    assert any(line.strip() == "hello from python" for line in result.stdout)


def test_run_code_captures_stderr(sdk_sandbox, sdk_e2e_config):
    result = sdk_sandbox.run_code(
        "import sys\nprint('hello stderr', file=sys.stderr)",
        timeout=sdk_e2e_config.run_code_timeout,
    )

    assert_code_ok(result)
    assert any(line.strip() == "hello stderr" for line in result.stderr)


@pytest.mark.p1
def test_run_code_preserves_kernel_state(sdk_sandbox, sdk_e2e_config):
    first = sdk_sandbox.run_code(
        "sdk_compat_value = 41",
        timeout=sdk_e2e_config.run_code_timeout,
    )
    second = sdk_sandbox.run_code(
        "sdk_compat_value + 1",
        timeout=sdk_e2e_config.run_code_timeout,
    )

    assert_code_ok(first)
    assert_code_ok(second)
    assert second.text == "42"


@pytest.mark.p1
def test_run_code_reports_python_errors(sdk_sandbox, sdk_e2e_config):
    result = sdk_sandbox.run_code(
        "raise ValueError('sdk compat boom')",
        timeout=sdk_e2e_config.run_code_timeout,
    )

    assert result.error is not None


@pytest.mark.p1
def test_run_code_reports_syntax_errors(sdk_sandbox, sdk_e2e_config):
    result = sdk_sandbox.run_code(
        "def broken(:\n    pass",
        timeout=sdk_e2e_config.run_code_timeout,
    )

    assert result.error is not None
