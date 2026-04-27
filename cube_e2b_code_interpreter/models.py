"""
cube_e2b_code_interpreter.models
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~
与 e2b_code_interpreter 完全对齐的数据模型。
"""
from __future__ import annotations
import json
from dataclasses import dataclass, field
from typing import List, Optional, Callable, Any


@dataclass
class OutputMessage:
    text: str
    timestamp: str = ""
    is_stderr: bool = False


@dataclass
class Logs:
    stdout: List[str] = field(default_factory=list)
    stderr: List[str] = field(default_factory=list)

    def to_json(self) -> dict:
        return {"stdout": self.stdout, "stderr": self.stderr}


@dataclass
class ExecutionError:
    name: str
    value: str
    traceback: List[str] = field(default_factory=list)

    def to_json(self) -> dict:
        return {"name": self.name, "value": self.value, "traceback": self.traceback}


@dataclass
class Result:
    """
    与 e2b_code_interpreter.Result 完全对齐。
    is_main_result=True 表示这是 cell 的返回值（最后一行表达式）。
    """
    text: Optional[str] = None
    html: Optional[str] = None
    markdown: Optional[str] = None
    svg: Optional[str] = None
    png: Optional[str] = None
    jpeg: Optional[str] = None
    pdf: Optional[str] = None
    latex: Optional[str] = None
    json: Optional[dict] = None
    javascript: Optional[str] = None
    data: Optional[dict] = None
    is_main_result: bool = False
    extra: Optional[dict] = None

    def formats(self) -> List[str]:
        fmts = []
        for f in ("text", "html", "markdown", "svg", "png", "jpeg", "pdf", "latex", "json"):
            if getattr(self, f) is not None:
                fmts.append(f)
        return fmts


@dataclass
class Execution:
    """
    与 e2b_code_interpreter.Execution 完全对齐。

    execution.text        → 最后一行表达式的文本结果
    execution.logs.stdout → print() 输出列表
    execution.logs.stderr → 错误输出列表
    execution.error       → ExecutionError 或 None
    """
    results: List[Result] = field(default_factory=list)
    logs: Logs = field(default_factory=Logs)
    error: Optional[ExecutionError] = None
    execution_count: Optional[int] = None

    @property
    def text(self) -> Optional[str]:
        """最后一行表达式的文本结果（对应 e2b execution.text）"""
        for r in self.results:
            if r.is_main_result:
                return r.text
        return None

    def to_json(self) -> str:
        return json.dumps({
            "results": [{"text": r.text, "is_main_result": r.is_main_result} for r in self.results],
            "logs": self.logs.to_json(),
            "error": self.error.to_json() if self.error else None,
        })

    def __repr__(self) -> str:
        return f"Execution(text={self.text!r}, stdout={self.logs.stdout}, error={self.error})"


@dataclass
class Context:
    """Jupyter kernel context（用于跨 cell 共享状态）"""
    id: str
    language: str = "python"
    cwd: str = "/home/user"


# ---------------------------------------------------------------------------
# ndjson 行解析（完全对齐 e2b_code_interpreter._parse_output）
# ---------------------------------------------------------------------------

def parse_line(
    execution: Execution,
    line: str,
    on_stdout: Optional[Callable] = None,
    on_stderr: Optional[Callable] = None,
    on_result: Optional[Callable] = None,
    on_error: Optional[Callable] = None,
) -> None:
    """
    解析 /execute 流式响应的每一行 ndjson。

    e2b envd 返回格式：
        {"type": "stdout",   "text": "hello\n", "timestamp": "..."}
        {"type": "stderr",   "text": "warn\n",  "timestamp": "..."}
        {"type": "result",   "text": "2", "is_main_result": true, ...}
        {"type": "error",    "name": "NameError", "value": "...", "traceback": [...]}
        {"type": "number_of_executions", "execution_count": 3}
    """
    if not line:
        return
    try:
        data = json.loads(line)
    except json.JSONDecodeError:
        return

    dtype = data.pop("type", None)

    if dtype == "result":
        result = Result(**{k: v for k, v in data.items() if k in Result.__dataclass_fields__})
        execution.results.append(result)
        if on_result:
            on_result(result)

    elif dtype == "stdout":
        text = data.get("text", "")
        execution.logs.stdout.append(text)
        if on_stdout:
            on_stdout(OutputMessage(text, data.get("timestamp", ""), False))

    elif dtype == "stderr":
        text = data.get("text", "")
        execution.logs.stderr.append(text)
        if on_stderr:
            on_stderr(OutputMessage(text, data.get("timestamp", ""), True))

    elif dtype == "error":
        execution.error = ExecutionError(
            name=data.get("name", ""),
            value=data.get("value", ""),
            traceback=data.get("traceback", []),
        )
        if on_error:
            on_error(execution.error)

    elif dtype == "number_of_executions":
        execution.execution_count = data.get("execution_count")
