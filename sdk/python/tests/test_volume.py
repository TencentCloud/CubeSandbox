# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
Unit tests for the cubesandbox Volume SDK (persistent volumes).

All HTTP calls are intercepted via requests mocks so no real network is needed.
Mirrors the conventions used in test_sandbox.py.
"""

from __future__ import annotations

import json
from unittest.mock import MagicMock, patch

import pytest

from cubesandbox import (
    ApiError,
    Sandbox,
    Volume,
    VolumeInfo,
    VolumeMount,
    VolumeNotFoundError,
)
from cubesandbox._config import Config
from cubesandbox._volume import (
    MAX_VOLUME_NAME_LEN,
    _serialize_volume_mounts,
)

# ── helpers ───────────────────────────────────────────────────────────────────

VOLUME_ID = "my-data"
VOLUME_AND_TOKEN = {"volumeID": VOLUME_ID, "name": VOLUME_ID, "token": "tok-xyz"}
VOLUME_NO_TOKEN = {"volumeID": VOLUME_ID, "name": VOLUME_ID}

SANDBOX_DATA = {
    "sandboxID": "sb-test-001",
    "templateID": "tpl-test",
    "domain": "cube.app",
    "state": "running",
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


# ── POST /volumes ─────────────────────────────────────────────────────────────

class TestVolumeCreate:
    def test_create_success_returns_volume_info(self):
        with patch("requests.Session.post", return_value=mock_response(VOLUME_AND_TOKEN, status=201)):
            vol = Volume.create("my-data", config=make_config())
        assert isinstance(vol, VolumeInfo)
        assert vol.volume_id == VOLUME_ID
        assert vol.name == VOLUME_ID
        assert vol.token == "tok-xyz"

    def test_create_hits_correct_endpoint(self):
        with patch("requests.Session.post", return_value=mock_response(VOLUME_AND_TOKEN, status=201)) as m:
            Volume.create("my-data", config=make_config())
        assert m.call_args.args[0] == "http://localhost:3000/volumes"

    def test_create_sends_name(self):
        with patch("requests.Session.post", return_value=mock_response(VOLUME_AND_TOKEN, status=201)) as m:
            Volume.create("my-data", config=make_config())
        body = m.call_args.kwargs["json"]
        assert body["name"] == "my-data"
        assert "driver" not in body

    def test_create_driver_arg_sends_driver(self):
        with patch("requests.Session.post", return_value=mock_response(VOLUME_AND_TOKEN, status=201)) as m:
            Volume.create("my-data", driver="cos", config=make_config())
        body = m.call_args.kwargs["json"]
        assert body["driver"] == "cos"

    def test_create_with_empty_driver_omits_field(self):
        # An empty (falsy) driver is treated as "unspecified": no driver is
        # sent, so the backend falls back to its first configured plugin.
        with patch("requests.Session.post", return_value=mock_response(VOLUME_AND_TOKEN, status=201)) as m:
            Volume.create("my-data", driver="", config=make_config())
        body = m.call_args.kwargs["json"]
        assert "driver" not in body

    def test_create_with_none_driver_omits_field(self):
        # Explicit driver=None behaves the same as omitting it entirely.
        with patch("requests.Session.post", return_value=mock_response(VOLUME_AND_TOKEN, status=201)) as m:
            Volume.create("my-data", driver=None, config=make_config())
        body = m.call_args.kwargs["json"]
        assert "driver" not in body

    def test_create_driver_without_name_sends_both(self):
        # Pinning a driver while letting the server generate the name: the body
        # carries an empty name plus the driver.
        with patch("requests.Session.post", return_value=mock_response(VOLUME_AND_TOKEN, status=201)) as m:
            Volume.create(driver="cos", config=make_config())
        body = m.call_args.kwargs["json"]
        assert body["name"] == ""
        assert body["driver"] == "cos"

    def test_create_without_name_sends_empty_string(self):
        # Server generates a UUID when name is empty.
        resp = {"volumeID": "d6f...", "name": "d6f...", "token": ""}
        with patch("requests.Session.post", return_value=mock_response(resp, status=201)) as m:
            vol = Volume.create(config=make_config())
        body = m.call_args.kwargs["json"]
        assert body["name"] == ""
        assert vol.token == ""

    def test_create_invalid_name_raises_before_network(self):
        with patch("requests.Session.post") as m:
            with pytest.raises(ValueError, match=r"\^\[a-zA-Z0-9_-\]\+\$"):
                Volume.create("bad name!", config=make_config())
        m.assert_not_called()

    def test_create_too_long_name_raises(self):
        with pytest.raises(ValueError, match="at most"):
            Volume.create("a" * (MAX_VOLUME_NAME_LEN + 1), config=make_config())

    @pytest.mark.parametrize("name", ["ok", "with-dash", "with_underscore", "MixedCase123"])
    def test_create_accepts_valid_names(self, name):
        with patch("requests.Session.post",
                   return_value=mock_response({"volumeID": name, "name": name, "token": ""}, status=201)):
            vol = Volume.create(name, config=make_config())
        assert vol.volume_id == name

    def test_create_bad_request_raises_api_error(self):
        with patch("requests.Session.post",
                   return_value=mock_response({"message": "unknown driver"}, status=400)):
            with pytest.raises(ApiError):
                Volume.create("my-data", driver="nope", config=make_config())

    def test_create_server_error_raises(self):
        with patch("requests.Session.post",
                   return_value=mock_response({"message": "boom"}, status=500)):
            with pytest.raises(ApiError):
                Volume.create("my-data", config=make_config())


# ── GET /volumes ──────────────────────────────────────────────────────────────

class TestVolumeList:
    def test_list_returns_volume_infos(self):
        data = [{"volumeID": "a", "name": "a"}, {"volumeID": "b", "name": "b"}]
        with patch("requests.Session.get", return_value=mock_response(data)):
            vols = Volume.list(config=make_config())
        assert [v.volume_id for v in vols] == ["a", "b"]
        # list responses never carry a token
        assert all(v.token == "" for v in vols)

    def test_list_empty(self):
        with patch("requests.Session.get", return_value=mock_response([])):
            assert Volume.list(config=make_config()) == []

    def test_list_hits_correct_endpoint(self):
        with patch("requests.Session.get", return_value=mock_response([])) as m:
            Volume.list(config=make_config())
        assert m.call_args.args[0] == "http://localhost:3000/volumes"

    def test_list_unwraps_dict_shape(self):
        # tolerate a {"volumes": [...]} envelope
        wrapped = {"volumes": [{"volumeID": "a", "name": "a"}]}
        with patch("requests.Session.get", return_value=mock_response(wrapped)):
            vols = Volume.list(config=make_config())
        assert len(vols) == 1 and vols[0].volume_id == "a"

    def test_list_server_error_raises(self):
        with patch("requests.Session.get", return_value=mock_response({"message": "err"}, status=500)):
            with pytest.raises(ApiError):
                Volume.list(config=make_config())


# ── GET /volumes/{id} ─────────────────────────────────────────────────────────

class TestVolumeGet:
    def test_get_success(self):
        with patch("requests.Session.get", return_value=mock_response(VOLUME_AND_TOKEN)):
            vol = Volume.get(VOLUME_ID, config=make_config())
        assert vol.volume_id == VOLUME_ID
        assert vol.token == "tok-xyz"

    def test_get_hits_correct_endpoint(self):
        with patch("requests.Session.get", return_value=mock_response(VOLUME_AND_TOKEN)) as m:
            Volume.get(VOLUME_ID, config=make_config())
        assert m.call_args.args[0] == f"http://localhost:3000/volumes/{VOLUME_ID}"

    def test_get_not_found_raises(self):
        with patch("requests.Session.get", return_value=mock_response({"message": "not found"}, status=404)):
            with pytest.raises(VolumeNotFoundError):
                Volume.get("ghost", config=make_config())


# ── DELETE /volumes/{id} ──────────────────────────────────────────────────────

class TestVolumeDelete:
    def test_delete_success(self):
        with patch("requests.Session.delete", return_value=mock_response(status=204)) as m:
            Volume.delete(VOLUME_ID, config=make_config())
        assert m.call_args.args[0] == f"http://localhost:3000/volumes/{VOLUME_ID}"

    def test_delete_not_found_raises(self):
        with patch("requests.Session.delete", return_value=mock_response({"message": "not found"}, status=404)):
            with pytest.raises(VolumeNotFoundError):
                Volume.delete("ghost", config=make_config())

    def test_delete_server_error_raises(self):
        with patch("requests.Session.delete", return_value=mock_response({"message": "err"}, status=500)):
            with pytest.raises(ApiError):
                Volume.delete(VOLUME_ID, config=make_config())


# ── VolumeInfo / VolumeMount models ───────────────────────────────────────────

class TestVolumeModels:
    def test_volume_info_from_dict_camel_case(self):
        vol = VolumeInfo.from_dict({"volumeID": "x", "name": "x", "token": "t"})
        assert (vol.volume_id, vol.name, vol.token) == ("x", "x", "t")

    def test_volume_info_from_dict_snake_case_fallback(self):
        vol = VolumeInfo.from_dict({"volume_id": "x", "name": "x"})
        assert vol.volume_id == "x"
        assert vol.token == ""

    def test_volume_info_from_dict_null_token_becomes_empty(self):
        vol = VolumeInfo.from_dict({"volumeID": "x", "name": "x", "token": None})
        assert vol.token == ""

    def test_volume_mount_to_wire(self):
        assert VolumeMount(name="data", path="/workspace").to_wire() == {
            "name": "data", "path": "/workspace",
        }


# ── volume_mounts serialization ───────────────────────────────────────────────

class TestVolumeMountSerialize:
    def test_serialize_typed_mounts(self):
        wire = _serialize_volume_mounts([
            VolumeMount(name="data", path="/workspace"),
            VolumeMount(name="logs", path="/var/log"),
        ])
        assert wire == [
            {"name": "data", "path": "/workspace"},
            {"name": "logs", "path": "/var/log"},
        ]

    def test_serialize_dict_mounts(self):
        wire = _serialize_volume_mounts([{"name": "data", "path": "/workspace"}])
        assert wire == [{"name": "data", "path": "/workspace"}]

    def test_serialize_mixed_typed_and_dict(self):
        wire = _serialize_volume_mounts([
            VolumeMount(name="a", path="/a"),
            {"name": "b", "path": "/b"},
        ])
        assert wire == [{"name": "a", "path": "/a"}, {"name": "b", "path": "/b"}]

    def test_serialize_dict_missing_key_raises(self):
        with pytest.raises(ValueError, match="requires 'name' and 'path'"):
            _serialize_volume_mounts([{"name": "a"}])

    def test_serialize_bad_type_raises(self):
        with pytest.raises(ValueError, match="must be a VolumeMount or a dict"):
            _serialize_volume_mounts(["not-a-mount"])


# ── Sandbox.create(volume_mounts=...) wiring ──────────────────────────────────

class TestSandboxVolumeMounts:
    def test_create_sends_volume_mounts_typed(self):
        with patch("requests.Session.post", return_value=mock_response(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(
                volume_mounts=[VolumeMount(name="my-data", path="/workspace")],
                config=make_config(),
            )
        body = m.call_args.kwargs["json"]
        assert body["volumeMounts"] == [{"name": "my-data", "path": "/workspace"}]

    def test_create_sends_volume_mounts_dict(self):
        with patch("requests.Session.post", return_value=mock_response(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(
                volume_mounts=[{"name": "my-data", "path": "/workspace"}],
                config=make_config(),
            )
        body = m.call_args.kwargs["json"]
        assert body["volumeMounts"] == [{"name": "my-data", "path": "/workspace"}]

    def test_create_without_volume_mounts_omits_field(self):
        with patch("requests.Session.post", return_value=mock_response(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(config=make_config())
        body = m.call_args.kwargs["json"]
        assert "volumeMounts" not in body

    def test_create_empty_volume_mounts_omits_field(self):
        with patch("requests.Session.post", return_value=mock_response(SANDBOX_DATA, status=201)) as m:
            Sandbox.create(volume_mounts=[], config=make_config())
        body = m.call_args.kwargs["json"]
        assert "volumeMounts" not in body


# ── Exports ───────────────────────────────────────────────────────────────────

class TestVolumeExports:
    def test_symbols_in_all(self):
        import cubesandbox
        for name in ("Volume", "VolumeInfo", "VolumeMount", "VolumeNotFoundError"):
            assert name in cubesandbox.__all__
