#!/usr/bin/env python3
"""
cube_e2b OpenAPI 全链路验证
覆盖 cube-e2b-openapi.yaml 中全部已实现接口：

  GET  /health                        getHealth
  GET  /sandboxes                     listSandboxes
  GET  /v2/sandboxes                  listSandboxesV2  (state/limit 过滤)
  POST /sandboxes                     createSandbox
  GET  /sandboxes/:id                 getSandbox
  DELETE /sandboxes/:id               deleteSandbox
  POST /sandboxes/:id/pause           pauseSandbox
  POST /sandboxes/:id/resume          resumeSandbox  (deprecated)
  POST /sandboxes/:id/connect         connectSandbox
  POST /execute (via CubeProxy)       executeCode    (CUBE_PROXY_NODE_IP 绕过 DNS)

运行：
  pip3 install httpx requests
  python3 verify_openapi.py
"""
import os, sys, json, requests, httpx
from dataclasses import dataclass, field
from typing import List, Optional

# ── 配置 ─────────────────────────────────────────────────────────────────
CUBE_API_URL     = os.environ.get("CUBE_API_URL",       "http://9.135.79.34:3000")
CUBE_TEMPLATE_ID = os.environ.get("CUBE_TEMPLATE_ID",   "tpl-6265796cee124256b4dcd6a1")
PROXY_NODE_IP    = os.environ.get("CUBE_PROXY_NODE_IP", "9.135.79.34")
PROXY_PORT       = int(os.environ.get("CUBE_PROXY_PORT_HTTP", "80"))
API_KEY          = os.environ.get("E2B_API_KEY",        "dummy")
JUPYTER_PORT     = 49999


# ── DNS 绕过 Transport ────────────────────────────────────────────────────
class IPOverrideTransport(httpx.HTTPTransport):
    """
    yaml x-sdk-config.CUBE_PROXY_NODE_IP 实现。
    TCP 目标  → PROXY_NODE_IP:PROXY_PORT
    Host 头   → 原始虚拟主机名（CubeProxy 路由依据）
    """
    def __init__(self, ip, port, **kw):
        super().__init__(**kw)
        self._ip, self._port = ip, port

    def handle_request(self, req):
        orig_host = req.url.host
        url = req.url.copy_with(host=self._ip, port=self._port)
        r2 = httpx.Request(
            req.method, url,
            headers=[(k, orig_host if k.lower() == "host" else v)
                     for k, v in req.headers.raw],
            content=req.content,
        )
        return super().handle_request(r2)


# ── 数据模型 ──────────────────────────────────────────────────────────────
@dataclass
class Logs:
    stdout: List[str] = field(default_factory=list)
    stderr: List[str] = field(default_factory=list)

@dataclass
class ExecError:
    name: str; value: str; traceback: List[str] = field(default_factory=list)

@dataclass
class Result:
    text: Optional[str] = None
    is_main_result: bool = False

@dataclass
class Execution:
    results: List[Result] = field(default_factory=list)
    logs: Logs = field(default_factory=Logs)
    error: Optional[ExecError] = None

    @property
    def text(self):
        for r in self.results:
            if r.is_main_result: return r.text


def parse_line(execution, line, on_stdout=None):
    if not line: return
    try: d = json.loads(line)
    except: return
    t = d.pop("type", None)
    if t == "result":
        execution.results.append(Result(d.get("text"), d.get("is_main_result", False)))
    elif t == "stdout":
        execution.logs.stdout.append(d.get("text", ""))
        if on_stdout: on_stdout(d.get("text", ""))
    elif t == "stderr":
        execution.logs.stderr.append(d.get("text", ""))
    elif t == "error":
        execution.error = ExecError(d.get("name",""), d.get("value",""), d.get("traceback",[]))


# ── HTTP 客户端 ───────────────────────────────────────────────────────────
api = requests.Session()
api.headers.update({"X-API-Key": API_KEY, "Content-Type": "application/json"})

stream = httpx.Client(
    transport=IPOverrideTransport(PROXY_NODE_IP, PROXY_PORT) if PROXY_NODE_IP
              else httpx.HTTPTransport(),
    timeout=httpx.Timeout(connect=10, read=None, write=30, pool=30),
)


def execute_code(sandbox_id, domain, code, on_stdout=None):
    host = f"{JUPYTER_PORT}-{sandbox_id}.{domain}"
    ex = Execution()
    with stream.stream("POST", f"http://{host}/execute",
                       json={"code": code},
                       headers={"Content-Type": "application/json"}) as resp:
        if resp.status_code >= 400:
            raise RuntimeError(f"execute HTTP {resp.status_code}")
        for line in resp.iter_lines():
            parse_line(ex, line, on_stdout=on_stdout)
    return ex


# ── 工具函数 ──────────────────────────────────────────────────────────────
def sep(title):
    print(f"\n{'─'*54}\n  {title}\n{'─'*54}")

def ok(msg):   print(f"  ✅ {msg}")
def info(k,v): print(f"  {k:<22}: {v}")
def fail(msg): print(f"  ❌ {msg}"); sys.exit(1)


# ═══════════════════════════════════════════════════════════
print("=" * 54)
print("  cube_e2b OpenAPI 全链路验证")
print(f"  客户端: 9.134.82.254  →  服务: 9.135.79.34")
print(f"  API   : {CUBE_API_URL}")
print(f"  PROXY : {PROXY_NODE_IP}:{PROXY_PORT}  (DNS 绕过，无需 CoreDNS/mkcert)")
print("=" * 54)

# 1. GET /health
sep("1. GET /health  ─  getHealth")
r = api.get(f"{CUBE_API_URL}/health")
r.raise_for_status()
info("response", r.json())
assert r.json()["status"] == "ok"
ok("健康检查通过（无需鉴权）")

# 2. POST /sandboxes — 创建
sep("2. POST /sandboxes  ─  createSandbox")
r = api.post(f"{CUBE_API_URL}/sandboxes",
             json={"templateID": CUBE_TEMPLATE_ID, "timeout": 300})
r.raise_for_status()
c = r.json()
sid    = c["sandboxID"]
domain = c.get("domain", "cube.app")
info("sandboxID",   sid)
info("domain",      domain)
info("envdVersion", c.get("envdVersion"))
info("数据流 URL",  f"http://{JUPYTER_PORT}-{sid}.{domain}/execute")
info("DNS 绕过",    f"TCP→{PROXY_NODE_IP}:{PROXY_PORT}, Host={JUPYTER_PORT}-{sid}.{domain}")
ok("Sandbox 创建成功")

# 3. GET /sandboxes — v1 列表
sep("3. GET /sandboxes  ─  listSandboxes (v1)")
r = api.get(f"{CUBE_API_URL}/sandboxes")
r.raise_for_status()
all_ids = [s["sandboxID"] for s in r.json()]
info("total count", len(all_ids))
assert sid in all_ids
ok("v1 列表包含新建 sandbox")

# 4. GET /v2/sandboxes — v2 列表（state + limit 过滤）
sep("4. GET /v2/sandboxes  ─  listSandboxesV2 (state=running, limit=10)")
r = api.get(f"{CUBE_API_URL}/v2/sandboxes", params={"state": "running", "limit": 10})
r.raise_for_status()
v2 = r.json()
info("v2 count (filtered)", len(v2))
info("first state", v2[0].get("state") if v2 else "—")
info("has metadata", bool(v2[0].get("metadata")) if v2 else False)
ok("v2 列表（state/limit 过滤）成功")

# 5. GET /sandboxes/:id — 单个详情
sep("5. GET /sandboxes/:sandboxID  ─  getSandbox")
r = api.get(f"{CUBE_API_URL}/sandboxes/{sid}")
r.raise_for_status()
d = r.json()
info("state",     d.get("state"))
info("cpuCount",  d.get("cpuCount"))
info("memoryMB",  d.get("memoryMB"))
assert d.get("state") == "running"
ok("详情查询成功")

# 6. POST /execute — 数据流（经 CUBE_PROXY_NODE_IP 直连，全程无 DNS）
sep("6. POST /execute  ─  executeCode (via CubeProxy DNS 绕过)")
print(f"  实际 TCP: {PROXY_NODE_IP}:{PROXY_PORT}")
print(f"  Host 头 : {JUPYTER_PORT}-{sid}.{domain}")

print("\n  [6a] 变量持久化")
execute_code(sid, domain, "x = 1")
e = execute_code(sid, domain, "x += 1; x")
info("execution.text", repr(e.text))
assert e.text == "2"
ok("变量持久化 PASS")

print("\n  [6b] stdout 流式输出 + on_stdout 回调")
cb = []
e2 = execute_code(sid, domain, "for i in range(3): print(f'line {i}')",
                  on_stdout=lambda t: cb.append(t.strip()))
info("logs.stdout", e2.logs.stdout)
info("callback",    cb)
assert "line 0" in "".join(e2.logs.stdout)
ok("stdout 流式输出 PASS")

print("\n  [6c] 异常捕获 (StreamEventError)")
e3 = execute_code(sid, domain, "1/0")
info("error.name",  repr(e3.error.name))
info("error.value", repr(e3.error.value))
assert e3.error and e3.error.name == "ZeroDivisionError"
ok("异常捕获 PASS")

print("\n  [6d] 复杂表达式 (StreamEventResult)")
e4 = execute_code(sid, domain, "sum(range(101))")
info("execution.text", repr(e4.text))
assert e4.text == "5050"
ok("复杂表达式 PASS")

import time

# 7. POST /sandboxes/:id/pause
sep("7. POST /sandboxes/:sandboxID/pause  ─  pauseSandbox")
r = api.post(f"{CUBE_API_URL}/sandboxes/{sid}/pause")
info("status_code", r.status_code)
assert r.status_code in (200, 204)
# 等 sandbox 真正进入 paused 状态
for _ in range(10):
    time.sleep(2)
    state = api.get(f"{CUBE_API_URL}/sandboxes/{sid}").json().get("state")
    info("polling state", state)
    if state == "paused": break
ok("Sandbox 暂停成功")

# 8. POST /sandboxes/:id/resume (deprecated)
sep("8. POST /sandboxes/:sandboxID/resume  ─  resumeSandbox (deprecated)")
r = api.post(f"{CUBE_API_URL}/sandboxes/{sid}/resume",
             json={"timeout": 120})
info("status_code", r.status_code)
info("response",    r.json() if r.content else "(empty)")
assert r.status_code in (200, 201, 204)
ok("Sandbox resume 成功（deprecated，建议改用 connect）")

# 9. POST /sandboxes/:id/connect（替代 resume）
sep("9. POST /sandboxes/:sandboxID/connect  ─  connectSandbox")
# 先 pause，等 paused，再 connect
api.post(f"{CUBE_API_URL}/sandboxes/{sid}/pause")
for _ in range(10):
    time.sleep(2)
    state = api.get(f"{CUBE_API_URL}/sandboxes/{sid}").json().get("state")
    info("polling state", state)
    if state == "paused": break
r = api.post(f"{CUBE_API_URL}/sandboxes/{sid}/connect",
             json={"timeout": 120})
r.raise_for_status()
connected = r.json()
info("sandboxID",   connected.get("sandboxID"))
info("domain",      connected.get("domain"))
info("envdVersion", connected.get("envdVersion"))
assert connected["sandboxID"] == sid
ok("connectSandbox 成功（自动 resume，替代 resume 接口）")

# 10. DELETE /sandboxes/:id — 销毁
sep("10. DELETE /sandboxes/:sandboxID  ─  deleteSandbox")
r = api.delete(f"{CUBE_API_URL}/sandboxes/{sid}")
info("status_code", r.status_code)
assert r.status_code in (200, 204)
ok("Sandbox 销毁成功")

# 确认已从列表移除
r = api.get(f"{CUBE_API_URL}/sandboxes")
assert sid not in [s["sandboxID"] for s in r.json()]
ok("确认已从 v1 列表移除")

stream.close()

print("\n" + "=" * 54)
print("  全部通过 ✅  (9 个接口 + 4 个数据流用例)")
print(f"  验证机器 : 9.134.82.254 → 9.135.79.34")
print(f"  DNS 绕过 : CUBE_PROXY_NODE_IP={PROXY_NODE_IP}")
print(f"             TCP 直连，无需 CoreDNS / mkcert")
print("=" * 54)
