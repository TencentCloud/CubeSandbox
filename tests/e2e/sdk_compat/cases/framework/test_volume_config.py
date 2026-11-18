# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

from types import SimpleNamespace

import pytest
import requests

from framework.volume import probe_volume_plugin

pytestmark = pytest.mark.framework


class _Response:
    def __init__(self, status_code: int, body=None, text: str = "") -> None:
        self.status_code = status_code
        self._body = body
        self.text = text

    def json(self):
        return self._body


class _InvalidJsonResponse(_Response):
    def json(self):
        raise ValueError("invalid JSON")


@pytest.fixture
def config():
    return SimpleNamespace(volume_driver="s3")


def test_probe_skips_when_configured_driver_is_absent(monkeypatch, config):
    api = SimpleNamespace(
        create_volume_raw=lambda name, driver: _Response(
            400,
            {"code": 400, "message": "unknown driver: s3"},
            "unknown driver: s3",
        ),
        close=lambda: None,
    )
    monkeypatch.setattr("framework.volume.ApiClient", lambda unused: api)

    result = probe_volume_plugin(config)

    assert not result.available
    assert "'s3' is not configured" in result.reason


def test_probe_skips_when_configured_driver_config_is_absent(monkeypatch, config):
    message = (
        'CubeMaster returned error code 130593: plugin create error: binary plugin "s3" '
        'create: exit error: exit status 1: [cube-volume-s3] ERROR: open config '
        '"/usr/local/services/cubetoolbox/CubeMaster/plugin/volume-s3.conf": '
        "no such file or directory"
    )
    api = SimpleNamespace(
        create_volume_raw=lambda name, driver: _Response(
            500,
            {"code": 500, "message": message},
            message,
        ),
        close=lambda: None,
    )
    monkeypatch.setattr("framework.volume.ApiClient", lambda unused: api)

    result = probe_volume_plugin(config)

    assert not result.available
    assert "'s3' is not configured" in result.reason
    assert "config file is absent" in result.reason


def test_probe_does_not_hide_other_missing_files(monkeypatch, config):
    message = (
        'plugin create error: binary plugin "s3" create: '
        'open credentials "/run/secrets/s3": no such file or directory'
    )
    api = SimpleNamespace(
        create_volume_raw=lambda name, driver: _Response(
            500,
            {"code": 500, "message": message},
            message,
        ),
        close=lambda: None,
    )
    monkeypatch.setattr("framework.volume.ApiClient", lambda unused: api)

    with pytest.raises(RuntimeError, match="HTTP 500"):
        probe_volume_plugin(config)


def test_probe_reports_malformed_error_messages(monkeypatch, config):
    api = SimpleNamespace(
        create_volume_raw=lambda name, driver: _Response(
            400,
            {"code": 400, "message": None},
            '{"code":400,"message":null}',
        ),
        close=lambda: None,
    )
    monkeypatch.setattr("framework.volume.ApiClient", lambda unused: api)

    with pytest.raises(RuntimeError, match="HTTP 400"):
        probe_volume_plugin(config)


def test_probe_skips_when_volume_api_is_unavailable(monkeypatch, config):
    api = SimpleNamespace(
        create_volume_raw=lambda name, driver: _Response(404, {}, "not found"),
        close=lambda: None,
    )
    monkeypatch.setattr("framework.volume.ApiClient", lambda unused: api)

    result = probe_volume_plugin(config)

    assert not result.available
    assert "HTTP 404" in result.reason


def test_probe_does_not_hide_method_rejection(monkeypatch, config):
    api = SimpleNamespace(
        create_volume_raw=lambda name, driver: _Response(405, {}, "method not allowed"),
        close=lambda: None,
    )
    monkeypatch.setattr("framework.volume.ApiClient", lambda unused: api)

    with pytest.raises(RuntimeError, match="HTTP 405"):
        probe_volume_plugin(config)


def test_probe_creates_and_deletes_volume(monkeypatch, config):
    deleted = []
    api = SimpleNamespace(
        create_volume_raw=lambda name, driver: _Response(
            201, {"volumeID": "probe-volume", "name": name}
        ),
        delete_volume=lambda volume_id: deleted.append(volume_id) or 204,
        close=lambda: None,
    )
    monkeypatch.setattr("framework.volume.ApiClient", lambda unused: api)

    result = probe_volume_plugin(config)

    assert result.available
    assert deleted == ["probe-volume"]


def test_probe_does_not_repeat_cleanup_after_cleanup_failure(monkeypatch, config):
    deleted = []
    api = SimpleNamespace(
        create_volume_raw=lambda name, driver: _Response(
            201, {"volumeID": "probe-volume", "name": name}
        ),
        delete_volume=lambda volume_id: deleted.append(volume_id) or 500,
        close=lambda: None,
    )
    monkeypatch.setattr("framework.volume.ApiClient", lambda unused: api)
    monkeypatch.setattr("framework.volume.time.sleep", lambda unused: None)

    with pytest.raises(RuntimeError, match="cleanup failed"):
        probe_volume_plugin(config)

    assert deleted == ["probe-volume"] * 5


def test_probe_does_not_hide_plugin_backend_failure(monkeypatch, config):
    api = SimpleNamespace(
        create_volume_raw=lambda name, driver: _Response(
            500,
            {"code": 500, "message": "plugin create error: backend unavailable"},
            "plugin create error: backend unavailable",
        ),
        close=lambda: None,
    )
    monkeypatch.setattr("framework.volume.ApiClient", lambda unused: api)

    with pytest.raises(RuntimeError, match="HTTP 500"):
        probe_volume_plugin(config)


def test_probe_reports_ambiguous_create_timeout(monkeypatch, config):
    def timeout(name, driver):
        raise requests.Timeout("timed out after the server may have created the volume")

    api = SimpleNamespace(
        create_volume_raw=timeout,
        close=lambda: None,
    )
    monkeypatch.setattr("framework.volume.ApiClient", lambda unused: api)

    with pytest.raises(RuntimeError, match="create outcome is unknown"):
        probe_volume_plugin(config)


@pytest.mark.parametrize(
    "response",
    [_InvalidJsonResponse(201), _Response(201, ["not", "an", "object"])],
)
def test_probe_cleans_up_malformed_success_body(monkeypatch, config, response):
    deleted = []
    api = SimpleNamespace(
        create_volume_raw=lambda name, driver: response,
        delete_volume=lambda volume_id: deleted.append(volume_id) or 204,
        close=lambda: None,
    )
    monkeypatch.setattr("framework.volume.ApiClient", lambda unused: api)

    with pytest.raises(RuntimeError, match="returned (invalid JSON|non-object body)"):
        probe_volume_plugin(config)

    assert len(deleted) == 1
    assert deleted[0].startswith("sdk-e2e-volume-probe-")


def test_probe_cleans_up_success_without_volume_id(monkeypatch, config):
    deleted = []
    api = SimpleNamespace(
        create_volume_raw=lambda name, driver: _Response(201, {"name": name}),
        delete_volume=lambda volume_id: deleted.append(volume_id) or 204,
        close=lambda: None,
    )
    monkeypatch.setattr("framework.volume.ApiClient", lambda unused: api)

    with pytest.raises(RuntimeError, match="returned an invalid volume ID"):
        probe_volume_plugin(config)

    assert len(deleted) == 1
    assert deleted[0].startswith("sdk-e2e-volume-probe-")


def test_probe_accepts_cleanup_after_transient_failure(monkeypatch, config):
    statuses = iter([500, 204])
    deleted = []
    api = SimpleNamespace(
        create_volume_raw=lambda name, driver: _Response(201, {"volumeID": name}),
        delete_volume=lambda volume_id: deleted.append(volume_id) or next(statuses),
        close=lambda: None,
    )
    monkeypatch.setattr("framework.volume.ApiClient", lambda unused: api)
    monkeypatch.setattr("framework.volume.time.sleep", lambda unused: None)

    result = probe_volume_plugin(config)

    assert result.available
    assert len(deleted) == 2
