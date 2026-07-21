# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import json
import time
from pathlib import Path
from typing import Any

from framework.trace import sanitize


class JsonlReporter:
    def __init__(self, report_dir: Path) -> None:
        self._report_dir = report_dir
        self._path = report_dir / "events.jsonl"
        self._report_dir.mkdir(parents=True, exist_ok=True)
        self._file = self._path.open("a", encoding="utf-8")

    def record(self, event: str, **fields: Any) -> None:
        if self._file.closed:
            return
        # Keep the reporter as an independent redaction boundary. Trace events
        # are sanitized when captured, but callers may pass other raw fields.
        payload = sanitize({"ts": time.time(), "event": event, **fields})
        try:
            self._file.write(json.dumps(payload, ensure_ascii=False, sort_keys=True) + "\n")
            self._file.flush()
        except ValueError as exc:
            if "closed file" not in str(exc):
                raise

    def record_test_result(self, **fields: Any) -> None:
        self.record("test_result", **fields)

    def close(self) -> None:
        if not self._file.closed:
            self._file.close()
