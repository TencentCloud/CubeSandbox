# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import contextlib
import io
import sys
import unittest
from pathlib import Path

EXAMPLE = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(EXAMPLE))

import env_utils


class DirectModeWarningTests(unittest.TestCase):
    def test_runtime_warning_names_risk_and_safer_entrypoint(self) -> None:
        stream = io.StringIO()
        with contextlib.redirect_stderr(stream):
            env_utils.warn_direct_mode()
        warning = stream.getvalue()
        self.assertIn("[security]", warning)
        self.assertIn("open", warning)
        self.assertIn("network_policy.py", warning)
        self.assertNotIn("Bearer", warning)


if __name__ == "__main__":
    unittest.main()
