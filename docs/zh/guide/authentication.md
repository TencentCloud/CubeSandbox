# 鉴权配置

Cube API Server 默认不启用鉴权，所有请求直接放通。如需鉴权，启动时指定一个回调地址即可。启用后，每个入站请求的凭证 header 都会被转发到你的回调服务——Cube API Server 仅负责转发，鉴权决策由回调方完成。

## 启用鉴权

通过 `--auth-callback-url` 启动参数或对应的环境变量指定回调地址：

```bash
# 启动参数
./cube-api --auth-callback-url https://your-auth-service/verify

# 或环境变量
export AUTH_CALLBACK_URL=https://your-auth-service/verify
./cube-api
```

未设置 `AUTH_CALLBACK_URL` 时（默认），所有请求无需凭证即可通过。

## 工作原理

请求到达时，Cube API Server 按以下流程处理：

1. 从请求 header 中提取凭证（`Authorization: Bearer` 优先于 `X-API-Key`）。
2. 向回调地址发送 `POST` 请求，透传凭证 header、原始请求路径**以及 HTTP 方法**。
3. 回调返回 **HTTP 200** → 放行请求。
4. 其他状态码 → 返回客户端 **HTTP 401 Unauthorized**。

```
客户端 ──→ Cube API Server
                │
                ├─ 提取凭证（Bearer / API Key）
                ├─ 记录方法（GET / POST / DELETE / PATCH …）
                │
                └─ POST → 你的鉴权服务
                                │
                       200 ─────┤──→ 放行请求
                    非 200 ─────┘──→ 401 Unauthorized
```

## SDK 侧配置

E2B SDK 会将 `E2B_API_KEY` 的值以 `Authorization: Bearer <key>` 的形式附加到每个请求中：

```bash
export E2B_API_KEY=your-actual-api-key
```

如果不使用 E2B SDK，也可以直接发送 `X-API-Key`：

```
X-API-Key: your-actual-api-key
```

两种格式均支持。两者同时存在时，`Authorization: Bearer` 优先。

## 回调请求格式

Cube API Server 向回调地址发送的 `POST` 请求包含以下 header：

| Header | 值 |
|--------|---|
| `Authorization` | `Bearer <token>` — 客户端使用 Bearer 鉴权时透传 |
| `X-API-Key` | `<key>` — 客户端使用 API Key 鉴权时透传 |
| `X-Request-Path` | 原始请求路径，如 `/templates/my-tmpl` |
| `X-Request-Method` | 原始请求的 HTTP 方法，如 `GET`、`DELETE` |

两个凭证 header 互斥，回调方收到哪个取决于客户端发送的是哪种格式。

::: warning 必须同时校验路径**和**方法
同一路径上挂载了多个 HTTP 方法——例如 `/templates/:id` 同时处理 `GET`（读取）、`POST`（重建）、`DELETE`（删除）和 `PATCH`（更新）。如果回调仅按路径白名单授权，则读权限可能被放大为删除或覆写操作：持有只读凭证的调用方发送 `DELETE` 请求时，路径匹配不会拦截它。

请在回调中**同时校验** `X-Request-Path` 和 `X-Request-Method`。
:::

### 回调示例（Python/FastAPI）

```python
from fastapi import FastAPI, Request
from fastapi.responses import Response

app = FastAPI()

# 读操作方法集合
READ_METHODS = {"GET", "HEAD"}

# 只读凭证与完全访问凭证分开管理。
# 必须同时校验路径和方法——同一路径（如 /templates/:id）
# 既有 GET（读取），也有 DELETE、POST（重建）、PATCH（更新）。
READONLY_KEYS = {"readonly-key-1"}
FULL_ACCESS_KEYS = {"secret-key-1", "secret-key-2"}

@app.post("/verify")
async def verify(request: Request):
    path = request.headers.get("X-Request-Path", "")
    method = request.headers.get("X-Request-Method", "").upper()

    # 提取凭证（Bearer 优先）
    key = None
    auth = request.headers.get("Authorization", "")
    if auth.startswith("Bearer "):
        key = auth.removeprefix("Bearer ").strip()
    else:
        key = request.headers.get("X-API-Key", "")

    if not key:
        return Response(status_code=401)

    if key in FULL_ACCESS_KEYS:
        return {}                           # 200 → 放行所有操作

    if key in READONLY_KEYS:
        if method in READ_METHODS:
            return {}                       # 200 → 允许读操作
        return Response(status_code=403)   # 拒绝写/删操作

    return Response(status_code=401)
```

## 错误响应

| 场景 | HTTP 状态码 |
|------|------------|
| 请求未携带凭证 | `401 Unauthorized` |
| 回调返回非 200 | `401 Unauthorized` |
| 回调地址不可达 | `500 Internal Server Error` |
