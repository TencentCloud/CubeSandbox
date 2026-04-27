"""
cube_e2b_code_interpreter.sandbox
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~
主 Sandbox 类，接口与 e2b_code_interpreter.Sandbox 完全对齐。

用法（与原版 e2b 完全相同）：

    from cube_e2b_code_interpreter import Sandbox

    with Sandbox.create() as sb:
        sb.run_code("x = 1")
        execution = sb.run_code("x += 1; x")
        print(execution.text)  # 2
"""
from __future__ import annotations

import json
from typing import Any, Callable, Dict, Optional

import httpx
import requests
from requests.adapters import HTTPAdapter

from .config import SandboxConfig
from .exceptions import ApiError, AuthenticationError, SandboxNotFoundError, TemplateNotFoundError
from .models import Context, Execution, ExecutionError, OutputMessage, Result, parse_line
from .transport import build_httpx_client

JUPYTER_PORT = 49999   # 与 e2b_code_interpreter.constants.JUPYTER_PORT 一致


def _raise_for_api_status(resp: requests.Response) -> None:
    if resp.ok:
        return
    try:
        body = resp.json()
        msg = body.get("message") or body.get("detail") or resp.text
    except Exception:
        msg = resp.text or f"HTTP {resp.status_code}"
    code = resp.status_code
    if code in (401, 403):
        raise AuthenticationError(msg, code)
    if code == 404:
        if "template" in msg.lower():
            raise TemplateNotFoundError(msg, code)
        raise SandboxNotFoundError(msg, code)
    raise ApiError(msg, code)


class _IPOverrideAdapter(HTTPAdapter):
    """requests adapter：DNS 绕过，直连 proxy IP。"""
    def __init__(self, ip: str, port: int, **kw):
        self._ip = ip
        self._port = port
        super().__init__(**kw)

    def send(self, request, **kw):
        from urllib.parse import urlparse, urlunparse
        p = urlparse(request.url)
        original_host = p.netloc.split(":")[0]
        new_netloc = f"{self._ip}:{self._port}"
        request.url = urlunparse(p._replace(netloc=new_netloc))
        request.headers["Host"] = original_host
        return super().send(request, **kw)


class Sandbox:
    """
    CubeSandbox code interpreter — drop-in for e2b_code_interpreter.Sandbox.

    关键差异（对用户透明）：

    e2b cloud                         CubeSandbox
    ─────────────────────────────     ─────────────────────────────────────
    域名: 49999-<id>.e2b.app          域名: 49999-<id>.cube.app
    DNS:  e2b cloud 维护              DNS:  CoreDNS 本机 / CUBE_PROXY_NODE_IP
    API:  api.e2b.app:443             API:  CUBE_API_URL (默认 :3000)
    Auth: E2B_API_KEY (真实)           Auth: E2B_API_KEY (任意字符串)
    """

    def __init__(self, data: dict, config: SandboxConfig) -> None:
        self._data = data
        self._cfg = config
        self._api_session = self._build_api_session()
        self._stream_client: httpx.Client | None = None

    # ------------------------------------------------------------------
    # 内部
    # ------------------------------------------------------------------

    def _build_api_session(self) -> requests.Session:
        """CubeAPI（端口 3000）不需要 DNS 绕过，直接访问 IP:3000。"""
        s = requests.Session()
        s.headers.update({"X-API-Key": self._cfg.api_key, "Content-Type": "application/json"})
        if self._cfg.ssl_cert_file:
            s.verify = self._cfg.ssl_cert_file
        return s

    def _get_stream_client(self) -> httpx.Client:
        """返回用于访问 sandbox 服务（端口 49999 等）的 httpx client。"""
        if self._stream_client is None:
            self._stream_client = build_httpx_client(self._cfg)
        return self._stream_client

    # ------------------------------------------------------------------
    # 属性
    # ------------------------------------------------------------------

    @property
    def sandbox_id(self) -> str:
        return self._data["sandboxID"]

    @property
    def template_id(self) -> str:
        return self._data["templateID"]

    @property
    def domain(self) -> str:
        return self._data.get("domain") or self._cfg.sandbox_domain

    # ------------------------------------------------------------------
    # URL 工具（对齐 e2b_code_interpreter.get_host）
    # ------------------------------------------------------------------

    def get_host(self, port: int) -> str:
        """
        返回 sandbox 服务的虚拟主机名。

        e2b cloud:    "49999-<id>.e2b.app"
        CubeSandbox:  "49999-<id>.cube.app"
        """
        return f"{port}-{self.sandbox_id}.{self.domain}"

    @property
    def _jupyter_url(self) -> str:
        """
        Jupyter kernel 的 HTTP 地址。

        e2b cloud: https://49999-<id>.e2b.app   (走 e2b cloud DNS)
        CubeSandbox：
          - 本机:         http://49999-<id>.cube.app   (CoreDNS 解析)
          - 远程+IP绕过:  http://49999-<id>.cube.app   (IPOverrideTransport 直连代理 IP)
        """
        return f"http://{self.get_host(JUPYTER_PORT)}"

    # ------------------------------------------------------------------
    # Factory
    # ------------------------------------------------------------------

    @classmethod
    def create(
        cls,
        template: str | None = None,
        *,
        timeout: int | None = None,
        env_vars: Dict[str, str] | None = None,
        metadata: Dict[str, str] | None = None,
        config: SandboxConfig | None = None,
        **kwargs: Any,
    ) -> "Sandbox":
        """
        创建 sandbox。接口与 e2b_code_interpreter.Sandbox.create() 完全对齐。

        示例：
            sb = Sandbox.create(template="tpl-xxxx", timeout=300)
        """
        cfg = config or SandboxConfig()
        tpl = template or cfg.template_id
        if not tpl:
            raise ValueError("template 未提供，请设置 CUBE_TEMPLATE_ID 环境变量。")
        ttl = timeout if timeout is not None else cfg.default_timeout

        s = requests.Session()
        s.headers.update({"X-API-Key": cfg.api_key, "Content-Type": "application/json"})
        if cfg.ssl_cert_file:
            s.verify = cfg.ssl_cert_file

        payload: dict = {"templateID": tpl, "timeout": ttl}
        if env_vars:
            payload["envVars"] = env_vars
        if metadata:
            payload["metadata"] = metadata
        payload.update(kwargs)

        resp = s.post(f"{cfg.api_url}/sandboxes", json=payload)
        _raise_for_api_status(resp)
        return cls(resp.json(), config=cfg)

    @classmethod
    def connect(cls, sandbox_id: str, *, config: SandboxConfig | None = None) -> "Sandbox":
        """连接已有 sandbox。"""
        cfg = config or SandboxConfig()
        s = requests.Session()
        s.headers.update({"X-API-Key": cfg.api_key})
        resp = s.get(f"{cfg.api_url}/sandboxes/{sandbox_id}")
        _raise_for_api_status(resp)
        return cls(resp.json(), config=cfg)

    # ------------------------------------------------------------------
    # run_code（核心，完全对齐 e2b_code_interpreter）
    # ------------------------------------------------------------------

    def run_code(
        self,
        code: str,
        *,
        language: str | None = None,
        context: Context | None = None,
        on_stdout: Callable[[OutputMessage], None] | None = None,
        on_stderr: Callable[[OutputMessage], None] | None = None,
        on_result: Callable[[Result], None] | None = None,
        on_error: Callable[[ExecutionError], None] | None = None,
        envs: Dict[str, str] | None = None,
        timeout: float | None = None,
        **kwargs,
    ) -> Execution:
        """
        在 sandbox 内执行 Python 代码，返回 Execution 对象。

        完全兼容 e2b_code_interpreter.Sandbox.run_code()：
            execution.text        → 最后表达式结果
            execution.logs.stdout → print() 输出
            execution.error       → 异常信息

        数据流说明：
            POST {jupyter_url}/execute  (ndjson 流式响应)
            每行格式: {"type": "stdout"|"stderr"|"result"|"error", ...}

            当 CUBE_PROXY_NODE_IP 已设置时，httpx 通过 IPOverrideTransport
            直连代理 IP，Host 头保留虚拟主机名供 CubeProxy 路由——
            无需 DNS 解析 *.cube.app。
        """
        context_id = context.id if context else None
        payload = {
            "code": code,
            "context_id": context_id,
            "language": language,
            "env_vars": envs,
        }
        headers = {"Content-Type": "application/json"}

        client = self._get_stream_client()
        execution = Execution()

        with client.stream(
            "POST",
            f"{self._jupyter_url}/execute",
            json=payload,
            headers=headers,
            timeout=httpx.Timeout(connect=self._cfg.request_timeout, read=timeout, write=30, pool=30),
        ) as response:
            if response.status_code >= 400:
                raise ApiError(f"execute failed: HTTP {response.status_code}", response.status_code)
            for line in response.iter_lines():
                parse_line(execution, line,
                           on_stdout=on_stdout, on_stderr=on_stderr,
                           on_result=on_result, on_error=on_error)

        return execution

    # ------------------------------------------------------------------
    # context 管理
    # ------------------------------------------------------------------

    def create_context(self, language: str = "python", cwd: str = "/home/user") -> Context:
        """创建一个新的 kernel context（跨 cell 共享状态）。"""
        client = self._get_stream_client()
        resp = client.post(f"{self._jupyter_url}/contexts",
                           json={"language": language, "cwd": cwd})
        resp.raise_for_status()
        data = resp.json()
        return Context(id=data["id"], language=data.get("language", language), cwd=data.get("cwd", cwd))

    def delete_context(self, context: Context) -> None:
        client = self._get_stream_client()
        client.delete(f"{self._jupyter_url}/contexts/{context.id}")

    # ------------------------------------------------------------------
    # 生命周期
    # ------------------------------------------------------------------

    def kill(self) -> None:
        resp = self._api_session.delete(f"{self._cfg.api_url}/sandboxes/{self.sandbox_id}")
        _raise_for_api_status(resp)

    def pause(self) -> None:
        resp = self._api_session.post(f"{self._cfg.api_url}/sandboxes/{self.sandbox_id}/pause")
        _raise_for_api_status(resp)

    def resume(self, timeout: int = 300) -> None:
        resp = self._api_session.post(f"{self._cfg.api_url}/sandboxes/{self.sandbox_id}/resume",
                                      json={"timeout": timeout})
        _raise_for_api_status(resp)

    def set_timeout(self, timeout: int) -> None:
        """延长 sandbox TTL（对齐 e2b_code_interpreter.Sandbox.set_timeout）。"""
        resp = self._api_session.post(f"{self._cfg.api_url}/sandboxes/{self.sandbox_id}/timeout",
                                      json={"timeout": timeout})
        _raise_for_api_status(resp)

    def get_info(self) -> dict:
        resp = self._api_session.get(f"{self._cfg.api_url}/sandboxes/{self.sandbox_id}")
        _raise_for_api_status(resp)
        return resp.json()

    # ------------------------------------------------------------------
    # context manager
    # ------------------------------------------------------------------

    def __enter__(self) -> "Sandbox":
        return self

    def __exit__(self, *args) -> None:
        try:
            self.kill()
        except Exception:
            pass
        if self._stream_client:
            self._stream_client.close()

    def __repr__(self) -> str:
        proxy = f", proxy={self._cfg.proxy_node_ip}" if self._cfg.proxy_node_ip else ""
        return f"Sandbox(id={self.sandbox_id!r}, domain={self.domain!r}{proxy})"
