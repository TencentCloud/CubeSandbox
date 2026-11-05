# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Run each scenario independently, plus all three after a new template build."""
import pytest

from framework.envd_acceptance import (
    commands, files, health_identity, required_env, require_cube_sdk,
    sandbox, selection, template,
)

pytestmark = [pytest.mark.e2e, pytest.mark.sdk_compat, pytest.mark.envd_acceptance]


@pytest.fixture
def selected_sandbox(sdk_e2e_config, sdk_e2e_reporter):
    provider, commit = selection()
    with sandbox(sdk_e2e_config, sdk_e2e_reporter, sdk_e2e_config.cube_template_id, provider=provider) as adapter:
        health_identity(adapter, sdk_e2e_config, sdk_e2e_reporter, provider, commit)
        yield adapter


def test_health_identity(selected_sandbox):
    assert selected_sandbox.sandbox_id


def test_sdk_commands(selected_sandbox, sdk_e2e_config):
    commands(selected_sandbox, sdk_e2e_config.command_timeout)


def test_sdk_files(selected_sandbox, sdk_e2e_config):
    files(selected_sandbox, sdk_e2e_config.command_timeout)


@pytest.mark.creates_template
def test_new_template_chain(sdk_e2e_config, sdk_e2e_reporter):
    require_cube_sdk(sdk_e2e_config)
    provider, commit = selection()
    image = required_env("SDK_ENVD_IMAGE")
    with template(sdk_e2e_config, sdk_e2e_reporter, image) as template_id:
        with sandbox(sdk_e2e_config, sdk_e2e_reporter, template_id, provider=provider) as adapter:
            health_identity(adapter, sdk_e2e_config, sdk_e2e_reporter, provider, commit)
            commands(adapter, sdk_e2e_config.command_timeout)
            files(adapter, sdk_e2e_config.command_timeout)
