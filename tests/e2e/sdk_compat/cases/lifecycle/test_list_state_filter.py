# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""``GET /v2/sandboxes`` ``state`` filter: E2B-compatible list and strict validation."""

from __future__ import annotations

import pytest

from adapters.api_adapter import ApiClient
from adapters.e2b_adapter import list_sandboxes_by_state
from framework.capabilities import LIFECYCLE, PAUSE_RESUME
from framework.lifecycle import metadata_from_info, wait_until_paused, wait_until_running

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.lifecycle,
    pytest.mark.p1,
    pytest.mark.requires_capability(LIFECYCLE),
]

# The only states E2B's SDK enum can parse; anything else breaks `Sandbox.list()`.
E2B_STATES = {"running", "paused"}


@pytest.fixture()
def api(sdk_e2e_config):
    client = ApiClient(sdk_e2e_config)
    try:
        yield client
    finally:
        client.close()


def _own_metadata(adapter) -> dict[str, str]:
    run_id = metadata_from_info(adapter.info().raw).get("test_run_id")
    assert run_id, "sdk_sandbox must carry test_run_id metadata"
    return {"test_run_id": str(run_id)}


def _ids(entries: list[dict]) -> list[str]:
    return [entry.get("sandboxID") or entry.get("sandbox_id") for entry in entries]


def _states(entries: list[dict]) -> set[str]:
    return {str(entry.get("state")) for entry in entries}


def _require_e2b(sdk_backend: str) -> None:
    if sdk_backend != "e2b":
        pytest.skip("SandboxQuery is an E2B SDK API; the CubeSandbox SDK has no state filter")


def test_list_state_accepts_comma_separated_e2b_states(sdk_sandbox, sdk_e2e_config, api):
    wait_until_running(sdk_sandbox, timeout=sdk_e2e_config.default_timeout)
    own = f"test_run_id={_own_metadata(sdk_sandbox)['test_run_id']}"

    entries = api.list_sandboxes_v2({"state": "running,paused", "metadata": own})

    assert _ids(entries) == [sdk_sandbox.sandbox_id]
    assert _states(entries) == {"running"}


def test_list_without_state_only_reports_states_an_e2b_client_can_parse(sdk_sandbox, api):
    entries = api.list_sandboxes_v2()

    assert sdk_sandbox.sandbox_id in _ids(entries)
    assert _states(entries) <= E2B_STATES, (
        f"default list must not expose states outside {sorted(E2B_STATES)}: {_states(entries)}"
    )


@pytest.mark.parametrize("state", ["running,pause", "pause", "", "running,"])
def test_list_rejects_the_whole_request_on_a_bad_state(sdk_sandbox, api, state):
    own = f"test_run_id={_own_metadata(sdk_sandbox)['test_run_id']}"

    resp = api.list_sandboxes_v2_raw({"state": state, "metadata": own})

    assert resp.status_code == 400, f"state={state!r} must not return a partial list: {resp.text}"
    assert resp.json().get("code") == 400


def test_e2b_sdk_lists_with_and_without_a_state_query(sdk_sandbox, sdk_backend, sdk_e2e_config):
    _require_e2b(sdk_backend)
    wait_until_running(sdk_sandbox, timeout=sdk_e2e_config.default_timeout)
    own = _own_metadata(sdk_sandbox)

    default_listing = list_sandboxes_by_state(sdk_e2e_config, metadata=own)
    assert _ids(default_listing) == [sdk_sandbox.sandbox_id]

    both = list_sandboxes_by_state(sdk_e2e_config, ["running", "paused"], metadata=own)
    assert _ids(both) == [sdk_sandbox.sandbox_id]

    paused_only = list_sandboxes_by_state(sdk_e2e_config, ["paused"], metadata=own)
    assert _ids(paused_only) == []


@pytest.mark.requires_capability(PAUSE_RESUME)
def test_list_state_filter_separates_paused_from_running(sdk_sandbox, sdk_e2e_config, api):
    own = f"test_run_id={_own_metadata(sdk_sandbox)['test_run_id']}"
    sdk_sandbox.pause(timeout=sdk_e2e_config.default_timeout)
    wait_until_paused(sdk_sandbox, timeout=sdk_e2e_config.default_timeout)

    paused = api.list_sandboxes_v2({"state": "paused", "metadata": own})
    assert _ids(paused) == [sdk_sandbox.sandbox_id]
    assert _states(paused) == {"paused"}

    assert api.list_sandboxes_v2({"state": "running", "metadata": own}) == []
    assert _ids(api.list_sandboxes_v2({"state": "running,paused", "metadata": own})) == [
        sdk_sandbox.sandbox_id
    ]
    assert _ids(api.list_sandboxes_v2({"metadata": own})) == [sdk_sandbox.sandbox_id]


@pytest.mark.requires_capability(PAUSE_RESUME)
def test_e2b_sdk_state_query_finds_a_paused_sandbox(sdk_sandbox, sdk_backend, sdk_e2e_config):
    _require_e2b(sdk_backend)
    own = _own_metadata(sdk_sandbox)
    sdk_sandbox.pause(timeout=sdk_e2e_config.default_timeout)
    wait_until_paused(sdk_sandbox, timeout=sdk_e2e_config.default_timeout)

    paused = list_sandboxes_by_state(sdk_e2e_config, ["paused"], metadata=own)
    assert _ids(paused) == [sdk_sandbox.sandbox_id]
    assert _states(paused) == {"paused"}

    running = list_sandboxes_by_state(sdk_e2e_config, ["running"], metadata=own)
    assert _ids(running) == []
