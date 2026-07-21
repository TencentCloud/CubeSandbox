from __future__ import annotations

from unittest.mock import MagicMock, patch

import pytest

from cubesandbox import Sandbox, Volume, VolumeInfo, VolumeMount
from cubesandbox._config import Config


def _config() -> Config:
    return Config(api_url="http://localhost:3000", template_id="tpl-test")


def _sandbox_response() -> MagicMock:
    response = MagicMock()
    response.ok = True
    response.status_code = 201
    response.json.return_value = {
        "sandboxID": "sb-test",
        "templateID": "tpl-test",
    }
    return response


def test_create_serializes_read_only_volume_mount_without_changing_plain_mounts():
    dataset = VolumeInfo(volume_id="dataset-vol", name="dataset")

    with patch("requests.Session.post", return_value=_sandbox_response()) as post:
        Sandbox.create(
            volume_mounts={
                "/data": VolumeMount(dataset, read_only=True),
                "/workspace": "workspace-vol",
            },
            config=_config(),
        )

    assert post.call_args.kwargs["json"]["volumeMounts"] == [
        {"name": "dataset-vol", "path": "/data", "readOnly": True},
        {"name": "workspace-vol", "path": "/workspace"},
    ]


@pytest.mark.parametrize(
    "volume",
    [
        Volume("shared-vol", "shared"),
        VolumeInfo(volume_id="shared-vol", name="shared"),
        "shared-vol",
    ],
)
def test_read_only_volume_mount_preserves_existing_volume_reference_styles(volume):
    with patch("requests.Session.post", return_value=_sandbox_response()) as post:
        Sandbox.create(
            volume_mounts={"/shared": VolumeMount(volume, read_only=True)},
            config=_config(),
        )

    assert post.call_args.kwargs["json"]["volumeMounts"] == [
        {"name": "shared-vol", "path": "/shared", "readOnly": True}
    ]


def test_volume_mount_default_does_not_change_e2b_payload():
    with patch("requests.Session.post", return_value=_sandbox_response()) as post:
        Sandbox.create(
            volume_mounts={"/workspace": VolumeMount("workspace-vol")},
            config=_config(),
        )

    assert post.call_args.kwargs["json"]["volumeMounts"] == [
        {"name": "workspace-vol", "path": "/workspace"}
    ]
