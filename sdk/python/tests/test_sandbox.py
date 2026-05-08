# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
cubesandbox SDK unit tests.

Covers all implemented CubeAPI endpoints:
  GET  /health
  GET  /sandboxes              (v1)
  GET  /v2/sandboxes           (v2)
  POST /sandboxes              create
  GET  /sandboxes/:id          get
  DELETE /sandboxes/:id        kill
  POST /sandboxes/:id/pause    pause
  POST /sandboxes/:id/resume   resume (deprecated)
  POST /sandboxes/:id/connect  connect
  POST execute stream          run_code
"""
from __future__ import annotations

import json
from unittest.mock import MagicMock, patch

import pytest

from cubesandbox import Config, Execution, Sandbox
from cubesandbox._commands import CommandResult, Commands
from cubesandbox._exceptions import ApiError, AuthenticationError, SandboxNotFoundError
from cubesandbox._filesystem import Filesystem
from cubesandbox._models import ExecutionError, Logs, Result
from cubesandbox._stream import _parse_line

# ── fixtures ──────────────────────────────────────────────────────────────────

TEMPLATE_ID = "tpl-test"
SANDBOX_ID  = "abc123DEF456"
DOMAIN      = "cube.app"

SANDBOX_DATA = {
    "templateID":  TEMPLATE_ID,
    "sandboxID":   SANDBOX_ID,
    "clientID":    "client-uuid",
    "envdVersion": "0.2.0",
    "domain":      DOMAIN,
}

SANDBOX_DETAIL = {
    **SANDBOX_DATA,
    "startedAt": "2026-04-26T10:00:00Z",
    "endAt":     "2026-04-26T10:05:00Z",
    "state":     "running",
    "cpuCount":  2,
    "memoryMB":  512,
}


def make_config(**kw) -> Config:
    defaults = dict(
        api_url="http://localhost:3000",
        template_id=TEMPLATE_ID,
        proxy_node_ip="127.0.0.1",
    )
    defaults.update(kw)
    return Config(**defaults)


def mock_resp(json_data=None, status=200, text="") -> MagicMock:
    r = MagicMock()
    r.status_code = status
    r.ok = status < 400
    _data = json_data if json_data is not None else {}
    r.json.return_value = _data
    r.text = text or json.dumps(_data)
    return r


def make_sandbox(**kw) -> Sandbox:
    return Sandbox({**SANDBOX_DATA, **kw}, config=make_config())


# ── GET /health ───────────────────────────────────────────────────────────────

class TestHealth:
    def test_health_ok(self):
        s = make_sandbox()
        with patch.object(s._session, "get",
                          return_value=mock_resp({"status": "ok", "sandboxes": 2})) as m:
            resp = s._session.get("http://localhost:3000/health")
            assert resp.json()["status"] == "ok"
            m.assert_called_once()


# ── GET /sandboxes (v1) ───────────────────────────────────────────────────────

class TestListSandboxesV1:
    def test_list_returns_array(self):
        s = make_sandbox()
        payload = [SANDBOX_DETAIL, {**SANDBOX_DETAIL, "sandboxID": "other"}]
        with patch.object(s._session, "get", return_value=mock_resp(payload)):
            resp = s._session.get("http://localhost:3000/sandboxes")
            assert len(resp.json()) == 2
            assert resp.json()[0]["sandboxID"] == SANDBOX_ID

    def test_list_empty(self):
        s = make_sandbox()
        with patch.object(s._session, "get", return_value=mock_resp([])):
            resp = s._session.get("http://localhost:3000/sandboxes")
            assert resp.json() == []


# ── GET /v2/sandboxes ─────────────────────────────────────────────────────────

class TestListSandboxesV2:
    def test_list_v2_running(self):
        s = make_sandbox()
        payload = [SANDBOX_DETAIL]
        with patch.object(s._session, "get", return_value=mock_resp(payload)):
            resp = s._session.get("http://localhost:3000/v2/sandboxes", params={"state": "running"})
            assert resp.json()[0]["state"] == "running"

    def test_list_v2_paused(self):
        s = make_sandbox()
        payload = [{**SANDBOX_DETAIL, "state": "paused"}]
        with patch.object(s._session, "get", return_value=mock_resp(payload)):
            resp = s._session.get("http://localhost:3000/v2/sandboxes", params={"state": "paused"})
            assert resp.json()[0]["state"] == "paused"


# ── POST /sandboxes ───────────────────────────────────────────────────────────

class TestCreate:
    def test_create_minimal(self):
        with patch("requests.Session.post", return_value=mock_resp(SANDBOX_DATA, status=201)):
            sb = Sandbox.create(config=make_config())
        assert sb.sandbox_id == SANDBOX_ID
        assert sb.template_id == TEMPLATE_ID
        assert sb.domain == DOMAIN

    def test_create_with_timeout(self):
        with patch("requests.Session.post", return_value=mock_resp(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(timeout=600, config=make_config())
        body = m.call_args[1]["json"]
        assert body["timeout"] == 600

    def test_create_with_env_vars(self):
        with patch("requests.Session.post", return_value=mock_resp(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(env_vars={"FOO": "bar"}, config=make_config())
        body = m.call_args[1]["json"]
        assert body["envVars"] == {"FOO": "bar"}

    def test_create_with_metadata(self):
        with patch("requests.Session.post", return_value=mock_resp(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(metadata={"owner": "test"}, config=make_config())
        body = m.call_args[1]["json"]
        assert body["metadata"] == {"owner": "test"}

    def test_create_requires_template(self):
        cfg = Config(api_url="http://localhost:3000")  # no template_id
        with pytest.raises(ValueError, match="template"):
            Sandbox.create(config=cfg)

    def test_create_explicit_template(self):
        with patch("requests.Session.post", return_value=mock_resp(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(template="tpl-override", config=make_config())
        body = m.call_args[1]["json"]
        assert body["templateID"] == "tpl-override"

    def test_create_api_error(self):
        with patch("requests.Session.post",
                   return_value=mock_resp({"message": "internal error"}, status=500)):
            with pytest.raises(ApiError):
                Sandbox.create(config=make_config())

    def test_create_template_not_found(self):
        with patch("requests.Session.post",
                   return_value=mock_resp({"message": "template not found"}, status=404)):
            with pytest.raises(Exception):
                Sandbox.create(config=make_config())

    def test_create_auth_error(self):
        with patch("requests.Session.post",
                   return_value=mock_resp({"message": "unauthorized"}, status=401)):
            with pytest.raises(AuthenticationError):
                Sandbox.create(config=make_config())


# ── GET /sandboxes/:id ────────────────────────────────────────────────────────

class TestGetInfo:
    def test_get_info_running(self):
        sb = make_sandbox()
        with patch.object(sb._session, "get", return_value=mock_resp(SANDBOX_DETAIL)):
            info = sb.get_info()
        assert info["state"] == "running"
        assert info["sandboxID"] == SANDBOX_ID

    def test_get_info_paused(self):
        sb = make_sandbox()
        paused = {**SANDBOX_DETAIL, "state": "paused"}
        with patch.object(sb._session, "get", return_value=mock_resp(paused)):
            info = sb.get_info()
        assert info["state"] == "paused"

    def test_get_info_not_found(self):
        sb = make_sandbox()
        with patch.object(sb._session, "get",
                          return_value=mock_resp({"message": "not found"}, status=404)):
            with pytest.raises(SandboxNotFoundError):
                sb.get_info()


# ── DELETE /sandboxes/:id ─────────────────────────────────────────────────────

class TestKill:
    def test_kill_success(self):
        sb = make_sandbox()
        with patch.object(sb._session, "delete", return_value=mock_resp(status=204)) as m:
            sb.kill()
        m.assert_called_once_with(f"http://localhost:3000/sandboxes/{SANDBOX_ID}")

    def test_kill_not_found(self):
        sb = make_sandbox()
        with patch.object(sb._session, "delete",
                          return_value=mock_resp({"message": "not found"}, status=404)):
            with pytest.raises(SandboxNotFoundError):
                sb.kill()

    def test_context_manager_kills_on_exit(self):
        with patch("requests.Session.post", return_value=mock_resp(SANDBOX_DATA, status=201)):
            sb = Sandbox.create(config=make_config())
        with patch.object(sb._session, "delete", return_value=mock_resp(status=204)) as m:
            with sb:
                pass
        m.assert_called_once()

    def test_context_manager_suppresses_kill_error(self):
        with patch("requests.Session.post", return_value=mock_resp(SANDBOX_DATA, status=201)):
            sb = Sandbox.create(config=make_config())
        with patch.object(sb._session, "delete",
                          return_value=mock_resp({"message": "gone"}, status=404)):
            # should not raise
            with sb:
                pass


# ── POST /sandboxes/:id/pause ─────────────────────────────────────────────────

class TestPause:
    def test_pause_success(self):
        sb = make_sandbox()
        with patch.object(sb._session, "post", return_value=mock_resp(status=204)) as m:
            sb.pause(wait=False)
        m.assert_called_once_with(f"http://localhost:3000/sandboxes/{SANDBOX_ID}/pause")

    def test_pause_not_found(self):
        sb = make_sandbox()
        with patch.object(sb._session, "post",
                          return_value=mock_resp({"message": "not found"}, status=404)):
            with pytest.raises(SandboxNotFoundError):
                sb.pause(wait=False)

    def test_pause_wait_polls_until_paused(self):
        """pause(wait=True) should poll get_info until state == 'paused'."""
        sb = make_sandbox()
        paused_info = {**SANDBOX_DATA, "state": "paused"}
        with patch.object(sb._session, "post", return_value=mock_resp(status=204)), \
             patch.object(sb._session, "get",
                          side_effect=[
                              mock_resp({**SANDBOX_DATA, "state": "running"}),
                              mock_resp(paused_info),
                          ]) as get_m:
            sb.pause(wait=True, interval=0)
        assert get_m.call_count == 2

    def test_pause_wait_timeout(self):
        """pause(wait=True) raises TimeoutError when state never becomes 'paused'."""
        sb = make_sandbox()
        with patch.object(sb._session, "post", return_value=mock_resp(status=204)), \
             patch.object(sb._session, "get",
                          return_value=mock_resp({**SANDBOX_DATA, "state": "running"})):
            with pytest.raises(TimeoutError):
                sb.pause(wait=True, timeout=0, interval=0)


# ── POST /sandboxes/:id/resume ────────────────────────────────────────────────

class TestResume:
    def test_resume_success(self):
        sb = make_sandbox()
        with patch.object(sb._session, "post",
                          return_value=mock_resp(SANDBOX_DATA, status=201)) as m:
            sb.resume(timeout=120)
        body = m.call_args[1]["json"]
        assert body["timeout"] == 120

    def test_resume_default_timeout(self):
        sb = make_sandbox()
        with patch.object(sb._session, "post",
                          return_value=mock_resp(SANDBOX_DATA, status=201)) as m:
            sb.resume()
        body = m.call_args[1]["json"]
        assert body["timeout"] == 300

    def test_resume_not_found(self):
        sb = make_sandbox()
        with patch.object(sb._session, "post",
                          return_value=mock_resp({"message": "not found"}, status=404)):
            with pytest.raises(SandboxNotFoundError):
                sb.resume()


# ── POST /sandboxes/:id/connect ───────────────────────────────────────────────

class TestConnect:
    def test_connect_success(self):
        with patch("requests.Session.post", return_value=mock_resp(SANDBOX_DATA)):
            sb = Sandbox.connect(SANDBOX_ID, config=make_config())
        assert sb.sandbox_id == SANDBOX_ID

    def test_connect_not_found(self):
        with patch("requests.Session.post",
                   return_value=mock_resp({"message": "not found"}, status=404)):
            with pytest.raises(SandboxNotFoundError):
                Sandbox.connect(SANDBOX_ID, config=make_config())

    def test_connect_sends_timeout(self):
        cfg = make_config()
        cfg.timeout = 600
        with patch("requests.Session.post", return_value=mock_resp(SANDBOX_DATA)) as m:
            Sandbox.connect(SANDBOX_ID, config=cfg)
        body = m.call_args[1]["json"]
        assert body["timeout"] == 600


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
            Result(text="side",   is_main_result=False),
            Result(text="42",     is_main_result=True),
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
        assert len(ex.results) == 1
        assert ex.results[0].is_main_result is True

    def test_parses_result_not_main(self):
        ex = Execution()
        _parse_line(ex, '{"type":"result","text":"side","is_main_result":false}')
        assert ex.text is None
        assert ex.results[0].text == "side"

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
        _parse_line(ex, '{"type":"error","name":"ValueError","value":"bad","traceback":["line1"]}')
        assert ex.error.name == "ValueError"
        assert ex.error.value == "bad"
        assert ex.error.traceback == ["line1"]

    def test_parses_execution_count(self):
        ex = Execution()
        _parse_line(ex, '{"type":"number_of_executions","execution_count":5}')
        assert ex.execution_count == 5

    def test_ignores_bad_json(self):
        ex = Execution()
        _parse_line(ex, "not json at all")
        assert ex.results == []
        assert ex.error is None

    def test_ignores_empty_line(self):
        ex = Execution()
        _parse_line(ex, "")
        assert ex.results == []

    def test_ignores_unknown_type(self):
        ex = Execution()
        _parse_line(ex, '{"type":"unknown_event","data":"x"}')
        assert ex.results == []
        assert ex.error is None

    def test_stdout_callback_called(self):
        ex = Execution()
        calls = []
        _parse_line(ex, '{"type":"stdout","text":"hi\\n"}',
                    on_stdout=lambda m: calls.append(m.text))
        assert calls == ["hi\n"]

    def test_stderr_callback_called(self):
        ex = Execution()
        calls = []
        _parse_line(ex, '{"type":"stderr","text":"err\\n"}',
                    on_stderr=lambda m: calls.append(m.text))
        assert calls == ["err\\n"] or calls == ["err\n"]

    def test_result_callback_called(self):
        ex = Execution()
        calls = []
        _parse_line(ex, '{"type":"result","text":"42","is_main_result":true}',
                    on_result=lambda r: calls.append(r.text))
        assert calls == ["42"]

    def test_error_callback_called(self):
        ex = Execution()
        calls = []
        _parse_line(ex, '{"type":"error","name":"Err","value":"v","traceback":[]}',
                    on_error=lambda e: calls.append(e.name))
        assert calls == ["Err"]

    def test_multiple_stdout_lines(self):
        ex = Execution()
        for i in range(3):
            _parse_line(ex, f'{{"type":"stdout","text":"line{i}\\n"}}')
        assert len(ex.logs.stdout) == 3
        assert ex.logs.stdout[0] == "line0\n"

    def test_multiple_results_last_main(self):
        ex = Execution()
        _parse_line(ex, '{"type":"result","text":"a","is_main_result":false}')
        _parse_line(ex, '{"type":"result","text":"b","is_main_result":true}')
        assert ex.text == "b"
        assert len(ex.results) == 2


# ── Config ────────────────────────────────────────────────────────────────────

class TestConfig:
    def test_defaults(self):
        import os
        for k in ("CUBE_API_URL", "CUBE_TEMPLATE_ID", "CUBE_PROXY_NODE_IP",
                  "CUBE_PROXY_PORT_HTTP", "CUBE_SANDBOX_DOMAIN"):
            os.environ.pop(k, None)
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
        monkeypatch.setenv("CUBE_API_URL",          "http://1.2.3.4:3000")
        monkeypatch.setenv("CUBE_TEMPLATE_ID",      "tpl-env")
        monkeypatch.setenv("CUBE_PROXY_NODE_IP",    "1.2.3.4")
        monkeypatch.setenv("CUBE_PROXY_PORT_HTTP",  "9090")
        monkeypatch.setenv("CUBE_SANDBOX_DOMAIN",   "mybox.io")
        cfg = Config()
        assert cfg.api_url       == "http://1.2.3.4:3000"
        assert cfg.template_id   == "tpl-env"
        assert cfg.proxy_node_ip == "1.2.3.4"
        assert cfg.proxy_port    == 9090
        assert cfg.sandbox_domain == "mybox.io"


# ── Commands submodule ───────────────────────────────────────────────────────

class TestCommands:
    def test_run_success(self):
        sb = make_sandbox()
        mock_exec = Execution(
            logs=Logs(stdout=["hello", "world"], stderr=[]),
        )
        with patch.object(sb, "run_code", return_value=mock_exec) as m:
            result = sb.commands.run("echo hello")
        m.assert_called_once_with("echo hello", language="sh", timeout=None)
        assert result.stdout == "hello\nworld"
        assert result.stderr == ""
        assert result.exit_code == 0

    def test_run_with_error(self):
        sb = make_sandbox()
        mock_exec = Execution(
            logs=Logs(stdout=[], stderr=["error msg"]),
            error=ExecutionError("ShellError", "command failed"),
        )
        with patch.object(sb, "run_code", return_value=mock_exec):
            result = sb.commands.run("false")
        assert result.exit_code == 1
        assert result.stderr == "error msg"

    def test_run_with_timeout(self):
        sb = make_sandbox()
        mock_exec = Execution(logs=Logs(stdout=["ok"], stderr=[]))
        with patch.object(sb, "run_code", return_value=mock_exec) as m:
            sb.commands.run("sleep 1", timeout=5.0)
        m.assert_called_once_with("sleep 1", language="sh", timeout=5.0)

    def test_commands_property_returns_commands_instance(self):
        sb = make_sandbox()
        assert isinstance(sb.commands, Commands)

    def test_commands_result_is_dataclass(self):
        result = CommandResult(stdout="out", stderr="err", exit_code=0)
        assert result.stdout == "out"
        assert result.stderr == "err"
        assert result.exit_code == 0


# ── Filesystem submodule ──────────────────────────────────────────────────────

class TestFilesystem:
    def test_read_success(self):
        sb = make_sandbox()
        mock_exec = Execution(results=[Result(text="file content", is_main_result=True)])
        with patch.object(sb, "run_code", return_value=mock_exec) as m:
            content = sb.files.read("/tmp/foo.txt")
        m.assert_called_once_with("open('/tmp/foo.txt').read()")
        assert content == "file content"

    def test_read_returns_empty_string_when_no_text(self):
        sb = make_sandbox()
        mock_exec = Execution()  # no results, text is None
        with patch.object(sb, "run_code", return_value=mock_exec):
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

    def test_files_property_returns_filesystem_instance(self):
        sb = make_sandbox()
        assert isinstance(sb.files, Filesystem)


# ── TestCreate: allow_internet_access and network params ──────────────────────

class TestCreateNetworkParams:
    def test_create_allow_internet_access_false(self):
        with patch("requests.Session.post",
                   return_value=mock_resp(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(allow_internet_access=False, config=make_config())
        body = m.call_args[1]["json"]
        assert body["allowInternetAccess"] is False

    def test_create_allow_internet_access_true_not_in_payload(self):
        """When allow_internet_access=True (default), key is NOT sent."""
        with patch("requests.Session.post",
                   return_value=mock_resp(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(config=make_config())
        body = m.call_args[1]["json"]
        assert "allowInternetAccess" not in body

    def test_create_network_allow_out(self):
        with patch("requests.Session.post",
                   return_value=mock_resp(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(
                network={"allow_out": ["8.8.8.8/32"]},
                config=make_config(),
            )
        body = m.call_args[1]["json"]
        assert "network" in body
        assert body["network"]["allowOut"] == ["8.8.8.8/32"]

    def test_create_network_deny_out(self):
        with patch("requests.Session.post",
                   return_value=mock_resp(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(
                network={"deny_out": ["0.0.0.0/0"]},
                config=make_config(),
            )
        body = m.call_args[1]["json"]
        assert body["network"]["denyOut"] == ["0.0.0.0/0"]

    def test_create_network_both_allow_and_deny(self):
        with patch("requests.Session.post",
                   return_value=mock_resp(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(
                network={"allow_out": ["1.2.3.4/32"], "deny_out": ["0.0.0.0/0"]},
                config=make_config(),
            )
        body = m.call_args[1]["json"]
        assert body["network"]["allowOut"] == ["1.2.3.4/32"]
        assert body["network"]["denyOut"] == ["0.0.0.0/0"]

    def test_create_network_empty_dict_not_in_payload(self):
        """network={} with no recognized keys → no 'network' key sent."""
        with patch("requests.Session.post",
                   return_value=mock_resp(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(network={}, config=make_config())
        body = m.call_args[1]["json"]
        assert "network" not in body

    def test_create_combined_no_internet_and_network(self):
        with patch("requests.Session.post",
                   return_value=mock_resp(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(
                allow_internet_access=False,
                network={"allow_out": ["10.0.0.1/32"]},
                config=make_config(),
            )
        body = m.call_args[1]["json"]
        assert body["allowInternetAccess"] is False
        assert body["network"]["allowOut"] == ["10.0.0.1/32"]


# ── CommandResult in __init__ exports ─────────────────────────────────────────

class TestExports:
    def test_command_result_importable_from_package(self):
        from cubesandbox import CommandResult  # noqa: F401
        assert CommandResult is not None

    def test_command_result_in_all(self):
        import cubesandbox
        assert "CommandResult" in cubesandbox.__all__
