# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
cubesandbox SDK unit tests.

All HTTP calls are intercepted via requests/httpx mocks
so no real network is needed.
"""

from __future__ import annotations

import base64
import json
import struct
import threading
import time
from unittest.mock import MagicMock, patch

import httpx
import pytest

from cubesandbox import CommandResult, Template
from cubesandbox._template import TemplateInfo
from cubesandbox._commands import Commands, _collect_process_events
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


def mock_response(body=None, status: int = 200):
    """Build a duck-typed requests.Response for control-plane SDK calls."""
    response = MagicMock()
    response.ok = 200 <= status < 400
    response.status_code = status
    response.text = json.dumps(body) if body is not None else ""
    response.json.return_value = body if body is not None else {}
    return response


def make_sandbox(**data_overrides) -> Sandbox:
    d = {**SANDBOX_DATA, **data_overrides}
    return Sandbox(d, config=make_config())


def connect_envelope(flags: int, payload: str) -> bytes:
    raw = payload.encode("utf-8")
    return bytes([flags]) + struct.pack(">I", len(raw)) + raw


def decode_connect_payload(raw: bytes) -> dict:
    assert len(raw) >= 5
    flags = raw[0]
    size = struct.unpack(">I", raw[1:5])[0]
    assert flags == 0
    assert len(raw) == 5 + size
    return json.loads(raw[5:].decode("utf-8"))


# ── POST /sandboxes ───────────────────────────────────────────────────────────

class TestCreate:
    def test_create_success(self):
        with patch("requests.Session.post", return_value=mock_response(SANDBOX_DATA, status=201)):
            sb = Sandbox.create(config=make_config())
        assert sb.sandbox_id == SANDBOX_ID

    def test_create_missing_template_raises(self):
        cfg = make_config(template_id=None)
        with pytest.raises(ValueError, match="template"):
            Sandbox.create(config=cfg)

    def test_create_sends_template_and_timeout(self):
        with patch("requests.Session.post", return_value=mock_response(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(template="tpl-foo", timeout=600, config=make_config())
        body = m.call_args.kwargs["json"]
        assert body["templateID"] == "tpl-foo"
        assert body["timeout"] == 600

    def test_create_sends_env_vars(self):
        with patch("requests.Session.post", return_value=mock_response(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(env_vars={"FOO": "bar"}, config=make_config())
        body = m.call_args.kwargs["json"]
        assert body["envVars"] == {"FOO": "bar"}

    def test_create_sends_metadata(self):
        meta = {"network-policy": "deny-all"}
        with patch("requests.Session.post", return_value=mock_response(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(metadata=meta, config=make_config())
        body = m.call_args.kwargs["json"]
        assert body["metadata"] == meta

    def test_create_template_not_found(self):
        with patch("requests.Session.post",
                   return_value=mock_response({"message": "template not found"}, status=404)):
            with pytest.raises(TemplateNotFoundError):
                Sandbox.create(config=make_config())

    def test_create_server_error(self):
        with patch("requests.Session.post",
                   return_value=mock_response({"message": "internal error"}, status=500)):
            with pytest.raises(ApiError):
                Sandbox.create(config=make_config())

    def test_create_allow_internet_access_false(self):
        with patch("requests.Session.post", return_value=mock_response(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(allow_internet_access=False, config=make_config())
        body = m.call_args.kwargs["json"]
        assert body["allow_internet_access"] is False

    def test_create_allow_internet_access_true_not_in_payload(self):
        with patch("requests.Session.post", return_value=mock_response(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(config=make_config())
        body = m.call_args.kwargs["json"]
        assert "allow_internet_access" not in body

    def test_create_network_allow_out(self):
        with patch("requests.Session.post", return_value=mock_response(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(network={"allow_out": ["8.8.8.8/32"]}, config=make_config())
        body = m.call_args.kwargs["json"]
        assert body["network"]["allowOut"] == ["8.8.8.8/32"]

    def test_create_network_deny_out(self):
        with patch("requests.Session.post", return_value=mock_response(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(network={"deny_out": ["0.0.0.0/0"]}, config=make_config())
        body = m.call_args.kwargs["json"]
        assert body["network"]["denyOut"] == ["0.0.0.0/0"]

    def test_create_network_empty_not_in_payload(self):
        with patch("requests.Session.post", return_value=mock_response(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(network={}, config=make_config())
        body = m.call_args.kwargs["json"]
        assert "network" not in body


# ── domain filtering (DNS allow-list) ────────────────────────────────────────

class TestDomainFiltering:
    def _payload_for_network(self, network, **kwargs):
        with patch("requests.Session.post", return_value=mock_response(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(network=network, config=make_config(), **kwargs)
        return m.call_args.kwargs["json"]

    @pytest.mark.parametrize("domain", [
        "api.github.com",
        "api.deepseek.com",
        "example.com",
    ])
    def test_create_network_allow_out_accepts_domain_target(self, domain):
        body = self._payload_for_network({
            "allow_out": [domain],
            "deny_out": ["0.0.0.0/0"],
        })
        assert body["network"]["allowOut"] == [domain]
        assert body["network"]["denyOut"] == ["0.0.0.0/0"]
        assert "rules" not in body["network"]

    @pytest.mark.parametrize("domain", [
        "*.example.com",
        "*.githubusercontent.com",
        "*.internal.example.org",
    ])
    def test_create_network_allow_out_accepts_wildcard_domain_target(self, domain):
        body = self._payload_for_network({
            "allow_out": [domain],
            "deny_out": ["0.0.0.0/0"],
        })
        assert body["network"]["allowOut"] == [domain]

    def test_create_network_allow_out_preserves_mixed_domain_and_cidr_targets(self):
        allow_out = ["api.deepseek.com", "172.67.0.0/16", "*.githubusercontent.com"]
        body = self._payload_for_network({"allow_out": allow_out, "deny_out": ["0.0.0.0/0"]})
        assert body["network"]["allowOut"] == allow_out

    def test_create_network_allow_out_preserves_order_and_duplicates(self):
        allow_out = ["api.example.com", "*.example.com", "api.example.com"]
        body = self._payload_for_network({"allow_out": allow_out, "deny_out": ["0.0.0.0/0"]})
        assert body["network"]["allowOut"] == allow_out

    def test_create_network_domain_filtering_preserves_literal_domain_values(self):
        allow_out = ["API.Example.COM", "service.example.com."]
        body = self._payload_for_network({"allow_out": allow_out, "deny_out": ["0.0.0.0/0"]})
        assert body["network"]["allowOut"] == allow_out

    def test_create_network_domain_filtering_with_deny_all_mode(self):
        allow_out = ["api.example.com", "*.example.org"]
        body = self._payload_for_network(
            {"allow_out": allow_out},
            allow_internet_access=False,
        )
        assert body["allow_internet_access"] is False
        assert body["network"]["allowOut"] == allow_out
        assert "denyOut" not in body["network"]

    def test_create_network_domain_filtering_rejects_without_deny_all(self):
        with pytest.raises(ApiError, match="must disable public outbound traffic or include '0.0.0.0/0' in deny_out") as exc:
            Sandbox.create(
                network={"allow_out": ["api.example.com"], "deny_out": ["203.0.113.0/24"]},
                config=make_config(),
            )
        assert exc.value.status_code == 400

    def test_create_network_domain_filtering_accepts_deny_all_among_other_deny_out(self):
        allow_out = ["api.example.com", "*.example.org"]
        deny_out = ["203.0.113.0/24", "0.0.0.0/0"]
        body = self._payload_for_network({
            "allow_out": allow_out,
            "deny_out": deny_out,
        })
        assert body["network"]["allowOut"] == allow_out
        assert body["network"]["denyOut"] == deny_out

    def test_create_network_domain_filtering_alongside_l7_rules(self):
        from cubesandbox import Action, Match, Rule
        allow_out = ["api.github.com", "*.githubusercontent.com"]
        rules = [Rule(
            name="github_api",
            match=Match(host="api.github.com", path="/repos/*"),
            action=Action(allow=True, audit="metadata"),
        )]
        body = self._payload_for_network({
            "allow_out": allow_out,
            "deny_out": ["0.0.0.0/0"],
            "rules": rules,
        })
        assert body["network"]["allowOut"] == allow_out
        assert body["network"]["denyOut"] == ["0.0.0.0/0"]
        assert body["network"]["rules"] == [{
            "name": "github_api",
            "match": {"host": "api.github.com", "path": "/repos/*"},
            "action": {"allow": True, "audit": "metadata"},
        }]

    def test_create_network_empty_allow_out_is_still_sent(self):
        body = self._payload_for_network({"allow_out": []})
        assert body["network"]["allowOut"] == []


# ── network rules (L7 policy) ────────────────────────────────────────────────

class TestNetworkRules:
    def test_create_network_rules_dataclass(self):
        from cubesandbox import Action, Match, Rule
        rules = [Rule(
            name="deepseek_api",
            match=Match(scheme="https", host="api.deepseek.com",
                        method=["POST"], path="/v1/chat",
                        sni="api.deepseek.com"),
            action=Action(allow=True, audit="metadata"),
        )]
        with patch("requests.Session.post", return_value=mock_response(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(network={"rules": rules}, config=make_config())
        body = m.call_args.kwargs["json"]
        assert body["network"]["rules"] == [{
            "name": "deepseek_api",
            "match": {
                "scheme": "https",
                "host": "api.deepseek.com",
                "method": ["POST"],
                "path": "/v1/chat",
                "sni": "api.deepseek.com",
            },
            "action": {"allow": True, "audit": "metadata"},
        }]

    def test_create_network_rules_dict_passthrough(self):
        # Plain-dict rules pass through verbatim to the wire — no
        # snake_case → camelCase rename happens for match keys today.
        rules = [{
            "name": "deepseek_api",
            "match": {"path": "/v1/chat", "sni": "api.deepseek.com"},
            "action": {"allow": True},
        }]
        with patch("requests.Session.post", return_value=mock_response(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(network={"rules": rules}, config=make_config())
        body = m.call_args.kwargs["json"]
        wire = body["network"]["rules"][0]
        assert wire["name"] == "deepseek_api"
        assert wire["match"]["path"] == "/v1/chat"
        assert wire["match"]["sni"] == "api.deepseek.com"

    def test_create_network_rules_with_inject(self):
        from cubesandbox import Action, Inject, Match, Rule
        rules = [Rule(
            name="r1",
            match=Match(host="api.example.com"),
            action=Action(allow=True, inject=[
                Inject(header="Authorization", format="Bearer ${SECRET}", secret="sk_xxx"),
            ]),
        )]
        with patch("requests.Session.post", return_value=mock_response(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(network={"rules": rules}, config=make_config())
        body = m.call_args.kwargs["json"]
        inject_wire = body["network"]["rules"][0]["action"]["inject"]
        assert inject_wire == [{
            "header": "Authorization",
            "secret": "sk_xxx",
            "format": "Bearer ${SECRET}",
        }]

    def test_create_network_rules_alongside_allow_out(self):
        from cubesandbox import Action, Match, Rule
        rules = [Rule(name="r1", match=Match(host="x.com"), action=Action(allow=True))]
        with patch("requests.Session.post", return_value=mock_response(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(
                network={"allow_out": ["1.2.3.0/24"], "rules": rules},
                config=make_config(),
            )
        body = m.call_args.kwargs["json"]
        assert body["network"]["allowOut"] == ["1.2.3.0/24"]
        assert len(body["network"]["rules"]) == 1

    def test_create_network_rules_domain_only_does_not_require_deny_all(self):
        from cubesandbox import Action, Match, Rule
        rules = [Rule(name="r1", match=Match(host="x.com"), action=Action(allow=True))]
        with patch("requests.Session.post", return_value=mock_response(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(network={"rules": rules}, config=make_config())
        body = m.call_args.kwargs["json"]
        assert body["network"]["rules"][0]["match"]["host"] == "x.com"
        assert "denyOut" not in body["network"]

    def test_create_network_rules_empty_list_omitted(self):
        with patch("requests.Session.post", return_value=mock_response(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(network={"rules": []}, config=make_config())
        body = m.call_args.kwargs["json"]
        assert "network" not in body

    def test_inject_render(self):
        from cubesandbox import Inject
        assert Inject(header="Authorization", format="Bearer ${SECRET}",
                      secret="sk_xxx").render() == "Bearer sk_xxx"
        # no format → defaults to "${SECRET}"
        assert Inject(header="X-Token", secret="abc").render() == "abc"

    def test_match_to_wire_omits_none_fields(self):
        from cubesandbox import Match
        wire = Match(host="example.com").to_wire()
        assert wire == {"host": "example.com"}
        assert "scheme" not in wire

    def test_action_to_wire_omits_none_inject_and_audit(self):
        from cubesandbox import Action
        assert Action(allow=False).to_wire() == {"allow": False}


# ── POST /sandboxes/:id/connect ───────────────────────────────────────────────

class TestConnect:
    def test_connect_success(self):
        with patch("requests.Session.post", return_value=mock_response(SANDBOX_DATA)):
            sb = Sandbox.connect(SANDBOX_ID, config=make_config())
        assert sb.sandbox_id == SANDBOX_ID

    def test_connect_not_found(self):
        with patch("requests.Session.post",
                   return_value=mock_response({"message": "not found"}, status=404)):
            with pytest.raises(SandboxNotFoundError):
                Sandbox.connect(SANDBOX_ID, config=make_config())

    def test_connect_sends_timeout(self):
        cfg = make_config()
        cfg.timeout = 600
        with patch("requests.Session.post", return_value=mock_response(SANDBOX_DATA)) as m:
            Sandbox.connect(SANDBOX_ID, config=cfg)
        body = m.call_args.kwargs["json"]
        assert body["timeout"] == 600


# ── GET /sandboxes ────────────────────────────────────────────────────────────

class TestListSandboxesV1:
    def test_list_returns_list(self):
        data = [SANDBOX_DATA]
        with patch("requests.Session.get", return_value=mock_response(data)):
            result = Sandbox.list(config=make_config())
        assert result == data

    def test_list_empty(self):
        with patch("requests.Session.get", return_value=mock_response([])):
            result = Sandbox.list(config=make_config())
        assert result == []

    def test_list_calls_correct_endpoint(self):
        with patch("requests.Session.get", return_value=mock_response([])) as m:
            Sandbox.list(config=make_config())
        assert "/sandboxes" in str(m.call_args)

    def test_list_server_error(self):
        with patch("requests.Session.get",
                   return_value=mock_response({"message": "error"}, status=500)):
            with pytest.raises(ApiError):
                Sandbox.list(config=make_config())


# ── GET /v2/sandboxes ─────────────────────────────────────────────────────────

class TestListSandboxesV2:
    def test_list_v2_returns_list(self):
        data = [SANDBOX_DATA]
        with patch("requests.Session.get", return_value=mock_response(data)):
            result = Sandbox.list_v2(config=make_config())
        assert result == data

    def test_list_v2_calls_correct_endpoint(self):
        with patch("requests.Session.get", return_value=mock_response([])) as m:
            Sandbox.list_v2(config=make_config())
        assert "/v2/sandboxes" in str(m.call_args)


# ── GET /health ───────────────────────────────────────────────────────────────

class TestHealth:
    def test_health_ok(self):
        with patch("requests.Session.get",
                   return_value=mock_response({"status": "ok", "sandboxes": 2})):
            result = Sandbox.health(config=make_config())
        assert result["status"] == "ok"
        assert result["sandboxes"] == 2

    def test_health_server_error(self):
        with patch("requests.Session.get",
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
        with patch("requests.Session.post", return_value=mock_response(SANDBOX_DATA, status=201)):
            sb = Sandbox.create(config=make_config())
        with patch.object(sb._session, "delete", return_value=mock_response(status=204)) as m:
            with sb:
                pass
        m.assert_called_once()

    def test_context_manager_suppresses_kill_error(self):
        with patch("requests.Session.post", return_value=mock_response(SANDBOX_DATA, status=201)):
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

    def test_error_traceback_list_is_e2b_string(self):
        err = ExecutionError("ValueError", "bad", ["line1", "line2"])
        assert err.traceback == "line1\nline2"

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

    def test_result_e2b_fields_and_legacy_json_alias(self):
        result = Result(json={"a": 1}, data={"b": 2}, chart={"type": "bar"})
        assert result.json == {"a": 1}
        assert result.json_data == {"a": 1}
        assert set(result.formats()) == {"json", "data", "chart"}

    def test_result_legacy_json_data_constructor(self):
        result = Result(json_data={"a": 1})
        assert result.json == {"a": 1}

    def test_execution_to_json(self):
        ex = Execution(results=[Result(text="2", is_main_result=True)])
        assert '"results"' in ex.to_json()
        assert '"text": "2"' in ex.to_json()

    def test_output_message_e2b_and_legacy_aliases(self):
        msg = OutputMessage("hello\n", 123, True)
        assert msg.line == "hello\n"
        assert msg.text == "hello\n"
        assert msg.error is True
        assert msg.is_stderr is True
        assert str(msg) == "hello\n"

    def test_output_message_legacy_constructor(self):
        msg = OutputMessage(text="warn\n", is_stderr=True)
        assert msg.line == "warn\n"
        assert msg.error is True


# ── _parse_line (ndjson stream) ───────────────────────────────────────────────

class TestParseStream:
    def test_parses_result(self):
        ex = Execution()
        _parse_line(ex, '{"type":"result","text":"2","is_main_result":true}')
        assert ex.text == "2"

    def test_parses_e2b_result_fields(self):
        ex = Execution()
        _parse_line(
            ex,
            '{"type":"result","json":{"a":1},"data":{"b":2},"chart":{"type":"bar"},"is_main_result":true}',
        )
        result = ex.results[0]
        assert result.json == {"a": 1}
        assert result.json_data == {"a": 1}
        assert result.data == {"b": 2}
        assert result.chart == {"type": "bar"}

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
                    on_stdout=lambda m: calls.append((m.line, m.text, m.error)))
        assert calls == [("hi\n", "hi\n", False)]

    def test_stderr_callback(self):
        ex, calls = Execution(), []
        _parse_line(ex, '{"type":"stderr","text":"warn\\n"}',
                    on_stderr=lambda m: calls.append((m.line, m.text, m.error, m.is_stderr)))
        assert calls == [("warn\n", "warn\n", True, True)]

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
        seen = {}

        def handler(request: httpx.Request) -> httpx.Response:
            seen["method"] = request.method
            seen["host"] = request.url.host
            seen["path"] = request.url.path
            seen["headers"] = request.headers
            seen["payload"] = decode_connect_payload(request.content)
            stdout = base64.b64encode(b"hello\nworld\n").decode()
            body = b"".join(
                [
                    connect_envelope(0, '{"event":{"start":{"pid":123}}}'),
                    connect_envelope(0, json.dumps({"event": {"data": {"stdout": stdout}}})),
                    connect_envelope(0, '{"event":{"end":{"exitCode":0,"exited":true}}}'),
                    connect_envelope(0x02, "{}"),
                ]
            )
            return httpx.Response(200, stream=httpx.ByteStream(body))

        client = httpx.Client(transport=httpx.MockTransport(handler))
        with (
            patch.object(Commands, "_run_with_e2b_connect", side_effect=ImportError),
            patch.object(sb, "_build_data_client", return_value=client),
        ):
            result = sb.commands.run("echo hello", cwd="/work", env={"A": "B"})

        assert result.stdout == "hello\nworld\n"
        assert result.exit_code == 0
        assert seen["method"] == "POST"
        assert seen["host"] == f"49983-{SANDBOX_ID}.{DOMAIN}"
        assert seen["path"] == "/process.Process/Start"
        assert seen["headers"]["content-type"] == "application/connect+json"
        assert seen["headers"]["connect-protocol-version"] == "1"
        assert seen["headers"]["connect-content-encoding"] == "identity"
        assert seen["headers"]["authorization"] == "Basic cm9vdDo="
        assert seen["payload"]["process"]["cmd"] == "/bin/bash"
        assert seen["payload"]["process"]["cwd"] == "/work"
        assert seen["payload"]["process"]["envs"] == {"A": "B"}
        assert seen["payload"]["process"]["args"] == ["-l", "-c", "echo hello"]

    def test_run_stderr_event(self):
        sb = make_sandbox()

        def handler(request: httpx.Request) -> httpx.Response:
            stderr = base64.b64encode(b"warn\nerror\n").decode()
            body = b"".join(
                [
                    connect_envelope(0, '{"event":{"start":{"pid":123}}}'),
                    connect_envelope(0, json.dumps({"event": {"data": {"stderr": stderr}}})),
                    connect_envelope(0, '{"event":{"end":{"exitCode":0,"exited":true}}}'),
                    connect_envelope(0x02, "{}"),
                ]
            )
            return httpx.Response(200, stream=httpx.ByteStream(body))

        client = httpx.Client(transport=httpx.MockTransport(handler))
        with (
            patch.object(Commands, "_run_with_e2b_connect", side_effect=ImportError),
            patch.object(sb, "_build_data_client", return_value=client),
        ):
            result = sb.commands.run("echo warn >&2")

        assert result.stdout == ""
        assert result.stderr == "warn\nerror\n"
        assert result.exit_code == 0

    def test_run_exit_code_nonzero(self):
        sb = make_sandbox()

        def handler(request: httpx.Request) -> httpx.Response:
            body = b"".join(
                [
                    connect_envelope(0, '{"event":{"start":{"pid":123}}}'),
                    connect_envelope(0, '{"event":{"end":{"exitCode":1,"exited":true}}}'),
                    connect_envelope(0x02, "{}"),
                ]
            )
            return httpx.Response(200, stream=httpx.ByteStream(body))

        client = httpx.Client(transport=httpx.MockTransport(handler))
        with (
            patch.object(Commands, "_run_with_e2b_connect", side_effect=ImportError),
            patch.object(sb, "_build_data_client", return_value=client),
        ):
            result = sb.commands.run("false")
        assert result.exit_code == 1

    def test_run_exit_code_from_status_string(self):
        sb = make_sandbox()

        def handler(request: httpx.Request) -> httpx.Response:
            body = b"".join(
                [
                    connect_envelope(0, '{"event":{"start":{"pid":123}}}'),
                    connect_envelope(0, '{"event":{"end":{"exited":true,"status":"exit status 7"}}}'),
                    connect_envelope(0x02, "{}"),
                ]
            )
            return httpx.Response(200, stream=httpx.ByteStream(body))

        client = httpx.Client(transport=httpx.MockTransport(handler))
        with (
            patch.object(Commands, "_run_with_e2b_connect", side_effect=ImportError),
            patch.object(sb, "_build_data_client", return_value=client),
        ):
            result = sb.commands.run("false")
        assert result.exit_code == 7

    def test_run_exit_code_from_signal_status(self):
        sb = make_sandbox()

        def handler(request: httpx.Request) -> httpx.Response:
            body = b"".join(
                [
                    connect_envelope(0, '{"event":{"start":{"pid":123}}}'),
                    connect_envelope(
                        0,
                        '{"event":{"end":{"exited":false,"status":"signal 9 (SIGKILL)"}}}',
                    ),
                    connect_envelope(0x02, "{}"),
                ]
            )
            return httpx.Response(200, stream=httpx.ByteStream(body))

        client = httpx.Client(transport=httpx.MockTransport(handler))
        with (
            patch.object(Commands, "_run_with_e2b_connect", side_effect=ImportError),
            patch.object(sb, "_build_data_client", return_value=client),
        ):
            result = sb.commands.run("kill")
        assert result.exit_code == 137

    def test_collect_process_events_prefers_status_when_exit_code_unset(self):
        class End:
            exit_code = 0
            status = "exit status 7"
            exited = True
            error = ""

            def HasField(self, name):
                return False

        class Event:
            end = End()

            def HasField(self, name):
                return name == "end"

        class Response:
            event = Event()

            def HasField(self, name):
                return name == "event"

        result = _collect_process_events([Response()])
        assert result.exit_code == 7

    def test_run_timeout_forwarded(self):
        sb = make_sandbox()
        seen = {}

        def handler(request: httpx.Request) -> httpx.Response:
            seen["headers"] = request.headers
            body = b"".join(
                [
                    connect_envelope(0, '{"event":{"start":{"pid":123}}}'),
                    connect_envelope(0, '{"event":{"end":{"exitCode":0,"exited":true}}}'),
                    connect_envelope(0x02, "{}"),
                ]
            )
            return httpx.Response(200, stream=httpx.ByteStream(body))

        client = httpx.Client(transport=httpx.MockTransport(handler))
        with (
            patch.object(Commands, "_run_with_e2b_connect", side_effect=ImportError),
            patch.object(sb, "_build_data_client", return_value=client),
        ):
            sb.commands.run("sleep 1", timeout=5.0)
        assert seen["headers"]["connect-timeout-ms"] == "5000"

    def test_run_http_error_includes_response_body(self):
        sb = make_sandbox()

        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(400, json={"message": "sandbox is not ready"})

        client = httpx.Client(transport=httpx.MockTransport(handler))
        with (
            patch.object(Commands, "_run_with_e2b_connect", side_effect=ImportError),
            patch.object(sb, "_build_data_client", return_value=client),
        ):
            with pytest.raises(RuntimeError, match="HTTP 400: sandbox is not ready"):
                sb.commands.run("echo hello")

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
        seen = {}

        def handler(request: httpx.Request) -> httpx.Response:
            seen["method"] = request.method
            seen["host"] = request.url.host
            seen["path"] = request.url.path
            seen["query_path"] = request.url.params.get("path")
            seen["query_user"] = request.url.params.get("username")
            return httpx.Response(200, text="file content")

        client = httpx.Client(transport=httpx.MockTransport(handler))
        with patch.object(sb, "_build_data_client", return_value=client):
            content = sb.files.read("/tmp/foo.txt")
        assert content == "file content"
        assert seen["method"] == "GET"
        assert seen["host"] == f"49983-{SANDBOX_ID}.{DOMAIN}"
        assert seen["path"] == "/files"
        assert seen["query_path"] == "/tmp/foo.txt"
        assert seen["query_user"] == "root"

    def test_read_empty_when_no_text(self):
        sb = make_sandbox()

        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(200, text="")

        client = httpx.Client(transport=httpx.MockTransport(handler))
        with patch.object(sb, "_build_data_client", return_value=client):
            content = sb.files.read("/tmp/empty.txt")
        assert content == ""

    def test_read_raises_on_error(self):
        sb = make_sandbox()

        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(404, json={"message": "No such file or directory"})

        client = httpx.Client(transport=httpx.MockTransport(handler))
        with patch.object(sb, "_build_data_client", return_value=client):
            with pytest.raises(IOError, match="Failed to read"):
                sb.files.read("/tmp/missing.txt")

    def test_write_uses_envd_file_api(self):
        sb = make_sandbox()
        seen = {}

        def handler(request: httpx.Request) -> httpx.Response:
            seen["method"] = request.method
            seen["host"] = request.url.host
            seen["path"] = request.url.path
            seen["query_path"] = request.url.params.get("path")
            seen["query_user"] = request.url.params.get("username")
            seen["content_type"] = request.headers.get("content-type")
            seen["body"] = request.content
            return httpx.Response(200, json=[{"path": "/tmp/foo.txt"}])

        client = httpx.Client(transport=httpx.MockTransport(handler))
        with patch.object(sb, "_build_data_client", return_value=client):
            sb.files.write("/tmp/foo.txt", "file content")

        assert seen["method"] == "POST"
        assert seen["host"] == f"49983-{SANDBOX_ID}.{DOMAIN}"
        assert seen["path"] == "/files"
        assert seen["query_path"] == "/tmp/foo.txt"
        assert seen["query_user"] == "root"
        assert seen["content_type"] == "application/octet-stream"
        assert seen["body"] == b"file content"

    def test_write_falls_back_to_multipart_for_old_envd(self):
        sb = make_sandbox()
        seen = {"calls": 0}

        def handler(request: httpx.Request) -> httpx.Response:
            seen["calls"] += 1
            if seen["calls"] == 1:
                return httpx.Response(
                    415,
                    text="unsupported media type",
                )
            seen["content_type"] = request.headers.get("content-type")
            seen["body"] = request.content
            return httpx.Response(200, json=[{"path": "/tmp/foo.txt"}])

        client = httpx.Client(transport=httpx.MockTransport(handler))
        with patch.object(sb, "_build_data_client", return_value=client):
            sb.files.write("/tmp/foo.txt", "file content", user="root")

        assert seen["calls"] == 2
        assert "multipart/form-data" in seen["content_type"]
        assert b"file content" in seen["body"]

    def test_files_property(self):
        assert isinstance(make_sandbox().files, Filesystem)


# ── IPOverrideTransport ───────────────────────────────────────────────────────

class TestIPOverrideTransport:
    """IPOverrideTransport must copy request content for all body types."""

    def _handle(self, request: httpx.Request) -> None:
        """Run handle_request and assert the request body was copied successfully
        before the connection attempt fails."""
        from cubesandbox._transport import IPOverrideTransport

        transport = IPOverrideTransport("127.0.0.1", 1)
        with pytest.raises(httpx.ConnectError):
            transport.handle_request(request)

    def test_content_bytes(self):
        """content=bytes (files.write first attempt) must not raise."""
        req = httpx.Request(
            "POST",
            "http://49983-sb-test.cube.app/files",
            params={"path": "/tmp/t.txt", "username": "root"},
            content=b"hello",
        )
        self._handle(req)

    def test_files_multipart(self):
        """files= multipart (files.write fallback) must reach ConnectError."""
        req = httpx.Request(
            "POST",
            "http://49983-sb-test.cube.app/files",
            params={"path": "/tmp/t.txt", "username": "root"},
            files={"file": ("t.txt", b"hello")},
        )
        self._handle(req)

    def test_get_no_body(self):
        """GET (files.read) with no body must reach ConnectError."""
        req = httpx.Request(
            "GET",
            "http://49983-sb-test.cube.app/files",
            params={"path": "/tmp/t.txt", "username": "root"},
        )
        self._handle(req)


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


# ── Sandbox.rollback ──────────────────────────────────────────────────────────


class TestRollback:
    """Tests for ``Sandbox.rollback`` and its connection-reset side effect.

    Background: rolling back restarts the sandbox process, so any
    keep-alive sockets the SDK was holding against jupyter-server (or the
    cube-api control plane) become half-closed. The next ``run_code`` would
    block on a zombie socket. ``rollback`` must drop those pools so the
    next request opens a fresh connection.
    """

    @staticmethod
    def _requests_response(body=None, status: int = 200):
        """Build a duck-typed requests.Response. ``_check_response`` reads
        ``.ok``, ``.status_code``, ``.text`` and ``.json()`` — give it those
        directly via MagicMock instead of the project's httpx-based
        ``mock_response`` helper (which is what makes the other test classes
        in this file fail; orthogonal to rollback)."""
        m = MagicMock()
        m.ok = 200 <= status < 400
        m.status_code = status
        m.text = json.dumps(body) if body is not None else ""
        m.json.return_value = body if body is not None else {}
        return m

    def test_rollback_returns_response_body(self):
        sb = make_sandbox()
        body = {"sandboxID": sb.sandbox_id, "snapshotID": "snap-1", "status": "success"}
        with patch.object(sb._session, "post", return_value=self._requests_response(body)):
            assert sb.rollback("snap-1") == body

    def test_rollback_posts_snapshot_id_to_correct_url(self):
        sb = make_sandbox()
        with patch.object(sb._session, "post",
                          return_value=self._requests_response({"status": "success"})) as post_m:
            sb.rollback("snap-xyz")
        url = post_m.call_args.args[0]
        assert url.endswith(f"/sandboxes/{sb.sandbox_id}/rollback")
        assert post_m.call_args.kwargs["json"] == {"snapshotID": "snap-xyz"}

    def test_rollback_not_found_raises(self):
        sb = make_sandbox()
        with patch.object(sb._session, "post",
                          return_value=self._requests_response({"message": "not found"}, status=404)):
            with pytest.raises(SandboxNotFoundError):
                sb.rollback("snap-1")

    def test_rollback_closes_httpx_client_so_run_code_rebuilds(self):
        """After rollback, the cached httpx.Client must be dropped so the next
        run_code() lazily opens a fresh connection (the old one points at a
        torn-down jupyter kernel)."""
        sb = make_sandbox()
        mock_client = MagicMock()
        sb._client = mock_client
        with patch.object(sb._session, "post",
                          return_value=self._requests_response({"status": "success"})):
            sb.rollback("snap-1")
        mock_client.close.assert_called_once()
        assert sb._client is None

    def test_rollback_swallows_client_close_errors(self):
        """_client.close() failing should not fail rollback — the goal is just
        to drop the reference so the next call opens a new client."""
        sb = make_sandbox()
        mock_client = MagicMock()
        mock_client.close.side_effect = RuntimeError("already closed")
        sb._client = mock_client
        with patch.object(sb._session, "post",
                          return_value=self._requests_response({"status": "ok"})):
            sb.rollback("snap-1")  # should not raise
        assert sb._client is None

    def test_rollback_when_client_never_built_is_safe(self):
        """If run_code was never called, _client is None — rollback should
        still reset cleanly without trying to close None."""
        sb = make_sandbox()
        assert sb._client is None
        with patch.object(sb._session, "post",
                          return_value=self._requests_response({"status": "ok"})):
            sb.rollback("snap-1")  # must not raise AttributeError on None.close()

    def test_rollback_rebuilds_session_pool(self):
        """The requests.Session is also pooled and recreated post-rollback so
        intermediate proxies that drop idle conns on rollback don't bite us."""
        sb = make_sandbox()
        old_session = sb._session
        with patch.object(old_session, "post",
                          return_value=self._requests_response({"status": "ok"})):
            sb.rollback("snap-1")
        # New session object, not the same instance.
        assert sb._session is not old_session

    def test_rollback_does_not_reset_until_after_response_check(self):
        """If the rollback request itself fails (e.g. 404), we shouldn't have
        reset connections — the sandbox state didn't change, so the old
        client is still valid."""
        sb = make_sandbox()
        mock_client = MagicMock()
        sb._client = mock_client
        with patch.object(sb._session, "post",
                          return_value=self._requests_response({"message": "not found"}, status=404)):
            with pytest.raises(SandboxNotFoundError):
                sb.rollback("snap-1")
        # _client untouched because rollback didn't actually happen
        mock_client.close.assert_not_called()
        assert sb._client is mock_client


# ── Sandbox.clone ─────────────────────────────────────────────────────────────


class TestClone:
    """Tests for ``Sandbox.clone(n, *, concurrency)`` (1.6)."""

    @staticmethod
    def _patch_clone_internals(snapshot_id: str = "snap-test"):
        """Return a context-manager-yielding tuple of (snapshot_mock, create_mock,
        delete_mock). Each ``Sandbox.create`` call returns a freshly minted
        Sandbox instance with a unique ``sandboxID``.
        """
        from contextlib import ExitStack
        from itertools import count

        snap_obj = MagicMock()
        snap_obj.snapshot_id = snapshot_id
        counter = count()

        def _make(*_args, **_kwargs):
            n = next(counter)
            return Sandbox({**SANDBOX_DATA, "sandboxID": f"sb-clone-{n:03d}"},
                           config=make_config())

        stack = ExitStack()
        snap_p = stack.enter_context(
            patch.object(Sandbox, "create_snapshot", return_value=snap_obj)
        )
        create_p = stack.enter_context(
            patch.object(Sandbox, "create", side_effect=_make)
        )
        delete_p = stack.enter_context(
            patch.object(Sandbox, "delete_snapshot")
        )
        return stack, snap_p, create_p, delete_p

    # ─── basic / sequential ──────────────────────────────────────────────────

    def test_clone_default_returns_one(self):
        sb = make_sandbox()
        stack, _snap, create_p, delete_p = self._patch_clone_internals()
        with stack:
            result = sb.clone()
        assert len(result) == 1
        assert create_p.call_count == 1
        delete_p.assert_called_once()

    def test_clone_n_sequential(self):
        sb = make_sandbox()
        stack, _snap, create_p, _delete = self._patch_clone_internals()
        with stack:
            result = sb.clone(n=5)
        assert len(result) == 5
        assert create_p.call_count == 5

    def test_clone_zero(self):
        """n=0 short-circuits — no create calls, snapshot still cleaned up."""
        sb = make_sandbox()
        stack, _snap, create_p, delete_p = self._patch_clone_internals()
        with stack:
            result = sb.clone(n=0)
        assert result == []
        assert create_p.call_count == 0
        delete_p.assert_called_once()

    def test_clone_uses_snapshot_id_as_template(self):
        sb = make_sandbox()
        stack, _snap, create_p, _delete = self._patch_clone_internals(
            snapshot_id="snap-xyz"
        )
        with stack:
            sb.clone(n=2)
        for call in create_p.call_args_list:
            assert call.kwargs["template"] == "snap-xyz"

    def test_clone_deletes_snapshot_on_success(self):
        sb = make_sandbox()
        stack, _snap, _create, delete_p = self._patch_clone_internals(
            snapshot_id="snap-to-clean"
        )
        with stack:
            sb.clone(n=3)
        delete_p.assert_called_once_with("snap-to-clean", config=sb._config)

    def test_clone_deletes_snapshot_even_on_error(self):
        """If a Sandbox.create call raises, the ephemeral snapshot is still
        deleted via the ``finally`` branch."""
        sb = make_sandbox()
        snap_obj = MagicMock()
        snap_obj.snapshot_id = "snap-err"
        with (
            patch.object(Sandbox, "create_snapshot", return_value=snap_obj),
            patch.object(Sandbox, "create", side_effect=ApiError("boom")),
            patch.object(Sandbox, "delete_snapshot") as delete_p,
        ):
            with pytest.raises(ApiError):
                sb.clone(n=2)
        delete_p.assert_called_once_with("snap-err", config=sb._config)

    def test_clone_swallows_delete_snapshot_failure(self):
        """A failing ``delete_snapshot`` is best-effort and must not propagate."""
        sb = make_sandbox()
        stack, _snap, _create, delete_p = self._patch_clone_internals()
        delete_p.side_effect = ApiError("snapshot delete failed")
        with stack:
            result = sb.clone(n=2)  # should not raise
        assert len(result) == 2

    # ─── concurrent ──────────────────────────────────────────────────────────

    def test_clone_concurrent_returns_n(self):
        sb = make_sandbox()
        stack, _snap, create_p, _delete = self._patch_clone_internals()
        with stack:
            result = sb.clone(n=8, concurrency=4)
        assert len(result) == 8
        assert create_p.call_count == 8

    def test_clone_concurrency_caps_at_n(self):
        """``concurrency=10`` with ``n=3`` should not blow up — workers ==
        ``min(n, concurrency) == 3``."""
        sb = make_sandbox()
        stack, _snap, create_p, _delete = self._patch_clone_internals()
        with stack:
            result = sb.clone(n=3, concurrency=10)
        assert len(result) == 3
        assert create_p.call_count == 3

    def test_clone_concurrent_partial_failure_propagates(self):
        """If any clone fails the exception bubbles up, the snapshot is still
        cleaned up via ``finally``, AND every sibling sandbox that succeeded
        is killed before the exception leaves ``clone()``.

        This is the canonical "no resource leak on partial failure" test.
        Previously ``clone()`` returned a partial list silently (or, after
        as_completed broke early, dropped some results entirely). The new
        contract is all-or-nothing: caller either gets *n* sandboxes or
        gets an exception with no orphans left running on the backend.
        """
        sb = make_sandbox()
        snap_obj = MagicMock()
        snap_obj.snapshot_id = "snap-partial"
        # Counter is mutated from worker threads — guard with a lock so the
        # "fail on the 3rd call" trigger is deterministic regardless of how
        # the GIL slices the LOAD/ADD/STORE around ``+= 1``.
        lock = threading.Lock()
        call_count = {"n": 0}
        created: list[Sandbox] = []  # track every Sandbox we hand back

        def _flaky(*_args, **_kwargs):
            with lock:
                call_count["n"] += 1
                idx = call_count["n"]
            if idx == 3:
                raise ApiError("create failed")
            inst = Sandbox(
                {**SANDBOX_DATA, "sandboxID": f"sb-{idx:03d}"},
                config=make_config(),
            )
            with lock:
                created.append(inst)
            return inst

        with (
            patch.object(Sandbox, "create_snapshot", return_value=snap_obj),
            patch.object(Sandbox, "create", side_effect=_flaky),
            patch.object(Sandbox, "delete_snapshot") as delete_p,
            patch.object(Sandbox, "kill") as kill_p,
        ):
            with pytest.raises(ApiError, match="create failed"):
                sb.clone(n=5, concurrency=3)

        # Snapshot got cleaned up exactly once.
        delete_p.assert_called_once()
        # Every sandbox that ``Sandbox.create`` actually returned must have
        # been killed by clone() before it raised. We assert the count
        # matches what _flaky produced — n=5 with the 3rd raising means 4
        # successes (calls 1, 2, 4, 5).
        assert len(created) == 4, f"sanity: _flaky should have produced 4 sandboxes, got {len(created)}"
        assert kill_p.call_count == 4, (
            f"clone() leaked {4 - kill_p.call_count} sandbox(es) on partial failure"
        )

    def test_clone_concurrent_drains_all_futures_on_failure(self):
        """Even if an *early* future fails, clone() must wait for every other
        in-flight future and either kill or return its result. The bug being
        defended against: ``as_completed`` + ``raise`` short-circuits the
        loop, leaving in-flight futures to complete in the background and
        silently leak their sandboxes once the executor's __exit__ joins.
        """
        sb = make_sandbox()
        snap_obj = MagicMock()
        snap_obj.snapshot_id = "snap-drain"

        # Force a deterministic failure ordering: first call fails fast,
        # the rest sleep a bit and succeed. as_completed will yield the
        # failure first; a buggy implementation drops the slow ones.
        lock = threading.Lock()
        call_idx = {"n": 0}
        observed: list[str] = []

        def _mixed(*_args, **_kwargs):
            with lock:
                call_idx["n"] += 1
                idx = call_idx["n"]
            if idx == 1:
                observed.append(f"fail-{idx}")
                raise ApiError("first one fails")
            time.sleep(0.05)  # let the failure win the as_completed race
            inst = Sandbox(
                {**SANDBOX_DATA, "sandboxID": f"sb-{idx:03d}"},
                config=make_config(),
            )
            observed.append(f"ok-{idx}")
            return inst

        with (
            patch.object(Sandbox, "create_snapshot", return_value=snap_obj),
            patch.object(Sandbox, "create", side_effect=_mixed),
            patch.object(Sandbox, "delete_snapshot"),
            patch.object(Sandbox, "kill") as kill_p,
        ):
            with pytest.raises(ApiError, match="first one fails"):
                sb.clone(n=4, concurrency=4)

        # All 4 backend calls must have completed (1 fail + 3 ok); the SDK
        # must not have abandoned any in-flight future.
        ok_count = sum(1 for x in observed if x.startswith("ok"))
        assert ok_count == 3, (
            f"expected all 3 successful futures to be drained, got {ok_count}: {observed}"
        )
        # And every successful sandbox must have been killed.
        assert kill_p.call_count == 3, (
            f"expected 3 cleanup kills for the drained successes, got {kill_p.call_count}"
        )

    def test_clone_concurrent_kill_failure_does_not_mask_original(self):
        """If cleanup kill() itself raises, clone() must still propagate the
        original create failure — best-effort cleanup, never mask the cause."""
        sb = make_sandbox()
        snap_obj = MagicMock()
        snap_obj.snapshot_id = "snap-kill-fail"
        lock = threading.Lock()
        idx = {"n": 0}

        def _one_fails(*_args, **_kwargs):
            with lock:
                idx["n"] += 1
                i = idx["n"]
            if i == 2:
                raise ApiError("creation boom")
            return Sandbox(
                {**SANDBOX_DATA, "sandboxID": f"sb-{i:03d}"},
                config=make_config(),
            )

        with (
            patch.object(Sandbox, "create_snapshot", return_value=snap_obj),
            patch.object(Sandbox, "create", side_effect=_one_fails),
            patch.object(Sandbox, "delete_snapshot"),
            patch.object(Sandbox, "kill", side_effect=RuntimeError("kill boom")),
        ):
            # Original ApiError must propagate, not the RuntimeError from kill().
            with pytest.raises(ApiError, match="creation boom"):
                sb.clone(n=3, concurrency=3)

    def test_clone_concurrency_one_no_threads(self):
        """``concurrency=1`` must not spawn a ThreadPoolExecutor.

        We patch ``ThreadPoolExecutor`` at the import site so any accidental
        instantiation raises immediately.
        """
        sb = make_sandbox()
        stack, _snap, create_p, _delete = self._patch_clone_internals()
        with stack:
            with patch("concurrent.futures.ThreadPoolExecutor",
                       side_effect=AssertionError("must not be used")):
                result = sb.clone(n=4, concurrency=1)
        assert len(result) == 4
        assert create_p.call_count == 4

    def test_clone_n_one_no_threads_even_with_concurrency(self):
        """Single-clone fast path skips the executor."""
        sb = make_sandbox()
        stack, _snap, create_p, _delete = self._patch_clone_internals()
        with stack:
            with patch("concurrent.futures.ThreadPoolExecutor",
                       side_effect=AssertionError("must not be used")):
                result = sb.clone(n=1, concurrency=8)
        assert len(result) == 1
        assert create_p.call_count == 1

    def test_clone_no_longer_accepts_snapshot_name(self):
        """``snapshot_name`` was removed in this revision (it leaked
        implementation detail and forced ephemeral snapshots to share names
        under concurrency)."""
        sb = make_sandbox()
        stack, _snap, _create, _delete = self._patch_clone_internals()
        with stack:
            with pytest.raises(TypeError, match="snapshot_name"):
                sb.clone(n=1, snapshot_name="leaky")  # type: ignore[call-arg]


# ── Templates ─────────────────────────────────────────────────────────────────

class TestTemplateAPI:
    def test_build_uses_current_templates_endpoint(self):
        body = {
            "jobID": "job-001",
            "templateID": "tpl-python",
            "status": "running",
            "phase": "Pulling",
            "progress": 10,
        }
        config = make_config()

        with patch("requests.Session.post", return_value=mock_response(body)) as post:
            job = Template.build(
                template_id="tpl-python",
                image="python:3.11-slim",
                instance_type="default",
                writable_layer_size="1G",
                exposed_ports=[80],
                probe_port=80,
                probe_path="/health",
                cpu_count=2000,
                memory_mb=2048,
                envs={"A": "1"},
                allow_internet_access=True,
                config=config,
            )

        post.assert_called_once_with(
            "http://localhost:3000/templates",
            json={
                "image": "python:3.11-slim",
                "instanceType": "default",
                "writableLayerSize": "1G",
                "exposedPorts": [80],
                "probePort": 80,
                "probePath": "/health",
                "cpu": 2000,
                "memory": 2048,
                "env": ["A=1"],
                "allowInternetAccess": True,
            },
            headers={"Content-Type": "application/json"},
        )
        assert job.job_id == "job-001"
        assert job.template_id == "tpl-python"

    def test_build_forwards_create_from_image_options(self):
        body = {
            "jobID": "job-002",
            "templateID": "tpl-network",
            "status": "accepted",
            "phase": "",
            "progress": 0,
        }
        config = make_config()

        with patch("requests.Session.post", return_value=mock_response(body)) as post:
            Template.build(
                image="registry.example.com/app:latest",
                writable_layer_size="20Gi",
                network_type="tap",
                nodes=["node-a", "10.0.0.12"],
                registry_username="pull-user",
                registry_password="pull-pass",
                command=["/bin/sh", "-c"],
                args=["sleep infinity"],
                dns=["8.8.8.8", "1.1.1.1"],
                allow_out=["172.67.0.0/16"],
                deny_out=["10.0.0.0/8"],
                config=config,
            )

        post.assert_called_once_with(
            "http://localhost:3000/templates",
            json={
                "image": "registry.example.com/app:latest",
                "writableLayerSize": "20Gi",
                "networkType": "tap",
                "nodes": ["node-a", "10.0.0.12"],
                "registryUsername": "pull-user",
                "registryPassword": "pull-pass",
                "command": ["/bin/sh", "-c"],
                "args": ["sleep infinity"],
                "dns": ["8.8.8.8", "1.1.1.1"],
                "allowOut": ["172.67.0.0/16"],
                "denyOut": ["10.0.0.0/8"],
            },
            headers={"Content-Type": "application/json"},
        )

    def test_build_rejects_allow_out_domain_without_deny_all(self):
        with pytest.raises(ApiError, match="must disable public outbound traffic or include '0.0.0.0/0' in deny_out") as exc:
            Template.build(
                image="registry.example.com/app:latest",
                allow_out=["api.example.com"],
                config=make_config(),
            )
        assert exc.value.status_code == 400

    def test_build_accepts_allow_out_domain_when_internet_disabled(self):
        body = {
            "jobID": "job-domain",
            "templateID": "tpl-domain",
            "status": "accepted",
        }

        with patch("requests.Session.post", return_value=mock_response(body)) as post:
            Template.build(
                image="registry.example.com/app:latest",
                allow_out=["api.example.com"],
                allow_internet_access=False,
                config=make_config(),
            )

        sent = post.call_args.kwargs["json"]
        assert sent["allowOut"] == ["api.example.com"]
        assert sent["allowInternetAccess"] is False
        assert "denyOut" not in sent

    def test_build_accepts_allow_out_domain_with_deny_all(self):
        body = {
            "jobID": "job-domain",
            "templateID": "tpl-domain",
            "status": "accepted",
        }
        config = make_config()

        with patch("requests.Session.post", return_value=mock_response(body)) as post:
            Template.build(
                image="registry.example.com/app:latest",
                allow_out=["api.example.com"],
                deny_out=["0.0.0.0/0"],
                config=config,
            )

        sent = post.call_args.kwargs["json"]
        assert sent["allowOut"] == ["api.example.com"]
        assert sent["denyOut"] == ["0.0.0.0/0"]

    def test_template_info_from_dict_handles_empty_aliases(self):
        info = TemplateInfo.from_dict({
            "templateID": "tpl-test",
            "aliases": [],
            "networkType": "tap",
            "allowInternetAccess": True,
        })
        assert info.template_id == "tpl-test"
        assert info.name == ""
        assert info.network_type == "tap"
        assert info.allow_internet_access is True

    def test_template_get_parses_network_fields(self):
        body = {
            "templateID": "tpl-network",
            "status": "READY",
            "networkType": "tap",
            "allowInternetAccess": False,
            "createRequest": {
                "network_type": "tap",
                "cubevs_context": {
                    "allowInternetAccess": False,
                    "allowOut": ["172.67.0.0/16"],
                    "denyOut": ["10.0.0.0/8"],
                },
            },
        }
        config = make_config()

        with patch("requests.Session.get", return_value=mock_response(body)) as get:
            info = Template.get("tpl-network", config=config)

        get.assert_called_once_with(
            "http://localhost:3000/templates/tpl-network",
            params={},
        )
        assert info.template_id == "tpl-network"
        assert info.network_type == "tap"
        assert info.allow_internet_access is False
        assert info.create_request["cubevs_context"]["allowOut"] == ["172.67.0.0/16"]

    def test_build_rejects_unsupported_models(self):
        with pytest.raises(ValueError, match="image is required"):
            Template.build(config=make_config())
        with pytest.raises(ValueError, match="dockerfile"):
            Template.build(image="python:3.11-slim", dockerfile="FROM python", config=make_config())
        with pytest.raises(ValueError, match="start_cmd"):
            Template.build(image="python:3.11-slim", start_cmd="python app.py", config=make_config())

    def test_rebuild_uses_post_templates_template_id(self):
        body = {"jobID": "job-002", "templateID": "tpl-python", "status": "queued"}

        with patch("requests.Session.post", return_value=mock_response(body)) as post:
            job = Template.rebuild("tpl-python", force=True, config=make_config())

        post.assert_called_once_with(
            "http://localhost:3000/templates/tpl-python",
            json={"force": True},
            headers={"Content-Type": "application/json"},
        )
        assert job.job_id == "job-002"

    def test_update_is_not_supported_locally(self):
        with patch("requests.Session.patch") as patch_call:
            with pytest.raises(NotImplementedError, match="does not support"):
                Template.update("tpl-python", name="new-name", config=make_config())

        patch_call.assert_not_called()


# ── Exports ───────────────────────────────────────────────────────────────────

class TestExports:
    def test_command_result_importable(self):
        from cubesandbox import CommandResult  # noqa: F401

    def test_command_result_in_all(self):
        import cubesandbox
        assert "CommandResult" in cubesandbox.__all__
