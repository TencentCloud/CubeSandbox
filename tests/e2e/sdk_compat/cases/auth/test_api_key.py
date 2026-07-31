# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Live CUBE_API_KEY simple-key authentication cases.

These verify the CubeAPI ``unified_auth`` simple-key mode end-to-end against a
running server started with ``CUBE_API_KEY``:

- a matching ``X-API-Key`` header is accepted;
- a matching ``Authorization: Bearer`` header is accepted;
- a wrong key is rejected with 401;
- a missing credential is rejected with 401;
- ``GET /health`` stays exempt from auth.

Prerequisites (manual; not provisioned by this suite):
- CubeAPI started with ``CUBE_API_KEY=<key>`` set in its environment.
- The same key exported as ``CUBE_API_KEY`` for the test runner so the accept
  paths use the real key.
- ``cubesandbox`` backend (e2b uses a different key format / server).
"""

from __future__ import annotations

import requests

import pytest

# Backend/credential gating lives in the autouse ``_require_simple_key_auth``
# fixture in this directory's conftest.py. A ``requires_capability`` marker here
# would be inert: that marker is only evaluated inside the ``sdk_sandbox``
# fixture body, which these control-plane cases never request.
pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.auth,
    pytest.mark.p1,
]

# A protected control-plane endpoint that needs no pre-existing template or
# sandbox: listing templates always sits behind unified_auth.
PROTECTED_PATH = "/templates"


def _get(cfg, path: str, headers: dict[str, str] | None = None) -> requests.Response:
    return requests.get(
        f"{cfg.cube_api_url}{path}",
        headers=headers or {},
        timeout=cfg.api_timeout,
    )


def test_x_api_key_header_is_accepted(sdk_e2e_config, _require_simple_key_auth):
    # A matching X-API-Key must pass auth (any non-401 status: the endpoint is
    # reachable, auth did not reject it).
    resp = _get(
        sdk_e2e_config, PROTECTED_PATH, {"X-API-Key": _require_simple_key_auth}
    )
    assert resp.status_code != 401, (
        f"matching X-API-Key was rejected: HTTP {resp.status_code} {resp.text}"
    )


def test_bearer_token_is_accepted(sdk_e2e_config, _require_simple_key_auth):
    # A matching Authorization: Bearer credential must pass auth.
    resp = _get(
        sdk_e2e_config,
        PROTECTED_PATH,
        {"Authorization": f"Bearer {_require_simple_key_auth}"},
    )
    assert resp.status_code != 401, (
        f"matching Bearer token was rejected: HTTP {resp.status_code} {resp.text}"
    )


# These three cases do not consume the fixture's return value; the autouse
# ``_require_simple_key_auth`` fixture still gates them, so the parameter is
# omitted. A 200 (instead of 401) on the rejection cases usually means the live
# server was started without ``CUBE_API_KEY`` even though the runner has it set.
_SERVER_NOT_ENFORCING = (
    "if this is 200 the server likely was not started with CUBE_API_KEY"
)


def test_wrong_key_is_rejected(sdk_e2e_config):
    # A mismatched key must return 401 regardless of endpoint behavior.
    resp = _get(
        sdk_e2e_config, PROTECTED_PATH, {"X-API-Key": "wrong-key-does-not-match"}
    )
    assert resp.status_code == 401, (
        f"wrong key should be rejected with 401, got HTTP {resp.status_code} "
        f"{resp.text} ({_SERVER_NOT_ENFORCING})"
    )


def test_missing_credential_is_rejected(sdk_e2e_config):
    # No credential at all must return 401.
    resp = _get(sdk_e2e_config, PROTECTED_PATH)
    assert resp.status_code == 401, (
        f"missing credential should be rejected with 401, got HTTP "
        f"{resp.status_code} {resp.text} ({_SERVER_NOT_ENFORCING})"
    )


def test_health_is_exempt_from_auth(sdk_e2e_config):
    # GET /health must remain reachable without any credential even when the
    # server enforces simple-key auth.
    resp = _get(sdk_e2e_config, "/health")
    assert resp.status_code == 200, (
        f"/health should be exempt from auth, got HTTP {resp.status_code} {resp.text}"
    )
