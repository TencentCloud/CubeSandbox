# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import re
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


def _check_response(resp: requests.Response) -> None:
    if resp.ok:
        return
    try:
        msg = resp.json().get("message") or resp.json().get("detail") or resp.text
    except Exception:
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
    also works against an auth-enabled backend. (The server treats a
    ``Authorization: Bearer`` header as higher priority, but the e2b-style key
    is surfaced as ``X-API-Key`` here.)
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


@dataclass
class VolumeMount:
    """Declares that a volume should be mounted into a sandbox at ``path``.

    Wire shape (``volumeMounts[]`` in ``POST /sandboxes``): ``{"name", "path"}``
    where ``name`` must be an existing ``volumeID``.
    """

    name: str
    path: str

    def to_wire(self) -> dict[str, str]:
        return {"name": self.name, "path": self.path}


# Accepted by Sandbox.create(volume_mounts=...): typed VolumeMount or a plain
# dict with ``name`` and ``path`` keys.
VolumeMountArg = Union["VolumeMount", Dict[str, str]]


def _serialize_volume_mount(mount: VolumeMountArg) -> dict[str, str]:
    """Normalize one mount (typed or dict) into the ``{name, path}`` wire shape."""
    if isinstance(mount, VolumeMount):
        return mount.to_wire()
    if isinstance(mount, dict):
        try:
            name = mount["name"]
            path = mount["path"]
        except KeyError as exc:
            raise ValueError(
                f"volume mount dict requires 'name' and 'path' keys, missing {exc}"
            ) from exc
        return {"name": str(name), "path": str(path)}
    raise ValueError(
        f"volume mount must be a VolumeMount or a dict with 'name'/'path', got {type(mount).__name__}"
    )


def _serialize_volume_mounts(mounts: list[VolumeMountArg]) -> list[dict[str, str]]:
    return [_serialize_volume_mount(m) for m in mounts]


class Volume:
    """Class-level helper for CubeSandbox persistent-volume management.

    All methods are class-methods — no instance required. Volumes are e2b-compatible
    persistent stores backed by a volume plugin (COS, NFS, ...); create one here,
    then mount it into a sandbox via ``Sandbox.create(volume_mounts=[...])``.

    Example::

        # Create a volume (name auto-generated when omitted)
        vol = Volume.create("my-data")
        print(vol.volume_id, vol.token)

        # Mount it into a sandbox
        from cubesandbox import Sandbox, VolumeMount
        with Sandbox.create(
            volume_mounts=[VolumeMount(name=vol.volume_id, path="/workspace")],
        ) as sb:
            sb.files.write("/workspace/note.txt", "persisted!")

        # List / get / delete
        for v in Volume.list():
            print(v.volume_id, v.name)
        Volume.get(vol.volume_id)
        Volume.delete(vol.volume_id)
    """

    @classmethod
    def create(
        cls,
        name: str | None = None,
        *,
        driver: str | None = None,
        config: Config | None = None,
    ) -> VolumeInfo:
        """POST /volumes — Create a new persistent volume.

        e2b-compatible by default: when ``driver`` is omitted (``None`` or an
        empty string), **no driver is sent**, so CubeMaster falls back to the
        *first configured* volume plugin. Pass a non-empty ``driver`` to pin a
        specific plugin (a CubeSandbox extension), e.g.::

            Volume.create("my-data")                 # backend's first plugin
            Volume.create("my-data", driver="cos")   # pin the "cos" plugin

        Args:
            name: Optional display name. Must match ``^[a-zA-Z0-9_-]+$`` and be
                at most 128 characters. When omitted, CubeMaster generates a
                UUID and uses it as both the name and the volume ID.
            driver: Optional plugin/driver name (matches ``volume_plugins[].name``
                in CubeMaster config, e.g. ``"cos"`` or ``"nfs"``). When falsy
                (``None`` / ``""``) the field is not sent and the backend picks
                its first configured plugin.
            config: SDK config. Uses default (env-based) config if omitted.

        Returns:
            :class:`VolumeInfo` with ``volume_id``, ``name`` and ``token``.

        Raises:
            ValueError: If ``name`` is provided but violates the naming rules.
            ApiError: On unexpected backend error (HTTP 400/500).
        """
        _validate_name(name or "")
        cfg = config or Config()
        payload: dict = {"name": name or ""}
        if driver:
            payload["driver"] = driver
        s = requests.Session()
        resp = s.post(
            f"{cfg.api_url}/volumes",
            json=payload,
            headers={"Content-Type": "application/json", **_auth_headers(cfg)},
        )
        _check_response(resp)
        return VolumeInfo.from_dict(resp.json())

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
        s = requests.Session()
        resp = s.get(f"{cfg.api_url}/volumes", headers=_auth_headers(cfg))
        _check_response(resp)
        data = resp.json() or []
        if isinstance(data, dict):
            data = data.get("volumes") or data.get("items") or []
        return [VolumeInfo.from_dict(d) for d in data]

    @classmethod
    def get(cls, volume_id: str, *, config: Config | None = None) -> VolumeInfo:
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
        s = requests.Session()
        resp = s.get(f"{cfg.api_url}/volumes/{volume_id}", headers=_auth_headers(cfg))
        _check_response(resp)
        return VolumeInfo.from_dict(resp.json())

    @classmethod
    def delete(cls, volume_id: str, *, config: Config | None = None) -> None:
        """DELETE /volumes/{volumeID} — Delete a volume.

        .. note::
            Deleting a volume does **not** auto-detach it from running
            sandboxes. Destroy any sandbox that mounts the volume first,
            otherwise the backend mount may leak.

        Args:
            volume_id: Volume identifier.
            config: SDK config. Uses default (env-based) config if omitted.

        Raises:
            VolumeNotFoundError: If the volume does not exist (HTTP 404).
            ApiError: On unexpected backend error.
        """
        cfg = config or Config()
        s = requests.Session()
        resp = s.delete(f"{cfg.api_url}/volumes/{volume_id}", headers=_auth_headers(cfg))
        _check_response(resp)
