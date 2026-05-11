# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
cubesandbox SDK unit tests.

All HTTP calls are intercepted via httpx.MockTransport / unittest.mock.patch
so no real network is needed.
"""

from __future__ import annotations

import json
from unittest.mock import MagicMock, patch

import httpx
import pytest

from cubesandbox import CommandResult
from cubesandbox._commands import Commands
from cubesandbox._config import Config
from cubesandbox._exceptions import (
    ApiError,
    AuthenticationError,
    CubeSandboxError,
    SandboxNotFoundError,
    TemplateNotFoundError,
)
from cubesandbox._filesystem import Filesystem
from cubesandbox._models import Execution, ExecutionError, Logs, OutputMessage, Result
from cubesandbox._stream import _parse_line
from cubesandbox.sandbox import Sandbox

# ── helpers ───────────────────────────────────────────────────────────────────

SANDBOX_ID = "sb-test-001"
DOMAIN = "cube.app"
SANDBOX_DATA = {
    "sandboxID": SANDBOX_ID,
    "templateID": "tpl-test",
    "domain": DOMAIN,
    "state": "running",
    "cpuCount": 2,
    "memoryMB": 512,
}


def make_config(**kwargs) -> Config:
    defaults = dict(api_url="http://localhost:3000", template_id="tpl-test")
    defaults.update(kwargs)
    return Config(**defaults)


def mock_response(body=None, status: int = 200) -> httpx.Response:
    """Build a fake httpx.Response."""
    if body is None:
        content = b""
    elif isinstance(body, (dict, list)):
        content = json.dumps(body).encode()
    else:
        content = str(body).encode()
    return httpx.Response(status_code=status, content=content)


def make_sandbox(**data_overrides) -> Sandbox:
    d = {**SANDBOX_DATA, **data_overrides}
    return Sandbox(d, config=make_config())


# ── POST /sandboxes ───────────────────────────────────────────────────────────

class TestCreate:
    def test_create_success(self):
        with patch("httpx.Client.post", return_value=mock_response(SANDBOX_DATA, status=201)):
            sb = Sandbox.create(config=make_config())
        assert sb.sandbox_id == SANDBOX_ID

    def test_create_missing_template_raises(self):
        cfg = make_config(template_id=None)
        with pytest.raises(ValueError, match="template"):
            Sandbox.create(config=cfg)

    def test_create_sends_template_and_timeout(self):
        with patch("httpx.Client.post", return_value=mock_response(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(template="tpl-foo", timeout=600, config=make_config())
        body = m.call_args.kwargs["json"]
        assert body["templateID"] == "tpl-foo"
        assert body["timeout"] == 600

    def test_create_sends_env_vars(self):
        with patch("httpx.Client.post", return_value=mock_response(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(env_vars={"FOO": "bar"}, config=make_config())
        body = m.call_args.kwargs["json"]
        assert body["envVars"] == {"FOO": "bar"}

    def test_create_sends_metadata(self):
        meta = {"network-policy": "deny-all"}
        with patch("httpx.Client.post", return_value=mock_response(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(metadata=meta, config=make_config())
        body = m.call_args.kwargs["json"]
        assert body["metadata"] == meta

    def test_create_template_not_found(self):
        with patch("httpx.Client.post",
                   return_value=mock_response({"message": "template not found"}, status=404)):
            with pytest.raises(TemplateNotFoundError):
                Sandbox.create(config=make_config())

    def test_create_server_error(self):
        with patch("httpx.Client.post",
                   return_value=mock_response({"message": "internal error"}, status=500)):
            with pytest.raises(ApiError):
                Sandbox.create(config=make_config())

    def test_create_allow_internet_access_false(self):
        with patch("httpx.Client.post", return_value=mock_response(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(allow_internet_access=False, config=make_config())
        body = m.call_args.kwargs["json"]
        assert body["allowInternetAccess"] is False

    def test_create_allow_internet_access_true_not_in_payload(self):
        with patch("httpx.Client.post", return_value=mock_response(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(config=make_config())
        body = m.call_args.kwargs["json"]
        assert "allowInternetAccess" not in body

    def test_create_network_allow_out(self):
        with patch("httpx.Client.post", return_value=mock_response(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(network={"allow_out": ["8.8.8.8/32"]}, config=make_config())
        body = m.call_args.kwargs["json"]
        assert body["network"]["allowOut"] == ["8.8.8.8/32"]

    def test_create_network_deny_out(self):
        with patch("httpx.Client.post", return_value=mock_response(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(network={"deny_out": ["0.0.0.0/0"]}, config=make_config())
        body = m.call_args.kwargs["json"]
        assert body["network"]["denyOut"] == ["0.0.0.0/0"]

    def test_create_network_empty_not_in_payload(self):
        with patch("httpx.Client.post", return_value=mock_response(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(network={}, config=make_config())
        body = m.call_args.kwargs["json"]
        assert "network" not in body


# ── POST /sandboxes/:id/connect ───────────────────────────────────────────────

class TestConnect:
    def test_connect_success(self):
        with patch("httpx.Client.post", return_value=mock_response(SANDBOX_DATA)):
            sb = Sandbox.connect(SANDBOX_ID, config=make_config())
        assert sb.sandbox_id == SANDBOX_ID

    def test_connect_not_found(self):
        with patch("httpx.Client.post",
                   return_value=mock_response({"message": "not found"}, status=404)):
            with pytest.raises(SandboxNotFoundError):
                Sandbox.connect(SANDBOX_ID, config=make_config())

    def test_connect_sends_timeout(self):
        cfg = make_config()
        cfg.timeout = 600
        with patch("httpx.Client.post", return_value=mock_response(SANDBOX_DATA)) as m:
            Sandbox.connect(SANDBOX_ID, config=cfg)
        body = m.call_args.kwargs["json"]
        assert body["timeout"] == 600


# ── GET /sandboxes ────────────────────────────────────────────────────────────

class TestListSandboxesV1:
    def test_list_returns_list(self):
        data = [SANDBOX_DATA]
        with patch("httpx.Client.get", return_value=mock_response(data)):
            result = Sandbox.list(config=make_config())
        assert result == data

    def test_list_empty(self):
        with patch("httpx.Client.get", return_value=mock_response([])):
            result = Sandbox.list(config=make_config())
        assert result == []

    def test_list_calls_correct_endpoint(self):
        with patch("httpx.Client.get", return_value=mock_response([])) as m:
            Sandbox.list(config=make_config())
        assert "/sandboxes" in str(m.call_args)

    def test_list_server_error(self):
        with patch("httpx.Client.get",
                   return_value=mock_response({"message": "error"}, status=500)):
            with pytest.raises(ApiError):
                Sandbox.list(config=make_config())


# ── GET /v2/sandboxes ─────────────────────────────────────────────────────────

class TestListSandboxesV2:
    def test_list_v2_returns_list(self):
        data = [SANDBOX_DATA]
        with patch("httpx.Client.get", return_value=mock_response(data)):
            result = Sandbox.list_v2(config=make_config())
        assert result == data

    def test_list_v2_calls_correct_endpoint(self):
        with patch("httpx.Client.get", return_value=mock_response([])) as m:
            Sandbox.list_v2(config=make_config())
        assert "/v2/sandboxes" in str(m.call_args)


# ── GET /health ───────────────────────────────────────────────────────────────

class TestHealth:
    def test_health_ok(self):
        with patch("httpx.Client.get",
                   return_value=mock_response({"status": "ok", "sandboxes": 2})):
            result = Sandbox.health(config=make_config())
        assert result["status"] == "ok"
        assert result["sandboxes"] == 2

    def test_health_server_error(self):
        with patch("httpx.Client.get",
                   return_value=mock_response({"message": "error"}, status=500)):
            with pytest.raises(ApiError):
                Sandbox.health(config=make_config())


# ── GET /sandboxes/:id ────────────────────────────────────────────────────────

class TestGetInfo:
    def test_get_info_success(self):
        sb = make_sandbox()
        info = {**SANDBOX_DATA, "state": "paused"}
        with patch.object(sb._session, "get", return_value=mock_response(info)):
            result = sb.get_info()
        assert result["state"] == "paused"

    def test_get_info_not_found(self):
        sb = make_sandbox()
        with patch.object(sb._session, "get",
                          return_value=mock_response({"message": "not found"}, status=404)):
            with pytest.raises(SandboxNotFoundError):
                sb.get_info()


# ── DELETE /sandboxes/:id ─────────────────────────────────────────────────────

class TestKill:
    def test_kill_success(self):
        sb = make_sandbox()
        with patch.object(sb._session, "delete", return_value=mock_response(status=204)) as m:
            sb.kill()
        m.assert_called_once()

    def test_kill_not_found(self):
        sb = make_sandbox()
        with patch.object(sb._session, "delete",
                          return_value=mock_response({"message": "not found"}, status=404)):
            with pytest.raises(SandboxNotFoundError):
                sb.kill()

    def test_context_manager_kills_on_exit(self):
        with patch("httpx.Client.post", return_value=mock_response(SANDBOX_DATA, status=201)):
            sb = Sandbox.create(config=make_config())
        with patch.object(sb._session, "delete", return_value=mock_response(status=204)) as m:
            with sb:
                pass
        m.assert_called_once()

    def test_context_manager_suppresses_kill_error(self):
        with patch("httpx.Client.post", return_value=mock_response(SANDBOX_DATA, status=201)):
            sb = Sandbox.create(config=make_config())
        with patch.object(sb._session, "delete",
                          return_value=mock_response({"message": "gone"}, status=404)):
            with sb:
                pass  # should not raise


# ── POST /sandboxes/:id/pause ─────────────────────────────────────────────────

class TestPause:
    def test_pause_success(self):
        sb = make_sandbox()
        with patch.object(sb._session, "post", return_value=mock_response(status=204)):
            sb.pause(wait=False)

    def test_pause_not_found(self):
        sb = make_sandbox()
        with patch.object(sb._session, "post",
                          return_value=mock_response({"message": "not found"}, status=404)):
            with pytest.raises(SandboxNotFoundError):
                sb.pause(wait=False)

    def test_pause_wait_polls_until_paused(self):
        sb = make_sandbox()
        paused_info = {**SANDBOX_DATA, "state": "paused"}
        with patch.object(sb._session, "post", return_value=mock_response(status=204)), \
             patch.object(sb._session, "get", side_effect=[
                 mock_response({**SANDBOX_DATA, "state": "running"}),
                 mock_response(paused_info),
             ]) as get_m:
            sb.pause(wait=True, interval=0)
        assert get_m.call_count == 2

    def test_pause_wait_timeout(self):
        sb = make_sandbox()
        with patch.object(sb._session, "post", return_value=mock_response(status=204)), \
             patch.object(sb._session, "get",
                          return_value=mock_response({**SANDBOX_DATA, "state": "running"})):
            with pytest.raises(TimeoutError):
                sb.pause(wait=True, timeout=0, interval=0)


# ── POST /sandboxes/:id/resume ────────────────────────────────────────────────

class TestResume:
    def test_resume_success(self):
        sb = make_sandbox()
        with patch.object(sb._session, "post",
                          return_value=mock_response(SANDBOX_DATA, status=201)) as m:
            sb.resume(timeout=120)
        body = m.call_args.kwargs["json"]
        assert body["timeout"] == 120

    def test_resume_default_timeout(self):
        sb = make_sandbox()
        with patch.object(sb._session, "post",
                          return_value=mock_response(SANDBOX_DATA, status=201)) as m:
            sb.resume()
        body = m.call_args.kwargs["json"]
        assert body["timeout"] == 300

    def test_resume_not_found(self):
        sb = make_sandbox()
        with patch.object(sb._session, "post",
                          return_value=mock_response({"message": "not found"}, status=404)):
            with pytest.raises(SandboxNotFoundError):
                sb.resume()


# ── properties / get_host ─────────────────────────────────────────────────────

class TestProperties:
    def test_get_host(self):
        sb = make_sandbox()
        assert sb.get_host(49999) == f"49999-{SANDBOX_ID}.{DOMAIN}"

    def test_get_host_custom_port(self):
        sb = make_sandbox()
        assert sb.get_host(8080) == f"8080-{SANDBOX_ID}.{DOMAIN}"

    def test_domain_fallback_to_config(self):
        sb = Sandbox(
            {**SANDBOX_DATA, "domain": ""},
            config=make_config(sandbox_domain="mycompany.internal"),
        )
        assert sb.domain == "mycompany.internal"

    def test_repr(self):
        sb = make_sandbox()
        assert SANDBOX_ID in repr(sb)
        assert DOMAIN in repr(sb)


# ── Execution model ───────────────────────────────────────────────────────────

class TestExecutionModel:
    def test_text_returns_main_result(self):
        ex = Execution(results=[
            Result(text="side",  is_main_result=False),
            Result(text="42",    is_main_result=True),
        ])
        assert ex.text == "42"

    def test_text_none_when_no_results(self):
        assert Execution().text is None

    def test_text_none_when_no_main(self):
        ex = Execution(results=[Result(text="x", is_main_result=False)])
        assert ex.text is None

    def test_error_captured(self):
        ex = Execution(error=ExecutionError("ZeroDivisionError", "division by zero"))
        assert ex.error.name == "ZeroDivisionError"
        assert ex.text is None

    def test_logs_defaults_empty(self):
        ex = Execution()
        assert ex.logs.stdout == []
        assert ex.logs.stderr == []

    def test_repr_with_text(self):
        ex = Execution(results=[Result(text="99", is_main_result=True)])
        assert "99" in repr(ex)

    def test_repr_with_error(self):
        ex = Execution(error=ExecutionError("ValueError", "bad"))
        assert "ValueError" in repr(ex)


# ── _parse_line (ndjson stream) ───────────────────────────────────────────────

class TestParseStream:
    def test_parses_result(self):
        ex = Execution()
        _parse_line(ex, '{"type":"result","text":"2","is_main_result":true}')
        assert ex.text == "2"

    def test_parses_stdout(self):
        ex = Execution()
        _parse_line(ex, '{"type":"stdout","text":"hello\\n","timestamp":"t1"}')
        assert ex.logs.stdout == ["hello\n"]

    def test_parses_stderr(self):
        ex = Execution()
        _parse_line(ex, '{"type":"stderr","text":"warn\\n","timestamp":"t1"}')
        assert ex.logs.stderr == ["warn\n"]

    def test_parses_error(self):
        ex = Execution()
        _parse_line(ex, '{"type":"error","name":"ValueError","value":"bad","traceback":["l1"]}')
        assert ex.error.name == "ValueError"

    def test_parses_execution_count(self):
        ex = Execution()
        _parse_line(ex, '{"type":"number_of_executions","execution_count":5}')
        assert ex.execution_count == 5

    def test_ignores_bad_json(self):
        ex = Execution()
        _parse_line(ex, "not json at all")
        assert ex.results == []

    def test_ignores_empty_line(self):
        ex = Execution()
        _parse_line(ex, "")
        assert ex.results == []

    def test_ignores_unknown_type(self):
        ex = Execution()
        _parse_line(ex, '{"type":"unknown_event","data":"x"}')
        assert ex.results == []

    def test_stdout_callback(self):
        ex, calls = Execution(), []
        _parse_line(ex, '{"type":"stdout","text":"hi\\n"}',
                    on_stdout=lambda m: calls.append(m.text))
        assert calls == ["hi\n"]

    def test_result_callback(self):
        ex, calls = Execution(), []
        _parse_line(ex, '{"type":"result","text":"42","is_main_result":true}',
                    on_result=lambda r: calls.append(r.text))
        assert calls == ["42"]

    def test_error_callback(self):
        ex, calls = Execution(), []
        _parse_line(ex, '{"type":"error","name":"Err","value":"v","traceback":[]}',
                    on_error=lambda e: calls.append(e.name))
        assert calls == ["Err"]

    def test_multiple_stdout_lines(self):
        ex = Execution()
        for i in range(3):
            _parse_line(ex, f'{{"type":"stdout","text":"line{i}\\n"}}')
        assert len(ex.logs.stdout) == 3

    def test_multiple_results_last_main(self):
        ex = Execution()
        _parse_line(ex, '{"type":"result","text":"a","is_main_result":false}')
        _parse_line(ex, '{"type":"result","text":"b","is_main_result":true}')
        assert ex.text == "b"
        assert len(ex.results) == 2


# ── Config ────────────────────────────────────────────────────────────────────

class TestConfig:
    def test_defaults(self, monkeypatch):
        for k in ("CUBE_API_URL", "CUBE_TEMPLATE_ID", "CUBE_PROXY_NODE_IP",
                  "CUBE_PROXY_PORT_HTTP", "CUBE_SANDBOX_DOMAIN"):
            monkeypatch.delenv(k, raising=False)
        cfg = Config()
        assert cfg.api_url == "http://127.0.0.1:3000"
        assert cfg.proxy_port == 80
        assert cfg.sandbox_domain == "cube.app"
        assert cfg.template_id is None
        assert cfg.proxy_node_ip is None

    def test_trailing_slash_stripped(self):
        cfg = Config(api_url="http://localhost:3000/")
        assert cfg.api_url == "http://localhost:3000"

    def test_env_override(self, monkeypatch):
        monkeypatch.setenv("CUBE_API_URL",         "http://1.2.3.4:3000")
        monkeypatch.setenv("CUBE_TEMPLATE_ID",     "tpl-env")
        monkeypatch.setenv("CUBE_PROXY_NODE_IP",   "1.2.3.4")
        monkeypatch.setenv("CUBE_PROXY_PORT_HTTP", "9090")
        monkeypatch.setenv("CUBE_SANDBOX_DOMAIN",  "mybox.io")
        cfg = Config()
        assert cfg.api_url        == "http://1.2.3.4:3000"
        assert cfg.template_id    == "tpl-env"
        assert cfg.proxy_node_ip  == "1.2.3.4"
        assert cfg.proxy_port     == 9090
        assert cfg.sandbox_domain == "mybox.io"


# ── Commands submodule ────────────────────────────────────────────────────────

class TestCommands:
    def test_run_success(self):
        sb = make_sandbox()
        def fake_run(code, *, timeout=None, on_stdout=None, **kw):
            if on_stdout:
                on_stdout(OutputMessage(text="hello\nworld\n0\n"))
            return Execution(logs=Logs(stdout=["hello\nworld\n0\n"], stderr=[]))
        with patch.object(sb, "run_code", side_effect=fake_run):
            result = sb.commands.run("echo hello")
        assert result.stdout == "hello\nworld\n"
        assert result.exit_code == 0

    def test_run_exit_code_nonzero(self):
        sb = make_sandbox()
        def fake_run(code, *, timeout=None, on_stdout=None, **kw):
            if on_stdout:
                on_stdout(OutputMessage(text="1\n"))
            return Execution(logs=Logs(stdout=["1\n"], stderr=[]))
        with patch.object(sb, "run_code", side_effect=fake_run):
            result = sb.commands.run("false")
        assert result.exit_code == 1

    def test_run_timeout_forwarded(self):
        sb = make_sandbox()
        def fake_run(code, *, timeout=None, on_stdout=None, **kw):
            if on_stdout:
                on_stdout(OutputMessage(text="ok\n0\n"))
            return Execution(logs=Logs(stdout=["ok\n0\n"], stderr=[]))
        with patch.object(sb, "run_code", side_effect=fake_run) as m:
            sb.commands.run("sleep 1", timeout=5.0)
        assert m.call_args.kwargs.get("timeout") == 5.0

    def test_commands_property(self):
        assert isinstance(make_sandbox().commands, Commands)

    def test_command_result_fields(self):
        r = CommandResult(stdout="out", stderr="err", exit_code=0)
        assert r.stdout == "out"
        assert r.stderr == "err"
        assert r.exit_code == 0


# ── Filesystem submodule ──────────────────────────────────────────────────────

class TestFilesystem:
    def test_read_success(self):
        sb = make_sandbox()
        mock_exec = Execution(results=[Result(text="file content", is_main_result=True)])
        with patch.object(sb, "run_code", return_value=mock_exec):
            content = sb.files.read("/tmp/foo.txt")
        assert content == "file content"

    def test_read_empty_when_no_text(self):
        sb = make_sandbox()
        with patch.object(sb, "run_code", return_value=Execution()):
            content = sb.files.read("/tmp/empty.txt")
        assert content == ""

    def test_read_raises_on_error(self):
        sb = make_sandbox()
        mock_exec = Execution(
            error=ExecutionError("FileNotFoundError", "No such file or directory"),
        )
        with patch.object(sb, "run_code", return_value=mock_exec):
            with pytest.raises(IOError, match="Failed to read"):
                sb.files.read("/tmp/missing.txt")

    def test_files_property(self):
        assert isinstance(make_sandbox().files, Filesystem)


# ── close / __del__ ───────────────────────────────────────────────────────────

class TestClose:
    def test_close_is_idempotent(self):
        sb = make_sandbox()
        sb.close()
        sb.close()  # should not raise

    def test_del_closes_client(self):
        sb = make_sandbox()
        mock_client = MagicMock()
        sb._client = mock_client
        sb.__del__()
        mock_client.close.assert_called_once()


# ── Exports ───────────────────────────────────────────────────────────────────

class TestExports:
    def test_command_result_importable(self):
        from cubesandbox import CommandResult  # noqa: F401

    def test_command_result_in_all(self):
        import cubesandbox
        assert "CommandResult" in cubesandbox.__all__
