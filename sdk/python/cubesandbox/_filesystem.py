# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from .sandbox import Sandbox


class Filesystem:
    def __init__(self, sandbox: "Sandbox") -> None:
        self._sandbox = sandbox

    def read(self, path: str) -> str:
        result = self._sandbox.run_code(f"open({path!r}).read()")
        if result.error:
            raise IOError(f"Failed to read {path}: {result.error.value}")
        return result.text or ""
