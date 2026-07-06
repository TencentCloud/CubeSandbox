# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Shared pytest fixtures for the sandbox-backend example tests."""

from __future__ import annotations

import sys
from pathlib import Path

import pytest

EXAMPLE_DIR = Path(__file__).resolve().parent
if str(EXAMPLE_DIR) not in sys.path:
    sys.path.insert(0, str(EXAMPLE_DIR))


@pytest.fixture
def tmp_state_dir(tmp_path, monkeypatch):
    """Provide an isolated temp directory for cubesandbox_exec state files."""
    state_dir = tmp_path / "hook-state"
    monkeypatch.setenv("CUBE_HOOK_STATE_DIR", str(state_dir))
    return state_dir
