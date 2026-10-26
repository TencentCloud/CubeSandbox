# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Shared L7 egress command helpers."""

from __future__ import annotations

import os

L7_HTTP_TIMEOUT = int(os.environ.get("SDK_E2E_L7_HTTP_TIMEOUT", "15"))
L7_ATTEMPTS = 3
L7_BLOCKING_PHASES_PER_ATTEMPT = 4
L7_COMMAND_MARGIN = 5


def l7_command_timeout(
    configured_timeout: int,
    *,
    request_timeout: int = L7_HTTP_TIMEOUT,
    attempts: int = L7_ATTEMPTS,
) -> int:
    backoff_budget = sum(min(2**attempt, 5) for attempt in range(attempts - 1))
    request_budget = request_timeout * L7_BLOCKING_PHASES_PER_ATTEMPT * attempts
    margin = max(L7_COMMAND_MARGIN, request_timeout)
    retry_budget = request_budget + backoff_budget + margin
    return max(configured_timeout, retry_budget)


def http_json_command(
    url: str,
    *,
    method: str = "GET",
    timeout: int = L7_HTTP_TIMEOUT,
    attempts: int = L7_ATTEMPTS,
    backoff_enabled: bool = True,
) -> str:
    """Fetch a URL, retrying transport failures and non-policy 502/504 responses."""
    return (
        "python3 - <<'PY'\n"
        "import ssl, time, urllib.error, urllib.request\n"
        f"url = {url!r}\n"
        f"method = {method!r}\n"
        f"timeout = {timeout!r}\n"
        f"attempts = {attempts!r}\n"
        f"backoff_enabled = {backoff_enabled!r}\n"
        "ctx = ssl._create_unverified_context()\n"
        "last = None\n"
        "for attempt in range(attempts):\n"
        "    try:\n"
        "        req = urllib.request.Request(url, method=method)\n"
        "        with urllib.request.urlopen(req, context=ctx, timeout=timeout) as resp:\n"
        "            body = resp.read().decode('utf-8', errors='replace')\n"
        "            print(f'STATUS:{resp.status}')\n"
        "            print(body)\n"
        "        break\n"
        "    except urllib.error.HTTPError as exc:\n"
        "        if exc.code in (502, 504) and attempt + 1 < attempts:\n"
        "            exc.close()\n"
        "            if backoff_enabled:\n"
        "                time.sleep(min(2 ** attempt, 5))\n"
        "            continue\n"
        "        try:\n"
        "            body = exc.read().decode('utf-8', errors='replace')\n"
        "        except Exception as body_exc:\n"
        "            body = f'<failed to read response body: {body_exc}>'\n"
        "        print(f'STATUS:{exc.code}')\n"
        "        print(body)\n"
        "        break\n"
        "    except Exception as exc:\n"
        "        last = exc\n"
        "        if attempt + 1 < attempts and backoff_enabled:\n"
        "            time.sleep(min(2 ** attempt, 5))\n"
        "else:\n"
        "    print(f'ERROR:{type(last).__name__}:{last}')\n"
        "PY"
    )
