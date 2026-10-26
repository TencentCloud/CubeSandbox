# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os
import warnings
from functools import lru_cache

import requests

import pytest

from framework.capabilities import AUTH_SIMPLE_KEY, capabilities_for_backend

# A protected control-plane endpoint that needs no pre-existing template or
# sandbox; an unauthenticated request tells us whether the live server enforces
# simple-key auth.
_AUTH_PROBE_PATH = "/templates"


# These cases drive the CubeAPI control plane directly (not the sdk_sandbox
# fixture), so the per-module requires_capability marker would not fire on its
# own — this autouse fixture is the real gate. It skips when:
#   - the backend lacks simple-key auth (e.g. e2b uses its own key format);
#   - the live server was not started with CUBE_API_KEY, so it does not enforce
#     auth and the reject paths would spuriously fail with 200 instead of 401;
#   - the runner has no CUBE_API_KEY, so the accept paths cannot send the real
#     key.
# The server-enforcement probe is what makes "CubeAPI started without
# CUBE_API_KEY" skip cleanly regardless of the runner's own environment, instead
# of turning the 401-rejection cases into false failures.
@pytest.fixture(autouse=True)
def _require_simple_key_auth(sdk_backend: str, sdk_e2e_config) -> str:
    if AUTH_SIMPLE_KEY not in capabilities_for_backend(sdk_backend):
        pytest.skip(
            f"backend {sdk_backend!r} does not support capability {AUTH_SIMPLE_KEY!r}"
        )

    # Probe lazily and only after the capability gate: an unsupported backend
    # (e.g. e2b) skips above without any network I/O. The server config is fixed
    # for a run, so the result is memoized to probe once per session rather than
    # once per test.
    if not _server_enforces_auth(sdk_e2e_config.cube_api_url, sdk_e2e_config.api_timeout):
        pytest.skip(
            "CubeAPI is not enforcing simple-key auth (unauthenticated "
            f"{_AUTH_PROBE_PATH} did not return 401); start CubeAPI with "
            "CUBE_API_KEY to exercise the accept/reject paths"
        )

    api_key = os.environ.get("CUBE_API_KEY", "").strip()
    if not api_key:
        pytest.skip(
            "CUBE_API_KEY is unset for the test runner; export the server's key "
            "to exercise the simple-key auth accept paths"
        )
    return api_key


@lru_cache(maxsize=None)
def _server_enforces_auth(api_url: str, timeout: float) -> bool:
    # Probe with no credentials: a 401 means unified_auth is active. Any other
    # status (typically 200) means the server was started without CUBE_API_KEY.
    # Treat probe failures as "cannot confirm enforcement" so the cases skip
    # rather than fail; preflight already verified /health reachability.
    try:
        with requests.get(f"{api_url}{_AUTH_PROBE_PATH}", timeout=timeout) as resp:
            return resp.status_code == 401
    except requests.RequestException as exc:
        # Surface why the probe failed so a cryptic skip is traceable to the
        # unreachable/erroring server rather than a plain "did not return 401".
        warnings.warn(
            f"auth-enforcement probe to {api_url}{_AUTH_PROBE_PATH} failed "
            f"({exc!r}); treating server as not enforcing auth",
            stacklevel=2,
        )
        return False
