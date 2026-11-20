# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import json

from ._exceptions import (
    AuthenticationException,
    RateLimitException,
    ServiceBusyException,
    VolumeException,
    VolumeNotFoundException,
    VolumePathNotFoundException,
)


def _body_preview(content: bytes, limit: int = 200) -> str:
    if not content:
        return "no response body"
    text = content[:limit].decode("utf-8", errors="replace")
    return text if len(content) <= limit else text + "..."


def check_volume_response(status: int, content: bytes, *, path: str | None = None,
                          volume_id: str | None = None, token: str | None = None) -> None:
    if 200 <= status < 300:
        return
    try:
        body = json.loads(content) if content else {}
    except ValueError:
        body = {}
    message = None
    if isinstance(body, dict):
        # Keep the pre-existing fallbacks: `message` first, then `detail`.
        for key in ("message", "detail"):
            value = body.get(key)
            if isinstance(value, str) and value:
                message = value
                break
    if token:
        if isinstance(message, str):
            message = message.replace(token, "***")
        content = content.replace(token.encode(), b"***")
    if status == 404 and path is not None:
        raise VolumePathNotFoundException(f"Path {path} not found", status)
    if status == 404 and volume_id is not None:
        raise VolumeNotFoundException(f"Volume {volume_id} not found", status)
    special = {
        401: (AuthenticationException, "401: Unauthorized, please check your credentials."),
        429: (RateLimitException, "429: Rate limit exceeded, please try again later."),
        503: (ServiceBusyException, "503: Service temporarily unavailable, please retry."),
    }
    if status in special:
        error, text = special[status]
        exc = error(text + (f" - {message}" if message else ""))
        exc.status_code = status
        raise exc
    if status == 404:
        # Management-plane 404 (no content-path context): keep the
        # VolumeNotFoundError contract of the pre-0.8 SDK.
        raise VolumeNotFoundException(f"404: {message if message is not None else 'not found'}", status)
    raise VolumeException(f"{status}: {message if message is not None else _body_preview(content)}", status)
