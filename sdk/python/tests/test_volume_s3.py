from __future__ import annotations

import io
from datetime import datetime, timezone
from typing import Any

import pytest

from cubesandbox import VolumeFileType, VolumePathNotFoundException
from cubesandbox._volume_content import S3VolumeContent


class MissingObject(Exception):
    def __init__(self):
        self.response = {
            "Error": {"Code": "NoSuchKey"},
            "ResponseMetadata": {"HTTPStatusCode": 404},
        }


class FakeS3Client:
    def __init__(self):
        self.objects: dict[str, tuple[bytes, dict[str, str]]] = {}

    def put_object(self, *, Bucket: str, Key: str, Body: Any, Metadata: dict[str, str], **kwargs: Any):
        data = Body if isinstance(Body, bytes) else Body.read()
        if kwargs.get("IfNoneMatch") == "*" and Key in self.objects:
            error = Exception("precondition failed")
            error.response = {"ResponseMetadata": {"HTTPStatusCode": 412}}
            raise error
        self.objects[Key] = (data, Metadata)
        return {"ETag": '"test"'}

    def get_object(self, *, Bucket: str, Key: str):
        if Key not in self.objects:
            raise MissingObject()
        body, _ = self.objects[Key]
        return {"Body": io.BytesIO(body)}

    def head_object(self, *, Bucket: str, Key: str):
        if Key not in self.objects:
            raise MissingObject()
        body, metadata = self.objects[Key]
        return {
            "ContentLength": len(body), "Metadata": metadata,
            "LastModified": datetime(2026, 1, 1, tzinfo=timezone.utc),
        }

    def list_objects_v2(self, *, Bucket: str, Prefix: str, Delimiter: str | None = None,
                        MaxKeys: int | None = None, **kwargs: Any):
        matching = sorted(key for key in self.objects if key.startswith(Prefix))
        if MaxKeys == 1:
            matching = matching[:1]
        common_prefixes: set[str] = set()
        contents = []
        for key in matching:
            remainder = key[len(Prefix):]
            if Delimiter and Delimiter in remainder:
                common_prefixes.add(Prefix + remainder.split(Delimiter, 1)[0] + Delimiter)
            else:
                body, _ = self.objects[key]
                contents.append({"Key": key, "Size": len(body), "LastModified": datetime.now(timezone.utc)})
        return {
            "Contents": contents,
            "CommonPrefixes": [{"Prefix": prefix} for prefix in sorted(common_prefixes)],
            "KeyCount": len(contents) + len(common_prefixes),
            "IsTruncated": False,
        }

    def delete_object(self, *, Bucket: str, Key: str):
        self.objects.pop(Key, None)
        return {}


@pytest.fixture
def credentials() -> dict[str, Any]:
    return {
        "provider": "s3",
        "endpoint": "https://s3.example.test",
        "bucket": "volume-bucket",
        "region": "test-region",
        "prefix": "volumes/vol-s3/",
        "addressing_style": "path",
        "access_key_id": "temporary-id",
        "secret_access_key": "temporary-secret",
        "session_token": "temporary-session",
        "expires_at": 1_800_000_000,
    }


def test_s3_volume_content_reads_writes_lists_and_removes_with_volume_prefix(credentials):
    client = FakeS3Client()
    content = S3VolumeContent("vol-s3", credentials, client=client)

    written = content.write_file("/folder/file.txt", "hello s3", mode=0o600)
    assert written.type is VolumeFileType.FILE
    assert written.size == 8
    assert client.objects["volumes/vol-s3/folder/file.txt"] == (
        b"hello s3", {"mode": "600"}
    )
    assert content.read_file("/folder/file.txt") == "hello s3"
    assert content.read_file("/folder/file.txt", "bytes") == b"hello s3"
    assert b"".join(content.read_file("/folder/file.txt", "stream")) == b"hello s3"
    assert content.get_info("/folder/file.txt").size == 8
    assert [item.path for item in content.list("/")] == ["/folder"]
    assert content.list("/folder")[0].path == "/folder/file.txt"

    content.remove("/folder/file.txt")
    with pytest.raises(VolumePathNotFoundException):
        content.read_file("/folder/file.txt", "bytes")


def test_s3_volume_content_accepts_file_objects_and_force_false(credentials):
    client = FakeS3Client()
    content = S3VolumeContent("vol-s3", credentials, client=client)
    stream = io.BytesIO(b"streamed")

    assert content.write_file("/stream.bin", stream, force=False).size == 8
    assert not stream.closed
    with pytest.raises(FileExistsError):
        content.write_file("/stream.bin", b"replacement", force=False)


def test_s3_volume_content_rejects_credentials_outside_volume_scope(credentials):
    credentials["prefix"] = "volumes/another-volume/"
    with pytest.raises(ValueError, match="scoped"):
        S3VolumeContent("vol-s3", credentials, client=FakeS3Client())


def test_s3_volume_content_rejects_unsafe_endpoint_and_addressing_style(credentials):
    credentials["endpoint"] = "http://s3.example.test"
    with pytest.raises(ValueError, match="HTTPS"):
        S3VolumeContent("vol-s3", credentials, client=FakeS3Client())

    credentials["endpoint"] = "https://s3.example.test"
    credentials["addressing_style"] = "invalid"
    with pytest.raises(ValueError, match="addressing_style"):
        S3VolumeContent("vol-s3", credentials, client=FakeS3Client())
