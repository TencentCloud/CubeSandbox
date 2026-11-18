# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import sys
import time
import uuid
from contextlib import contextmanager
from dataclasses import dataclass
from typing import Iterator

import requests

from adapters.api_adapter import ApiClient
from framework.config import SdkE2EConfig
from framework.volume_hints import (
    VOLUME_MOUNT_PATH,
    VOLUME_PLUGIN_GUIDE_URL,
    VOLUME_PLUGIN_SKIP_REASON,
    volume_plugin_misconfigured_hint,
)

__all__ = [
    "VOLUME_MOUNT_PATH",
    "VOLUME_PLUGIN_GUIDE_URL",
    "VOLUME_PLUGIN_SKIP_REASON",
    "volume_plugin_misconfigured_hint",
    "VolumePluginProbe",
    "probe_volume_plugin",
    "volume_id_from_payload",
    "volume_in_list",
    "wait_delete_volume_status",
    "safe_delete_volume",
    "managed_volume",
]


@dataclass(frozen=True)
class VolumePluginProbe:
    available: bool
    reason: str = ""


def probe_volume_plugin(config: SdkE2EConfig) -> VolumePluginProbe:
    """Check that the configured driver exists by creating and deleting a probe volume.

    An absent driver, its absent config file, or an older CubeAPI without the
    volume route is a skip. Authentication, transport, backend, malformed-config,
    and cleanup failures remain test errors so infrastructure regressions are not
    hidden as missing optional functionality.
    """
    api = ApiClient(config)
    name = f"sdk-e2e-volume-probe-{uuid.uuid4().hex[:12]}"
    volume_id: str | None = None
    cleanup_attempted = False
    try:
        try:
            response = api.create_volume_raw(name, driver=config.volume_driver)
        except requests.RequestException as exc:
            raise RuntimeError(
                "volume plugin probe create outcome is unknown; "
                f"inspect volume {name!r} before cleanup"
            ) from exc
        if response.status_code == 201:
            # Named creates use the name as volumeID. Remember it before parsing so
            # a malformed success response still gets a best-effort cleanup.
            volume_id = name
            try:
                payload = response.json()
            except ValueError as exc:
                raise RuntimeError(
                    f"volume plugin probe {name!r} returned invalid JSON"
                ) from exc
            if not isinstance(payload, dict):
                raise RuntimeError(
                    f"volume plugin probe {name!r} returned non-object body: {payload!r}"
                )
            try:
                volume_id = volume_id_from_payload(payload)
            except AssertionError as exc:
                raise RuntimeError(
                    f"volume plugin probe {name!r} returned an invalid volume ID"
                ) from exc
        elif response.status_code == 404:
            # A create can also return a business-level 404 (for example a
            # missing backend resource). Only skip when the list route confirms
            # that this CubeAPI version does not expose the Volume API at all.
            try:
                route_response = api.list_volumes_raw()
            except requests.RequestException as exc:
                raise RuntimeError(
                    "volume plugin probe could not distinguish a missing Volume API "
                    "from a create backend failure"
                ) from exc
            if route_response.status_code == 404:
                return VolumePluginProbe(
                    False,
                    "CubeAPI does not expose the Volume API (HTTP 404)",
                )
            raise RuntimeError(
                "volume plugin probe create returned HTTP 404 while the Volume API "
                f"route returned HTTP {route_response.status_code}: {response.text}. "
                f"{volume_plugin_misconfigured_hint()}"
            )
        else:
            try:
                body = response.json()
            except ValueError:
                body = {}
            message = body.get("message", "") if isinstance(body, dict) else ""
            driver_absent = (
                response.status_code == 400
                and isinstance(message, str)
                and "unknown driver:" in message
            )
            driver_config_absent = (
                response.status_code == 500
                and isinstance(message, str)
                and f'binary plugin "{config.volume_driver}" create:' in message
                and "open config " in message
                and "no such file or directory" in message.lower()
            )
            if driver_absent or driver_config_absent:
                detail = "plugin config file is absent" if driver_config_absent else "driver is absent"
                return VolumePluginProbe(
                    False,
                    f"volume driver {config.volume_driver!r} is not configured ({detail})",
                )
            raise RuntimeError(
                f"volume plugin probe failed: HTTP {response.status_code} {response.text}. "
                f"{volume_plugin_misconfigured_hint()}"
            )

        cleanup_attempted = True
        cleaned, cleanup_errors = _delete_volume_with_retries(api, volume_id)
        if not cleaned:
            raise RuntimeError("volume plugin probe cleanup failed: " + "; ".join(cleanup_errors))
        volume_id = None
        return VolumePluginProbe(True)
    finally:
        try:
            if volume_id is not None and not cleanup_attempted:
                cleaned, cleanup_errors = _delete_volume_with_retries(api, volume_id)
                if not cleaned:
                    active_error = sys.exc_info()[1]
                    cleanup_error = RuntimeError(
                        "volume plugin probe cleanup failed after an invalid create response: "
                        + "; ".join(cleanup_errors)
                    )
                    if active_error is not None:
                        raise cleanup_error from active_error
                    raise cleanup_error
        finally:
            api.close()


def volume_id_from_payload(payload: dict) -> str:
    volume_id = payload.get("volumeID") or payload.get("volume_id")
    if not isinstance(volume_id, str) or not volume_id.strip():
        raise AssertionError(f"volume response missing volumeID: {payload!r}")
    return volume_id.strip()


def volume_in_list(volumes: list[dict], volume_id: str) -> bool:
    for entry in volumes:
        if not isinstance(entry, dict):
            continue
        entry_id = entry.get("volumeID") or entry.get("volume_id")
        if entry_id == volume_id:
            return True
    return False


def wait_delete_volume_status(
    api: ApiClient,
    volume_id: str,
    expect: int,
    *,
    timeout: float = 60,
    interval: float = 2,
) -> int:
    """Retry DELETE /volumes/{id} until ``expect`` (409 while bound, 204 after unbind)."""
    assert expect in (204, 409), f"wait_delete_volume_status only supports 204/409, got {expect}"
    deadline = time.monotonic() + timeout
    last_status = -1
    while time.monotonic() < deadline:
        last_status = api.delete_volume(volume_id)
        if last_status == expect:
            return last_status
        if expect == 409:
            # Already gone → attach never held a ref; fail fast.
            if last_status in (204, 404):
                break
        else:  # expect == 204
            # Still in use → keep waiting for RefCount to drop.
            if last_status != 409:
                break
        time.sleep(interval)
    hint = ""
    if last_status not in (204, 409, 404):
        hint = f" {volume_plugin_misconfigured_hint()}"
    raise AssertionError(
        f"DELETE /volumes/{volume_id} expected {expect}, last status={last_status}.{hint}"
    )


def _delete_volume_with_retries(
    api: ApiClient, volume_id: str, *, attempts: int = 5
) -> tuple[bool, list[str]]:
    errors: list[str] = []
    for _ in range(attempts):
        try:
            status = api.delete_volume(volume_id)
        except Exception as exc:  # noqa: BLE001 - cleanup must continue
            errors.append(f"DELETE /volumes/{volume_id} failed: {exc}")
            time.sleep(2)
            continue
        if status in (204, 404):
            return True, errors
        if status == 409:
            time.sleep(2)
            continue
        errors.append(f"DELETE /volumes/{volume_id} returned HTTP {status}")
        time.sleep(2)
    errors.append(f"DELETE /volumes/{volume_id} did not reach 204/404")
    return False, errors


def safe_delete_volume(api: ApiClient, volume_id: str, *, attempts: int = 5) -> list[str]:
    _, errors = _delete_volume_with_retries(api, volume_id, attempts=attempts)
    return errors


@contextmanager
def managed_volume(
    config: SdkE2EConfig,
    *,
    name: str | None = None,
    driver: str | None = None,
) -> Iterator[tuple[str, ApiClient]]:
    """Create a volume and best-effort delete it on exit."""
    api = ApiClient(config)
    volume_name = name or f"vol-e2e-{uuid.uuid4().hex[:12]}"
    volume_driver = driver if driver is not None else config.volume_driver
    created: str | None = None
    try:
        # create_volume raises with a plugin-misconfigured hint on failure.
        payload = api.create_volume(volume_name, driver=volume_driver or None)
        created = volume_id_from_payload(payload)
        yield created, api
    finally:
        if created is not None:
            safe_delete_volume(api, created)
        api.close()
