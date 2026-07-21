# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import re
import threading
from dataclasses import dataclass
from typing import Dict, Union

import requests

from ._config import Config
from ._exceptions import ApiError, AuthenticationError, VolumeNotFoundError

# Mirrors CubeAPI's `MAX_VOLUME_NAME_LEN` and the `^[a-zA-Z0-9_-]+$` rule
# enforced in `CubeAPI/src/models/mod.rs` / `handlers/volumes.rs`. Validating
# client-side turns an opaque HTTP 400 into a clean ValueError at the call site.
MAX_VOLUME_NAME_LEN = 128
_VOLUME_NAME_RE = re.compile(r"^[a-zA-Z0-9_-]+$")

# A thread-local session reused across the Volume control-plane classmethods so
# HTTP keep-alive / connection pooling kicks in — otherwise every call paid for
# a fresh TCP (plus TLS) handshake. We keep one session per thread (mirroring
# the e2b SDK) rather than a single global one: requests.Session is not
# guaranteed thread-safe for concurrent use, so thread-local isolation gives the
# pooling benefit without sharing a Session across threads.
_thread_local = threading.local()


def _shared_session() -> requests.Session:
    """Return this thread's :class:`requests.Session` for volume calls."""
    s = getattr(_thread_local, "session", None)
    if s is None:
        s = requests.Session()
        _thread_local.session = s
    return s


def _check_response(resp: requests.Response) -> None:
    if resp.ok:
        return
    try:
        body = resp.json()
        msg = (
            (body.get("message") or body.get("detail") or resp.text)
            if isinstance(body, dict)
            else resp.text
        )
    except (ValueError, requests.JSONDecodeError):
        msg = resp.text or f"HTTP {resp.status_code}"
    code = resp.status_code
    if code in (401, 403):
        raise AuthenticationError(msg, code)
    if code == 404:
        raise VolumeNotFoundError(msg, code)
    raise ApiError(msg, code)


def _auth_headers(cfg: Config) -> dict[str, str]:
    """Return the ``X-API-Key`` header when an API key is configured.

    CubeAPI only enforces auth when it is started with an auth-callback URL;
    in the default deployment no callback is set and every request passes
    through unauthenticated. So the key is optional here: when
    ``CUBE_API_KEY`` / ``Config.api_key`` is unset we send no auth header and
    behavior is unchanged; when set we attach ``X-API-Key: <key>`` so the SDK
    also works against an auth-enabled backend.
    """
    if cfg.api_key:
        return {"X-API-Key": cfg.api_key}
    return {}


def _validate_name(name: str) -> None:
    """Raise ValueError if a non-empty ``name`` violates CubeAPI's constraints.

    An empty ``name`` is allowed — CubeMaster then generates a UUID and uses it
    as both the volume name and volume ID.
    """
    if not name:
        return
    if len(name) > MAX_VOLUME_NAME_LEN:
        raise ValueError(
            f"volume name must be at most {MAX_VOLUME_NAME_LEN} characters, got {len(name)}"
        )
    if not _VOLUME_NAME_RE.match(name):
        raise ValueError(
            "volume name must match ^[a-zA-Z0-9_-]+$ "
            f"(letters, digits, '_' and '-'), got {name!r}"
        )


def _validate_volume_id(volume_id: str) -> str:
    """Raise ``ValueError`` if ``volume_id`` is unsafe to embed in a URL path.

    Defense-in-depth against path traversal: a malicious ``volume_id`` such as
    ``../other`` or ``..%2f`` must not be interpolated into the request URL.
    We accept the same character class as volume names (letters, digits, ``_``,
    ``-``) — this covers both named volumes and auto-generated UUIDs, and
    rejects any ``/`` or segment-breaking character.
    """
    if not isinstance(volume_id, str) or not volume_id:
        raise ValueError(f"volume_id must be a non-empty string, got {volume_id!r}")
    if not _VOLUME_NAME_RE.match(volume_id):
        raise ValueError(
            "volume_id must match ^[a-zA-Z0-9_-]+$ "
            f"(letters, digits, '_' and '-'), got {volume_id!r}"
        )
    return volume_id


@dataclass
class VolumeInfo:
    """A CubeSandbox persistent volume descriptor.

    Attributes:
        volume_id: Stable identifier (equals ``name`` or an auto-generated UUID).
        name: Human-readable display name.
        token: Optional auth token returned by the volume plugin. Empty string
            when the plugin does not issue one, and always empty for entries
            returned by :meth:`Volume.list` (tokens are only surfaced on
            create / get-single).
    """

    volume_id: str
    name: str
    token: str = ""

    @classmethod
    def from_dict(cls, data: dict) -> "VolumeInfo":
        return cls(
            volume_id=data.get("volumeID") or data.get("volume_id", ""),
            name=data.get("name", ""),
            token=data.get("token", "") or "",
        )

    def __repr__(self) -> str:
        # Mask the token so it never leaks into logs / REPL / exception text.
        return (
            f"VolumeInfo(volume_id={self.volume_id!r}, name={self.name!r}, "
            f"token={'***' if self.token else ''!r})"
        )


# ``Sandbox.create(volume_mounts=...)`` argument — e2b-compatible mapping:
# ``{mount_path: Volume | VolumeInfo | volume_id_str}``.
VolumeMountsArg = "Dict[str, Union[Volume, VolumeInfo, str]]"


def _resolve_volume_id(vol: "Volume | VolumeInfo | str") -> str:
    """Resolve the backend ``volumeID`` for a mapping-form volume value.

    Accepts a live :class:`Volume`, a :class:`VolumeInfo`, or a plain
    ``volumeID`` string. e2b keys mounts by ``volume.name``; because our
    backend requires ``volumeMounts[].name`` to be an existing ``volumeID``
    (and ``volume_id == name`` for named volumes) we resolve to ``volume_id``.
    """
    if isinstance(vol, (Volume, VolumeInfo)):
        return vol.volume_id
    if isinstance(vol, str):
        return vol
    raise ValueError(
        "volume_mounts mapping value must be a Volume, VolumeInfo or volume-id "
        f"string, got {type(vol).__name__}"
    )


def _validate_mount_path(path: str) -> str:
    """Validate a mount path before forwarding it to the backend.

    Defense-in-depth against path traversal: the path comes from user-supplied
    ``volume_mounts`` dict keys, so we require a clean absolute POSIX path and
    reject empty values, relative paths and any ``.``/``..`` segment. The
    backend is expected to validate as well; this turns a malicious/typo'd path
    into a clean ``ValueError`` at the call site.
    """
    if not isinstance(path, str) or not path:
        raise ValueError(f"volume mount path must be a non-empty string, got {path!r}")
    if not path.startswith("/"):
        raise ValueError(f"volume mount path must be absolute (start with '/'), got {path!r}")
    segments = [seg for seg in path.split("/") if seg]
    if any(seg in (".", "..") for seg in segments):
        raise ValueError(f"volume mount path must not contain '.' or '..' segments, got {path!r}")
    return path


def _serialize_volume_mounts(mounts: VolumeMountsArg) -> list[dict[str, str]]:
    """Serialize the e2b-style ``{path: volume}`` mapping into the wire format."""
    return [
        {"name": _resolve_volume_id(vol), "path": _validate_mount_path(str(path))}
        for path, vol in mounts.items()
    ]


class Volume:
    """Class-level helper for CubeSandbox persistent-volume management.

    e2b-compatible: methods mirror the e2b SDK. Following e2b, ``create`` and
    ``connect`` return a live :class:`Volume` **instance** (carrying
    ``volume_id`` / ``name`` / ``token``), while ``list`` / ``get_info`` return
    plain :class:`VolumeInfo` data and ``destroy`` returns ``bool``. Volumes are
    e2b-compatible persistent stores backed by a volume plugin (COS, NFS, ...);
    create one here, then mount it into a sandbox via
    ``Sandbox.create(volume_mounts={...})``.

    Example::

        # Create a volume (name auto-generated when omitted) -> Volume instance
        vol = Volume.create("my-data")
        print(vol.volume_id, vol.token)

        # Mount it into a sandbox
        from cubesandbox import Sandbox
        with Sandbox.create(
            volume_mounts={"/workspace": vol},
        ) as sb:
            sb.files.write("/workspace/note.txt", "persisted!")

        # List / get_info / connect / destroy
        for v in Volume.list():           # list[VolumeInfo]
            print(v.volume_id, v.name)
        Volume.get_info(vol.volume_id)    # VolumeInfo
        vol = Volume.connect("my-data")   # Volume instance (== e2b)
        Volume.destroy(vol.volume_id)     # bool
    """

    def __init__(
        self,
        volume_id: str,
        name: str,
        token: str = "",
        *,
        config: Config | None = None,
    ) -> None:
        """Construct a volume handle. Prefer :meth:`create` / :meth:`connect`.

        Args:
            volume_id: Stable identifier.
            name: Human-readable display name.
            token: Optional plugin-issued auth token (empty string when none).
            config: SDK config bound to this handle (used by instance helpers).
        """
        self._volume_id = volume_id
        self._name = name
        self._token = token or ""
        self._config = config or Config()

    @property
    def volume_id(self) -> str:
        """Stable identifier (equals ``name`` or an auto-generated UUID)."""
        return self._volume_id

    @property
    def name(self) -> str:
        """Human-readable display name."""
        return self._name

    @property
    def token(self) -> str:
        """Plugin-issued auth token; empty string when the plugin issues none."""
        return self._token

    def __repr__(self) -> str:
        return (
            f"Volume(volume_id={self._volume_id!r}, name={self._name!r}, "
            f"token={'***' if self._token else ''!r})"
        )

    def __eq__(self, other: object) -> bool:
        if isinstance(other, Volume):
            return (
                self._volume_id == other._volume_id
                and self._name == other._name
                and self._token == other._token
            )
        # Allow comparison against VolumeInfo for ergonomic parity.
        if isinstance(other, VolumeInfo):
            return (
                self._volume_id == other.volume_id
                and self._name == other.name
                and self._token == other.token
            )
        return NotImplemented

    def __hash__(self) -> int:
        return hash((self._volume_id, self._name, self._token))

    @classmethod
    def _from_info(cls, info: VolumeInfo, config: Config) -> "Volume":
        return cls(info.volume_id, info.name, info.token, config=config)

    @classmethod
    def create(
        cls,
        name: str | None = None,
        *,
        driver: str | None = None,
        config: Config | None = None,
    ) -> "Volume":
        """POST /volumes — Create a new persistent volume.

        e2b-compatible by default: when ``driver`` is omitted (``None`` or
        ``""``), **no driver is sent**, so the backend falls back to the
        *first configured* volume plugin. Pass a non-empty ``driver`` to pin a
        specific plugin (a CubeSandbox extension), e.g.::

            Volume.create("my-data")                   # backend's first plugin
            Volume.create("my-data", driver="cos")     # pin the "cos" plugin

        Args:
            name: Volume name. Must match ``^[a-zA-Z0-9_-]+$`` and be at most
                128 characters. Optional — when omitted (``None`` / ``""``)
                the server generates a UUID and uses it as both the volume name
                and volume ID.
            driver: Optional plugin/driver name (matches ``volume_plugins[].name``
                in CubeMaster config, e.g. ``"cos"`` or ``"nfs"``). When falsy
                (``None`` / ``""``) the field is not sent and the backend picks
                its first configured plugin.
            config: SDK config. Uses default (env-based) config if omitted.

        Returns:
            A live :class:`Volume` instance with ``volume_id``, ``name`` and
            ``token`` populated from the backend response.

        Raises:
            ValueError: If ``name`` is provided but violates the naming rules.
            ApiError: On unexpected backend error (HTTP 400/500).
        """
        _validate_name(name or "")
        cfg = config or Config()
        payload: dict = {"name": name or ""}
        if driver:
            payload["driver"] = driver
        s = _shared_session()
        resp = s.post(
            f"{cfg.api_url}/volumes",
            json=payload,
            headers={"Content-Type": "application/json", **_auth_headers(cfg)},
            timeout=cfg.request_timeout,
        )
        _check_response(resp)
        return cls._from_info(VolumeInfo.from_dict(resp.json()), cfg)

    @classmethod
    def list(cls, *, config: Config | None = None) -> list[VolumeInfo]:
        """GET /volumes — List all volumes.

        The returned entries never carry a ``token`` (it is only surfaced on
        create / get-single); :attr:`VolumeInfo.token` is an empty string here.

        Args:
            config: SDK config. Uses default (env-based) config if omitted.

        Returns:
            A list of :class:`VolumeInfo` objects.

        Raises:
            ApiError: On unexpected backend error.
        """
        cfg = config or Config()
        s = _shared_session()
        resp = s.get(
            f"{cfg.api_url}/volumes",
            headers=_auth_headers(cfg),
            timeout=cfg.request_timeout,
        )
        _check_response(resp)
        data = resp.json() or []
        if isinstance(data, dict):
            data = data.get("volumes") or data.get("items") or []
        return [VolumeInfo.from_dict(d) for d in data]

    @classmethod
    def get_info(
        cls, volume_id: str, *, config: Config | None = None
    ) -> VolumeInfo:
        """GET /volumes/{volumeID} — Fetch a single volume, including its token.

        Args:
            volume_id: Volume identifier.
            config: SDK config. Uses default (env-based) config if omitted.

        Returns:
            :class:`VolumeInfo` with ``token`` populated when the plugin issues one.

        Raises:
            VolumeNotFoundError: If the volume does not exist (HTTP 404).
            ApiError: On unexpected backend error.
        """
        cfg = config or Config()
        _validate_volume_id(volume_id)
        s = _shared_session()
        resp = s.get(
            f"{cfg.api_url}/volumes/{volume_id}",
            headers=_auth_headers(cfg),
            timeout=cfg.request_timeout,
        )
        _check_response(resp)
        return VolumeInfo.from_dict(resp.json())

    @classmethod
    def connect(
        cls,
        volume_id: str,
        *,
        config: Config | None = None,
    ) -> "Volume":
        """GET /volumes/{volumeID} — Connect to an existing volume.

        e2b-compatible: fetches the volume via ``get_info`` (``GET
        /volumes/{volumeID}``) and returns a live :class:`Volume` **instance**
        bound to the given config — mirroring e2b's ``Volume.connect``::

            vol = Volume.connect("vol-123")
            print(vol.token)
            Sandbox.create(volume_mounts={"/workspace": vol})

        Args:
            volume_id: Volume identifier.
            config: SDK config. Uses default (env-based) config if omitted.

        Returns:
            A live :class:`Volume` instance with ``token`` populated when the
            plugin issues one.

        Raises:
            VolumeNotFoundError: If the volume does not exist (HTTP 404).
            ApiError: On unexpected backend error.
        """
        cfg = config or Config()
        info = cls.get_info(volume_id, config=cfg)
        return cls._from_info(info, cfg)

    @classmethod
    def destroy(
        cls,
        volume_id: str,
        *,
        config: Config | None = None,
    ) -> bool:
        """DELETE /volumes/{volumeID} — Permanently delete a volume.

        e2b-compatible: returns ``True`` on success, ``False`` when the volume
        does not exist (treated as idempotent — "already gone").

        .. note::
            Deleting a volume does **not** auto-detach it from running
            sandboxes. Destroy any sandbox that mounts the volume first,
            otherwise the backend mount may leak.

        Args:
            volume_id: Volume identifier.
            config: SDK config. Uses default (env-based) config if omitted.

        Returns:
            ``True`` on successful deletion.

        Raises:
            ApiError: On unexpected backend error (non-404).
        """
        cfg = config or Config()
        _validate_volume_id(volume_id)
        s = _shared_session()
        resp = s.delete(
            f"{cfg.api_url}/volumes/{volume_id}",
            headers=_auth_headers(cfg),
            timeout=cfg.request_timeout,
        )
        if resp.status_code == 404:
            return False
        _check_response(resp)
        return True


