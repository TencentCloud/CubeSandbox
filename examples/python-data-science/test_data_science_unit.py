# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import io
import tarfile
from pathlib import Path

import pytest

from test_data_science import _cube_sandbox_id, _safe_extract_tar


def _open_archive(member: tarfile.TarInfo, data: bytes = b"") -> tarfile.TarFile:
    buffer = io.BytesIO()
    with tarfile.open(fileobj=buffer, mode="w:gz") as archive:
        member.size = len(data)
        archive.addfile(member, io.BytesIO(data) if data else None)
    buffer.seek(0)
    return tarfile.open(fileobj=buffer, mode="r:gz")


def test_cube_sandbox_id_accepts_native_and_client_suffixed_ids() -> None:
    cube_id = "0123456789abcdef0123456789abcdef"

    assert _cube_sandbox_id(cube_id) == cube_id
    assert (
        _cube_sandbox_id(
            f"{cube_id}-12345678-1234-1234-1234-123456789abc"
        )
        == cube_id
    )


@pytest.mark.parametrize(
    "sandbox_id",
    ["", "../sandbox", "not-a-cube-id", "0123456789abcdef0123456789abcdeg"],
)
def test_cube_sandbox_id_rejects_unexpected_values(sandbox_id: str) -> None:
    with pytest.raises(ValueError, match="Unexpected Cube sandbox id format"):
        _cube_sandbox_id(sandbox_id)


def test_safe_extract_tar_extracts_regular_files(tmp_path: Path) -> None:
    member = tarfile.TarInfo("state/result.txt")

    with _open_archive(member, b"ok\n") as archive:
        _safe_extract_tar(archive, tmp_path)

    assert (tmp_path / "state/result.txt").read_text(encoding="utf-8") == "ok\n"


def test_safe_extract_tar_rejects_path_traversal(tmp_path: Path) -> None:
    member = tarfile.TarInfo("../outside.txt")

    with _open_archive(member, b"unsafe") as archive:
        with pytest.raises(RuntimeError, match="escapes output directory"):
            _safe_extract_tar(archive, tmp_path)

    assert not (tmp_path.parent / "outside.txt").exists()


@pytest.mark.parametrize("member_type", [tarfile.SYMTYPE, tarfile.LNKTYPE])
def test_safe_extract_tar_rejects_links(
    tmp_path: Path, member_type: bytes
) -> None:
    member = tarfile.TarInfo("state/link")
    member.type = member_type
    member.linkname = "../outside.txt"

    with _open_archive(member) as archive:
        with pytest.raises(RuntimeError, match="unsafe link"):
            _safe_extract_tar(archive, tmp_path)
