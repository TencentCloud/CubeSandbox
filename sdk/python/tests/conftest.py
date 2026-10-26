# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations


def pytest_addoption(parser):
    e2e = parser.getgroup("cubesandbox e2e")
    e2e.addoption(
        "--run-e2e",
        action="store_true",
        default=False,
        help="run tests that hit a live CubeAPI instance",
    )
    e2e.addoption(
        "--cube-api-url",
        default=None,
        help="CubeAPI URL for e2e tests; defaults to CUBE_API_URL or http://127.0.0.1:3000",
    )
    e2e.addoption(
        "--cube-template-id",
        default=None,
        help="existing template ID for read-only e2e tests; defaults to CUBE_TEMPLATE_ID",
    )
    e2e.addoption(
        "--cube-template-image",
        default=None,
        help="image used by template create e2e; defaults to CUBE_TEMPLATE_E2E_IMAGE",
    )
