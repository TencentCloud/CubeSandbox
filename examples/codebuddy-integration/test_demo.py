"""Unit tests for demo.py — tests output parsing and env loading logic.

Run with:
    python3 -m unittest test_demo.py
"""

import json
import os
import sys
import unittest
from unittest.mock import patch

# Make demo.py importable
sys.path.insert(0, os.path.dirname(__file__))


class TestParseCodebuddyOutput(unittest.TestCase):
    """Tests for parse_codebuddy_output()."""

    def setUp(self):
        # Import after sys.path is set
        import demo
        self.parse = demo.parse_codebuddy_output

    def test_list_format_extracts_assistant_text(self):
        """codebuddy --output-format json returns a list; extract assistant text."""
        stdout = json.dumps([
            {"type": "message", "role": "user", "content": [{"type": "input_text", "text": "hi"}]},
            {"type": "file-history-snapshot"},
            {"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "HELLO"}]},
            {"type": "result", "session_id": "abc-123", "usage": {"input_tokens": 10, "output_tokens": 5}},
        ])
        result_text, meta = self.parse(stdout)
        self.assertEqual(result_text, "HELLO")
        self.assertEqual(meta.get("session_id"), "abc-123")
        self.assertEqual(meta.get("usage"), {"input_tokens": 10, "output_tokens": 5})

    def test_list_format_finds_last_assistant(self):
        """When multiple assistant messages exist, return the last one."""
        stdout = json.dumps([
            {"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "FIRST"}]},
            {"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "SECOND"}]},
            {"type": "result"},
        ])
        result_text, _ = self.parse(stdout)
        self.assertEqual(result_text, "SECOND")

    def test_list_format_text_type_fallback(self):
        """Some versions use 'text' instead of 'output_text'."""
        stdout = json.dumps([
            {"type": "message", "role": "assistant", "content": [{"type": "text", "text": "FALLBACK"}]},
            {"type": "result"},
        ])
        result_text, _ = self.parse(stdout)
        self.assertEqual(result_text, "FALLBACK")

    def test_dict_format_fallback(self):
        """Fallback: if output is a dict, treat it as meta + result."""
        stdout = json.dumps({"result": "DICT_RESULT", "model": "test-model", "session_id": "s1"})
        result_text, meta = self.parse(stdout)
        self.assertEqual(result_text, "DICT_RESULT")
        self.assertEqual(meta.get("model"), "test-model")

    def test_no_assistant_message_returns_raw(self):
        """If no assistant message found, return raw stdout."""
        stdout = json.dumps([{"type": "message", "role": "user", "content": []}])
        result_text, _ = self.parse(stdout)
        self.assertEqual(result_text, stdout)

    def test_invalid_json_returns_raw(self):
        """Invalid JSON should return raw stdout, not raise."""
        result, meta = self.parse("not json at all")
        self.assertEqual(result, "not json at all")
        self.assertEqual(meta, {})


class TestLoadEnv(unittest.TestCase):
    """Tests for load_env()."""

    def test_missing_required_env_exits(self):
        """Missing required env vars should raise SystemExit."""
        import demo
        with patch.dict(os.environ, {}, clear=True):
            with patch.object(demo, "load_dotenv"):
                with self.assertRaises(SystemExit):
                    demo.load_env()

    def test_missing_e2b_api_url_exits(self):
        """Missing E2B_API_URL (with others present) should raise SystemExit."""
        import demo
        env = {
            "E2B_API_KEY": "e2b_000000",
            "CUBE_TEMPLATE_ID": "tpl-test",
            "CODEBUDDY_API_KEY": "ck_test",
        }
        with patch.dict(os.environ, env, clear=True):
            with patch.object(demo, "load_dotenv"):
                with self.assertRaises(SystemExit):
                    demo.load_env()

    def test_sets_dummy_e2b_api_key(self):
        """E2B_API_KEY should be set to a dummy value if not present."""
        import demo
        env = {
            "E2B_API_URL": "http://localhost:3000",
            "CUBE_TEMPLATE_ID": "tpl-test",
            "CODEBUDDY_API_KEY": "ck_test",
        }
        with patch.dict(os.environ, env, clear=True):
            os.environ.pop("E2B_API_KEY", None)  # ensure not set
            with patch.object(demo, "load_dotenv"):
                demo.load_env()
            self.assertEqual(os.environ["E2B_API_KEY"], "e2b_000000")

    def test_preserves_existing_e2b_api_key(self):
        """If E2B_API_KEY is already set, don't override it."""
        import demo
        env = {
            "E2B_API_URL": "http://localhost:3000",
            "CUBE_TEMPLATE_ID": "tpl-test",
            "CODEBUDDY_API_KEY": "ck_test",
            "E2B_API_KEY": "my_custom_key",
        }
        with patch.dict(os.environ, env, clear=True):
            with patch.object(demo, "load_dotenv"):
                demo.load_env()
            self.assertEqual(os.environ["E2B_API_KEY"], "my_custom_key")


if __name__ == "__main__":
    unittest.main()
