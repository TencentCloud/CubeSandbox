# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Pytest configuration — add the example directory to sys.path so tests can
import the integration modules (``env_utils``, ``_opencode_common``,
``sandbox_exec``, ``mcp_server``) without packaging them.
"""

from __future__ import annotations

import os
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))