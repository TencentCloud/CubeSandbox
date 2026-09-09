from __future__ import annotations

import asyncio
import datetime as dt
import json
import logging
import os
import uuid
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from pathlib import Path
from typing import Any

import httpx
from fastapi import FastAPI, HTTPException
from fastapi.middleware.cors import CORSMiddleware
from pydantic import BaseModel, Field
from starlette.responses import StreamingResponse
from websockets.client import WebSocketClientProtocol, connect


JUPYTER_BASE_URL = os.getenv("JUPYTER_BASE_URL", "http://127.0.0.1:8888")
JUPYTER_WS_URL = JUPYTER_BASE_URL.replace("http://", "ws://").replace("https://", "wss://")
ENVD_ENVS_URL = f"http://127.0.0.1:{os.getenv('ENVD_PORT', '49983')}/envs"
ENVD_TIMEOUT = float(os.getenv("ENVD_TIMEOUT", "2"))
WORKDIR = Path(os.getenv("CODE_INTERPRETER_WORKDIR", "/workspace")).resolve()
DEFAULT_LANGUAGE = "python"
PING_TIMEOUT = 30

logging.basicConfig(level=os.getenv("CODE_INTERPRETER_LOG_LEVEL", "INFO"))
logger = logging.getLogger("lightweight-code-interpreter")


class ExecuteRequest(BaseModel):
    code: str = Field(..., min_length=1)
    context_id: str | None = None
    language: str | None = None
    timeout: float | None = None
    cwd: str | None = None
    env: dict[str, str] | None = None
    env_vars: dict[str, str] | None = None


class CreateContext(BaseModel):
    cwd: str | None = None
    language: str | None = DEFAULT_LANGUAGE


class Context(BaseModel):
    id: str
    language: str
    cwd: str


def _event(event_type: str, **payload: Any) -> str:
    return json.dumps({"type": event_type, **payload}, ensure_ascii=False) + "\n"


def _normalize_language(language: str | None) -> str:
    value = (language or DEFAULT_LANGUAGE).lower().strip()
    if value in {"python", "python3", "py"}:
        return DEFAULT_LANGUAGE
    raise HTTPException(status_code=400, detail="only python language is supported")


def _resolve_cwd(cwd: str | None) -> str:
    path = Path(cwd).resolve() if cwd else WORKDIR
    try:
        path.relative_to(WORKDIR)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail="cwd must be under CODE_INTERPRETER_WORKDIR") from exc
    path.mkdir(parents=True, exist_ok=True)
    return str(path)


async def _fetch_sandbox_envs(
    http_client: httpx.AsyncClient | None = None,
) -> dict[str, str]:
    owns_client = http_client is None
    if http_client is None:
        http_client = httpx.AsyncClient()

    try:
        response = await http_client.get(ENVD_ENVS_URL, timeout=ENVD_TIMEOUT)
        response.raise_for_status()
    except httpx.HTTPError as exc:
        raise HTTPException(
            status_code=502,
            detail="failed to read sandbox environment from envd",
        ) from exc
    finally:
        if owns_client:
            await http_client.aclose()

    try:
        payload = response.json()
    except ValueError as exc:
        raise HTTPException(
            status_code=502,
            detail="envd returned an invalid environment payload",
        ) from exc

    if not isinstance(payload, dict) or not all(
        isinstance(key, str) and isinstance(value, str)
        for key, value in payload.items()
    ):
        raise HTTPException(
            status_code=502,
            detail="envd returned an invalid environment payload",
        )
    return payload


def _format_result(data: dict[str, Any]) -> dict[str, Any]:
    text = data.pop("text/plain", None)
    if text and (
        (text.startswith("'") and text.endswith("'"))
        or (text.startswith('"') and text.endswith('"'))
    ):
        text = text[1:-1]
    return {
        "text": text,
        "html": data.pop("text/html", None),
        "markdown": data.pop("text/markdown", None),
        "svg": data.pop("image/svg+xml", None),
        "png": data.pop("image/png", None),
        "jpeg": data.pop("image/jpeg", None),
        "pdf": data.pop("application/pdf", None),
        "latex": data.pop("text/latex", None),
        "json": data.pop("application/json", None),
        "javascript": data.pop("application/javascript", None),
        "data": data.pop("e2b/data", None),
        "chart": data.pop("e2b/chart", None),
        "extra": data or None,
    }


class KernelContext:
    def __init__(self, context_id: str, session_id: str, cwd: str):
        self.context_id = context_id
        self.session_id = session_id
        self.cwd = cwd
        self.language = DEFAULT_LANGUAGE
        self.ws_url = f"{JUPYTER_WS_URL}/api/kernels/{context_id}/channels"
        self.ws: WebSocketClientProtocol | None = None
        self.receive_task: asyncio.Task | None = None
        self.executions: dict[str, asyncio.Queue[dict[str, Any]]] = {}
        self.lock = asyncio.Lock()
        self.env_lock = asyncio.Lock()
        self.sandbox_envs: dict[str, str] | None = None
        self.sandbox_envs_applied = False
        self.cleanup_task: asyncio.Task[None] | None = None

    async def connect(self) -> None:
        ws_logger = logging.getLogger("websockets.client")
        ws_logger.setLevel(logging.ERROR)
        self.ws = await connect(
            self.ws_url,
            ping_timeout=PING_TIMEOUT,
            max_size=None,
            max_queue=None,
            logger=ws_logger,
        )
        self.receive_task = asyncio.create_task(self._receive_messages())
        await self._run_background(f"%cd {self.cwd}")

    async def _run_background(self, code: str) -> None:
        if self.ws is None:
            raise RuntimeError("kernel websocket is not connected")

        msg_id = str(uuid.uuid4())
        queue: asyncio.Queue[dict[str, Any]] = asyncio.Queue()
        self.executions[msg_id] = queue

        try:
            await self.ws.send(self._execute_request(msg_id, code, silent=True))
            while True:
                item = await queue.get()
                if item["type"] == "end_of_execution":
                    break
                if item["type"] == "unexpected_end_of_execution":
                    raise RuntimeError("background execution ended unexpectedly")
                if item["type"] == "error":
                    raise RuntimeError(f"background execution failed: {item}")
        finally:
            self.executions.pop(msg_id, None)

    async def load_sandbox_envs(self) -> None:
        async with self.env_lock:
            if self.sandbox_envs is not None:
                return
            self.sandbox_envs = await _fetch_sandbox_envs()

    @staticmethod
    def _build_env_assignments(env_vars: dict[str, str]) -> list[str]:
        return [
            f"import os; os.environ[{json.dumps(key)}] = {json.dumps(value)}"
            for key, value in env_vars.items()
        ]

    def _build_env_setup_code(
        self,
        per_call_envs: dict[str, str] | None,
        snapshot_name: str | None,
    ) -> str:
        lines: list[str] = []
        if not self.sandbox_envs_applied:
            lines.extend(self._build_env_assignments(self.sandbox_envs or {}))
        if per_call_envs:
            if snapshot_name is None:
                raise ValueError("snapshot_name is required for per-call envs")
            keys = json.dumps(list(per_call_envs))
            lines.append(
                f"import os; {snapshot_name} = "
                f"{{key: (key in os.environ, os.environ.get(key)) for key in {keys}}}"
            )
        lines.extend(self._build_env_assignments(per_call_envs or {}))
        return "\n".join(lines)

    @staticmethod
    def _build_env_cleanup_code(snapshot_name: str) -> str:
        restore_name = f"{snapshot_name}_restore"
        return "\n".join(
            [
                f"def {restore_name}():",
                "    import os",
                f"    snapshot = globals().pop({json.dumps(snapshot_name)}, {{}})",
                "    for key, (existed, value) in snapshot.items():",
                "        if existed:",
                "            os.environ[key] = value",
                "        else:",
                "            os.environ.pop(key, None)",
                f"{restore_name}()",
                f"del {restore_name}",
            ]
        )

    async def _cleanup_env_vars(self, snapshot_name: str) -> None:
        await self._run_background(self._build_env_cleanup_code(snapshot_name))

    async def _rollback_env_setup(self, snapshot_name: str) -> None:
        try:
            await self._cleanup_env_vars(snapshot_name)
        except Exception as exc:
            logger.warning("per-call environment setup rollback failed: %s", exc)

    async def _wait_for_cleanup(self) -> None:
        if self.cleanup_task is None:
            return
        try:
            await self.cleanup_task
        except Exception as exc:
            logger.warning("per-call environment cleanup failed: %s", exc)
        finally:
            self.cleanup_task = None

    def _execute_request(self, msg_id: str, code: str, *, silent: bool = False) -> str:
        return json.dumps(
            {
                "header": {
                    "msg_id": msg_id,
                    "username": "e2b",
                    "session": self.session_id,
                    "msg_type": "execute_request",
                    "version": "5.3",
                    "date": dt.datetime.now(dt.timezone.utc).isoformat(),
                },
                "parent_header": {},
                "metadata": {
                    "trusted": True,
                    "deletedCells": [],
                    "recordTiming": False,
                    "cellId": str(uuid.uuid4()),
                },
                "content": {
                    "code": code,
                    "silent": silent,
                    "store_history": not silent,
                    "user_expressions": {},
                    "stop_on_error": True,
                    "allow_stdin": False,
                },
            }
        )

    async def execute(
        self,
        code: str,
        *,
        env_vars: dict[str, str] | None,
    ) -> AsyncIterator[dict[str, Any]]:
        if self.ws is None:
            await self.connect()

        async with self.lock:
            await self._wait_for_cleanup()
            assert self.ws is not None
            apply_sandbox_envs = not self.sandbox_envs_applied
            snapshot_name = (
                f"_cube_lci_env_snapshot_{uuid.uuid4().hex}"
                if env_vars
                else None
            )
            env_code = self._build_env_setup_code(env_vars, snapshot_name)
            try:
                if env_code:
                    await self._run_background(env_code)
            except BaseException:
                if snapshot_name is not None:
                    await self._rollback_env_setup(snapshot_name)
                raise
            if apply_sandbox_envs:
                self.sandbox_envs_applied = True

            msg_id = str(uuid.uuid4())
            queue: asyncio.Queue[dict[str, Any]] = asyncio.Queue()
            self.executions[msg_id] = queue

            try:
                await self.ws.send(self._execute_request(msg_id, code))

                while True:
                    item = await queue.get()
                    if item["type"] == "end_of_execution":
                        break
                    if item["type"] == "unexpected_end_of_execution":
                        yield {
                            "type": "error",
                            "name": "UnexpectedEndOfExecution",
                            "value": "Connection to the execution was closed before the execution finished",
                            "traceback": "",
                        }
                        break
                    yield item
            finally:
                self.executions.pop(msg_id, None)
                if snapshot_name is not None:
                    self.cleanup_task = asyncio.create_task(
                        self._cleanup_env_vars(snapshot_name)
                    )

    async def _receive_messages(self) -> None:
        if self.ws is None:
            return
        try:
            async for raw in self.ws:
                await self._process_message(json.loads(raw))
        except Exception as exc:
            logger.warning("kernel websocket closed: %s", exc)
        finally:
            for queue in self.executions.values():
                await queue.put({"type": "unexpected_end_of_execution"})

    async def _process_message(self, data: dict[str, Any]) -> None:
        parent_msg_id = data.get("parent_header", {}).get("msg_id")
        if not parent_msg_id:
            return

        queue = self.executions.get(parent_msg_id)
        if queue is None:
            return

        msg_type = data.get("msg_type")
        content = data.get("content", {})

        if msg_type == "stream":
            stream_type = "stdout" if content.get("name") == "stdout" else "stderr"
            await queue.put(
                {
                    "type": stream_type,
                    "text": content.get("text", ""),
                    "timestamp": data.get("header", {}).get("date"),
                }
            )
        elif msg_type in {"display_data", "execute_result"}:
            await queue.put(
                {
                    "type": "result",
                    "is_main_result": msg_type == "execute_result",
                    **_format_result(dict(content.get("data", {}))),
                }
            )
        elif msg_type == "error":
            await queue.put(
                {
                    "type": "error",
                    "name": content.get("ename", ""),
                    "value": content.get("evalue", ""),
                    "traceback": "".join(content.get("traceback", [])),
                }
            )
        elif msg_type == "execute_input":
            await queue.put(
                {
                    "type": "number_of_executions",
                    "execution_count": content.get("execution_count", 0),
                }
            )
        elif msg_type == "execute_reply" and content.get("status") == "abort":
            await queue.put(
                {
                    "type": "error",
                    "name": "ExecutionAborted",
                    "value": "Execution was aborted",
                    "traceback": "",
                }
            )
            await queue.put({"type": "end_of_execution"})
        elif msg_type == "status" and content.get("execution_state") == "idle":
            await queue.put({"type": "end_of_execution"})

    async def close(self) -> None:
        if self.cleanup_task is not None:
            if not self.cleanup_task.done():
                self.cleanup_task.cancel()
            try:
                await self.cleanup_task
            except asyncio.CancelledError:
                pass
            except Exception as exc:
                logger.warning("per-call environment cleanup failed during close: %s", exc)
            self.cleanup_task = None
        if self.ws is not None:
            await self.ws.close()
        if self.receive_task is not None:
            self.receive_task.cancel()


async def _wait_for_jupyter() -> None:
    async with httpx.AsyncClient(timeout=2) as http_client:
        for _ in range(120):
            try:
                response = await http_client.get(f"{JUPYTER_BASE_URL}/api/status")
                if response.is_success:
                    return
            except Exception:
                pass
            await asyncio.sleep(0.5)
    raise RuntimeError("Jupyter Server did not become ready")


contexts: dict[str, KernelContext] = {}
default_context_id: str | None = None
client: httpx.AsyncClient | None = None


async def create_context(language: str | None = None, cwd: str | None = None) -> Context:
    _normalize_language(language)
    resolved_cwd = _resolve_cwd(cwd)
    assert client is not None

    response = await client.post(
        f"{JUPYTER_BASE_URL}/api/sessions",
        json={
            "path": str(uuid.uuid4()),
            "kernel": {"name": "python3"},
            "type": "notebook",
            "name": str(uuid.uuid4()),
        },
    )
    if not response.is_success:
        raise HTTPException(status_code=500, detail=f"failed to create context: {response.text}")

    session = response.json()
    context_id = session["kernel"]["id"]
    kernel = KernelContext(context_id, session["id"], resolved_cwd)
    await kernel.connect()
    contexts[context_id] = kernel
    return Context(id=context_id, language=DEFAULT_LANGUAGE, cwd=resolved_cwd)


@asynccontextmanager
async def lifespan(app: FastAPI):
    global client, default_context_id
    await _wait_for_jupyter()
    client = httpx.AsyncClient()
    default = await create_context(DEFAULT_LANGUAGE, str(WORKDIR))
    default_context_id = default.id
    try:
        yield
    finally:
        for context in list(contexts.values()):
            await context.close()
        contexts.clear()
        if client is not None:
            await client.aclose()


app = FastAPI(title="Lightweight Python Code Interpreter", version="0.2.0", lifespan=lifespan)
app.add_middleware(CORSMiddleware, allow_origins=["*"], allow_methods=["*"], allow_headers=["*"])


@app.get("/")
async def root() -> dict[str, Any]:
    return {"ok": True, "service": "lightweight-python-code-interpreter"}


@app.get("/health")
async def health() -> str:
    return "OK"


@app.post("/execute")
async def execute(exec_request: ExecuteRequest) -> StreamingResponse:
    if exec_request.context_id and exec_request.language:
        raise HTTPException(status_code=400, detail="only one of context_id or language can be provided")

    context_id = exec_request.context_id or default_context_id
    if exec_request.language:
        _normalize_language(exec_request.language)
    if exec_request.cwd and not exec_request.context_id:
        context = await create_context(DEFAULT_LANGUAGE, exec_request.cwd)
        context_id = context.id

    if context_id is None or context_id not in contexts:
        raise HTTPException(status_code=404, detail=f"context {exec_request.context_id} not found")

    env_vars = {}
    if exec_request.env:
        env_vars.update(exec_request.env)
    if exec_request.env_vars:
        env_vars.update(exec_request.env_vars)

    context = contexts[context_id]
    await context.load_sandbox_envs()

    async def stream() -> AsyncIterator[str]:
        async for item in context.execute(exec_request.code, env_vars=env_vars or None):
            yield json.dumps(item, ensure_ascii=False) + "\n"
        yield _event("end_of_execution")

    return StreamingResponse(stream(), media_type="application/x-ndjson")


@app.post("/contexts")
async def post_contexts(request: CreateContext) -> Context:
    return await create_context(request.language, request.cwd)


@app.get("/contexts")
async def get_contexts() -> list[Context]:
    return [
        Context(id=context.context_id, language=context.language, cwd=context.cwd)
        for context in contexts.values()
    ]


@app.post("/contexts/{context_id}/restart")
async def restart_context(context_id: str) -> None:
    context = contexts.get(context_id)
    if context is None or client is None:
        raise HTTPException(status_code=404, detail=f"context {context_id} not found")

    await context.close()
    response = await client.post(f"{JUPYTER_BASE_URL}/api/kernels/{context_id}/restart")
    if not response.is_success:
        raise HTTPException(status_code=500, detail=f"failed to restart context {context_id}")

    restarted = KernelContext(context.context_id, context.session_id, context.cwd)
    await restarted.connect()
    contexts[context_id] = restarted


@app.delete("/contexts/{context_id}")
async def remove_context(context_id: str) -> None:
    context = contexts.get(context_id)
    if context is None or client is None:
        raise HTTPException(status_code=404, detail=f"context {context_id} not found")
    if context_id == default_context_id:
        raise HTTPException(status_code=400, detail="default context cannot be deleted")

    await context.close()
    response = await client.delete(f"{JUPYTER_BASE_URL}/api/kernels/{context_id}")
    if not response.is_success:
        raise HTTPException(status_code=500, detail=f"failed to remove context {context_id}")
    contexts.pop(context_id, None)
