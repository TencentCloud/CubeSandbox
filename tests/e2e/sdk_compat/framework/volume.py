# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import time
import uuid
from contextlib import contextmanager
from typing import Iterator

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
    "volume_id_from_payload",
    "volume_in_list",
    "wait_delete_volume_status",
    "safe_delete_volume",
    "managed_volume",
]


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


def safe_delete_volume(api: ApiClient, volume_id: str, *, attempts: int = 5) -> list[str]:
    errors: list[str] = []
    for _ in range(attempts):
        try:
            status = api.delete_volume(volume_id)
        except Exception as exc:  # noqa: BLE001 - cleanup must continue
            errors.append(f"DELETE /volumes/{volume_id} failed: {exc}")
            time.sleep(2)
            continue
        if status in (204, 404):
            return errors
        if status == 409:
            time.sleep(2)
            continue
        errors.append(f"DELETE /volumes/{volume_id} returned HTTP {status}")
        time.sleep(2)
    errors.append(f"DELETE /volumes/{volume_id} did not reach 204/404")
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
