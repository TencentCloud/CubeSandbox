# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""``metadata`` filter on the sandbox list: E2B SDK encoding, older callers, malformed values."""

from __future__ import annotations

from urllib.parse import quote, urlencode

import pytest

from adapters.api_adapter import ApiClient
from adapters.e2b_adapter import list_sandboxes_by_state
from framework.capabilities import LIFECYCLE
from framework.lifecycle import metadata_from_info

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.lifecycle,
    pytest.mark.p1,
    pytest.mark.requires_capability(LIFECYCLE),
]

# Values E2B's SDKs percent-encode twice: space, slash, non-ASCII, '=', '&' and '%'.
SPECIAL = {
    "sdk_compat_owner": "a b/c",
    "sdk_compat_note": "中文",
    "sdk_compat_eq": "x=y",
    "sdk_compat_amp": "a&b",
    "sdk_compat_pct": "50%",
}


@pytest.fixture()
def api(sdk_e2e_config):
    client = ApiClient(sdk_e2e_config)
    try:
        yield client
    finally:
        client.close()


def _ids(entries: list[dict]) -> list[str]:
    return [entry.get("sandboxID") or entry.get("sandbox_id") for entry in entries]


def _run_id(adapter) -> dict[str, str]:
    run_id = metadata_from_info(adapter.info().raw).get("test_run_id")
    assert run_id, "sdk_sandbox must carry test_run_id metadata"
    return {"test_run_id": str(run_id)}


def _require_e2b(sdk_backend: str) -> None:
    if sdk_backend != "e2b":
        pytest.skip("SandboxQuery is an E2B SDK API; the CubeSandbox SDK has no metadata filter")


@pytest.mark.sandbox_create_options(metadata=SPECIAL)
@pytest.mark.parametrize("key", sorted(SPECIAL))
def test_e2b_sdk_metadata_filter_finds_special_characters(
    sdk_sandbox, sdk_backend, sdk_e2e_config, key
):
    _require_e2b(sdk_backend)

    found = list_sandboxes_by_state(sdk_e2e_config, metadata={key: SPECIAL[key]})
    assert sdk_sandbox.sandbox_id in _ids(found), f"{key}={SPECIAL[key]!r} must match"

    missed = list_sandboxes_by_state(sdk_e2e_config, metadata={key: SPECIAL[key] + "-other"})
    assert sdk_sandbox.sandbox_id not in _ids(missed)


@pytest.mark.sandbox_create_options(metadata=SPECIAL)
def test_e2b_sdk_metadata_filter_combines_keys_with_and(sdk_sandbox, sdk_backend, sdk_e2e_config):
    _require_e2b(sdk_backend)
    own = _run_id(sdk_sandbox)

    both = list_sandboxes_by_state(
        sdk_e2e_config, metadata={**own, "sdk_compat_owner": SPECIAL["sdk_compat_owner"]}
    )
    assert _ids(both) == [sdk_sandbox.sandbox_id]

    one_wrong = list_sandboxes_by_state(
        sdk_e2e_config, metadata={**own, "sdk_compat_owner": "nobody"}
    )
    assert _ids(one_wrong) == []


@pytest.mark.sandbox_create_options(metadata=SPECIAL)
def test_metadata_filter_still_accepts_a_value_encoded_once(sdk_sandbox, api):
    own = f"test_run_id={_run_id(sdk_sandbox)['test_run_id']}"

    for metadata in (own, f"{own}&sdk_compat_owner=a b/c", f"{own}&sdk_compat_note=中文"):
        expected = [sdk_sandbox.sandbox_id]
        assert _ids(api.list_sandboxes_v2({"metadata": metadata})) == expected, metadata
        assert _ids(api.list_sandboxes(metadata)) == expected, f"v1 {metadata}"


@pytest.mark.sandbox_create_options(metadata=SPECIAL)
@pytest.mark.parametrize(
    "encode",
    [
        pytest.param(urlencode, id="urlencode"),
        pytest.param(
            lambda pairs: "&".join(f"{quote(k)}={quote(v)}" for k, v in pairs.items()),
            id="quote",
        ),
    ],
)
def test_metadata_filter_accepts_a_value_encoded_by_the_caller(sdk_sandbox, api, encode):
    # '&' and '=' inside a value are escaped by the caller and must not split the term.
    metadata = encode({**_run_id(sdk_sandbox), **SPECIAL})

    expected = [sdk_sandbox.sandbox_id]
    assert _ids(api.list_sandboxes_v2({"metadata": metadata})) == expected, metadata
    assert _ids(api.list_sandboxes(metadata)) == expected, f"v1 {metadata}"


@pytest.mark.sandbox_create_options(metadata=SPECIAL)
def test_metadata_filter_hand_built_url_needs_the_separator_double_encoded(sdk_sandbox, api):
    # A hand-built URL must keep the value's `&` and `=` percent-encoded after the URL layer,
    # i.e. double-encoded, or they become term separators and the request is a 400.
    own = _run_id(sdk_sandbox)["test_run_id"]
    value = SPECIAL["sdk_compat_amp"]  # "a&b"
    urlencoded_value = value.replace("&", "%2526")  # one layer per URL decode pass

    query = f"metadata=test_run_id%3D{own}%26sdk_compat_amp%3D{urlencoded_value}"
    resp = api.list_sandboxes_v2_raw_query(query)
    assert resp.status_code == 200, resp.text
    assert _ids(resp.json()) == [sdk_sandbox.sandbox_id]

    # A single layer of encoding is decoded away by the URL layer and splits the term.
    single = query.replace("%2526", "%26")
    resp = api.list_sandboxes_v2_raw_query(single)
    assert resp.status_code == 400, f"{single!r} must not silently filter: {resp.text}"


@pytest.mark.parametrize("metadata", ["env", "env=prod&oops", "a=1&&b=2"])
def test_metadata_filter_rejects_a_value_that_is_not_key_value_pairs(sdk_sandbox, api, metadata):
    for resp in (
        api.list_sandboxes_v2_raw({"metadata": metadata}),
        api.list_sandboxes_raw(metadata),
    ):
        assert resp.status_code == 400, f"metadata={metadata!r} must not be ignored: {resp.text}"
        assert resp.json().get("code") == 400


def test_metadata_filter_empty_value_is_no_filter_not_a_400(sdk_sandbox, api):
    # E2B's JS SDK serializes `metadata: {}` as `URLSearchParams({}).toString()`, i.e. "";
    # that must behave like `metadata` being omitted, not reject the whole request.
    for resp in (
        api.list_sandboxes_v2_raw({"metadata": ""}),
        api.list_sandboxes_raw(""),
    ):
        assert resp.status_code == 200, f"empty metadata must not be rejected: {resp.text}"
        assert sdk_sandbox.sandbox_id in _ids(resp.json())
