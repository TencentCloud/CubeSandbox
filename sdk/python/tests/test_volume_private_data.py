# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from unittest.mock import MagicMock, patch

import pytest

from cubesandbox import Config, Volume, VolumeNotFoundError


def config():
    return Config(api_url="http://127.0.0.1:3000", api_key="test-key", request_timeout=7)


def response(status_code=201):
    result = MagicMock(status_code=status_code, content=b"{}")
    result.json.return_value = {"volumeID": "vol-pd", "name": "vol-pd", "token": "tok"}
    return result


@pytest.mark.parametrize("driver", [None, "", "s3"])
def test_legacy_create_keeps_its_route_and_payload(driver):
    with patch("cubesandbox._volume._shared_session") as session:
        session.return_value.post.return_value = response()
        volume = Volume.create("vol-pd", driver=driver, config=config())
    post = session.return_value.post
    payload = {"name": "vol-pd"}
    if driver:
        payload["driver"] = driver
    assert post.call_count == 1
    assert post.call_args.args == ("http://127.0.0.1:3000/volumes",)
    assert post.call_args.kwargs["json"] == payload
    assert volume.volume_id == "vol-pd"


@pytest.mark.parametrize("value", [
    "", "  opaque 桶 '\" \\ $()\n ", "a" * 1024, "界" * 341 + "a",
    "é中\u2028\uFFFF\uFFFD", r"literal \ud83d\ude00",
])
def test_versioned_create_preserves_opaque_input_and_existing_config(value):
    with patch("cubesandbox._volume._shared_session") as session:
        session.return_value.post.return_value = response()
        volume = Volume.create_with_private_data("vol-pd", driver="s3", private_data=value, config=config())
    post = session.return_value.post
    assert post.call_count == 1
    assert post.call_args.args == ("http://127.0.0.1:3000/v2/volumes",)
    assert post.call_args.kwargs["json"] == {"name": "vol-pd", "driver": "s3", "private_data": value}
    assert post.call_args.kwargs["headers"]["X-API-Key"] == "test-key"
    assert post.call_args.kwargs["timeout"] == 7
    assert volume.volume_id == "vol-pd"
    assert volume.token == "tok"
    assert "private_data" not in vars(volume)


@pytest.mark.parametrize("value", ["a" * 1025, "界" * 342, "😀", "𠀀", "private-data-sentinel\0tail"])
def test_versioned_create_rejects_invalid_input_before_request(value):
    with patch("cubesandbox._volume._shared_session") as session:
        with pytest.raises(ValueError, match="private_data") as error:
            Volume.create_with_private_data(private_data=value, config=config())
    assert value not in str(error.value)
    assert "private-data-sentinel" not in str(error.value)
    session.return_value.post.assert_not_called()


@pytest.mark.parametrize("value", ["\ud800", "before\udfffafter"])
def test_versioned_create_rejects_surrogates_without_echo(value):
    with patch("cubesandbox._volume._shared_session") as session:
        with pytest.raises(ValueError, match="private_data must be valid UTF-8") as error:
            Volume.create_with_private_data(private_data=value, config=config())
    assert value not in str(error.value)
    session.return_value.post.assert_not_called()


@pytest.mark.parametrize("value", [None, {}, [], 123])
def test_versioned_create_requires_a_string(value):
    with patch("cubesandbox._volume._shared_session") as session:
        with pytest.raises(TypeError, match="private_data must be a string"):
            Volume.create_with_private_data(private_data=value, config=config())
    session.return_value.post.assert_not_called()


def test_versioned_create_preserves_name_validation():
    with patch("cubesandbox._volume._shared_session") as session:
        with pytest.raises(ValueError, match="volume name"):
            Volume.create_with_private_data("bad/name", private_data="", config=config())
    session.return_value.post.assert_not_called()


def test_versioned_create_does_not_fallback_to_legacy_api():
    result = response(status_code=404)
    result.content = b'{"message": "route not found"}'
    result.json.return_value = {"message": "route not found"}
    with patch("cubesandbox._volume._shared_session") as session:
        session.return_value.post.return_value = result
        with pytest.raises(VolumeNotFoundError):
            Volume.create_with_private_data(private_data="input-A", config=config())
    post = session.return_value.post
    assert post.call_count == 1
    assert post.call_args.args == ("http://127.0.0.1:3000/v2/volumes",)


@pytest.mark.parametrize("driver", [None, "", "s3"])
def test_legacy_create_without_name_uses_server_generated_uuid(driver):
    with patch("cubesandbox._volume._shared_session") as session:
        session.return_value.post.return_value = response()
        volume = Volume.create(driver=driver, config=config())
    post = session.return_value.post
    payload = {"name": ""} if not driver else {"name": "", "driver": driver}
    assert post.call_args.args == ("http://127.0.0.1:3000/volumes",)
    assert post.call_args.kwargs["json"] == payload
    assert volume.volume_id == "vol-pd"
