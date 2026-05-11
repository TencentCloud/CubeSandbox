# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import json
import logging
from typing import Callable, Optional

from ._models import Execution, ExecutionError, OutputMessage, Result

logger = logging.getLogger(__name__)


def _parse_line(
    execution: Execution,
    line: str,
    on_stdout: Optional[Callable[[OutputMessage], None]] = None,
    on_stderr: Optional[Callable[[OutputMessage], None]] = None,
    on_result: Optional[Callable[[Result], None]] = None,
    on_error: Optional[Callable[[ExecutionError], None]] = None,
) -> None:
    if not line:
        return
    try:
        data = json.loads(line)
    except json.JSONDecodeError:
        logger.debug("_parse_line: malformed JSON, skipping: %r", line)
        return

    event_type = data.pop("type", None)

    if event_type == "result":
        result = Result(**{k: v for k, v in data.items() if k in Result.__dataclass_fields__})
        execution.results.append(result)
        if on_result:
            on_result(result)

    elif event_type == "stdout":
        text = data.get("text", "")
        execution.logs.stdout.append(text)
        if on_stdout:
            on_stdout(OutputMessage(text, data.get("timestamp", "")))

    elif event_type == "stderr":
        text = data.get("text", "")
        execution.logs.stderr.append(text)
        if on_stderr:
            on_stderr(OutputMessage(text, data.get("timestamp", ""), is_stderr=True))

    elif event_type == "error":
        execution.error = ExecutionError(
            name=data.get("name", ""),
            value=data.get("value", ""),
            traceback=data.get("traceback", []),
        )
        if on_error:
            on_error(execution.error)

    elif event_type == "number_of_executions":
        execution.execution_count = data.get("execution_count")

    else:
        logger.debug("_parse_line: unknown event type %r, skipping", event_type)
