# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import os
import unittest
from unittest.mock import patch

import _common
import env_utils


class HelperTests(unittest.TestCase):
    def test_string_zero_exit_code_is_success(self):
        _common.ensure_success(type("Result", (), {"exit_code": "0"})(), "test")

    def test_provider_is_case_insensitive(self):
        with patch.dict(os.environ, {"CODEBUDDY_PROVIDER": "OpenAI"}, clear=True):
            self.assertEqual(env_utils.codebuddy_provider(), "openai")
            self.assertEqual(env_utils.provider_key_name(), "OPENAI_API_KEY")


if __name__ == "__main__":
    unittest.main()
