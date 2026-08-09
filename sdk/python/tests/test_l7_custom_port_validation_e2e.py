# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Negative / validation e2e cases for custom-port CubeEgress rules.

These complement ``test_l7_custom_port_e2e.py`` (which covers the happy
paths: custom HTTP inject, custom HTTPS allow/deny). Here we assert the
*rejection* contract for malformed or conflicting custom-port specs:

* two rules pinning the same ``(host, port)`` to different schemes are
  rejected as a whole policy by CubeMaster/CubeEgress;
* more than ``MAX_L7_PORTS_PER_HOST`` (8) distinct ``(port, scheme)``
  tuples on one host are rejected.

(The client-side "port requires scheme" contract is covered
unconditionally by ``tests/test_policy.py::test_port_without_scheme_rejected``;
both cases below drive a real ``Sandbox.create`` and therefore require a
live cluster.)

Required opt-in (same as the happy-path e2e):

- ``CUBE_E2E=1`` or pytest ``--run-e2e``
- ``CUBE_TEMPLATE_ID`` or pytest ``--cube-template-id``
"""

from __future__ import annotations

import os

import pytest

from cubesandbox import Action, Config, Match, Rule, Sandbox
from cubesandbox._exceptions import ApiError

pytestmark = pytest.mark.e2e


def _skip_unless_e2e(pytestconfig: pytest.Config) -> str | None:
    if not pytestconfig.getoption("--run-e2e") and os.environ.get("CUBE_E2E") != "1":
        pytest.skip("use --run-e2e or set CUBE_E2E=1")
    template_id = (
        pytestconfig.getoption("--cube-template-id")
        or os.environ.get("CUBE_TEMPLATE_ID")
    )
    if not template_id:
        pytest.skip("set CUBE_TEMPLATE_ID or --cube-template-id")
    return template_id


def test_l7_custom_scheme_conflict_rejected(
    pytestconfig: pytest.Config,
) -> None:
    # (host, port) pinned to two different schemes is a whole-policy
    # rejection: iptables can only steer one port to one listener.
    template_id = _skip_unless_e2e(pytestconfig)
    config = Config(api_url=os.environ.get("CUBE_API_URL", "http://127.0.0.1:3000"))
    rules = [
        Rule(
            name="custom-8080-http",
            match=Match(host="api.example.com", port=8080, scheme="http"),
            action=Action(allow=True),
        ),
        Rule(
            name="custom-8080-https",
            match=Match(host="api.example.com", port=8080, scheme="https"),
            action=Action(allow=True),
        ),
    ]
    # Assert the *policy* rejection specifically (ApiError mentioning the
    # conflict), not just any failure — a connection error or unrelated 500
    # must not satisfy this.
    with pytest.raises(ApiError, match="conflict"):
        with Sandbox.create(
            template=template_id,
            timeout=120,
            allow_internet_access=False,
            network={"rules": rules},
            config=config,
        ):
            pass


def test_l7_custom_port_budget_exceeded_rejected(
    pytestconfig: pytest.Config,
) -> None:
    # More than 8 distinct (port, scheme) tuples on one host must be
    # rejected rather than silently truncated.
    template_id = _skip_unless_e2e(pytestconfig)
    config = Config(api_url=os.environ.get("CUBE_API_URL", "http://127.0.0.1:3000"))
    rules = [
        Rule(
            name=f"custom-{i}",
            match=Match(host="api.example.com", port=8000 + i, scheme="http"),
            action=Action(allow=True),
        )
        for i in range(9)
    ]
    # Assert the *budget* rejection specifically (ApiError mentioning the
    # exceeded tuple limit), not just any failure.
    with pytest.raises(ApiError, match="exceeds"):
        with Sandbox.create(
            template=template_id,
            timeout=120,
            allow_internet_access=False,
            network={"rules": rules},
            config=config,
        ):
            pass
