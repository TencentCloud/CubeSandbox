# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os


def auth_headers() -> dict[str, str]:
    """Auth headers for raw control-plane requests in tests.

    Mirrors how the SDK authenticates: when ``CUBE_API_KEY`` is set, send it as
    an ``X-API-Key`` header (see ``cubesandbox/_config.py``); otherwise send
    nothing. This is a test-owned boundary so cases never import SDK-private
    symbols yet still exercise the same auth code path the SDK uses.
    """
    api_key = os.environ.get("CUBE_API_KEY", "").strip()
    if api_key:
        return {"X-API-Key": api_key}
    return {}
