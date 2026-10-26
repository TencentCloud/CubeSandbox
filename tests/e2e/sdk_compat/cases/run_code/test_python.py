# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import json

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


@pytest.mark.sandbox_create_options(
    env_vars={
        "SDK_COMPAT_RUN_CODE_BASE": "sandbox-base",
        "SDK_COMPAT_RUN_CODE_OVERRIDE": "sandbox-value",
    }
)
@pytest.mark.requires_run_code_env_inheritance
def test_run_code_merges_create_and_per_call_envs(sdk_sandbox, sdk_e2e_config):
    code = (
        "import json, os\n"
        "if os.environ.get('SDK_COMPAT_RUN_CODE_DELETE_DURING_CALL'):\n"
        "    del os.environ['SDK_COMPAT_RUN_CODE_DELETE_DURING_CALL']\n"
        "print(json.dumps({"
        "'base': os.environ.get('SDK_COMPAT_RUN_CODE_BASE'), "
        "'override': os.environ.get('SDK_COMPAT_RUN_CODE_OVERRIDE'), "
        "'per_call_only': os.environ.get('SDK_COMPAT_RUN_CODE_PER_CALL_ONLY'), "
        "'kernel_existing': os.environ.get('SDK_COMPAT_RUN_CODE_KERNEL_EXISTING'), "
        "'delete_during_call': os.environ.get('SDK_COMPAT_RUN_CODE_DELETE_DURING_CALL'), "
        "'leak_candidate': os.environ.get('SDK_COMPAT_RUN_CODE_LEAK_CANDIDATE')"
        "}, sort_keys=True))"
    )

    inherited = sdk_sandbox.run_code(
        code,
        timeout=sdk_e2e_config.run_code_timeout,
    )
    assert_code_ok(inherited)
    inherited_env = json.loads("".join(inherited.stdout))
    assert inherited_env == {
        "base": "sandbox-base",
        "delete_during_call": None,
        "kernel_existing": None,
        "leak_candidate": None,
        "override": "sandbox-value",
        "per_call_only": None,
    }, f"unexpected inherited run_code env: {inherited_env!r}"

    seeded = sdk_sandbox.run_code(
        "import os; os.environ['SDK_COMPAT_RUN_CODE_KERNEL_EXISTING'] = 'kernel-value'",
        timeout=sdk_e2e_config.run_code_timeout,
    )
    assert_code_ok(seeded)

    overridden = sdk_sandbox.run_code(
        code,
        env_vars={
            "SDK_COMPAT_RUN_CODE_DELETE_DURING_CALL": "delete-me",
            "SDK_COMPAT_RUN_CODE_LEAK_CANDIDATE": "must-not-leak",
            "SDK_COMPAT_RUN_CODE_OVERRIDE": "per-call-value",
            "SDK_COMPAT_RUN_CODE_PER_CALL_ONLY": "per-call-only",
            "SDK_COMPAT_RUN_CODE_KERNEL_EXISTING": "per-call-kernel-value",
        },
        timeout=sdk_e2e_config.run_code_timeout,
    )
    assert_code_ok(overridden)
    overridden_env = json.loads("".join(overridden.stdout))
    assert overridden_env == {
        "base": "sandbox-base",
        "delete_during_call": None,
        "kernel_existing": "per-call-kernel-value",
        "leak_candidate": "must-not-leak",
        "override": "per-call-value",
        "per_call_only": "per-call-only",
    }, f"unexpected per-call run_code env: {overridden_env!r}"

    restored = sdk_sandbox.run_code(
        code,
        timeout=sdk_e2e_config.run_code_timeout,
    )
    assert_code_ok(restored)
    restored_env = json.loads("".join(restored.stdout))
    assert restored_env == {
        "base": "sandbox-base",
        "delete_during_call": None,
        "kernel_existing": "kernel-value",
        "leak_candidate": None,
        "override": "sandbox-value",
        "per_call_only": None,
    }, f"unexpected restored run_code env: {restored_env!r}"


@pytest.mark.sandbox_create_options(
    env_vars={"SDK_COMPAT_RUN_CODE_FIRST_EXECUTION": "sandbox-value"}
)
@pytest.mark.requires_run_code_env_inheritance
def test_run_code_applies_create_env_before_invalid_first_code(
    sdk_sandbox,
    sdk_e2e_config,
):
    invalid = sdk_sandbox.run_code(
        "if",
        timeout=sdk_e2e_config.run_code_timeout,
    )
    assert invalid.error is not None, "expected the first execution to fail syntax"

    inherited = sdk_sandbox.run_code(
        "import os; print(os.environ.get('SDK_COMPAT_RUN_CODE_FIRST_EXECUTION'))",
        timeout=sdk_e2e_config.run_code_timeout,
    )
    assert_code_ok(inherited)
    assert "".join(inherited.stdout).strip() == "sandbox-value"


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
