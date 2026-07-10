# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import json
import time
from pathlib import Path
from typing import Any


class JsonlReporter:
    def __init__(self, report_dir: Path) -> None:
        self._report_dir = report_dir
        self._path = report_dir / "events.jsonl"

    def record(self, event: str, **fields: Any) -> None:
        self._report_dir.mkdir(parents=True, exist_ok=True)
        payload = {"ts": time.time(), "event": event, **fields}
        with self._path.open("a", encoding="utf-8") as fh:
            fh.write(json.dumps(payload, ensure_ascii=False, sort_keys=True) + "\n")

    def record_test_result(self, **fields: Any) -> None:
        self.record("test_result", **fields)
