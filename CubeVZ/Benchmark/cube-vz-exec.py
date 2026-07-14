#!/usr/bin/env python3
"""Small E2B-compatible code execution service for the CubeVZ guest.

The production CubeSandbox image normally gets the code interpreter from the
template image.  The ARM64 Virtualization.framework guest is assembled locally
though, so it needs a self-contained implementation of the data-plane
``POST /execute`` endpoint.  This service deliberately uses only the Python
standard library: the guest image can provide code execution without pulling in
Jupyter or a web framework.

The endpoint follows the SDK contract used by the repository's Python, Go and
Node clients.  Responses are newline-delimited JSON and are flushed after each
event so stdout/stderr remain live streams.  A single Python namespace is kept
for the lifetime of the service, which gives successive ``run_code`` calls the
same variable persistence as the code-interpreter images.
"""

from __future__ import annotations

import argparse
import ast
import json
import os
import subprocess
import sys
import threading
import time
import traceback
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Mapping


MAX_REQUEST_BYTES = 16 * 1024 * 1024
DEFAULT_WORKDIR = "/root"


def _timestamp() -> str:
    # Keep this ISO-8601 shape compatible with envd/Jupyter streams while
    # avoiding a dependency on dateutil in the tiny guest image.
    return time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime()) + ".%03dZ" % (
        (time.time_ns() // 1_000_000) % 1000
    )


class StreamAbort(Exception):
    """Raised internally when the HTTP peer closes a streaming response."""


class EventSink:
    def __init__(self, handler: "ExecuteHandler") -> None:
        self.handler = handler

    def emit(self, event: Mapping[str, Any]) -> None:
        self.handler.emit(dict(event))

    def text(self, event_type: str, value: str) -> None:
        if not value:
            return
        self.emit({"type": event_type, "text": value, "timestamp": _timestamp()})


class EventWriter:
    """File-like object used to turn Python writes into stream events."""

    encoding = "utf-8"

    def __init__(self, sink: EventSink, event_type: str) -> None:
        self.sink = sink
        self.event_type = event_type

    def write(self, value: Any) -> int:
        if isinstance(value, bytes):
            value = value.decode("utf-8", errors="replace")
        value = str(value)
        self.sink.text(self.event_type, value)
        return len(value)

    def flush(self) -> None:
        return None

    def isatty(self) -> bool:
        return False


class Executor:
    """Serializes executions and keeps a persistent Python namespace."""

    def __init__(self) -> None:
        self.lock = threading.Lock()
        self.namespace: dict[str, Any] = {"__name__": "__main__"}
        self.execution_count = 0

    def execute_python(
        self,
        code: str,
        env_vars: Mapping[str, str],
        sink: EventSink,
    ) -> None:
        with self.lock:
            self.execution_count += 1
            count = self.execution_count
            old_environment = os.environ.copy()
            os.environ.update({str(k): str(v) for k, v in env_vars.items()})
            old_stdout, old_stderr = sys.stdout, sys.stderr
            sys.stdout = EventWriter(sink, "stdout")  # type: ignore[assignment]
            sys.stderr = EventWriter(sink, "stderr")  # type: ignore[assignment]
            try:
                self._run_python(code, sink)
            except BaseException as error:  # code is untrusted by design
                sink.emit(
                    {
                        "type": "error",
                        "name": type(error).__name__,
                        "value": str(error),
                        "traceback": traceback.format_exception(
                            type(error), error, error.__traceback__
                        ),
                    }
                )
            finally:
                sys.stdout = old_stdout
                sys.stderr = old_stderr
                os.environ.clear()
                os.environ.update(old_environment)
                sink.emit({"type": "number_of_executions", "execution_count": count})

    def _run_python(self, code: str, sink: EventSink) -> None:
        tree = ast.parse(code, mode="exec")
        if tree.body and isinstance(tree.body[-1], ast.Expr):
            # Execute statements first, then evaluate the final expression so
            # ``x = 41\nx + 1`` produces the same result event as Jupyter.
            statements = ast.Module(body=tree.body[:-1], type_ignores=[])
            if statements.body:
                ast.fix_missing_locations(statements)
                exec(compile(statements, "<cube-vz>", "exec"), self.namespace)
            expression = ast.Expression(body=tree.body[-1].value)
            ast.fix_missing_locations(expression)
            value = eval(compile(expression, "<cube-vz>", "eval"), self.namespace)
            if value is not None:
                try:
                    text = repr(value)
                except BaseException:
                    text = f"<{type(value).__name__}>"
                # SDKs use is_main_result to populate Execution.text.
                sink.emit({"type": "result", "text": text, "is_main_result": True})
            return
        ast.fix_missing_locations(tree)
        exec(compile(tree, "<cube-vz>", "exec"), self.namespace)


class ExecuteHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "cube-vz-exec/1"

    @property
    def executor(self) -> Executor:
        return self.server.executor  # type: ignore[attr-defined]

    def log_message(self, format: str, *args: Any) -> None:
        # Keep the serial console useful without leaking request bodies.
        sys.__stderr__.write("cube-vz-exec: " + (format % args) + "\n")

    def emit(self, event: dict[str, Any]) -> None:
        if getattr(self, "_stream_closed", False):
            raise StreamAbort
        payload = (json.dumps(event, ensure_ascii=False, separators=(",", ":")) + "\n").encode(
            "utf-8"
        )
        try:
            self.wfile.write(payload)
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError, OSError) as error:
            self._stream_closed = True
            raise StreamAbort from error

    def do_GET(self) -> None:
        if self.path.split("?", 1)[0] in ("/", "/health", "/healthz"):
            self._send_bytes(200, b'{"status":"ok"}\n', "application/json")
            return
        self._send_bytes(404, b'{"message":"route not found"}\n', "application/json")

    def do_POST(self) -> None:
        if self.path.split("?", 1)[0] != "/execute":
            self._send_bytes(404, b'{"message":"route not found"}\n', "application/json")
            return
        try:
            length = int(self.headers.get("Content-Length", "-1"))
        except ValueError:
            length = -1
        if length < 0 or length > MAX_REQUEST_BYTES:
            self._send_bytes(413, b'{"message":"request body is too large"}\n', "application/json")
            return
        try:
            payload = json.loads(self.rfile.read(length))
            if not isinstance(payload, dict):
                raise ValueError("request body must be an object")
            code = payload.get("code")
            if not isinstance(code, str):
                raise ValueError("code must be a string")
            language = str(payload.get("language") or "python").lower()
            raw_env = payload.get("env_vars")
            if raw_env is None:
                raw_env = payload.get("envVars")
            if raw_env is None:
                raw_env = {}
            if not isinstance(raw_env, dict):
                raise ValueError("env_vars must be an object")
            env_vars = {str(key): str(value) for key, value in raw_env.items()}
        except (ValueError, TypeError, json.JSONDecodeError) as error:
            self._send_bytes(400, json.dumps({"message": str(error)}).encode() + b"\n", "application/json")
            return

        self.close_connection = True
        self.send_response(200)
        self.send_header("Content-Type", "application/x-ndjson")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        self.end_headers()
        sink = EventSink(self)
        try:
            if language in ("python", "python3", "py"):
                self.executor.execute_python(code, env_vars, sink)
            elif language in ("shell", "bash", "sh"):
                with self.executor.lock:
                    self.executor.execution_count += 1
                    count = self.executor.execution_count
                    try:
                        self._execute_shell(code, env_vars, sink, payload)
                    except BaseException as error:
                        self._emit_error(
                            sink,
                            type(error).__name__,
                            str(error),
                            traceback.format_exception(type(error), error, error.__traceback__),
                        )
                    finally:
                        sink.emit({"type": "number_of_executions", "execution_count": count})
            else:
                self._emit_error(
                    sink,
                    "ValueError",
                    f"unsupported language: {language}",
                    [],
                )
        except StreamAbort:
            return
        except BaseException as error:
            try:
                self._emit_error(
                    sink,
                    type(error).__name__,
                    str(error),
                    traceback.format_exception(type(error), error, error.__traceback__),
                )
            except StreamAbort:
                return

    def _execute_shell(
        self,
        code: str,
        env_vars: Mapping[str, str],
        sink: EventSink,
        payload: Mapping[str, Any],
    ) -> None:
        env = os.environ.copy()
        env.update(env_vars)
        cwd = payload.get("cwd") or DEFAULT_WORKDIR
        if not isinstance(cwd, str):
            cwd = DEFAULT_WORKDIR
        if not os.path.isdir(cwd):
            cwd = "/"
        process = subprocess.Popen(
            ["/bin/sh", "-c", code],
            cwd=cwd,
            env=env,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            bufsize=0,
        )

        def drain(stream: Any, event_type: str) -> None:
            while True:
                chunk = stream.read(16 * 1024)
                if not chunk:
                    return
                sink.text(event_type, chunk.decode("utf-8", errors="replace"))

        threads = [
            threading.Thread(target=drain, args=(process.stdout, "stdout"), daemon=True),
            threading.Thread(target=drain, args=(process.stderr, "stderr"), daemon=True),
        ]
        for thread in threads:
            thread.start()
        timeout = payload.get("timeout")
        timed_out = False
        try:
            process.wait(timeout=float(timeout) if timeout is not None else None)
        except subprocess.TimeoutExpired:
            timed_out = True
            process.kill()
            process.wait()
            self._emit_error(sink, "TimeoutError", "shell execution timed out", [])
        for thread in threads:
            thread.join()
        if process.returncode != 0 and not timed_out:
            self._emit_error(
                sink,
                "ProcessError",
                f"shell exited with code {process.returncode}",
                [],
            )
        sink.emit(
            {
                "type": "result",
                "text": "",
                "is_main_result": True,
                "extra": {"exit_code": process.returncode},
            }
        )

    @staticmethod
    def _emit_error(
        sink: EventSink,
        name: str,
        value: str,
        frames: list[str],
    ) -> None:
        sink.emit({"type": "error", "name": name, "value": value, "traceback": frames})

    def _send_bytes(self, status: int, body: bytes, content_type: str) -> None:
        self.close_connection = True
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(body)


class ExecServer(ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, address: tuple[str, int], handler: type[ExecuteHandler]) -> None:
        super().__init__(address, handler)
        self.executor = Executor()


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host", default="0.0.0.0")
    parser.add_argument("--port", type=int, default=49999)
    args = parser.parse_args()
    server = ExecServer((args.host, args.port), ExecuteHandler)
    print(f"CUBEVZ_EXEC_READY host={args.host} port={args.port}", flush=True)
    try:
        server.serve_forever(poll_interval=0.2)
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
