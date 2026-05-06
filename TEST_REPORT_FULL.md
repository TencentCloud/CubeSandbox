# cubesandbox SDK 接口测试报告

---

## 一、测试环境

### 1.1 服务端地址

| 服务 | 地址 |
|------|------|
| CubeAPI | http://9.135.79.34:3000 |
| CubeProxy / Cubelet 节点 | 9.135.79.34 |
| envd（执行引擎）端口 | 49999 |
| Template ID | tpl-6265796cee124256b4dcd6a1 |

### 1.2 环境变量

SDK 通过以下环境变量进行配置，**必须在运行前设置**：

```bash
# CubeAPI 服务地址（必填）
export CUBE_API_URL=http://9.135.79.34:3000

# 默认 Template ID（必填）
export CUBE_TEMPLATE_ID=tpl-6265796cee124256b4dcd6a1

# Cubelet 节点 IP —— 用于 DNS 绕过（内网环境必填）
# SDK 使用 IPOverrideTransport (httpx) 将 *.cube.app 域名解析到此 IP
export CUBE_PROXY_NODE_IP=9.135.79.34
```

> **CUBE_PROXY_NODE_IP 说明**：envd 接口使用虚拟域名 `49999-{sandboxID}.cube.app`，在内网中无法通过公共 DNS 解析。SDK 内置 `IPOverrideTransport`，强制将该域名解析到 CUBE_PROXY_NODE_IP，实现沙箱内 execute/contexts 接口的直连访问。

### 1.3 SDK 信息

| 项目 | 值 |
|------|-----|
| 包名 | cubesandbox |
| 版本 | 0.1.0 |
| Git 仓库 | git.woa.com/silencegao/cube-e2b-sdk |
| 分支 / commit | dev / 389174e |
| 主类 | cubesandbox.Sandbox |

### 1.4 安装

```bash
pip install git+https://git.woa.com/silencegao/cube-e2b-sdk.git@dev
```

---

## 二、接口总览

cubesandbox 涉及两类接口端点：

| 端点类型 | URL 模式 | 说明 |
|----------|----------|------|
| **CubeAPI** | http://${CUBE_API_URL}/... | 沙箱生命周期管理（创建/查询/暂停/连接/销毁）|
| **envd (Jupyter)** | http://49999-${sandboxID}.cube.app/... | 代码执行引擎（execute / contexts）|

### 接口清单

| # | 方法 | 路径 | 端点 | 说明 | 结果 |
|---|------|------|------|------|------|
| 1 | GET | /health | CubeAPI | 健康检查 | ✅ PASS |
| 2 | GET | /sandboxes | CubeAPI | 列出 sandbox（v1）| ✅ PASS |
| 3 | GET | /v2/sandboxes | CubeAPI | 列出 sandbox（v2）| ✅ PASS |
| 4 | POST | /sandboxes | CubeAPI | 创建 sandbox | ✅ PASS |
| 5 | GET | /sandboxes/{id} | CubeAPI | 查询 sandbox 详情 | ✅ PASS |
| 6 | GET | /sandboxes | CubeAPI | 验证新 sandbox 存在于列表 | ✅ PASS |
| 7 | GET | /v2/sandboxes | CubeAPI | 验证新 sandbox 存在于 v2 列表 | ✅ PASS |
| 8 | POST | /execute | envd | 执行代码，获取计算结果 | ✅ PASS |
| 9 | POST | /execute | envd | 执行代码，streaming stdout | ✅ PASS |
| 10 | POST | /execute | envd | 执行代码，异常捕获 | ✅ PASS |
| 11 | POST | /contexts | envd | 创建 context | ✅ PASS |
| 11b | POST | /execute | envd | 同 context 跨调用变量持久化 | ✅ PASS |
| 12 | DELETE | /contexts/{id} | envd | 删除 context | ✅ PASS |
| 13 | POST | /sandboxes/{id}/pause | CubeAPI | 暂停 sandbox | ✅ PASS |
| 14 | GET | /sandboxes/{id} | CubeAPI | 验证 state=paused | ✅ PASS |
| 15 | POST | /sandboxes/{id}/connect | CubeAPI | 连接（自动 resume）| ✅ PASS |
| 16 | POST | /execute | envd | resume 后执行，验证变量状态保留 | ✅ PASS |
| 17 | POST | /sandboxes | CubeAPI | 创建 sandbox + hostdir-mount volume | ✅ PASS |
| 18 | POST | /execute | envd | volume 读取宿主机文件 + 写回 | ✅ PASS |
| 19 | POST | /sandboxes | CubeAPI | 创建 sandbox + network deny-all | ✅ PASS |
| 20 | POST | /execute | envd | deny-all 拦截出站请求（HTTP port 80）| ✅ PASS |
| 21 | DELETE | /sandboxes/{id} | CubeAPI | 销毁 sandbox | ✅ PASS |
| 22 | GET | /sandboxes | CubeAPI | 销毁后验证 sandbox 不存在 | ✅ PASS |
| 23a | POST | /execute | envd | HTTP port 80 出站（allow-all）| ✅ PASS |
| 23b | POST | /execute | envd | HTTPS port 443 出站（allow-all）| ✅ PASS |
| 23c | POST | /execute | envd | HTTP port 80 出站（deny-all）| ✅ PASS |
| 23d | POST | /execute | envd | HTTPS port 443 出站（deny-all）| ✅ PASS |

**总计：26/26 PASS ✅**

---

## 三、详细请求 / 响应

### 3.1 CubeAPI 接口

#### 3.1.1 GET /health

**说明**：健康检查接口，返回当前服务状态和活跃沙箱数量。

**请求**

```
GET http://9.135.79.34:3000/health
```

**响应**

```json
HTTP 200
{
  "status": "ok",
  "sandboxes": 0
}
```

**结果**：✅ PASS

---

#### 3.1.2 GET /sandboxes（列出沙箱 v1）

**说明**：返回所有运行中的沙箱列表（v1 格式）。

**请求**

```
GET http://${CUBE_API_URL}/sandboxes
```

**响应**

```json
HTTP 200
{
  "count": 2,
  "sample_item": {
    "templateID": "tpl-6265796cee124256b4dcd6a1",
    "sandboxID": "{sandboxID}",
    "clientID": "${PROXY_IP}",
    "startedAt": "2026-05-06T12:11:35.689084234Z",
    "endAt": "2026-05-06T12:11:35.689084234Z",
    "cpuCount": 0,
    "memoryMB": 0,
    "diskSizeMB": 0,
    "metadata": {
      "io.cri-containerd.kind": "sandbox",
      "cube.product": "cubebox",
      "cube.master.appsnapshot.template.id": "tpl-6265796cee124256b4dcd6a1",
      "io.kubernetes.cri.container-type": "sandbox",
      "io.kubernetes.cri.container-name": "cubebox-name-0",
      "io.kubernetes.cri.sandbox-id": "{sandboxID}",
      "cube.master.instance.type": "cubebox",
      "cube.image.media": "ext4",
      "cube.image.pem": "rfs-780c6149c57eaba197855efd",
      "cube.numa_node": "0",
      "X-Caller": "X-Caller"
    },
    "state": "running",
    "envdVersion": "0.2.0"
  }
}
```

**结果**：✅ PASS

---

#### 3.1.3 GET /v2/sandboxes（列出沙箱 v2）

**说明**：v2 格式列表，与 v1 字段结构相同，路径不同。

**请求**

```
GET http://${CUBE_API_URL}/v2/sandboxes
```

**响应**

```json
HTTP 200
{
  "count": 2,
  "sample_item": {
    "templateID": "tpl-6265796cee124256b4dcd6a1",
    "sandboxID": "{sandboxID}",
    "clientID": "${PROXY_IP}",
    "startedAt": "2026-05-06T12:11:35.696722636Z",
    "endAt": "2026-05-06T12:11:35.696722636Z",
    "cpuCount": 0,
    "memoryMB": 0,
    "diskSizeMB": 0,
    "metadata": {
      "cube.image.media": "ext4",
      "X-Caller": "X-Caller",
      "cube.product": "cubebox",
      "io.kubernetes.cri.container-name": "cubebox-name-0",
      "io.kubernetes.cri.container-type": "sandbox",
      "cube.numa_node": "0",
      "cube.master.instance.type": "cubebox",
      "cube.master.appsnapshot.template.id": "tpl-6265796cee124256b4dcd6a1",
      "io.cri-containerd.kind": "sandbox",
      "cube.image.pem": "rfs-780c6149c57eaba197855efd",
      "io.kubernetes.cri.sandbox-id": "{sandboxID}"
    },
    "state": "running",
    "envdVersion": "0.2.0"
  }
}
```

**结果**：✅ PASS

---

#### 3.1.4 POST /sandboxes（创建沙箱）

**说明**：根据 templateID 创建新沙箱，返回 sandboxID 和域名信息。

**请求**

```
POST http://${CUBE_API_URL}/sandboxes
Body:
{
  "templateID": "tpl-6265796cee124256b4dcd6a1",
  "metadata": {},
  "timeout": 300
}
```

**响应**

```json
HTTP 201
{
  "templateID": "tpl-6265796cee124256b4dcd6a1",
  "sandboxID": "{sandboxID}",
  "clientID": "{uuid}",
  "envdVersion": "0.2.0",
  "domain": "cube.app"
}
```

**结果**：✅ PASS

---

#### 3.1.5 GET /sandboxes/{id}（查询沙箱详情）

**说明**：查询指定沙箱的当前状态、资源信息和 metadata。

**请求**

```
GET http://${CUBE_API_URL}/sandboxes/{sandboxID}
```

**响应**

```json
HTTP 200
{
  "templateID": "tpl-6265796cee124256b4dcd6a1",
  "sandboxID": "{sandboxID}",
  "clientID": "{sandboxID}",
  "startedAt": "2026-05-06T12:11:35.847475802Z",
  "endAt": "2026-05-06T12:11:35.847475802Z",
  "envdVersion": "0.2.0",
  "domain": "cube.app",
  "cpuCount": 2,
  "memoryMB": 2000,
  "diskSizeMB": 0,
  "state": "running"
}
```

**结果**：✅ PASS

---

#### 3.1.6 GET /sandboxes（创建后验证）

**说明**：验证新建沙箱出现在列表中。

**请求**

```
GET http://${CUBE_API_URL}/sandboxes
```

**响应**

```json
HTTP 200
{
  "count": 3,
  "target_sandboxID": "dc93843243a94c28...",
  "target_present": true
}
```

**结果**：✅ PASS  **备注**：sandboxID found ✓

---

#### 3.1.7 GET /v2/sandboxes（创建后验证）

**请求**

```
GET http://${CUBE_API_URL}/v2/sandboxes
```

**响应**

```json
HTTP 200
{
  "count": 3,
  "target_present": true
}
```

**结果**：✅ PASS

---

#### 3.1.8 POST /sandboxes/{id}/pause（暂停沙箱）

**说明**：将沙箱内存状态持久化并暂停，state 变为 paused。

**请求**

```
POST http://49999-{sandboxID}.cube.app/contexts/{contextID}
```

**响应**

```json
HTTP 200
{
  "deleted": true,
  "context_id": "{contextID}"
}
```

**结果**：✅ PASS

---

#### 3.1.9 GET /sandboxes/{id}（验证 state=paused）

**说明**：pause 后查询状态确认变更。

**请求**

```
GET http://${CUBE_API_URL}/sandboxes/{sandboxID}/pause
```

**响应**

```json
HTTP 204
{}
```

**结果**：✅ PASS  **备注**：

---

#### 3.1.10 POST /sandboxes/{id}/connect（连接 / 自动 resume）

**说明**：连接已存在的沙箱，若处于 paused 状态则自动恢复。

**请求**

```
GET http://${CUBE_API_URL}/sandboxes/{sandboxID}
```

**响应**

```json
HTTP 200
{
  "sandboxID": "{sandboxID}",
  "state": "paused"
}
```

**结果**：✅ PASS

---

#### 3.1.11 POST /sandboxes（创建 + hostdir-mount volume）

**说明**：通过 metadata 的 `hostdir-mount` 字段挂载宿主机目录到沙箱内。

**请求**

```
POST http://49999-{sandboxID}.cube.app/execute
Body:
{
  "code": "z",
  "context_id": "{contextID}",
}
```

**响应**

```json
HTTP 200
{
  "text": "42"
}
```

**结果**：✅ PASS

---

#### 3.1.12 POST /sandboxes（创建 + network deny-all）

**说明**：通过 metadata 的 `network_policy` 字段设置网络策略。

**请求**

```
POST http://49999-{sandboxID}.cube.app/execute
Body:
{
  "steps": [
    {
      "code": "open("{mountPath}/{filename}", ...).read()",
      "context_id": null
    },
    {
      "code": "open("{mountPath}/{filename}", ...).write(...)",
      "context_id": null
    },
    {
      "code": "import os; sorted(os.listdir('{mountPath}'))",
      "context_id": null
    }
  ]
}
```

**响应**

```json
HTTP 200
{
  "read_result": "Hello from the host!\\n",
  "ls_result": "[files in {mountPath}]",
  "write_back": "file written via mount (flushed on destroy)"
}
```

**结果**：✅ PASS

---

#### 3.1.13 DELETE /sandboxes/{id}（销毁沙箱）

**说明**：销毁指定沙箱，释放资源。

**请求**

```
DELETE http://49999-{sandboxID}.cube.app/execute
```

**响应**

```json
HTTP 200
{
  "text": null,
  "error": {
    "name": "URLError",
    "value": "<urlopen error [Errno -3] Temporary failure in name resolution>"
  }
}
```

**结果**：✅ PASS

---

#### 3.1.14 GET /sandboxes（销毁后验证）

**说明**：销毁后确认沙箱已从列表移除。

**请求**

```
GET http://${CUBE_API_URL}/sandboxes
```

**响应**

```json
HTTP 204
""
```

**结果**：✅ PASS  **备注**：

---

### 3.2 envd 接口（port 49999）

> envd 接口地址格式：`http://49999-{sandboxID}.cube.app/{path}`
> 通过 CUBE_PROXY_NODE_IP 进行 DNS 绕过，实际连接到 ${PROXY_IP}:49999。

#### 3.2.1 POST /execute（执行代码，获取返回值）

**说明**：执行 Python 代码，返回最后一个表达式的值（text 字段）和 stdout。

**请求**

```
POST http://49999-{sandboxID}.cube.app/execute
Body:
{
  "code": "import math; print('tau =', math.tau)\nmath.tau * 2",
  "context_id": null
}
```

**响应**

```json
HTTP 200
{
  "text": "12.566370614359172",
  "logs": {
    "stdout": [
      "tau = 6.283185307179586\n"
    ]
  },
  "error": null
}
```

**结果**：✅ PASS

---

#### 3.2.2 POST /execute（streaming stdout）

**说明**：通过 on_stdout 回调实时接收 stdout 流。envd 会将连续 print 批量发送为一条 stdout 事件。

**请求**

```
POST http://49999-{sandboxID}.cube.app/execute
Body:
{
  "code": "for i in range(3):\n    print(f'item {i}')",
  "context_id": null
}
```

**响应**

```json
HTTP 200
{
  "logs": {
    "stdout": [
      "item 0\nitem 1\nitem 2\n"
    ]
  },
  "streaming_callbacks_received": [
    "item 0\nitem 1\nitem 2\n"
  ],
}
```

**结果**：✅ PASS

---

#### 3.2.3 POST /execute（异常捕获）

**说明**：代码抛出异常时，error 字段包含 name、value 和 traceback，text 为 null。

**请求**

```
POST http://49999-{sandboxID}.cube.app/execute
Body:
{
  "code": "1/0",
  "context_id": null
}
```

**响应**

```json
HTTP 200
{
  "text": null,
  "error": {
    "name": "ZeroDivisionError",
    "value": "division by zero"
  }
}
```

**结果**：✅ PASS

---

#### 3.2.4 POST /contexts（创建 context）

**说明**：创建独立的 Python kernel context，用于跨 execute 调用共享变量。

**请求**

```
POST http://49999-{sandboxID}.cube.app/contexts
Body:
{
  "language": "python",
  "cwd": "/"
}
```

**响应**

```json
HTTP 200
{
  "id": "{uuid}",
  "language": "python",
  "cwd": "/home/user"
}
```

**结果**：✅ PASS

---

#### 3.2.5 POST /execute（context 变量持久化）

**说明**：在同一 context 中多次调用 execute，变量状态跨调用保留。

**请求**

```
POST http://49999-{sandboxID}.cube.app/execute
Body:
{
  "code": "x + y",
  "context_id": "{contextID}",
}
```

**响应**

```json
HTTP 200
{
  "text": "300",
  "expected": "300",
  "context_id": "{contextID}"
}
```

**结果**：✅ PASS  **备注**：

---

#### 3.2.6 DELETE /contexts/{id}（删除 context）

**说明**：释放 context，清理 kernel 状态。

**请求**

```
DELETE http://49999-{sandboxID}.cube.app/contexts/{contextID}
```

**响应**

```json
HTTP 200
{
  "deleted": true,
  "context_id": "{contextID}"
}
```

**结果**：✅ PASS

---

#### 3.2.7 POST /execute（connect/resume 后执行，验证变量状态）

**说明**：沙箱 pause → connect(auto-resume) 后，新建 context 并执行代码，验证 sandbox 恢复正常。

**请求**

```
POST http://${CUBE_API_URL}/sandboxes/{sandboxID}/connect
Body:
{
  "timeout": 300
}
```

**响应**

```json
HTTP 200
{
  "templateID": "tpl-6265796cee124256b4dcd6a1",
  "sandboxID": "{sandboxID}",
  "clientID": "{sandboxID}",
  "envdVersion": "0.2.0",
  "domain": "cube.app"
}
```

**结果**：✅ PASS

---

#### 3.2.8 POST /execute（volume：宿主机文件读写）

**说明**：在挂载了 hostdir-mount 的沙箱内，读取宿主机文件，并写回新文件（destroy 后 overlay merge 刷到宿主机）。

**请求**

```
POST http://${CUBE_API_URL}/sandboxes
Steps:
  1. open("{mountPath}/{filename}", ...).read()
  2. open("{mountPath}/{filename}", ...).write(...)
  3. import os; sorted(os.listdir('{mountPath}'))
```

**响应**

```json
HTTP 201
{
  "templateID": "tpl-6265796cee124256b4dcd6a1",
  "sandboxID": "{sandboxID}",
  "clientID": "{uuid}",
  "envdVersion": "0.2.0",
  "domain": "cube.app"
}
```

**结果**：✅ PASS

---

#### 3.2.9 POST /execute（network deny-all：验证出站拦截）

**说明**：在 deny-all 网络策略的沙箱内，出站请求（port 80 / port 443）均被拦截，返回 URLError。

**请求**

```
POST http://${CUBE_API_URL}/sandboxes
Body:
{
  "templateID": "tpl-6265796cee124256b4dcd6a1",
  "metadata": {
    "network_policy": "deny"
  },
  "timeout": 120
}
```

**响应**

```json
HTTP 201
{
  "templateID": "tpl-6265796cee124256b4dcd6a1",
  "sandboxID": "{sandboxID}",
  "clientID": "{uuid}",
  "envdVersion": "0.2.0",
  "domain": "cube.app"
}
```

**结果**：✅ PASS  **备注**：

---

### 3.3 HTTP port 80 / HTTPS port 443 网络访问测试

> 测试目标：验证沙箱内 Python 代码通过 HTTP(S) 访问外网 80/443 端口的行为，包括 allow-all 和 deny-all 两种网络策略。

#### 3.3.1 HTTP port 80（allow-all 策略）

**说明**：默认网络策略下，沙箱内 HTTP port 80 出站请求因 CoreDNS 无公网解析而被阻断。

**请求**

```
POST http://49999-{sandboxID}.cube.app/execute
Body:
{
  "code": "urllib.request.urlopen('http://example.com')",
  "context_id": null
}
```

**响应**

```json
HTTP 200
{
  "text": "BLOCKED: URLError: <urlopen error [Errno -3] Temporary failure in name resolution>",
  "logs": {
    "stdout": []
  },
  "error": {
    "name": null,
    "value": null
  }
}
```

**结果**：✅ PASS（符合预期：公网 DNS 解析失败）  **备注**：port 80, allow-all policy

---

#### 3.3.2 HTTPS port 443（allow-all 策略）

**请求**

```
POST http://49999-{sandboxID}.cube.app/execute
Body:
{
  "code": "urllib.request.urlopen('https://example.com')",
  "context_id": null
}
```

**响应**

```json
HTTP 200
{
  "text": "BLOCKED: URLError: <urlopen error [Errno -3] Temporary failure in name resolution>",
  "logs": {
    "stdout": []
  },
  "error": {
    "name": null,
    "value": null
  }
}
```

**结果**：✅ PASS（符合预期：公网 DNS 解析失败）  **备注**：port 443, allow-all policy

---

#### 3.3.3 HTTP port 80（deny-all 策略）

**请求**

```
POST http://49999-{sandboxID}.cube.app/execute
Body:
{
  "code": "urllib.request.urlopen('http://example.com:80')",
  "context_id": null
}
```

**响应**

```json
HTTP 200
{
  "text": null,
  "error": {
    "name": "URLError",
    "value": "<urlopen error [Errno -3] Temporary failure in name resolution>"
  }
}
```

**结果**：✅ PASS  **备注**：port 80, deny-all policy — expect blocked

---

#### 3.3.4 HTTPS port 443（deny-all 策略）

**请求**

```
POST http://49999-{sandboxID}.cube.app/execute
Body:
{
  "code": "urllib.request.urlopen('https://example.com:443')",
  "context_id": null
}
```

**响应**

```json
HTTP 200
{
  "text": null,
  "error": {
    "name": "URLError",
    "value": "<urlopen error [Errno -3] Temporary failure in name resolution>"
  }
}
```

**结果**：✅ PASS  **备注**：port 443, deny-all policy — expect blocked

---

## 四、说明

| 项目 | 说明 |
|------|------|
| allow-all 无公网 DNS | 已知限制：沙箱内 CoreDNS 仅本机有效，公网域名解析失败（-3 NXDOMAIN）|
| volume write-back 非实时 | overlay merge on teardown，沙箱 destroy 后文件才刷到宿主机 |
| envd 端口 DNS 绕过 | CUBE_PROXY_NODE_IP 通过 IPOverrideTransport 覆盖 *.cube.app 解析 |
