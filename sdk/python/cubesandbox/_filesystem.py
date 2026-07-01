# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import json
import logging
import struct
from typing import TYPE_CHECKING, Any, Iterator

logger = logging.getLogger(__name__)

from ._commands import DEFAULT_ENVD_USER, ENVD_PORT, MAX_CONNECT_ENVELOPE_SIZE
from ._exceptions import FilesystemNotFoundError, PartialWriteError

if TYPE_CHECKING:
    import httpx

    from .sandbox import Sandbox

CONNECT_PROTOCOL_VERSION = "1"
CONNECT_CONTENT_TYPE = "application/connect+json"


class Filesystem:
    def __init__(self, sandbox: "Sandbox") -> None:
        self._sandbox = sandbox

    def _ensure_client(self):
        if self._sandbox._client is None:
            self._sandbox._client = self._sandbox._build_data_client()

    def _base_headers(self) -> dict[str, str]:
        headers: dict[str, str] = {}
        access_token = self._sandbox._data.get("envdAccessToken")
        if access_token:
            headers["X-Access-Token"] = access_token
        return headers

    def _filesystem_rpc(self, method: str, payload: dict[str, Any]) -> dict[str, Any]:
        self._ensure_client()
        headers = self._base_headers()
        headers["Content-Type"] = "application/json"
        headers["Connect-Protocol-Version"] = CONNECT_PROTOCOL_VERSION

        resp = self._sandbox._client.post(
            f"http://{self._sandbox.get_host(ENVD_PORT)}/filesystem.Filesystem/{method}",
            json=payload,
            headers=headers,
        )
        if resp.status_code >= 400:
            message = resp.text or f"HTTP {resp.status_code}"
            try:
                body = resp.json()
            except (ValueError, json.JSONDecodeError):
                body = {}
            code = body.get("code", "")
            message = body.get("message") or body.get("detail") or message
            if code:
                message = f"{code}: {message}"
            if resp.status_code == 404 or code == "not_found":
                raise FilesystemNotFoundError(f"Filesystem {method} failed: {message}", status_code=resp.status_code)
            raise IOError(f"Filesystem {method} failed: {message}")
        if not resp.text:
            return {}
        return resp.json()

    def read(self, path: str, *, user: str | None = None) -> str:
        """Read a file through envd's HTTP file API."""
        self._ensure_client()
        effective_user = user or DEFAULT_ENVD_USER

        headers = self._base_headers()

        resp = self._sandbox._client.get(
            f"http://{self._sandbox.get_host(ENVD_PORT)}/files",
            params={"path": path, "username": effective_user},
            headers=headers,
        )
        if resp.status_code != 200:
            message = resp.text or f"HTTP {resp.status_code}"
            try:
                body = resp.json()
            except (ValueError, json.JSONDecodeError):
                body = {}
            message = body.get("message") or body.get("detail") or message
            raise IOError(f"Failed to read {path}: {message}")
        return resp.text

    def write(self, path: str, data: str | bytes, *, user: str | None = None) -> None:
        """Write a file through envd's HTTP file API."""
        self._ensure_client()
        effective_user = user or DEFAULT_ENVD_USER

        headers = {"Content-Type": "application/octet-stream"}
        headers.update(self._base_headers())

        body = data.encode("utf-8") if isinstance(data, str) else data
        params = {"path": path, "username": effective_user}
        resp = self._sandbox._client.post(
            f"http://{self._sandbox.get_host(ENVD_PORT)}/files",
            params=params,
            headers=headers,
            content=body,
        )
        if resp.status_code >= 400:
            multipart_headers = self._base_headers()
            resp = self._sandbox._client.post(
                f"http://{self._sandbox.get_host(ENVD_PORT)}/files",
                params=params,
                headers=multipart_headers,
                files={"file": (path, body)},
            )
        if resp.status_code >= 400:
            message = resp.text or f"HTTP {resp.status_code}"
            try:
                payload = resp.json()
            except (ValueError, json.JSONDecodeError):
                payload = {}
            message = payload.get("message") or payload.get("detail") or message
            raise IOError(f"Failed to write {path}: {message}")

    def write_files(
        self,
        files: list[tuple[str, str | bytes]],
        *,
        user: str | None = None,
    ) -> int:
        """Write multiple files. Returns the number of files written.

        Stops at the first error.
        """
        for i, (path, data) in enumerate(files):
            try:
                self.write(path, data, user=user)
            except IOError as e:
                raise PartialWriteError(
                    f"write_files failed at {path} ({i+1}/{len(files)}): {e}",
                    written=i,
                ) from e
        return len(files)

    def list(self, path: str) -> list[dict[str, Any]]:
        """List entries in a directory."""
        result = self._filesystem_rpc("ListDir", {"path": path})
        return result.get("entries", [])

    def stat(self, path: str) -> dict[str, Any]:
        """Return metadata for a file or directory."""
        result = self._filesystem_rpc("Stat", {"path": path})
        return result.get("entry", {})

    def exists(self, path: str) -> bool:
        """Return True if the path exists inside the sandbox."""
        try:
            self.stat(path)
            return True
        except FilesystemNotFoundError:
            return False

    def remove(self, path: str) -> None:
        """Delete a file or directory inside the sandbox."""
        self._filesystem_rpc("Remove", {"path": path})

    def rename(self, old_path: str, new_path: str) -> dict[str, Any]:
        """Move or rename a file or directory."""
        result = self._filesystem_rpc("Move", {"source": old_path, "destination": new_path})
        return result.get("entry", {})

    def make_dir(self, path: str) -> dict[str, Any]:
        """Create a directory inside the sandbox."""
        result = self._filesystem_rpc("MakeDir", {"path": path})
        return result.get("entry", {})

    def watch_dir(self, path: str) -> Watcher:
        """Watch a directory for filesystem changes.

        Returns a Watcher that yields WatchEvent dicts. Use as a context
        manager or call close() when done::

            with sb.files.watch_dir("/tmp") as w:
                for event in w:
                    print(event["name"], event["type"])
        """
        self._ensure_client()
        payload = json.dumps({"path": path}).encode()
        envelope = struct.pack(">bI", 0, len(payload)) + payload

        headers = self._base_headers()
        headers["Content-Type"] = CONNECT_CONTENT_TYPE
        headers["Connect-Protocol-Version"] = CONNECT_PROTOCOL_VERSION

        resp = self._sandbox._client.send(
            self._sandbox._client.build_request(
                "POST",
                f"http://{self._sandbox.get_host(ENVD_PORT)}/filesystem.Filesystem/WatchDir",
                content=envelope,
                headers=headers,
            ),
            stream=True,
        )
        if resp.status_code >= 400:
            resp.close()
            raise IOError(f"WatchDir failed: HTTP {resp.status_code}")
        return Watcher(resp)


class WatchEvent(dict):
    """A filesystem change event with ``name`` and ``type`` keys."""

    @property
    def name(self) -> str:
        return self.get("name", "")

    @property
    def type(self) -> str:
        return self.get("type", "")


class Watcher:
    """Iterates over WatchEvent items from a streaming WatchDir response."""

    def __init__(self, response: httpx.Response) -> None:
        self._response = response
        self._iter = response.stream.__iter__()
        self._closed = False
        self._buf = b""
        self._eof = False

    def __enter__(self) -> Watcher:
        return self

    def __exit__(self, *exc: Any) -> None:
        self.close()

    def close(self) -> None:
        if not self._closed:
            self._closed = True
            self._response.close()

    def __iter__(self) -> Iterator[WatchEvent]:
        return self

    def __next__(self) -> WatchEvent:
        while True:
            event = self._read_frame()
            if event is None:
                raise StopIteration
            if event is not _SKIP:
                return event

    def _fill(self, need: int) -> bool:
        while len(self._buf) < need and not self._eof:
            try:
                chunk = next(self._iter)
            except StopIteration:
                self._eof = True
                return len(self._buf) >= need
            if chunk:
                self._buf += chunk
        return len(self._buf) >= need

    def _read_frame(self) -> WatchEvent | None | object:
        try:
            if not self._fill(5):
                return None

            flags = self._buf[0]
            size = struct.unpack(">I", self._buf[1:5])[0]

            if size > MAX_CONNECT_ENVELOPE_SIZE:
                raise IOError(f"frame too large: {size} bytes")

            if not self._fill(5 + size):
                return None

            payload = self._buf[5 : 5 + size]
            self._buf = self._buf[5 + size :]

            if flags & 0x02:
                data = json.loads(payload)
                err = data.get("error")
                if err:
                    msg = err.get("message", "watch stream error")
                    raise IOError(msg)
                return None

            data = json.loads(payload)
            if "filesystem" in data:
                return WatchEvent(data["filesystem"])
            return _SKIP
        except IOError:
            raise
        except Exception:  # noqa: BLE001
            logger.debug("unexpected error parsing watch frame", exc_info=True)
            return None


_SKIP = object()
