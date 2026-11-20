from __future__ import annotations

import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest.mock import MagicMock, patch

import pytest

from cubesandbox import ApiError, Sandbox, Volume, VolumeInfo, VolumeAndToken, VolumeMount
from cubesandbox._volume_content import S3VolumeContent
from cubesandbox._config import Config
from cubesandbox._volume import _serialize_volume_mounts


def _config() -> Config:
    return Config(api_url="http://localhost:3000", template_id="tpl-test")


def _sandbox_response() -> MagicMock:
    response = MagicMock()
    response.ok = True
    response.status_code = 201
    response.json.return_value = {
        "sandboxID": "sb-test",
        "templateID": "tpl-test",
    }
    return response


def test_create_serializes_read_only_volume_mount_without_changing_plain_mounts():
    dataset = VolumeInfo(volume_id="dataset-vol", name="dataset")

    with patch("requests.Session.post", return_value=_sandbox_response()) as post:
        Sandbox.create(
            volume_mounts={
                "/data": VolumeMount(dataset, read_only=True),
                "/workspace": "workspace-vol",
            },
            config=_config(),
        )

    assert post.call_args.kwargs["json"]["volumeMounts"] == [
        {"name": "dataset-vol", "path": "/data", "readOnly": True},
        {"name": "workspace-vol", "path": "/workspace"},
    ]


@pytest.mark.parametrize(
    "volume",
    [
        Volume("shared-vol", "shared"),
        VolumeInfo(volume_id="shared-vol", name="shared"),
        "shared-vol",
    ],
)
def test_read_only_volume_mount_preserves_existing_volume_reference_styles(volume):
    with patch("requests.Session.post", return_value=_sandbox_response()) as post:
        Sandbox.create(
            volume_mounts={"/shared": VolumeMount(volume, read_only=True)},
            config=_config(),
        )

    assert post.call_args.kwargs["json"]["volumeMounts"] == [
        {"name": "shared-vol", "path": "/shared", "readOnly": True}
    ]


def test_volume_mount_default_does_not_change_e2b_payload():
    with patch("requests.Session.post", return_value=_sandbox_response()) as post:
        Sandbox.create(
            volume_mounts={"/workspace": VolumeMount("workspace-vol")},
            config=_config(),
        )

    assert post.call_args.kwargs["json"]["volumeMounts"] == [
        {"name": "workspace-vol", "path": "/workspace"}
    ]


def test_serialize_same_volume_preserves_access_mode_per_sandbox():
    volume = VolumeInfo(volume_id="shared-vol", name="shared")

    read_write = _serialize_volume_mounts({"/shared": volume})
    read_only = _serialize_volume_mounts(
        {"/shared": VolumeMount(volume, read_only=True)}
    )

    assert read_write == [{"name": "shared-vol", "path": "/shared"}]
    assert read_only == [
        {"name": "shared-vol", "path": "/shared", "readOnly": True}
    ]


@pytest.mark.parametrize("operation", ["create", "list", "get_info", "destroy"])
def test_volume_management_does_not_forward_api_key_on_redirect(operation):
    captured = []

    class Target(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def do_GET(self):
            captured.append(self.headers.get("X-API-Key"))
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b'{"volumeID":"v1","name":"one"}')

    target = ThreadingHTTPServer(("127.0.0.1", 0), Target)

    class Redirect(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def redirect(self):
            self.rfile.read(int(self.headers.get("Content-Length", 0)))
            self.send_response(303)
            self.send_header("Content-Length", "0")
            self.send_header("Location", f"http://127.0.0.1:{target.server_port}/capture")
            self.end_headers()

        do_GET = do_POST = do_DELETE = redirect

    source = ThreadingHTTPServer(("127.0.0.1", 0), Redirect)
    threads = [threading.Thread(target=server.serve_forever, daemon=True) for server in (target, source)]
    for thread in threads:
        thread.start()
    try:
        config = Config(api_url=f"http://127.0.0.1:{source.server_port}", api_key="management-secret")
        args = () if operation == "list" else ("v1",)
        with pytest.raises(ApiError) as exc:
            getattr(Volume, operation)(*args, config=config)
        assert exc.value.status_code == 303
        assert captured == []
    finally:
        for server in (source, target):
            server.shutdown()
            server.server_close()
        for thread in threads:
            thread.join(timeout=5)


@pytest.mark.parametrize("operation", ["create", "get_info", "connect"])
@pytest.mark.parametrize("body", [None, {}, [], {"volumeID": "../v", "name": "one"}, {"volumeID": 1, "name": "one"}, {"volumeID": "v1", "name": []}])
def test_invalid_volume_management_entry_is_api_error(operation, body):
    response = MagicMock(status_code=200)
    response.json.return_value = body
    with patch("cubesandbox._volume._shared_session") as session:
        session.return_value.post.return_value = response
        session.return_value.get.return_value = response
        with pytest.raises(ApiError) as exc:
            getattr(Volume, operation)("v1", config=_config())
    assert exc.value.status_code == 200


@pytest.mark.parametrize("body", [None, {}, {"error": "failure"}, {"volumes": None}, {"items": {}}, [{}]])
def test_invalid_volume_list_is_not_empty_success(body):
    response = MagicMock(status_code=200)
    response.json.return_value = body
    with patch("cubesandbox._volume._shared_session") as session:
        session.return_value.get.return_value = response
        with pytest.raises(ApiError):
            Volume.list(config=_config())


@pytest.mark.parametrize("body", [[], {"volumes": []}, {"items": []}])
def test_valid_empty_volume_list(body):
    response = MagicMock(status_code=200)
    response.json.return_value = body
    with patch("cubesandbox._volume._shared_session") as session:
        session.return_value.get.return_value = response
        assert Volume.list(config=_config()) == []


def test_e2b_management_kwargs_and_return_models():
    response = MagicMock(status_code=201)
    response.json.return_value = {"volumeID": "v1", "name": "one", "token": "volume-secret", "domain": "byoc.test"}
    opts = {"api_url": "https://management.test", "api_key": "management-secret", "headers": {"X-Correlation-ID": "test"}, "http_version": "1.1"}
    with patch("cubesandbox._volume._shared_session") as session:
        session.return_value.post.return_value = response
        session.return_value.get.return_value = response
        volume = Volume.create("one", **opts)
        assert volume._domain == "byoc.test"
        call = session.return_value.post.call_args
        assert call.args == ("https://management.test/volumes",)
        assert call.kwargs["json"] == {"name": "one"}
        assert call.kwargs["headers"] == {"Content-Type": "application/json", "X-API-Key": "management-secret", "X-Correlation-ID": "test"}
        info = Volume.get_info("v1", **opts)
        assert type(info) is VolumeAndToken
        assert vars(info) == {"volume_id": "v1", "name": "one", "token": "volume-secret", "domain": "byoc.test"}
        assert Volume.connect("v1", **opts)._domain == "byoc.test"
        response.json.return_value = [{"volumeID": "v1", "name": "one", "token": "must-not-return", "domain": "must-not-return"}]
        entries = Volume.list(**opts)
        assert type(entries[0]) is VolumeInfo
        assert vars(entries[0]) == {"volume_id": "v1", "name": "one", "token": ""}


def test_direct_content_uses_cached_s3_credentials_not_create_token():
    config = Config(api_url="https://management.test", api_key="management-secret")
    info = VolumeAndToken("v1", "one", "legacy-create-token", "legacy.test")
    response = MagicMock(status_code=200, content=b"")
    response.json.return_value = {
        "provider": "s3", "endpoint": "https://s3.example.test", "bucket": "volume-bucket",
        "region": "test-region", "prefix": "volumes/v1/", "addressing_style": "path",
        "access_key_id": "temporary-id", "secret_access_key": "temporary-secret",
        "session_token": "temporary-session", "expires_at": int(time.time()) + 600,
    }
    with patch("cubesandbox._volume._shared_session") as session, \
            patch.object(S3VolumeContent, "_new_client", return_value=object()):
        session.return_value.post.return_value = response
        volume = Volume._from_info(info, config)
        first = volume._content(request_timeout=0)
        second = volume._content(request_timeout=0)
        assert first is second
        assert first.bucket == "volume-bucket"
        assert first.prefix == "volumes/v1/"
        assert first.addressing_style == "path"
        assert session.return_value.post.call_count == 1
        call = session.return_value.post.call_args
        assert call.args == ("https://management.test/volumes/v1/content-token",)
        assert call.kwargs["timeout"] == 0
        assert call.kwargs["headers"] == {"X-API-Key": "management-secret"}
        assert volume.token == "legacy-create-token"


def test_e2b_positional_constructor_and_content_overrides():
    volume = Volume("v1", "one", "secret", "byoc.test", True, "http://proxy.test", http_version="1.1")
    content = volume._content(api_url="https://override.test", token="replacement", headers={"X-Correlation-ID": "test"})
    assert content.base_url == "https://override.test/volumecontent/v1"
    assert content.token == "replacement"
    assert content.proxy == "http://proxy.test"
    assert content.http_version == "1.1"
    assert content.headers == {"X-Correlation-ID": "test"}


def test_volume_direct_access_requests_credentials_from_management_api():
    response = MagicMock(status_code=200, content=b"{}")
    response.json.return_value = {"unexpected": "legacy gateway token response"}
    with patch("cubesandbox._volume._shared_session") as session:
        session.return_value.post.return_value = response
        with pytest.raises(ApiError, match="Invalid volume storage credential response"):
            Volume("v1", "one", config=Config(api_url="https://management.test"))._content()
    call = session.return_value.post.call_args
    assert call.args == ("https://management.test/volumes/v1/content-token",)
    assert call.kwargs["allow_redirects"] is False
