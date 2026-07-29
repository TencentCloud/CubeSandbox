# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Unit tests for common.py — no external dependencies required."""

import io
import os
import sys

# Add current dir to path for import
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from common import (
    cc_command,
    cc_llm_host,
    shell_join,
    _render_json_result,
    _render_stream_json_line,
)


def _capture(fn, *args):
    buf = io.StringIO()
    old = sys.stdout
    sys.stdout = buf
    try:
        fn(*args)
    finally:
        sys.stdout = old
    return buf.getvalue()


def test_cc_command_basic():
    cmd = cc_command("hello")
    assert "claude" in cmd
    assert "-p" in cmd
    assert "hello" in cmd
    assert "--output-format json" in cmd


def test_cc_command_stream_json():
    cmd = cc_command("x", output_format="stream-json")
    assert "--output-format stream-json" in cmd
    assert "--verbose" in cmd


def test_cc_command_effort():
    assert "--effort high" in cc_command("x", effort="high")


def test_cc_command_permission():
    assert "--permission-mode auto" in cc_command("x", permission_mode="auto")


def test_cc_command_dangerously_skip():
    assert "--dangerously-skip-permissions" in cc_command("x", dangerously_skip_permissions=True)


def test_cc_llm_host_default():
    saved = {k: os.environ.pop(k, None) for k in ["CC_LLM_HOST", "ANTHROPIC_BASE_URL"]}
    try:
        assert cc_llm_host() == "api.anthropic.com"
    finally:
        for k, v in saved.items():
            if v is not None:
                os.environ[k] = v


def test_shell_join():
    assert shell_join("a", "b") == "a && b"
    assert shell_join("a", "", "c") == "a && c"
    assert shell_join() == ""


def test_render_json_result():
    out = _capture(_render_json_result, {
        "type": "result", "result": "OK", "is_error": False,
        "total_cost_usd": 0.01, "usage": {"input_tokens": 10, "output_tokens": 5},
    })
    assert "OK" in out
    assert "cost:" in out


def test_render_json_result_error():
    out = _capture(_render_json_result, {
        "type": "result", "result": "fail", "is_error": True,
        "total_cost_usd": 0, "usage": {},
    })
    assert "error" in out.lower()


def test_render_stream_assistant():
    out = _capture(_render_stream_json_line, '{"type":"assistant","message":{"content":"hi"}}')
    assert "hi" in out


def test_render_stream_tool_use():
    line = '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}'
    out = _capture(_render_stream_json_line, line)
    assert "[tool]" in out
    assert "Bash" in out


def test_render_stream_init():
    line = '{"type":"system","subtype":"init","model":"claude-sonnet-4-6","claude_code_version":"2.1.220"}'
    out = _capture(_render_stream_json_line, line)
    assert "claude-sonnet-4-6" in out


if __name__ == "__main__":
    tests = [v for k, v in sorted(globals().items()) if k.startswith("test_") and callable(v)]
    failures = 0
    for fn in tests:
        try:
            fn()
            print(f"  PASS  {fn.__name__}")
        except Exception as e:
            print(f"  FAIL  {fn.__name__}: {e}")
            failures += 1
    print(f"\n{len(tests) - failures}/{len(tests)} passed")
    sys.exit(1 if failures else 0)
