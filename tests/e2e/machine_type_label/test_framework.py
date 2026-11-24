# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Hermetic checks for the label helpers.

These run in a plain ``pytest`` invocation (no cluster needed) and pin the
client-side parsing rules that the live tests rely on.
"""

from __future__ import annotations

import pytest

from ops_client import MACHINE_TYPE_KEY

pytestmark = [pytest.mark.framework]


def test_machine_type_key_is_the_protocol_value() -> None:
    # Cross-module protocol constant: must match CubeOps
    # model.LabelMachineType and restoreplace.MachineTypeLabelKey.
    assert MACHINE_TYPE_KEY == "cube.cubesandbox.io/machine-type"


def test_label_url_encoding_keeps_slash_in_query() -> None:
    from urllib.parse import quote

    assert quote(MACHINE_TYPE_KEY, safe="") == "cube.cubesandbox.io%2Fmachine-type"
