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

    def test_missing_exit_code_is_failure(self):
        with self.assertRaisesRegex(SystemExit, "no exit code"):
            _common.ensure_success(type("Result", (), {"exit_code": None})(), "test")

    def test_vault_env_drops_proxy_settings(self):
        with patch.dict(os.environ, {"HTTPS_PROXY": "http://proxy.example"}, clear=True):
            self.assertNotIn("HTTPS_PROXY", env_utils.build_claude_code_env(False))

    def test_renderer_flushes_partial_line(self):
        writer = _common.stream_json_render_writer()
        with patch.object(_common, "_render_stream_json_line") as render:
            writer("partial")
            writer.flush()
            render.assert_called_once_with("partial")

    def test_anthropic_base_url_selects_llm_host(self):
        with patch.dict(os.environ, {"ANTHROPIC_BASE_URL": "https://llm.example/v1"}, clear=True):
            self.assertEqual(env_utils.claude_code_llm_host(), "llm.example")


if __name__ == "__main__":
    unittest.main()
