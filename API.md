# cube_e2b SDK — 接口规格

## 一、配置参数

### 必填

| 环境变量 | 说明 | 示例 |
|---|---|---|
| `CUBE_API_URL` | cube-api 地址 | `http://9.135.79.34:3000` |
| `CUBE_TEMPLATE_ID` | 模板 ID | `tpl-6265796cee124256b4dcd6a1` |
| `CUBE_PROXY_NODE_IP` | CubeProxy 所在机器 IP（远程客户端必填） | `9.135.79.34` |

### 可选

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `CUBE_PROXY_PORT_HTTP` | `8081` | CubeProxy HTTP 端口（nginx listen 8081） |
| `CUBE_SANDBOX_DOMAIN` | `cube.app` | Host header 域名后缀 |

---

## 二、接口规格

### 1. `Sandbox.create()` — 创建沙箱

**请求** `POST {CUBE_API_URL}/sandboxes`

```json
{
  "templateID": "tpl-xxx",      // 必填
  "timeout":    300,            // 可选，秒，默认 300
  "envVars":    {"KEY": "val"}, // 可选
  "metadata":   {"k": "v"}     // 可选
}
```

**响应** `200 OK`

```json
{
  "templateID":  "tpl-xxx",
  "sandboxID":   "abc123",
  "clientID":    "uuid",
  "envdVersion": "0.2.0",
  "domain":      "cube.app"
}
```

---

### 2. `Sandbox.connect(sandbox_id)` — 连接/恢复沙箱

**请求** `POST {CUBE_API_URL}/sandboxes/{sandboxID}/connect`

```json
{
  "timeout": 300
}
```

**响应** `200 OK` — 同 create 响应结构

---

### 3. `sandbox.run_code(code)` — 执行代码

**请求**

```
POST http://{CUBE_PROXY_NODE_IP}:{CUBE_PROXY_PORT_HTTP}/execute
Host: 49999-{sandboxID}.{CUBE_SANDBOX_DOMAIN}
Content-Type: application/json
```

> TCP 直连 `CUBE_PROXY_NODE_IP:8081`，Host header 保留虚拟域名供 CubeProxy lua 路由

```json
{
  "code":       "1 + 1",      // 必填
  "context_id": null,         // 可选，复用 kernel（保留变量状态）
  "language":   null,         // 可选，默认 python
  "env_vars":   {"K": "v"}   // 可选，per-execution 环境变量
}
```

**响应** — `application/x-ndjson` 流，每行一个事件：

```jsonl
{"type": "stdout",               "text": "hello\n",             "timestamp": "..."}
{"type": "stderr",               "text": "warn\n",              "timestamp": "..."}
{"type": "result",               "text": "2",                   "is_main_result": true}
{"type": "error",                "name": "ZeroDivisionError",   "value": "division by zero", "traceback": [...]}
{"type": "number_of_executions", "execution_count": 1}
```

---

### 4. `sandbox.pause()` — 暂停沙箱

**请求** `POST {CUBE_API_URL}/sandboxes/{sandboxID}/pause`（无 body）

**响应** `204 No Content`

---

### 5. `sandbox.kill()` — 销毁沙箱

**请求** `DELETE {CUBE_API_URL}/sandboxes/{sandboxID}`（无 body）

**响应** `204 No Content`

---

### 6. `sandbox.resume(timeout)` — 恢复沙箱（已废弃，用 connect 代替）

**请求** `POST {CUBE_API_URL}/sandboxes/{sandboxID}/resume`

```json
{
  "timeout": 300
}
```

**响应** `201 Created` — 同 create 响应结构

---

### 7. `sandbox.get_info()` — 查询沙箱状态

**请求** `GET {CUBE_API_URL}/sandboxes/{sandboxID}`

**响应** `200 OK`

```json
{
  "templateID":  "tpl-xxx",
  "sandboxID":   "abc123",
  "clientID":    "uuid",
  "startedAt":   "2026-04-26T12:00:00Z",
  "endAt":       "2026-04-26T12:05:00Z",
  "state":       "running",
  "cpuCount":    2,
  "memoryMB":    512,
  "envdVersion": "0.2.0",
  "domain":      "cube.app"
}
```

---

## 三、请求链路示意

```
Client
  │
  ├─ POST http://CUBE_API_URL/sandboxes          → 拿 sandboxID
  │
  └─ POST http://CUBE_PROXY_NODE_IP:8081/execute
         Host: 49999-{sandboxID}.cube.app
              │
         CubeProxy (nginx+lua)
         解析 Host → sandboxID + port=49999
         查 Redis → HostIP / SandboxIP
              │
         反向代理 → sandbox 内 envd 进程
```
