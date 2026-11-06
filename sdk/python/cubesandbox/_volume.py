# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os
import random
import re
import threading
import time
from dataclasses import dataclass, replace
from typing import Any, IO, Dict, Iterator, Literal, Union, overload

import httpx
from typing_extensions import Self, Unpack

from ._config import Config
from ._exceptions import ApiError, InvalidArgumentException, NotFoundException
from ._volume_config import ApiParams, HttpVersion, ProxyTypes, VolumeApiParams, resolve_http_version
from ._volume_content import DualMethod, VolumeContent, VolumeEntryStat
from ._volume_errors import check_volume_response

MAX_VOLUME_NAME_LEN = 128
_VOLUME_NAME_RE = re.compile(r"^[a-zA-Z0-9_-]+$")
_thread_local = threading.local()


def _resolve_retries(opts: dict[str, Any]) -> int:
    retries = opts.get("retries")
    if retries is None:
        return 3
    if isinstance(retries, bool) or not isinstance(retries, int) or retries < 0:
        raise InvalidArgumentException("retries must be a non-negative integer")
    return retries


def _retry_delay(response: httpx.Response, attempt: int) -> float | None:
    directive = response.headers.get("Should-Retry")
    if directive is None:
        if response.status_code not in (429, 502, 503, 504):
            return None
    elif directive.strip().lower() != "true":
        return None
    value = response.headers.get("Retry-After", "")
    if value.isascii() and value.isdecimal() and len(value) <= 10 and int(value) <= 2147483647:
        return float(value)
    return min(0.1 * 2 ** attempt, 10.0) * (0.5 + random.random() * 0.5)


class _VolumeSession:
    def __init__(self, *, proxy: ProxyTypes | None = None, http_version: str | None = None):
        self.client = httpx.Client(
            proxy=proxy, http2=resolve_http_version(http_version) == "2",
            follow_redirects=False,
        )
        self.logger = None
        self.retries = 3

    def request(self, method: str, url: str, **kwargs: Any) -> httpx.Response:
        kwargs["follow_redirects"] = kwargs.pop("allow_redirects", False)
        timeout = kwargs.get("timeout")
        kwargs["timeout"] = timeout or None
        deadline = time.monotonic() + (timeout or 60.0)
        for attempt in range(self.retries + 1):
            if attempt:
                kwargs["timeout"] = max(deadline - time.monotonic(), 0.001)
            response = self.client.request(method, url, **kwargs)
            if self.logger is not None:
                self.logger.debug("volume management response: %s", response.status_code)
            delay = _retry_delay(response, attempt)
            if attempt == self.retries or delay is None or time.monotonic() + delay >= deadline:
                return response
            response.close()
            time.sleep(delay)
        raise AssertionError("unreachable retry state")

    def post(self, url: str, **kwargs: Any) -> httpx.Response:
        return self.request("POST", url, **kwargs)

    def get(self, url: str, **kwargs: Any) -> httpx.Response:
        return self.request("GET", url, **kwargs)

    def delete(self, url: str, **kwargs: Any) -> httpx.Response:
        return self.request("DELETE", url, **kwargs)


def _shared_session(opts: dict[str, Any] | None = None) -> _VolumeSession:
    opts = opts or {}
    proxy, version = opts.get("proxy"), resolve_http_version(opts.get("http_version"))
    key = (str(proxy), version)
    sessions = getattr(_thread_local, "sessions", None)
    if sessions is None:
        sessions = {}
        _thread_local.sessions = sessions
    if key not in sessions:
        if len(sessions) >= 4:
            _, oldest = sessions.popitem()
            oldest.client.close()
        sessions[key] = _VolumeSession(proxy=proxy, http_version=version)
    session = sessions[key]
    session.logger = opts.get("logger")
    session.retries = _resolve_retries(opts)
    return session


def _management_config(config: Config | None, opts: dict[str, Any]) -> Config:
    allowed = set(ApiParams.__annotations__) | {"config", "driver", "logger"}
    unknown = opts.keys() - allowed
    if unknown:
        raise TypeError(f"unexpected volume options: {', '.join(sorted(unknown))}")
    _resolve_retries(opts)
    resolve_http_version(opts.get("http_version"))
    domain = opts.get("domain") or os.environ.get("E2B_DOMAIN") or "e2b.app"
    debug = opts.get("debug")
    if debug is None:
        debug = os.environ.get("E2B_DEBUG", "").lower() == "true"
    cfg = config or Config(
        api_url=os.environ.get("CUBE_API_URL") or os.environ.get("E2B_API_URL")
        or ("http://localhost:3000" if debug else f"https://api.{domain}"),
        request_timeout=60,
    )
    return replace(cfg, **{key: opts[key] for key in ("api_url", "api_key", "request_timeout")
                           if opts.get(key) is not None})


def _auth_headers(cfg: Config, opts: dict[str, Any]) -> dict[str, str]:
    return {**({"X-API-Key": cfg.api_key} if cfg.api_key else {}),
            **(opts.get("headers") or {}), **(opts.get("api_headers") or {})}


def _validate_name(name: str) -> None:
    if name and (len(name) > MAX_VOLUME_NAME_LEN or not _VOLUME_NAME_RE.fullmatch(name)):
        raise ValueError("volume name must contain at most 128 letters, digits, '_' or '-'")


def _validate_volume_id(volume_id: str) -> str:
    if not isinstance(volume_id, str) or not _VOLUME_NAME_RE.fullmatch(volume_id):
        raise ValueError("volume_id must contain letters, digits, '_' or '-'")
    return volume_id


@dataclass
class VolumeInfo:
    volume_id: str
    name: str

    @classmethod
    def from_dict(cls, data: dict) -> Self:
        if not isinstance(data, dict):
            raise ValueError("invalid volume response")
        volume_id = _validate_volume_id(data.get("volumeID", data.get("volume_id")))
        name = data.get("name")
        if not isinstance(name, str):
            raise ValueError("invalid volume response name")
        if issubclass(cls, VolumeAndToken):
            token = data.get("token", "")
            if not isinstance(token, str):
                raise ValueError("invalid volume response token")
            domain = data.get("domain")
            return cls(volume_id=volume_id, name=name, token=token,
                       domain=domain if isinstance(domain, str) and domain else None)
        return cls(volume_id=volume_id, name=name)

    @classmethod
    def from_response(cls, response: httpx.Response) -> Self:
        try:
            return cls.from_dict(response.json())
        except (ValueError, TypeError) as exc:
            raise ApiError("Invalid volume response", response.status_code) from exc


@dataclass(repr=False)
class VolumeAndToken(VolumeInfo):
    token: str
    domain: str | None = None

    def __repr__(self) -> str:
        return (f"VolumeAndToken(volume_id={self.volume_id!r}, name={self.name!r}, "
                f"token={'***' if self.token else ''!r}, domain={self.domain!r})")


@dataclass(frozen=True)
class VolumeMount:
    volume: "Volume | VolumeInfo | str"
    read_only: bool = False


VolumeMountsArg = Dict[str, Union["Volume", VolumeInfo, str, VolumeMount]]


def _resolve_volume_id(vol: "Volume | VolumeInfo | str") -> str:
    if isinstance(vol, (Volume, VolumeInfo)):
        return vol.volume_id
    if isinstance(vol, str):
        return vol
    raise ValueError("volume_mounts mapping value must be a Volume, VolumeInfo or volume-id string")


def _validate_mount_path(path: str) -> str:
    if not isinstance(path, str) or not path.startswith("/"):
        raise ValueError("volume mount path must be absolute")
    if any(seg in (".", "..") for seg in path.split("/")):
        raise ValueError("volume mount path must not contain '.' or '..' segments")
    return path


def _serialize_volume_mounts(mounts: VolumeMountsArg) -> list[dict[str, object]]:
    serialized: list[dict[str, object]] = []
    for path, value in mounts.items():
        volume = value.volume if isinstance(value, VolumeMount) else value
        item: dict[str, object] = {"name": _resolve_volume_id(volume),
                                  "path": _validate_mount_path(str(path))}
        if isinstance(value, VolumeMount) and value.read_only:
            item["readOnly"] = True
        serialized.append(item)
    return serialized


class Volume:
    """E2B-compatible volume management and direct content operations; no sandbox is created."""

    _bound_api_params: ApiParams = {}

    def __init__(self, volume_id: str, name: str, token: str | None = None,
                 domain: str | None = None, debug: bool | None = None,
                 proxy: ProxyTypes | None = None, *, http_version: HttpVersion | None = None,
                 config: Config | None = None):
        self._volume_id = volume_id
        self._name = name
        self._token = token
        self._domain = domain
        self._debug = debug
        self._proxy = proxy
        self._http_version = http_version
        self._config = config or Config(request_timeout=60)

    @property
    def volume_id(self) -> str:
        return self._volume_id

    @property
    def name(self) -> str:
        return self._name

    @property
    def token(self) -> str | None:
        return self._token

    def __repr__(self) -> str:
        return (f"Volume(volume_id={self._volume_id!r}, name={self._name!r}, "
                f"token={'***' if self._token else self._token!r})")

    def __eq__(self, other: object) -> bool:
        if isinstance(other, (Volume, VolumeInfo)):
            return (self.volume_id, self.name, self.token) == (
                other.volume_id, other.name, getattr(other, "token", None))
        return NotImplemented

    def __hash__(self) -> int:
        return hash((self.volume_id, self.name, self.token))

    @classmethod
    def _resolve_api_params(cls, **opts: Unpack[ApiParams]) -> dict[str, Any]:
        return {**cls._bound_api_params, **{k: v for k, v in opts.items() if v is not None}}

    @classmethod
    def _from_info(cls, info: VolumeAndToken, config: Config, **opts: Any) -> Self:
        domain = info.domain or opts.get("domain") or os.environ.get("E2B_DOMAIN") or "e2b.app"
        debug = opts.get("debug")
        if debug is None:
            debug = os.environ.get("E2B_DEBUG", "").lower() == "true"
        volume = cls(info.volume_id, info.name, info.token, domain, debug, opts.get("proxy"),
                     http_version=resolve_http_version(opts.get("http_version")))
        volume._config = config
        return volume

    @classmethod
    def create(cls, name: str, **opts: Unpack[ApiParams]) -> Self:
        opts = cls._resolve_api_params(**opts)
        cfg = _management_config(opts.get("config"), opts)
        _validate_name(name)
        payload = {"name": name}
        if opts.get("driver"):
            payload["driver"] = opts["driver"]
        resp = _shared_session(opts).post(
            f"{cfg.api_url.rstrip('/')}/volumes", json=payload,
            headers={"Content-Type": "application/json", **_auth_headers(cfg, opts)},
            timeout=cfg.request_timeout, allow_redirects=False,
        )
        check_volume_response(resp.status_code, resp.content)
        return cls._from_info(VolumeAndToken.from_response(resp), cfg, **opts)

    @classmethod
    def _class_list(cls, **opts: Unpack[ApiParams]) -> list[VolumeInfo]:
        opts = cls._resolve_api_params(**opts)
        cfg = _management_config(opts.get("config"), opts)
        resp = _shared_session(opts).get(
            f"{cfg.api_url.rstrip('/')}/volumes", headers=_auth_headers(cfg, opts),
            timeout=cfg.request_timeout, allow_redirects=False,
        )
        check_volume_response(resp.status_code, resp.content)
        try:
            data = resp.json()
            if isinstance(data, dict):
                if "volumes" in data:
                    data = data["volumes"]
                elif "items" in data:
                    data = data["items"]
            if not isinstance(data, list):
                raise ValueError("expected volume list")
            return [VolumeInfo.from_dict(d) for d in data]
        except (ValueError, TypeError) as exc:
            raise ApiError("Invalid volume list response", resp.status_code) from exc

    @classmethod
    def _class_get_info(cls, volume_id: str, **opts: Unpack[ApiParams]) -> VolumeAndToken:
        opts = cls._resolve_api_params(**opts)
        cfg = _management_config(opts.get("config"), opts)
        _validate_volume_id(volume_id)
        resp = _shared_session(opts).get(
            f"{cfg.api_url.rstrip('/')}/volumes/{volume_id}", headers=_auth_headers(cfg, opts),
            timeout=cfg.request_timeout, allow_redirects=False,
        )
        check_volume_response(resp.status_code, resp.content, volume_id=volume_id)
        return VolumeAndToken.from_response(resp)

    @classmethod
    def connect(cls, volume_id: str, **opts: Unpack[ApiParams]) -> Self:
        opts = cls._resolve_api_params(**opts)
        cfg = _management_config(opts.get("config"), opts)
        info = cls.get_info(volume_id, **opts)
        volume = cls._from_info(info, cfg, **opts)
        volume._volume_id = volume_id
        return volume

    @classmethod
    def destroy(cls, volume_id: str, **opts: Unpack[ApiParams]) -> bool:
        opts = cls._resolve_api_params(**opts)
        cfg = _management_config(opts.get("config"), opts)
        _validate_volume_id(volume_id)
        resp = _shared_session(opts).delete(
            f"{cfg.api_url.rstrip('/')}/volumes/{volume_id}", headers=_auth_headers(cfg, opts),
            timeout=cfg.request_timeout, allow_redirects=False,
        )
        if resp.status_code == 404:
            return False
        check_volume_response(resp.status_code, resp.content)
        return True

    def _content(self, **opts: Unpack[VolumeApiParams]) -> VolumeContent:
        _validate_volume_id(self.volume_id)
        unknown = opts.keys() - VolumeApiParams.__annotations__.keys()
        if unknown:
            raise TypeError(f"unexpected volume content options: {', '.join(sorted(unknown))}")
        domain = opts.get("domain") or self._domain or os.environ.get("E2B_DOMAIN") or "e2b.app"
        debug = opts.get("debug") if opts.get("debug") is not None else self._debug
        if debug is None:
            debug = os.environ.get("E2B_DEBUG", "").lower() == "true"
        url = opts.get("api_url") or self._config.volume_api_url or (
            "http://localhost:8080" if debug else f"https://api.{domain}")
        config = replace(self._config, volume_api_url=url)
        return VolumeContent(
            self.volume_id, opts.get("token") or self.token, config, headers=opts.get("headers"),
            proxy=opts.get("proxy") if opts.get("proxy") is not None else self._proxy,
            http_version=opts.get("http_version") if opts.get("http_version") is not None else self._http_version,
            logger=opts.get("logger"),
        )

    def _instance_list(self, path: str, depth: int | None = None,
                       **opts: Unpack[VolumeApiParams]) -> list[VolumeEntryStat]:
        return self._content(**opts).list(path, depth, request_timeout=opts.get("request_timeout"))

    def _instance_get_info(self, path: str, **opts: Unpack[VolumeApiParams]) -> VolumeEntryStat:
        return self._content(**opts).get_info(path, request_timeout=opts.get("request_timeout"))

    list = DualMethod(_class_list, _instance_list)
    get_info = DualMethod(_class_get_info, _instance_get_info)

    @overload
    def read_file(self, path: str, format: Literal["text"] = "text",
                  **opts: Unpack[VolumeApiParams]) -> str: ...

    @overload
    def read_file(self, path: str, format: Literal["bytes"],
                  **opts: Unpack[VolumeApiParams]) -> bytes: ...

    @overload
    def read_file(self, path: str, format: Literal["stream"], stream_idle_timeout: float | None = None,
                  **opts: Unpack[VolumeApiParams]) -> Iterator[bytes]: ...

    def read_file(self, path: str, format: Literal["text", "bytes", "stream"] = "text",
                  stream_idle_timeout: float | None = None,
                  **opts: Unpack[VolumeApiParams]) -> str | bytes | Iterator[bytes]:
        """Read directly. Like E2B sync, stream_idle_timeout is ignored; transport idle timeout is 60s."""
        return self._content(**opts).read_file(path, format, request_timeout=opts.get("request_timeout"))

    def write_file(self, path: str, data: str | bytes | IO, uid: int | None = None,
                   gid: int | None = None, mode: int | None = None, force: bool | None = None,
                   **opts: Unpack[VolumeApiParams]) -> VolumeEntryStat:
        return self._content(**opts).write_file(path, data, uid=uid, gid=gid, mode=mode,
                                               force=force, request_timeout=opts.get("request_timeout"))

    def make_dir(self, path: str, uid: int | None = None, gid: int | None = None,
                 mode: int | None = None, force: bool | None = None,
                 **opts: Unpack[VolumeApiParams]) -> VolumeEntryStat:
        return self._content(**opts).make_dir(path, uid=uid, gid=gid, mode=mode,
                                             force=force, request_timeout=opts.get("request_timeout"))

    def exists(self, path: str, **opts: Unpack[VolumeApiParams]) -> bool:
        try:
            self.get_info(path, **opts)
            return True
        except NotFoundException:
            return False

    def update_metadata(self, path: str, uid: int | None = None, gid: int | None = None,
                        mode: int | None = None, **opts: Unpack[VolumeApiParams]) -> VolumeEntryStat:
        return self._content(**opts).update_metadata(path, uid=uid, gid=gid, mode=mode,
                                                    request_timeout=opts.get("request_timeout"))

    def remove(self, path: str, **opts: Unpack[VolumeApiParams]) -> None:
        self._content(**opts).remove(path, request_timeout=opts.get("request_timeout"))
