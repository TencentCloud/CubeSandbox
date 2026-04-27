# cube_e2b_code_interpreter — 实现与测试文档

> **替代目标**：官方 quickstart 中依赖 `mkcert` + `CoreDNS` 的方案
> ```bash
> # 官方方式（只能在部署机本机运行）
> export SSL_CERT_FILE="$(mkcert -CAROOT)/rootCA.pem"
> export E2B_API_URL="http://127.0.0.1:3000"
> ```
> **本方案**：用 `CUBE_PROXY_NODE_IP` 直连 CubeProxy IP，
> 无需 DNS、无需 mkcert，任意机器可用。

---

## 一、cube-api OpenAPI 规格（yaml）

> 基于 `.34` 实测接口逆向整理，cube-api 未暴露 `/openapi.json`。

```yaml
openapi: "3.0.3"
info:
  title: cube-api
  description: CubeSandbox REST API（E2B 兼容）
  version: "1.0.0"

servers:
  - url: "http://localhost:3000"
  - url: "http://{host}:3000"
    variables:
      host:
        default: "9.135.79.34"

paths:
  /health:
    get:
      summary: 健康检查
      operationId: getHealth
      responses:
        "200":
          content:
            application/json:
              schema:
                type: object
                properties:
                  status:   { type: string, example: "ok" }
                  sandboxes: { type: integer, example: 1 }

  /sandboxes:
    get:
      summary: 列出所有运行中的 sandbox
      operationId: listSandboxes
      security: [{ ApiKeyAuth: [] }]
      responses:
        "200":
          content:
            application/json:
              schema:
                type: array
                items: { $ref: "#/components/schemas/SandboxDetail" }

    post:
      summary: 创建 sandbox
      operationId: createSandbox
      security: [{ ApiKeyAuth: [] }]
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: "#/components/schemas/CreateSandboxRequest" }
            example:
              templateID: "tpl-6265796cee124256b4dcd6a1"
              timeout: 300
      responses:
        "200":
          content:
            application/json:
              schema: { $ref: "#/components/schemas/SandboxCreated" }
              example:
                templateID: "tpl-6265796cee124256b4dcd6a1"
                sandboxID:  "cf4595ba8f094a9cbf72ab76fa19e435"
                clientID:   "bdb40b5b-4777-488e-961a-68de8083ff41"
                envdVersion: "0.2.0"
                domain: "cube.app"

  /sandboxes/{sandboxID}:
    get:
      summary: 获取 sandbox 详情
      operationId: getSandbox
      security: [{ ApiKeyAuth: [] }]
      parameters: [{ $ref: "#/components/parameters/sandboxID" }]
      responses:
        "200":
          content:
            application/json:
              schema: { $ref: "#/components/schemas/SandboxDetail" }
    delete:
      summary: 销毁 sandbox
      operationId: deleteSandbox
      security: [{ ApiKeyAuth: [] }]
      parameters: [{ $ref: "#/components/parameters/sandboxID" }]
      responses:
        "200":
          description: 成功（空响应体）

  /sandboxes/{sandboxID}/pause:
    post:
      summary: 暂停 sandbox
      operationId: pauseSandbox
      security: [{ ApiKeyAuth: [] }]
      parameters: [{ $ref: "#/components/parameters/sandboxID" }]
      responses: { "200": { description: OK } }

  /sandboxes/{sandboxID}/resume:
    post:
      summary: 恢复 sandbox
      operationId: resumeSandbox
      security: [{ ApiKeyAuth: [] }]
      parameters: [{ $ref: "#/components/parameters/sandboxID" }]
      requestBody:
        content:
          application/json:
            schema:
              type: object
              properties:
                timeout: { type: integer, default: 300 }
      responses: { "200": { description: OK } }

# ── envd 数据流（通过 CubeProxy，不走 cube-api）─────────────────────────
# URL: http://49999-{sandboxID}.cube.app/execute
# DNS 绕过时：TCP 直连 CUBE_PROXY_NODE_IP:80，Host 头保留虚拟域名

x-envd-execute:
  method: POST
  url: "http://49999-{sandboxID}.{domain}/execute"
  requestBody:
    code: string         # 必填
    context_id: string   # 可选，跨 cell 共享状态
    language: string     # 可选，默认 python
    env_vars: object     # 可选，注入环境变量
  response: |
    ndjson 流，每行一个 JSON 事件：
    {"type":"stdout",   "text":"hello\n", "timestamp":"..."}
    {"type":"stderr",   "text":"warn\n",  "timestamp":"..."}
    {"type":"result",   "text":"2", "is_main_result":true, ...}
    {"type":"error",    "name":"NameError", "value":"...", "traceback":[...]}
    {"type":"number_of_executions", "execution_count":3}

components:
  securitySchemes:
    ApiKeyAuth:
      type: apiKey
      in: header
      name: X-API-Key
      description: 本地部署填任意字符串

  parameters:
    sandboxID:
      name: sandboxID
      in: path
      required: true
      schema: { type: string }

  schemas:
    CreateSandboxRequest:
      type: object
      required: [templateID]
      properties:
        templateID: { type: string }
        timeout:    { type: integer, default: 300 }
        envVars:    { type: object, additionalProperties: { type: string } }
        metadata:   { type: object, additionalProperties: { type: string } }

    SandboxCreated:
      type: object
      properties:
        templateID:  { type: string }
        sandboxID:   { type: string }
        clientID:    { type: string }
        envdVersion: { type: string }
        domain:      { type: string }

    SandboxDetail:
      type: object
      properties:
        templateID:  { type: string }
        sandboxID:   { type: string }
        clientID:    { type: string }
        startedAt:   { type: string, format: date-time }
        endAt:       { type: string, format: date-time }
        state:       { type: string, enum: [running, paused, stopped] }
        cpuCount:    { type: integer }
        memoryMB:    { type: integer }
        diskSizeMB:  { type: integer }
        envdVersion: { type: string }
        domain:      { type: string }
        metadata:    { type: object }

    ErrorResponse:
      type: object
      properties:
        code:    { type: integer }
        message: { type: string }
```

---

## 二、生成 Python SDK 的方式

### 方式 A：直接使用本仓库（推荐）

本仓库已是完整可用的 Python SDK，无需生成：

```bash
git clone <repo>
cd cube-e2b-sdk
pip install -e .          # 开发模式安装
# 或
pip install httpx requests  # 只装依赖，直接 sys.path 引用
```

### 方式 B：openapi-generator 从 yaml 生成

```bash
# 安装 openapi-generator
pip install openapi-generator-cli
# 或
npm install @openapitools/openapi-generator-cli -g

# 生成 Python SDK
openapi-generator-cli generate \
  -i cube-api-openapi.yaml \
  -g python \
  -o ./cube-api-client \
  --additional-properties=packageName=cube_api_client,projectName=cube-api-client

# 安装生成的 SDK
pip install -e ./cube-api-client
```

> ⚠️ 注意：openapi-generator 生成的是 **cube-api REST 部分**（创建/删除 sandbox）。
> `run_code` 的 ndjson 数据流部分不在标准 OpenAPI 范围内，需要用本仓库的 `transport.py` + `sandbox.py` 实现。

### 方式 C：datamodel-code-generator 生成 Pydantic 模型

```bash
pip install datamodel-code-generator

datamodel-codegen \
  --input cube-api-openapi.yaml \
  --input-file-type openapi \
  --output cube_api_models.py
```

---

## 三、SDK 核心实现说明

### DNS 绕过原理

```
官方方案（mkcert + CoreDNS）：
  客户端代码
      │ DNS: 49999-<id>.cube.app → 127.0.0.1（CoreDNS 本机解析）
      ↓
  CubeProxy:443 （mkcert 自签名 HTTPS）
      ↓
  sandbox envd:49999

本方案（CUBE_PROXY_NODE_IP）：
  客户端代码
      │ TCP 直连 9.135.79.34:80（无 DNS）
      │ Host: 49999-<id>.cube.app（CubeProxy 路由依据）
      ↓
  CubeProxy:80
      ↓
  sandbox envd:49999
```

### IPOverrideTransport（transport.py）

```python
class IPOverrideTransport(httpx.HTTPTransport):
    """等效于 curl --resolve host:port:ip"""
    def __init__(self, dest_ip, dest_port, **kw):
        super().__init__(**kw)
        self._dest_ip, self._dest_port = dest_ip, dest_port

    def handle_request(self, request):
        original_host = request.url.host          # 保留虚拟主机名
        url = request.url.copy_with(
            host=self._dest_ip, port=self._dest_port  # TCP 连接目标
        )
        new_req = httpx.Request(
            request.method, url,
            headers=[(k, original_host if k.lower()=="host" else v)
                     for k, v in request.headers.raw],  # Host 头不变
            content=request.content,
        )
        return super().handle_request(new_req)
```

### Sandbox.run_code 数据流

```
POST http://49999-{sandboxID}.cube.app/execute
     ↑
     经 IPOverrideTransport 实际 TCP 连接到 9.135.79.34:80
     Host 头保留 "49999-{sandboxID}.cube.app"
     ↓
ndjson 流式响应（每行一个 JSON 事件）
     ↓
parse_line() 解析 → Execution 对象
```

---

## 四、两台机器测试 Demo

### 环境信息

| 机器 | IP | 角色 |
|------|-----|------|
| 部署机 | 9.135.79.34 | CubeSandbox 部署，cube-api:3000，CubeProxy:80 |
| 客户端机 | 9.134.82.254 | 纯客户端，无 DNS，无证书 |

### 测试脚本（verify.py）

```python
#!/usr/bin/env python3
"""
在任意机器运行：
    pip3 install httpx requests
    python3 verify.py
"""
import os, sys

os.environ.setdefault("CUBE_API_URL",       "http://9.135.79.34:3000")
os.environ.setdefault("CUBE_TEMPLATE_ID",   "tpl-6265796cee124256b4dcd6a1")
os.environ.setdefault("CUBE_PROXY_NODE_IP", "9.135.79.34")

sys.path.insert(0, os.path.dirname(__file__))
from cube_e2b_code_interpreter import Sandbox

def main():
    print("=" * 55)
    print("  cube_e2b_code_interpreter — 验证数据流")
    print(f"  API:   {os.environ['CUBE_API_URL']}")
    print(f"  PROXY: {os.environ['CUBE_PROXY_NODE_IP']}")
    print("=" * 55)

    with Sandbox.create() as sb:
        print(f"\n[1] Sandbox 创建成功")
        print(f"    id:     {sb.sandbox_id}")
        print(f"    host:   {sb.get_host(49999)}")
        print(f"    proxy:  {sb._cfg.proxy_node_ip} (直连，无需 DNS)")

        print("\n[2] run_code — 变量持久化测试")
        sb.run_code("x = 1")
        execution = sb.run_code("x += 1; x")
        print(f"    execution.text = {execution.text!r}  (期望: '2')")
        assert execution.text == "2"
        print("    ✅ PASS")

        print("\n[3] run_code — stdout 流式输出测试")
        lines_received = []
        execution2 = sb.run_code(
            "for i in range(3): print(f'line {i}')",
            on_stdout=lambda msg: lines_received.append(msg.text)
        )
        print(f"    stdout lines: {execution2.logs.stdout}")
        print(f"    callback got: {lines_received}")
        assert "line 0" in "".join(execution2.logs.stdout)
        print("    ✅ PASS")

        print("\n[4] run_code — 异常捕获测试")
        execution3 = sb.run_code("1/0")
        print(f"    error.name  = {execution3.error.name!r}")
        print(f"    error.value = {execution3.error.value!r}")
        assert execution3.error is not None
        print("    ✅ PASS")

        print("\n[5] run_code — 复杂表达式")
        execution4 = sb.run_code("sum(range(101))")
        print(f"    execution.text = {execution4.text!r}  (期望: '5050')")
        assert execution4.text == "5050"
        print("    ✅ PASS")

    print("\n" + "=" * 55)
    print("  全部通过 ✅  Sandbox 已自动销毁")
    print("=" * 55)

if __name__ == "__main__":
    main()
```

### 测试执行命令

**在 9.134.82.254 上：**
```bash
# 1. 安装依赖
pip3 install httpx requests -q

# 2. 拉取 SDK
scp -P 36000 root@9.135.79.34:/tmp/cube-e2b-sdk.tar.gz /tmp/
tar xzf /tmp/cube-e2b-sdk.tar.gz -C /root/
cd /root/cube-e2b-sdk

# 3. 运行验证
python3 verify.py
```

### 测试结果（实测输出，2026-04-26）

**机器：9.134.82.254 → 9.135.79.34，跨机器，无 DNS，无证书**

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

## 五、与官方 quickstart 的完整对比

### 官方方案代码
```python
import os
from e2b_code_interpreter import Sandbox   # 原版

# 必须在部署机本机运行，且安装 mkcert
# export SSL_CERT_FILE="$(mkcert -CAROOT)/rootCA.pem"
# export E2B_API_URL="http://127.0.0.1:3000"
# export E2B_API_KEY="dummy"
# export CUBE_TEMPLATE_ID="tpl-xxx"

with Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"]) as sandbox:
    result = sandbox.run_code("print('Hello from Cube Sandbox!')")
    print(result)
```

### 本方案代码
```python
import os
from cube_e2b_code_interpreter import Sandbox  # 改一行 import

# 任意机器均可运行
# export CUBE_API_URL="http://9.135.79.34:3000"
# export CUBE_TEMPLATE_ID="tpl-xxx"
# export CUBE_PROXY_NODE_IP="9.135.79.34"    ← 唯一新增

with Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"]) as sandbox:
    result = sandbox.run_code("print('Hello from Cube Sandbox!')")
    print(result)  # 完全相同
```

### 差异对比表

| | 官方方案 | 本方案 |
|---|---|---|
| SDK | `e2b_code_interpreter` | `cube_e2b_code_interpreter` |
| 必须本机运行 | ✅ 是 | ❌ 任意机器 |
| 需要安装 mkcert | ✅ 是 | ❌ 否 |
| 需要 SSL_CERT_FILE | ✅ 是 | ❌ 否 |
| 需要 CoreDNS | ✅ 是（本机自动） | ❌ 否 |
| 协议 | HTTPS:443 | HTTP:80 |
| 新增环境变量 | 0 | 1（CUBE_PROXY_NODE_IP） |
| 迁移改动 | - | 改一行 import |

---

## 六、环境变量速查

| 变量 | 必填 | 默认值 | 说明 |
|------|:----:|--------|------|
| `CUBE_API_URL` | ✅ | `http://127.0.0.1:3000` | cube-api 地址 |
| `CUBE_TEMPLATE_ID` | ✅ | — | cubemastercli 创建的模板 ID |
| `CUBE_PROXY_NODE_IP` | 远程必填 | — | CubeProxy 节点 IP，绕过 DNS |
| `E2B_API_KEY` | — | `dummy` | 鉴权 key，本地填任意字符串 |
| `CUBE_PROXY_PORT_HTTP` | — | `80` | CubeProxy HTTP 端口 |
| `CUBE_SANDBOX_DOMAIN` | — | `cube.app` | sandbox 域名后缀 |
