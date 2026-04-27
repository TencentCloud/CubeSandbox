# cube_e2b_code_interpreter 交付文档

> **目标**：替代官方文档中依赖 `mkcert` + `CoreDNS` 的方案，
> 通过 `CUBE_PROXY_NODE_IP` 直连 CubeProxy IP，无需 DNS、无需证书，
> 直接对接 cube-api（`:3000`），跨机器开箱即用。

---

## 官方方案 vs 本方案

| | 官方方案（quickstart.md） | 本方案 |
|---|---|---|
| **SDK** | `e2b_code_interpreter`（原版） | `cube_e2b_code_interpreter`（drop-in） |
| **DNS** | CoreDNS 解析 `*.cube.app`（仅本机） | ❌ 不需要 |
| **证书** | `mkcert` 自签名 CA + `SSL_CERT_FILE` | ❌ 不需要 |
| **协议** | HTTPS（443） | HTTP（80） |
| **API** | `E2B_API_URL=http://127.0.0.1:3000` | `CUBE_API_URL=http://<ip>:3000` |
| **跨机器** | ❌ 需要在每台机器装 mkcert + 信任 CA | ✅ 设一个 IP 环境变量即可 |
| **迁移成本** | - | 改一行 import |

**核心原理**：`CUBE_PROXY_NODE_IP` 让 httpx 把 TCP 连接直连到代理 IP:80，
同时保留 `Host: 49999-<id>.cube.app` 头，CubeProxy 按此路由到对应 sandbox —— 完全绕过 DNS 解析。

---

## 目录结构

```
cube-e2b-sdk/
├── cube_e2b_code_interpreter/
│   ├── __init__.py       # 导出 Sandbox 等
│   ├── config.py         # 环境变量配置
│   ├── exceptions.py     # 异常层级
│   ├── models.py         # Execution / Result / Logs 数据模型
│   ├── transport.py      # IPOverrideTransport（DNS 绕过核心）
│   └── sandbox.py        # Sandbox 主类
├── verify.py             # 验证脚本
├── pyproject.toml
└── DELIVERY.md           # 本文档
```

---

## pyproject.toml

```toml
[build-system]
requires = ["setuptools>=68", "wheel"]
build-backend = "setuptools.backends.legacy:build"

[project]
name = "cube-e2b-code-interpreter"
version = "0.1.0"
description = "CubeSandbox Python SDK — drop-in replacement for e2b_code_interpreter"
readme = "README.md"
requires-python = ">=3.9"
license = { text = "Apache-2.0" }
dependencies = [
    "requests>=2.28",
    "httpx>=0.27",
    "websockets>=12.0",
]

[tool.setuptools.packages.find]
where = ["."]
include = ["cube_e2b*"]
```

**安装：**
```bash
# 开发模式（源码直接引用）
pip install -e /path/to/cube-e2b-sdk

# 或只装依赖后 sys.path 引用
pip install httpx requests
```

---

## 完整源码

### config.py

```python
from __future__ import annotations
import os
from dataclasses import dataclass, field

@dataclass
class SandboxConfig:
    """
    环境变量说明：

    CUBE_API_URL          cube-api 地址，默认 http://127.0.0.1:3000
    E2B_API_KEY           任意字符串（本地无需真实鉴权）
    CUBE_TEMPLATE_ID      模板 ID，cubemastercli tpl create 得到
    CUBE_PROXY_NODE_IP    ★ 关键：绕过 DNS，直连 CubeProxy IP
    CUBE_PROXY_PORT_HTTP  CubeProxy HTTP 端口，默认 80
    CUBE_SANDBOX_DOMAIN   sandbox 域名后缀，默认 cube.app
    """
    api_url: str = field(
        default_factory=lambda: os.environ.get(
            "CUBE_API_URL", os.environ.get("E2B_API_URL", "http://127.0.0.1:3000"))
    )
    api_key: str = field(
        default_factory=lambda: os.environ.get("E2B_API_KEY", "dummy")
    )
    template_id: str | None = field(
        default_factory=lambda: os.environ.get("CUBE_TEMPLATE_ID")
    )
    proxy_node_ip: str | None = field(
        default_factory=lambda: os.environ.get("CUBE_PROXY_NODE_IP")
    )
    proxy_port_http: int = field(
        default_factory=lambda: int(os.environ.get("CUBE_PROXY_PORT_HTTP", "80"))
    )
    sandbox_domain: str = field(
        default_factory=lambda: os.environ.get("CUBE_SANDBOX_DOMAIN", "cube.app")
    )
    default_timeout: int = 300
    request_timeout: float = 30.0

    def __post_init__(self):
        self.api_url = self.api_url.rstrip("/")
```

### transport.py

```python
"""
DNS 绕过核心。

官方方案：
    *.cube.app  ──CoreDNS──▶  127.0.0.1  ──▶  CubeProxy:443 (HTTPS + mkcert)

本方案（CUBE_PROXY_NODE_IP）：
    TCP 直连 CUBE_PROXY_NODE_IP:80
    Host: 49999-<sandboxID>.cube.app   ← CubeProxy 按此路由
    无需 DNS，无需证书
"""
from __future__ import annotations
import ssl
import httpx
from .config import SandboxConfig


class IPOverrideTransport(httpx.HTTPTransport):
    """
    等效于 curl --resolve <host>:<port>:<ip>
    把 TCP 连接重定向到固定 IP:port，同时保留 Host 头。
    """
    def __init__(self, dest_ip: str, dest_port: int, ssl_context=None, **kw):
        super().__init__(verify=ssl_context if ssl_context else True, **kw)
        self._dest_ip = dest_ip
        self._dest_port = dest_port

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        original_host = request.url.host
        url = request.url.copy_with(host=self._dest_ip, port=self._dest_port)
        new_request = httpx.Request(
            method=request.method,
            url=url,
            headers=[(k, original_host if k.lower() == "host" else v)
                     for k, v in request.headers.raw],
            content=request.content,
        )
        return super().handle_request(new_request)


def build_httpx_client(config: SandboxConfig) -> httpx.Client:
    """
    CUBE_PROXY_NODE_IP 已设置 → IPOverrideTransport 直连
    未设置              → 普通 httpx（依赖 OS DNS，适合部署机本机使用）
    """
    if config.proxy_node_ip:
        transport = IPOverrideTransport(
            dest_ip=config.proxy_node_ip,
            dest_port=config.proxy_port_http,
        )
    else:
        transport = httpx.HTTPTransport()

    return httpx.Client(
        transport=transport,
        timeout=httpx.Timeout(connect=config.request_timeout, read=None, write=30, pool=30),
        follow_redirects=True,
    )
```

### models.py

```python
from __future__ import annotations
import json
from dataclasses import dataclass, field
from typing import List, Optional, Callable


@dataclass
class OutputMessage:
    text: str
    timestamp: str = ""
    is_stderr: bool = False


@dataclass
class Logs:
    stdout: List[str] = field(default_factory=list)
    stderr: List[str] = field(default_factory=list)


@dataclass
class ExecutionError:
    name: str
    value: str
    traceback: List[str] = field(default_factory=list)


@dataclass
class Result:
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


@dataclass
class Execution:
    results: List[Result] = field(default_factory=list)
    logs: Logs = field(default_factory=Logs)
    error: Optional[ExecutionError] = None
    execution_count: Optional[int] = None

    @property
    def text(self) -> Optional[str]:
        """最后一行表达式的文本结果，对齐 e2b execution.text"""
        for r in self.results:
            if r.is_main_result:
                return r.text
        return None


@dataclass
class Context:
    id: str
    language: str = "python"
    cwd: str = "/home/user"


def parse_line(execution: Execution, line: str,
               on_stdout=None, on_stderr=None,
               on_result=None, on_error=None) -> None:
    """解析 /execute 流式 ndjson 每一行"""
    if not line:
        return
    try:
        data = json.loads(line)
    except json.JSONDecodeError:
        return
    t = data.pop("type", None)
    if t == "result":
        r = Result(**{k: v for k, v in data.items() if k in Result.__dataclass_fields__})
        execution.results.append(r)
        if on_result: on_result(r)
    elif t == "stdout":
        execution.logs.stdout.append(data.get("text", ""))
        if on_stdout: on_stdout(OutputMessage(data.get("text", ""), data.get("timestamp", "")))
    elif t == "stderr":
        execution.logs.stderr.append(data.get("text", ""))
        if on_stderr: on_stderr(OutputMessage(data.get("text", ""), is_stderr=True))
    elif t == "error":
        execution.error = ExecutionError(
            data.get("name", ""), data.get("value", ""), data.get("traceback", []))
        if on_error: on_error(execution.error)
    elif t == "number_of_executions":
        execution.execution_count = data.get("execution_count")
```

### exceptions.py

```python
from __future__ import annotations

class CubeCodeInterpreterError(Exception):
    def __init__(self, message: str, status_code: int | None = None):
        super().__init__(message)
        self.status_code = status_code

class SandboxNotFoundError(CubeCodeInterpreterError): pass
class TemplateNotFoundError(CubeCodeInterpreterError): pass
class AuthenticationError(CubeCodeInterpreterError): pass
class ApiError(CubeCodeInterpreterError): pass
```

### sandbox.py

```python
from __future__ import annotations
from typing import Any, Callable, Dict, Optional
import httpx
import requests
from .config import SandboxConfig
from .exceptions import ApiError, AuthenticationError, SandboxNotFoundError, TemplateNotFoundError
from .models import Context, Execution, ExecutionError, OutputMessage, Result, parse_line
from .transport import build_httpx_client

JUPYTER_PORT = 49999  # 与 e2b_code_interpreter.constants.JUPYTER_PORT 一致


def _raise_for_status(resp: requests.Response) -> None:
    if resp.ok: return
    try: msg = resp.json().get("message") or resp.json().get("detail") or resp.text
    except: msg = resp.text or f"HTTP {resp.status_code}"
    code = resp.status_code
    if code in (401, 403): raise AuthenticationError(msg, code)
    if code == 404:
        if "template" in msg.lower(): raise TemplateNotFoundError(msg, code)
        raise SandboxNotFoundError(msg, code)
    raise ApiError(msg, code)


class Sandbox:
    """
    CubeSandbox Python SDK — 与 e2b_code_interpreter.Sandbox 接口完全对齐。
    改一行 import 即可从 e2b cloud 迁移到 CubeSandbox 私有部署。
    """

    def __init__(self, data: dict, config: SandboxConfig) -> None:
        self._data = data
        self._cfg = config
        self._api = requests.Session()
        self._api.headers.update({"X-API-Key": config.api_key,
                                   "Content-Type": "application/json"})
        self._stream: httpx.Client | None = None

    # ── 属性 ──────────────────────────────────────────────────────────

    @property
    def sandbox_id(self) -> str:
        return self._data["sandboxID"]

    @property
    def domain(self) -> str:
        return self._data.get("domain") or self._cfg.sandbox_domain

    def get_host(self, port: int) -> str:
        """e2b: 49999-<id>.e2b.app  →  cube: 49999-<id>.cube.app"""
        return f"{port}-{self.sandbox_id}.{self.domain}"

    @property
    def _jupyter_url(self) -> str:
        return f"http://{self.get_host(JUPYTER_PORT)}"

    # ── 工厂方法 ──────────────────────────────────────────────────────

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
        cfg = config or SandboxConfig()
        tpl = template or cfg.template_id
        if not tpl:
            raise ValueError("template 未提供，请设置 CUBE_TEMPLATE_ID")
        s = requests.Session()
        s.headers.update({"X-API-Key": cfg.api_key, "Content-Type": "application/json"})
        payload = {"templateID": tpl, "timeout": timeout or cfg.default_timeout}
        if env_vars: payload["envVars"] = env_vars
        if metadata: payload["metadata"] = metadata
        payload.update(kwargs)
        resp = s.post(f"{cfg.api_url}/sandboxes", json=payload)
        _raise_for_status(resp)
        return cls(resp.json(), config=cfg)

    @classmethod
    def connect(cls, sandbox_id: str, *, config: SandboxConfig | None = None) -> "Sandbox":
        cfg = config or SandboxConfig()
        s = requests.Session()
        s.headers.update({"X-API-Key": cfg.api_key})
        resp = s.get(f"{cfg.api_url}/sandboxes/{sandbox_id}")
        _raise_for_status(resp)
        return cls(resp.json(), config=cfg)

    # ── run_code ──────────────────────────────────────────────────────

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
    ) -> Execution:
        if self._stream is None:
            self._stream = build_httpx_client(self._cfg)
        execution = Execution()
        with self._stream.stream(
            "POST", f"{self._jupyter_url}/execute",
            json={"code": code, "context_id": context.id if context else None,
                  "language": language, "env_vars": envs},
            headers={"Content-Type": "application/json"},
            timeout=httpx.Timeout(connect=self._cfg.request_timeout,
                                   read=timeout, write=30, pool=30),
        ) as resp:
            if resp.status_code >= 400:
                raise ApiError(f"execute HTTP {resp.status_code}", resp.status_code)
            for line in resp.iter_lines():
                parse_line(execution, line, on_stdout=on_stdout, on_stderr=on_stderr,
                           on_result=on_result, on_error=on_error)
        return execution

    # ── 生命周期 ──────────────────────────────────────────────────────

    def kill(self) -> None:
        resp = self._api.delete(f"{self._cfg.api_url}/sandboxes/{self.sandbox_id}")
        _raise_for_status(resp)

    def set_timeout(self, timeout: int) -> None:
        resp = self._api.post(f"{self._cfg.api_url}/sandboxes/{self.sandbox_id}/timeout",
                              json={"timeout": timeout})
        _raise_for_status(resp)

    def __enter__(self) -> "Sandbox": return self

    def __exit__(self, *args) -> None:
        try: self.kill()
        except: pass
        if self._stream: self._stream.close()

    def __repr__(self) -> str:
        proxy = f", proxy={self._cfg.proxy_node_ip}" if self._cfg.proxy_node_ip else ""
        return f"Sandbox(id={self.sandbox_id!r}{proxy})"
```

### __init__.py

```python
from .sandbox import Sandbox
from .models import Execution, Result, Logs, ExecutionError, OutputMessage, Context
from .config import SandboxConfig
from .exceptions import CubeCodeInterpreterError

__all__ = ["Sandbox", "Execution", "Result", "Logs", "ExecutionError",
           "OutputMessage", "Context", "SandboxConfig"]
__version__ = "0.1.0"
```

---

## Demo

### 最小用法（与官方 quickstart 对比）

```python
# ── 官方方案（需要 mkcert + CoreDNS）──────────────────────────────
import os
from e2b_code_interpreter import Sandbox

# 必须在部署机本机运行，且装了 mkcert
# export SSL_CERT_FILE="$(mkcert -CAROOT)/rootCA.pem"
# export E2B_API_URL="http://127.0.0.1:3000"
# export E2B_API_KEY="dummy"
# export CUBE_TEMPLATE_ID="tpl-xxx"

with Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"]) as sandbox:
    result = sandbox.run_code("print('hello')")
    print(result)


# ── 本方案（任意机器，无需 DNS/证书）─────────────────────────────
import os
from cube_e2b_code_interpreter import Sandbox  # 改这一行

# 任意机器上设置：
# export CUBE_API_URL="http://9.135.79.34:3000"
# export CUBE_TEMPLATE_ID="tpl-xxx"
# export CUBE_PROXY_NODE_IP="9.135.79.34"   ← 唯一新增

with Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"]) as sandbox:
    result = sandbox.run_code("print('hello')")  # 完全相同
    print(result)
```

### 完整 Demo

```python
import os
from cube_e2b_code_interpreter import Sandbox

# 环境变量配置
os.environ["CUBE_API_URL"]       = "http://9.135.79.34:3000"
os.environ["CUBE_TEMPLATE_ID"]   = "tpl-6265796cee124256b4dcd6a1"
os.environ["CUBE_PROXY_NODE_IP"] = "9.135.79.34"  # 绕过 *.cube.app DNS

with Sandbox.create() as sb:
    # 1. 变量跨 cell 持久
    sb.run_code("x = 1")
    e = sb.run_code("x += 1; x")
    print(e.text)           # "2"

    # 2. 捕获 stdout
    e2 = sb.run_code(
        "for i in range(3): print(f'line {i}')",
        on_stdout=lambda msg: print(">>", msg.text.strip())
    )
    print(e2.logs.stdout)   # ['line 0\nline 1\nline 2\n']

    # 3. 异常处理
    e3 = sb.run_code("1/0")
    print(e3.error.name)    # ZeroDivisionError
    print(e3.error.value)   # division by zero

    # 4. 数学计算
    e4 = sb.run_code("sum(range(101))")
    print(e4.text)          # "5050"

# with 块退出时自动调用 sandbox.kill()
```

---

## 验证结果（9.134.82.254 → 9.135.79.34，跨机器）

```
=======================================================
  cube_e2b_code_interpreter — 验证数据流
  API:   http://9.135.79.34:3000
  PROXY: 9.135.79.34
=======================================================

[1] Sandbox 创建成功
    id:     cf5c5b77f01641458dd30097b1c14060
    host:   49999-cf5c5b77f01641458dd30097b1c14060.cube.app
    proxy:  9.135.79.34 (直连，无需 DNS)

[2] run_code — 变量持久化测试
    execution.text = '2'  (期望: '2')
    ✅ PASS

[3] run_code — stdout 流式输出测试
    stdout lines: ['line 0\nline 1\nline 2\n']
    callback got: ['line 0\nline 1\nline 2\n']
    ✅ PASS

[4] run_code — 异常捕获测试
    error.name  = 'ZeroDivisionError'
    error.value = 'division by zero'
    ✅ PASS

[5] run_code — 复杂表达式
    execution.text = '5050'  (期望: '5050')
    ✅ PASS

=======================================================
  全部通过 ✅  Sandbox 已自动销毁
=======================================================
```

---

## 环境变量速查

| 变量 | 必填 | 默认值 | 说明 |
|------|------|--------|------|
| `CUBE_API_URL` | ✅ | `http://127.0.0.1:3000` | cube-api 地址 |
| `CUBE_TEMPLATE_ID` | ✅ | — | 模板 ID |
| `CUBE_PROXY_NODE_IP` | 远程机器必填 | — | CubeProxy 节点 IP，绕过 DNS |
| `E2B_API_KEY` | — | `dummy` | 任意字符串 |
| `CUBE_PROXY_PORT_HTTP` | — | `80` | CubeProxy HTTP 端口 |
| `CUBE_SANDBOX_DOMAIN` | — | `cube.app` | sandbox 域名后缀 |
