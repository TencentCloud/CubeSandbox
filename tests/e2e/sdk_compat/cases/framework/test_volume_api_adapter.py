# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Hermetic checks for v2 volume routing and the new cases' failure cleanup."""

from __future__ import annotations

import inspect
from dataclasses import replace
from unittest.mock import Mock

import pytest
import requests

from adapters import api_adapter
from cases.volume import test_private_data as volume_cases
from framework.config import SdkE2EConfig

pytestmark = pytest.mark.framework


@pytest.fixture
def volume_api(monkeypatch):
    session = Mock()
    session.headers = {}
    monkeypatch.setattr(api_adapter.requests, "Session", lambda: session)
    config = SdkE2EConfig.from_env(cube_api_url="https://cubeapi.example")
    api = api_adapter.ApiClient(config)
    try:
        yield api, session, config
    finally:
        api.close()


def _response(status, body=b'{"volumeID":"test-volume"}'):
    response = requests.Response()
    response.status_code = status
    response._content = body
    response.encoding = "utf-8"
    return response


@pytest.mark.parametrize(
    ("kwargs", "path"),
    [
        ({}, "/volumes"),
        ({"private_data": None}, "/volumes"),
        ({"private_data": ""}, "/v2/volumes"),
        ({"private_data": '  {"opaque": "quoted"}\n '}, "/v2/volumes"),
    ],
    ids=["omitted", "none", "empty-v2", "opaque-v2"],
)
def test_create_volume_preserves_route_and_payload(volume_api, kwargs, path):
    api, session, config = volume_api
    session.post.return_value = _response(201)
    assert api.create_volume("test-volume", driver="plugin-alias", **kwargs) == {
        "volumeID": "test-volume"
    }
    expected = {"name": "test-volume", "driver": "plugin-alias"}
    if kwargs.get("private_data") is not None:
        expected["private_data"] = kwargs["private_data"]
    session.post.assert_called_once_with(
        config.cube_api_url + path, json=expected, timeout=config.api_timeout
    )


@pytest.mark.parametrize("status", [404, 500])
def test_create_volume_does_not_fall_back_from_v2(volume_api, status):
    api, session, config = volume_api
    session.post.return_value = _response(status, b'{"message":"unavailable"}')
    with pytest.raises(RuntimeError, match=f"HTTP {status}"):
        api.create_volume("test-volume", private_data="opaque")
    session.post.assert_called_once_with(
        config.cube_api_url + "/v2/volumes",
        json={"name": "test-volume", "private_data": "opaque"},
        timeout=config.api_timeout,
    )


@pytest.mark.parametrize("status", [400, 500])
def test_raw_create_returns_error_response_without_local_validation(volume_api, status):
    api, session, _ = volume_api
    response = _response(status)
    session.post.return_value = response
    private_data = "a" * 1025 + "\x00\U0001f600"
    assert api.create_volume_raw("test-volume", private_data=private_data) is response
    assert session.post.call_args.kwargs["json"]["private_data"] == private_data
    session.post.assert_called_once()


def test_raw_create_propagates_transport_exception(volume_api):
    api, session, _ = volume_api
    failure = requests.Timeout("create timed out")
    session.post.side_effect = failure
    with pytest.raises(requests.Timeout) as raised:
        api.create_volume_raw("test-volume", private_data="opaque")
    assert raised.value is failure
    session.post.assert_called_once()


@pytest.mark.parametrize("body", [b"[]", b"not-json"])
def test_create_volume_keeps_success_body_validation(volume_api, body):
    api, session, _ = volume_api
    session.post.return_value = _response(201, body)
    with pytest.raises((RuntimeError, requests.exceptions.JSONDecodeError)):
        api.create_volume("test-volume")


@pytest.mark.parametrize("cred_ref", [None, "", " \t "])
def test_dynamic_credential_gate_skips_only_missing_reference(cred_ref):
    config = replace(SdkE2EConfig.from_env(), volume_cred_ref=cred_ref)
    with pytest.raises(pytest.skip.Exception, match="SDK_E2E_VOLUME_CRED_REF"):
        volume_cases.dynamic_volume_cred_ref.__wrapped__(config)


def test_dynamic_credential_gate_does_not_validate_or_skip_bad_reference():
    config = replace(SdkE2EConfig.from_env(), volume_cred_ref=" missing-on-server ")
    assert volume_cases.dynamic_volume_cred_ref.__wrapped__(config) == "missing-on-server"
    for case in (
        volume_cases.test_v2_volume_data_survives_sandbox_recreation,
        volume_cases.test_v2_volume_delete_preserves_sibling_prefix,
    ):
        assert "dynamic_volume_cred_ref" in inspect.signature(case).parameters
    assert "dynamic_volume_cred_ref" not in inspect.signature(
        volume_cases.test_v2_volume_rejects_invalid_private_data
    ).parameters


@pytest.mark.parametrize("has_primary_failure", [False, True])
def test_cleanup_orders_resources_and_reports_errors(monkeypatch, has_primary_failure):
    calls = []
    api = Mock()
    api.close.side_effect = lambda: calls.append("close")

    def kill(adapter, config):
        calls.append(f"kill:{adapter}")
        return [f"{adapter} kill error"]

    def delete(client, volume_id):
        assert client is api
        calls.append(f"delete:{volume_id}")
        return [f"{volume_id} delete error"]

    monkeypatch.setattr(volume_cases, "safe_kill", kill)
    monkeypatch.setattr(volume_cases, "safe_delete_volume", delete)
    failure = AssertionError("original failure") if has_primary_failure else None
    if failure is not None:
        volume_cases._cleanup(api, None, ["writer", "reader"], ["a", "b"], failure)
        assert str(failure) == "original failure"
        assert "reader kill error" in " ".join(failure.__notes__)
        assert "a delete error" in " ".join(failure.__notes__)
    else:
        with pytest.raises(pytest.fail.Exception, match="volume cleanup failed"):
            volume_cases._cleanup(api, None, ["writer", "reader"], ["a", "b"], None)
    assert calls == ["kill:reader", "kill:writer", "delete:b", "delete:a", "close"]


def test_known_created_volume_is_registered_before_parsing():
    volumes = []
    with pytest.raises(requests.exceptions.JSONDecodeError):
        volume_cases._remember_created_volume(_response(201, b"not-json"), "own-name", volumes)
    assert volumes == ["own-name"]


def test_unknown_create_result_is_diagnostic_and_does_not_delete(volume_api):
    api, session, config = volume_api
    failure = requests.Timeout("create timed out")
    session.post.side_effect = failure
    with pytest.raises(requests.Timeout) as raised:
        volume_cases._post_volume(
            api, config, "own-name", "opaque", prefix="sdk-e2e/own-prefix/"
        )
    assert raised.value is failure
    assert "own-name" in " ".join(failure.__notes__)
    assert "own-prefix" in " ".join(failure.__notes__)
    assert "outcome is unknown" in " ".join(failure.__notes__)
    session.delete.assert_not_called()


def test_mismatched_created_id_is_not_registered_for_cleanup():
    volumes = []
    with pytest.raises(AssertionError, match="different volume ID"):
        volume_cases._remember_created_volume(
            _response(201, b'{"volumeID":"another-volume"}'), "own-name", volumes
        )
    assert volumes == ["own-name"]
