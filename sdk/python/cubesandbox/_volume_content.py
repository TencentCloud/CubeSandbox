# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import math
import re
from dataclasses import dataclass
from datetime import datetime, timezone
from enum import Enum
from typing import Any, Callable, Iterator
from urllib.parse import urlsplit

import httpx

from ._config import Config
from ._exceptions import (
    ApiError,
    AuthenticationError,
    VolumeContentUnavailableError,
)
from ._volume_config import FILE_TIMEOUT, READ_TIMEOUT, resolve_http_version
from ._volume_errors import check_volume_response


class VolumeFileType(str, Enum):
    DIRECTORY = "directory"
    FILE = "file"
    SYMLINK = "symlink"
    UNKNOWN = "unknown"

    def __str__(self) -> str:
        return self.value


@dataclass
class VolumeEntryStat:
    name: str
    type: VolumeFileType
    path: str
    size: int
    mode: int
    uid: int
    gid: int
    atime: datetime
    mtime: datetime
    ctime: datetime
    target: str | None = None

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> VolumeEntryStat:
        def timestamp(key: str) -> datetime:
            value = data[key]
            if not isinstance(value, str):
                raise ValueError(f"invalid {key}")
            parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
            return parsed if parsed.tzinfo else parsed.replace(tzinfo=timezone.utc)

        def integer(key: str) -> int:
            value = data[key]
            if isinstance(value, bool) or not isinstance(value, int) or value < 0:
                raise ValueError(f"invalid {key}")
            return value

        for key in ("name", "path"):
            if not isinstance(data[key], str):
                raise ValueError(f"invalid {key}")
        validate_volume_path(data["path"])
        target = data.get("target")
        if target is not None and not isinstance(target, str):
            raise ValueError("invalid target")
        return cls(
            name=data["name"], type=VolumeFileType(data["type"]), path=data["path"],
            size=integer("size"), mode=integer("mode"), uid=integer("uid"), gid=integer("gid"),
            atime=timestamp("atime"), mtime=timestamp("mtime"), ctime=timestamp("ctime"),
            target=target,
        )


class DualMethod:
    """Retain class-level volume management alongside instance file methods."""

    def __init__(self, class_method: classmethod, instance_method: Callable[..., Any]):
        self.class_method = class_method
        self.instance_method = instance_method

    def __get__(self, instance: Any, owner: type) -> Callable[..., Any]:
        if instance is None:
            return self.class_method.__get__(None, owner)
        return self.instance_method.__get__(instance, owner)


def validate_volume_path(path: str) -> str:
    if not isinstance(path, str) or not path.startswith("/"):
        raise ValueError("volume path must be an absolute POSIX path")
    if "\x00" in path or "\\" in path or any(s in (".", "..") for s in path.split("/")):
        raise ValueError("volume path must not contain NUL, backslash, '.' or '..' segments")
    return path


def metadata_params(**values: Any) -> dict[str, Any]:
    params: dict[str, Any] = {}
    for key, value in values.items():
        if value is None:
            continue
        if key == "force":
            if not isinstance(value, bool):
                raise ValueError("force must be a bool")
        elif isinstance(value, bool) or not isinstance(value, int) or value < 0:
            raise ValueError(f"{key} must be a non-negative integer")
        params[key] = value
    return params


class VolumeContent:
    """Client for an explicitly configured, E2B-shaped volume-content gateway.

    This is not a sandbox client. The gateway must implement authorization,
    mount ownership and server-side confinement; local path checks are not a
    security boundary against symlinks or concurrent filesystem mutations.
    """

    def __init__(self, volume_id: str, token: str, config: Config, *, headers: dict[str, str] | None = None, proxy: Any = None, http_version: str | None = None, logger: Any = None):
        if not isinstance(volume_id, str) or re.fullmatch(r"[A-Za-z0-9_-]+", volume_id) is None:
            raise ValueError("invalid volume ID")
        raw_url = config.volume_api_url
        if not raw_url:
            raise VolumeContentUnavailableError(
                "Volume content service is not configured. Set CUBE_VOLUME_API_URL "
                "or Config.volume_api_url to a deployed volume-content gateway; "
                "the current CubeAPI only implements volume management."
            )
        try:
            url = urlsplit(raw_url)
            valid_url = url.scheme in ("http", "https") and bool(url.hostname) and url.port != 0
        except (TypeError, ValueError):
            valid_url = False
        if not valid_url or url.username or url.password or "?" in raw_url or "#" in raw_url:
            raise ValueError("volume_api_url must be an HTTP(S) base URL without credentials, query or fragment")
        if not token:
            raise AuthenticationError("A gateway-issued volume content token is required")
        if not isinstance(token, str) or re.fullmatch(r"[A-Za-z0-9._~+/-]+=*", token) is None:
            raise ValueError("volume content token must use the Bearer token character set")
        if url.scheme != "https" and url.hostname not in ("localhost", "127.0.0.1", "::1"):
            raise ValueError("volume content credentials require HTTPS outside loopback")
        self.base_url = f"{url.geturl().rstrip('/')}/volumecontent/{volume_id}"
        self.token = token
        self.config = config
        self.headers = dict(headers or {})
        self.proxy = proxy
        self.http_version = resolve_http_version(http_version)
        self.logger = logger

    def _client(self, request_timeout: float | None, stream: bool = False) -> httpx.Client:
        timeout = self.config.request_timeout if request_timeout is None else request_timeout
        if isinstance(timeout, bool) or not isinstance(timeout, (int, float)) or not math.isfinite(timeout) or timeout < 0:
            raise ValueError("request_timeout must be a finite non-negative number")
        return httpx.Client(
            headers={**self.headers, "Authorization": f"Bearer {self.token}"},
            timeout=httpx.Timeout(timeout or None, read=READ_TIMEOUT if stream and request_timeout is None else timeout or None),
            follow_redirects=False,
            proxy=self.proxy,
            http2=self.http_version == "2",
        )

    def _check_response(self, response: httpx.Response, path: str = "") -> None:
        check_volume_response(response.status_code, response.content, path=path, token=self.token)

    def request(self, method: str, endpoint: str, *, path: str, request_timeout: float | None = None,
                params: dict[str, Any] | None = None, content: Any = None, json: Any = None) -> httpx.Response:
        query = {"path": validate_volume_path(path), **(params or {})}
        with self._client(request_timeout) as client:
            response = client.request(
                method, f"{self.base_url}/{endpoint}", params=query, content=content, json=json,
                headers={"Content-Type": "application/octet-stream"} if content is not None else None,
            )
            if self.logger is not None:
                self.logger.debug("volume content response: %s", response.status_code)
            self._check_response(response, path)
            return response

    @staticmethod
    def entry(response: httpx.Response) -> VolumeEntryStat:
        try:
            return VolumeEntryStat.from_dict(response.json())
        except (ValueError, TypeError, KeyError) as exc:
            raise ApiError("Invalid volume entry response", response.status_code) from exc

    def list(self, path: str, depth: int | None = None, *, request_timeout: float | None = None) -> list[VolumeEntryStat]:
        response = self.request("GET", "dir", path=path, params=metadata_params(depth=depth), request_timeout=request_timeout)
        try:
            items = response.json()
            if not isinstance(items, list):
                raise ValueError("expected entries list")
            return [VolumeEntryStat.from_dict(item) for item in items]
        except (ValueError, TypeError, KeyError) as exc:
            raise ApiError("Invalid volume directory response", response.status_code) from exc

    def read_file(self, path: str, format: str = "text", *, request_timeout: float | None = None) -> str | bytes | Iterator[bytes]:
        validate_volume_path(path)
        if format not in ("text", "bytes", "stream"):
            raise ValueError("format must be 'text', 'bytes' or 'stream'")
        if format == "stream":
            return self._stream_file(path, request_timeout)
        response = self.request("GET", "file", path=path, request_timeout=FILE_TIMEOUT if request_timeout is None else request_timeout)
        return response.content if format == "bytes" else response.text

    def _stream_file(self, path: str, request_timeout: float | None) -> Iterator[bytes]:
        with self._client(request_timeout, stream=True) as client:
            with client.stream("GET", f"{self.base_url}/file", params={"path": path}) as response:
                if not 200 <= response.status_code < 300:
                    body = bytearray()
                    for chunk in response.iter_bytes(chunk_size=4096):
                        if len(body) + len(chunk) > 64 * 1024:
                            body.clear()
                            break
                        body.extend(chunk)
                    bounded = httpx.Response(response.status_code, content=bytes(body))
                    self._check_response(bounded, path)
                yield from response.iter_bytes()

    def write_file(self, path: str, data: Any, *, uid: int | None = None, gid: int | None = None,
                   mode: int | None = None, force: bool | None = None,
                   request_timeout: float | None = None) -> VolumeEntryStat:
        if isinstance(data, str):
            content: Any = data.encode("utf-8")
        elif isinstance(data, bytes):
            content = data
        elif hasattr(data, "read") and callable(data.read):
            def chunks() -> Iterator[bytes]:
                while True:
                    chunk = data.read(64 * 1024)
                    if not chunk:
                        break
                    if isinstance(chunk, str):
                        chunk = chunk.encode("utf-8")
                    if not isinstance(chunk, bytes):
                        raise ValueError("file-like data must return str or bytes")
                    yield chunk
            content = chunks()
        else:
            raise ValueError("data must be str, bytes or a readable file-like object")
        response = self.request(
            "PUT", "file", path=path, content=content,
            params=metadata_params(uid=uid, gid=gid, mode=mode, force=force),
            request_timeout=FILE_TIMEOUT if request_timeout is None else request_timeout,
        )
        return self.entry(response)

    def get_info(self, path: str, *, request_timeout: float | None = None) -> VolumeEntryStat:
        return self.entry(self.request("GET", "path", path=path, request_timeout=request_timeout))

    def make_dir(self, path: str, *, uid: int | None = None, gid: int | None = None,
                 mode: int | None = None, force: bool | None = None,
                 request_timeout: float | None = None) -> VolumeEntryStat:
        return self.entry(self.request(
            "POST", "dir", path=path,
            params=metadata_params(uid=uid, gid=gid, mode=mode, force=force),
            request_timeout=request_timeout,
        ))

    def update_metadata(self, path: str, *, uid: int | None = None, gid: int | None = None,
                        mode: int | None = None, request_timeout: float | None = None) -> VolumeEntryStat:
        return self.entry(self.request(
            "PATCH", "path", path=path, json=metadata_params(uid=uid, gid=gid, mode=mode),
            request_timeout=request_timeout,
        ))

    def remove(self, path: str, *, request_timeout: float | None = None) -> None:
        if validate_volume_path(path).rstrip("/") == "":
            raise ValueError("removing the volume root is not allowed")
        self.request("DELETE", "path", path=path, request_timeout=request_timeout)
