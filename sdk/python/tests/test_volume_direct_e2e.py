# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import hashlib
import io
import os
import posixpath
import uuid
from contextlib import closing
from datetime import datetime

import httpx
import pytest

from cubesandbox import (
    Config, CubeSandboxError, Sandbox, Volume, VolumeAndToken,
    VolumeEntryStat, VolumeFileType, VolumeInfo, VolumePathNotFoundException,
)
from cubesandbox._volume_content import validate_volume_path

pytestmark = pytest.mark.e2e


@pytest.fixture(scope="module")
def settings(request):
    if not request.config.getoption("--run-e2e"):
        pytest.skip("requires --run-e2e and an existing volume-content gateway")
    required = {
        "CUBE_VOLUME_API_URL": os.environ.get("CUBE_VOLUME_API_URL") or os.environ.get("E2B_VOLUME_API_URL"),
        "CUBE_VOLUME_ID": os.environ.get("CUBE_VOLUME_ID"),
        "CUBE_VOLUME_CONTENT_TOKEN": os.environ.get("CUBE_VOLUME_CONTENT_TOKEN"),
        "CUBE_VOLUME_TEST_PATH": os.environ.get("CUBE_VOLUME_TEST_PATH"),
    }
    missing = [key for key, value in required.items() if not value]
    if missing:
        pytest.fail("missing direct-volume e2e settings: " + ", ".join(missing), pytrace=False)
    validate_volume_path(required["CUBE_VOLUME_TEST_PATH"])
    digest = os.environ.get("CUBE_VOLUME_TEST_SHA256")
    if digest and (len(digest) != 64 or any(c not in "0123456789abcdefABCDEF" for c in digest)):
        pytest.fail("CUBE_VOLUME_TEST_SHA256 must be a 64-character hex digest", pytrace=False)
    return required


@pytest.fixture(autouse=True)
def forbid_sandboxes(settings, monkeypatch):
    def reject(*args, **kwargs):
        pytest.fail("direct-volume e2e must not create or connect a sandbox", pytrace=False)

    monkeypatch.setattr(Sandbox, "create", reject)
    monkeypatch.setattr(Sandbox, "connect", reject)
    monkeypatch.setattr(Sandbox, "__init__", reject)
    original_send = httpx.Client.send

    def send(client, request, *args, **kwargs):
        path = request.url.path
        assert "/volumecontent/" in path or path.endswith("/volumes") or "/volumes/" in path, (
            "direct-volume e2e attempted a non-volume HTTP route"
        )
        return original_send(client, request, *args, **kwargs)

    monkeypatch.setattr(httpx.Client, "send", send)


@pytest.fixture
def volume(settings):
    return Volume(
        settings["CUBE_VOLUME_ID"], "sdk-e2e", settings["CUBE_VOLUME_CONTENT_TOKEN"],
        config=Config(volume_api_url=settings["CUBE_VOLUME_API_URL"]),
    )


def assert_entry(info):
    assert type(info) is VolumeEntryStat
    assert isinstance(info.name, str) and info.path.startswith("/")
    assert isinstance(info.type, VolumeFileType)
    for key in ("size", "mode", "uid", "gid"):
        value = getattr(info, key)
        assert type(value) is int and value >= 0
    for key in ("atime", "mtime", "ctime"):
        assert isinstance(getattr(info, key), datetime)
    assert info.target is None or isinstance(info.target, str)


def test_read_bytes(volume, settings):
    data = volume.read_file(settings["CUBE_VOLUME_TEST_PATH"], format="bytes")
    assert type(data) is bytes
    if os.environ.get("CUBE_VOLUME_TEST_SHA256"):
        assert hashlib.sha256(data).hexdigest() == os.environ["CUBE_VOLUME_TEST_SHA256"].lower()


def test_stream_matches_buffered_read(volume, settings):
    path = settings["CUBE_VOLUME_TEST_PATH"]
    expected = hashlib.sha256(volume.read_file(path, format="bytes")).digest()
    digest = hashlib.sha256()
    with closing(volume.read_file(path, format="stream")) as chunks:
        for chunk in chunks:
            assert type(chunk) is bytes
            digest.update(chunk)
    assert digest.digest() == expected


def test_text_read(volume, settings):
    path = os.environ.get("CUBE_VOLUME_TEXT_PATH")
    if not path:
        pytest.skip("set CUBE_VOLUME_TEXT_PATH to an existing UTF-8 text file")
    raw = volume.read_file(path, format="bytes")
    assert volume.read_file(path) == raw.decode("utf-8")


def test_info_and_exists(volume, settings):
    path = settings["CUBE_VOLUME_TEST_PATH"]
    info = volume.get_info(path)
    assert_entry(info)
    assert info.type is VolumeFileType.FILE
    assert info.size == len(volume.read_file(path, format="bytes"))
    assert volume.exists(path) is True


@pytest.mark.parametrize("depth", [None, 0, 1])
def test_list_directory(volume, settings, depth):
    directory = os.environ.get("CUBE_VOLUME_TEST_DIR") or posixpath.dirname(settings["CUBE_VOLUME_TEST_PATH"]) or "/"
    entries = volume.list(directory, depth=depth)
    assert type(entries) is list
    for entry in entries:
        assert_entry(entry)


def test_missing_path(volume, settings):
    parent = posixpath.dirname(settings["CUBE_VOLUME_TEST_PATH"]) or "/"
    missing = posixpath.join(parent, "sdk-missing-" + uuid.uuid4().hex)
    assert volume.exists(missing) is False
    for action in (volume.read_file, volume.get_info):
        with pytest.raises(VolumePathNotFoundException):
            action(missing)
    with pytest.raises(VolumePathNotFoundException), closing(volume.read_file(missing, format="stream")) as chunks:
        next(chunks)


def test_invalid_token_is_not_missing_path(volume, settings):
    with pytest.raises(CubeSandboxError) as exc:
        volume.exists(settings["CUBE_VOLUME_TEST_PATH"], token="invalid-" + uuid.uuid4().hex)
    assert exc.value.status_code in (401, 403), "gateway must reject invalid volume credentials"


def test_per_call_address_token_and_headers(settings):
    volume = Volume(settings["CUBE_VOLUME_ID"], "sdk-e2e", "unused-token")
    info = volume.get_info(
        settings["CUBE_VOLUME_TEST_PATH"], api_url=settings["CUBE_VOLUME_API_URL"],
        token=settings["CUBE_VOLUME_CONTENT_TOKEN"], headers={"X-Correlation-ID": "sdk-volume-e2e"},
        request_timeout=30, http_version="1.1",
    )
    assert_entry(info)


def test_close_stream_early_and_read_again(volume, settings):
    path = settings["CUBE_VOLUME_TEST_PATH"]
    with closing(volume.read_file(path, format="stream")) as chunks:
        next(chunks, None)
    assert volume.exists(path) is True


def test_existing_management_apis(settings):
    if os.environ.get("CUBE_VOLUME_TEST_MANAGEMENT") != "1":
        pytest.skip("set CUBE_VOLUME_TEST_MANAGEMENT=1 to exercise existing management APIs")
    api_url = os.environ.get("CUBE_API_URL") or os.environ.get("E2B_API_URL")
    if not api_url:
        pytest.fail("management e2e requires explicit CUBE_API_URL or E2B_API_URL", pytrace=False)
    opts = {"api_url": api_url, "retries": 0}
    volume_id = settings["CUBE_VOLUME_ID"]
    entries = Volume.list(**opts)
    assert all(type(entry) is VolumeInfo and set(vars(entry)) == {"volume_id", "name"} for entry in entries)
    assert any(entry.volume_id == volume_id for entry in entries)
    detail = Volume.get_info(volume_id, **opts)
    assert type(detail) is VolumeAndToken and detail.volume_id == volume_id
    connected = Volume.connect(volume_id, **opts)
    assert connected.volume_id == volume_id
    assert_entry(connected.get_info(
        settings["CUBE_VOLUME_TEST_PATH"], api_url=settings["CUBE_VOLUME_API_URL"],
        token=settings["CUBE_VOLUME_CONTENT_TOKEN"],
    ))


@pytest.fixture
def writable_directory(volume):
    if os.environ.get("CUBE_VOLUME_E2E_WRITE") != "1":
        pytest.skip("set CUBE_VOLUME_E2E_WRITE=1 to enable isolated content mutations")
    root = validate_volume_path(os.environ.get("CUBE_VOLUME_WRITE_ROOT", "/"))
    directory = posixpath.join(root, "sdk-e2e-" + uuid.uuid4().hex)
    tracked = []
    created = False
    try:
        assert volume.exists(directory) is False
        info = volume.make_dir(directory, force=False)
        created = True
        assert_entry(info)
        assert info.type is VolumeFileType.DIRECTORY
        yield directory, tracked
    finally:
        if created:
            failures = []
            for path in reversed([directory] + tracked):
                try:
                    volume.remove(path)
                except VolumePathNotFoundException:
                    pass
                except Exception:
                    failures.append(path)
            if failures:
                pytest.fail("e2e cleanup failed; remove test paths manually: " + ", ".join(failures), pytrace=False)


@pytest.mark.parametrize("data_kind", ["text", "bytes", "binary-io", "text-io"])
def test_isolated_content_mutations(volume, writable_directory, data_kind):
    directory, tracked = writable_directory
    path = posixpath.join(directory, "a 空格#内容.bin")
    tracked.append(path)
    data = {
        "text": "hello 中文\n",
        "bytes": b"\x00\xffdirect-volume\n",
        "binary-io": io.BytesIO(b"\x00\xffcaller-owned-file"),
        "text-io": io.StringIO("caller-owned 中文\n"),
    }[data_kind]
    expected = data.getvalue() if hasattr(data, "getvalue") else data
    expected = expected.encode("utf-8") if isinstance(expected, str) else expected
    try:
        info = volume.write_file(path, data, mode=0o600, force=False)
        assert_entry(info)
        assert info.type is VolumeFileType.FILE and info.size == len(expected)
        if hasattr(data, "closed"):
            assert not data.closed
        assert volume.read_file(path, format="bytes") == expected
        with closing(volume.read_file(path, format="stream")) as chunks:
            assert b"".join(chunks) == expected
        assert posixpath.basename(path) in {entry.name for entry in volume.list(directory, depth=1)}
        changed = volume.update_metadata(path, mode=0o640)
        assert_entry(changed)
        assert changed.mode & 0o777 == 0o640
        volume.write_file(path, b"replaced", force=True)
        assert volume.read_file(path, format="bytes") == b"replaced"
        assert volume.remove(path) is None
        assert volume.exists(path) is False
    finally:
        if hasattr(data, "close"):
            data.close()
