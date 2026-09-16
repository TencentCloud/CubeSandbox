# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Regression tests for collection-time CubeSandbox SDK imports."""

from __future__ import annotations

import os
import shutil
import subprocess
import sys
from pathlib import Path

import pytest

pytestmark = pytest.mark.framework

REPO_ROOT = Path(__file__).resolve().parents[5]
SDK_COMPAT_ROOT = Path(__file__).resolve().parents[2]
DEFAULT_SDK_PATH = REPO_ROOT / "sdk" / "python"
DEFAULT_COLLECTION_TARGET = (
    SDK_COMPAT_ROOT / "cases" / "lifecycle" / "test_resume_timeout.py"
)
OVERRIDE_COLLECTION_TARGET = SDK_COMPAT_ROOT / "cases" / "templates" / "test_alias.py"

_COLLECTION_CHECK = """
from pathlib import Path
import sys

import pytest

expected_sdk_path = Path(sys.argv[1]).resolve()
target = sys.argv[2]
exit_code = pytest.main(["--collect-only", "-q", "-p", "no:cacheprovider", target])
if exit_code != pytest.ExitCode.OK:
    raise SystemExit(exit_code)

resolved_import_paths = {Path(entry).resolve() for entry in sys.path if entry}
if expected_sdk_path not in resolved_import_paths:
    raise SystemExit(f"collection did not add {expected_sdk_path} to sys.path")

import cubesandbox

sdk_module = Path(cubesandbox.__file__).resolve()
if not sdk_module.is_relative_to(expected_sdk_path):
    raise SystemExit(
        f"collected with cubesandbox from {sdk_module}, expected it under {expected_sdk_path}"
    )
"""


def _collect_with_sdk(
    expected_sdk_path: Path,
    configured_sdk_path: Path | None,
    collection_target: Path,
) -> None:
    env = os.environ.copy()
    env.pop("PYTHONPATH", None)
    if configured_sdk_path is None:
        # Keep a local .env from overriding the repository default under test.
        env["CUBE_PYTHON_SDK_PATH"] = ""
    else:
        env["CUBE_PYTHON_SDK_PATH"] = str(configured_sdk_path)

    result = subprocess.run(
        [
            sys.executable,
            "-c",
            _COLLECTION_CHECK,
            str(expected_sdk_path),
            str(collection_target),
        ],
        cwd=SDK_COMPAT_ROOT,
        env=env,
        capture_output=True,
        text=True,
        check=False,
    )
    assert result.returncode == 0, result.stdout + result.stderr


def test_collection_uses_repository_sdk_by_default():
    _collect_with_sdk(DEFAULT_SDK_PATH, None, DEFAULT_COLLECTION_TARGET)


def test_collection_honors_sdk_path_override(tmp_path):
    override_sdk_path = tmp_path / "python-sdk"
    shutil.copytree(DEFAULT_SDK_PATH / "cubesandbox", override_sdk_path / "cubesandbox")

    _collect_with_sdk(
        override_sdk_path,
        override_sdk_path,
        OVERRIDE_COLLECTION_TARGET,
    )


@pytest.mark.parametrize("package_exists", [False, True])
def test_collection_reports_invalid_sdk_path_override(tmp_path, package_exists):
    invalid_sdk_path = tmp_path / "invalid-sdk"
    if package_exists:
        (invalid_sdk_path / "cubesandbox").mkdir(parents=True)
    env = os.environ.copy()
    env.pop("PYTHONPATH", None)
    env["CUBE_PYTHON_SDK_PATH"] = str(invalid_sdk_path)

    result = subprocess.run(
        [
            sys.executable,
            "-m",
            "pytest",
            "--collect-only",
            "-q",
            "-p",
            "no:cacheprovider",
            str(DEFAULT_COLLECTION_TARGET),
        ],
        cwd=SDK_COMPAT_ROOT,
        env=env,
        capture_output=True,
        text=True,
        check=False,
    )

    assert result.returncode != 0
    expected_package = invalid_sdk_path / "cubesandbox" / "__init__.py"
    assert (
        f"CUBE_PYTHON_SDK_PATH ({invalid_sdk_path}) is invalid: expected {expected_package}"
        in result.stdout + result.stderr
    )
