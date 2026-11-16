# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import logging
import os
from typing import Literal, Optional, Union

import httpx
from typing_extensions import TypedDict

from ._exceptions import InvalidArgumentException

HttpVersion = Literal["1.1", "2"]
ProxyTypes = Union[str, httpx.URL, httpx.Proxy]
REQUEST_TIMEOUT = 60.0
FILE_TIMEOUT = 3600.0
READ_TIMEOUT = 60.0


class ApiParams(TypedDict, total=False):
    request_timeout: Optional[float]
    retries: Optional[int]
    headers: Optional[dict[str, str]]
    api_headers: Optional[dict[str, str]]
    api_key: Optional[str]
    validate_api_key: Optional[bool]
    domain: Optional[str]
    api_url: Optional[str]
    debug: Optional[bool]
    proxy: Optional[ProxyTypes]
    sandbox_url: Optional[str]
    http_version: Optional[HttpVersion]


class VolumeApiParams(TypedDict, total=False):
    domain: Optional[str]
    debug: Optional[bool]
    request_timeout: Optional[float]
    headers: Optional[dict[str, str]]
    token: Optional[str]
    api_url: Optional[str]
    proxy: Optional[ProxyTypes]
    http_version: Optional[HttpVersion]
    logger: Optional[logging.Logger]


def resolve_http_version(http_version: Optional[str] = None) -> HttpVersion:
    source = "http_version"
    if http_version is None:
        source = "E2B_HTTP_VERSION"
        http_version = os.environ.get("E2B_HTTP_VERSION") or "2"
    if http_version not in ("1.1", "2"):
        raise InvalidArgumentException(f"{source} must be '1.1' or '2', got {http_version!r}")
    return http_version
