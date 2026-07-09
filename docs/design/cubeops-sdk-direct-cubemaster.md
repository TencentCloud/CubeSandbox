# CubeOps 直连 CubeMaster 方案

> 消除 CubeOps → CubeAPI 反向代理跳转，WebUI 的 SDK 数据操作由 CubeOps 直接调用 CubeMaster HTTP REST API 获取。

## 1. 背景与动机

### 1.1 现有链路（待优化）

```
WebUI ──HTTP──▶ nginx /cubeops/api/v1/sdk/* ──▶ CubeOps :3010
                                                   │ (JWT 鉴权)
                                                   │ (剥 /sdk 前缀)
                                                   ▼
                                               CubeAPI :3000  ← 反向代理跳转
                                                   │ (E2B 格式翻译)
                                                   ▼
                                               CubeMaster :8089 (HTTP REST)
                                                   │
                                                   ▼
                                               Cubelet (gRPC)
```

问题：
- **多一跳**：CubeOps → CubeAPI → CubeMaster，CubeAPI 仅做格式翻译，纯中转
- **职责模糊**：CubeAPI 剥离运维逻辑后，仅存的 `/api/v1/sdk/*` 代理使其沦为 CubeOps 的"格式翻译插件"
- **部署耦合**：WebUI 的 SDK 操作依赖 CubeAPI 存活，增加了故障面

### 1.2 优化后链路

```
WebUI ──HTTP──▶ nginx /cubeops/api/v1/sdk/* ──▶ CubeOps :3010
                                                   │ (JWT 鉴权)
                                                   │ (直接调用)
                                                   ▼
                                               CubeMaster :8089 (HTTP REST)
                                                   │
                                                   ▼
                                               Cubelet (gRPC)
```

变化：
- **去掉 CubeAPI 中间跳**：CubeOps 直接调 CubeMaster HTTP REST API
- **CubeAPI 回归纯粹**：仅服务外部 E2B SDK 客户端（`0.0.0.0:3000`，公网）
- **CubeOps 自主翻译**：WebUI 请求格式由 CubeOps handler 直接翻译为 CubeMaster 调用

> **协议说明**：CubeMaster 对上游调用方暴露的是 **HTTP REST API**（`/cube/sandbox`、`/cube/snapshot`、`/cube/template` 等），CubeOps 的 `cubemaster.Client` 已在用 HTTP REST 调用 CubeMaster（agenthub/cluster 操作即如此），本方案复用同一通道。

## 2. 影响范围

### 2.1 CubeOps 改动

| 模块 | 改动 | 说明 |
|------|------|------|
| `internal/handler/proxy.go` | **删除** | 移除 CubeAPI 反向代理 |
| `internal/handler/sdk.go` | **新增** | SDK 数据操作 handler，直接调 `cubemaster.Client` |
| `internal/cubemaster/client.go` | **扩展** | 补齐 SDK 所需的 CubeMaster 端点方法 |
| `internal/server/server.go` | **修改** | `/api/v1/sdk/*` 路由从 `CubeAPIProxy` 改为 `sdkHandler` |
| `internal/config/config.go` | **可选移除** | `CUBE_API_URL` 配置项不再必需（仅 auth callback 场景保留） |

### 2.2 CubeAPI 改动

**无改动**。CubeAPI 继续作为外部 E2B SDK 客户端的入口（`0.0.0.0:3000`），WebUI 不再通过它。

### 2.3 WebUI / nginx 改动

**无路由改动**。`/cubeops/api/v1/sdk/*` 路径不变，只是上游从"代理到 CubeAPI"变为"CubeOps 直接处理"。

### 2.4 部署改动

`CUBE_API_URL` 环境变量从"必需"降级为"可选"（仅当启用 `AUTH_CALLBACK_URL` 时需要）。

## 3. 详细设计

### 3.1 CubeOps SDK Handler 设计

新增 `internal/handler/sdk.go`，为 WebUI 提供 SDK 数据操作的 RESTful 端点。与 agenthub handler 模式一致：接收请求 → 翻译为 CubeMaster 调用 → 返回 JSON。

```go
// internal/handler/sdk.go

package handler

type SDKHandler struct {
    cm *cubemaster.Client
}

func NewSDKHandler(cm *cubemaster.Client) *SDKHandler {
    return &SDKHandler{cm: cm}
}
```

### 3.2 端点映射表

CubeOps SDK 端点 → CubeMaster HTTP REST API 映射：

| CubeOps 端点 | Method | CubeMaster 端点 | 说明 |
|-------------|--------|----------------|------|
| `/api/v1/sdk/sandboxes` | GET | `POST /cube/sandbox/list` | 沙箱列表 |
| `/api/v1/sdk/sandboxes` | POST | `POST /cube/sandbox` | 创建沙箱 |
| `/api/v1/sdk/sandboxes/{id}` | GET | `GET /cube/sandbox/info?sandbox_id=&instance_type=` | 沙箱详情 |
| `/api/v1/sdk/sandboxes/{id}` | DELETE | `DELETE /cube/sandbox` | 删除沙箱 |
| `/api/v1/sdk/sandboxes/{id}/logs` | GET | `POST /cube/sandbox/logs` | 沙箱日志 |
| `/api/v1/sdk/sandboxes/{id}/timeout` | POST | `POST /cube/sandbox/timeout` | 设置 TTL |
| `/api/v1/sdk/sandboxes/{id}/refreshes` | POST | `POST /cube/sandbox/refresh` | 延长 TTL |
| `/api/v1/sdk/sandboxes/{id}/pause` | POST | `POST /cube/sandbox/update` (action=pause) | 暂停 |
| `/api/v1/sdk/sandboxes/{id}/resume` | POST | `POST /cube/sandbox/update` (action=resume) | 恢复 |
| `/api/v1/sdk/sandboxes/{id}/connect` | POST | `POST /cube/sandbox/connect` | 连接（恢复暂停沙箱） |
| `/api/v1/sdk/snapshots` | GET | `GET /cube/snapshot` | 快照列表 |
| `/api/v1/sdk/sandboxes/{id}/snapshots` | POST | `POST /cube/snapshot` | 创建快照 |
| `/api/v1/sdk/sandboxes/{id}/rollback` | POST | `POST /cube/sandbox/{id}/rollback` | 回滚 |
| `/api/v1/sdk/templates` | GET | `GET /cube/template` | 模板列表 |
| `/api/v1/sdk/templates` | POST | `POST /cube/template/from-image` | 从镜像创建模板 |
| `/api/v1/sdk/templates/{id}` | GET | `GET /cube/template?template_id=` | 模板详情 |
| `/api/v1/sdk/templates/{id}` | POST | `POST /cube/template/redo` | 重建模板 |
| `/api/v1/sdk/templates/{id}` | DELETE | `DELETE /cube/template` | 删除模板 |
| `/api/v1/sdk/templates/{id}/builds/{buildID}` | POST | `POST /cube/template/build/{buildID}` | 启动构建 |
| `/api/v1/sdk/templates/{id}/builds/{buildID}/status` | GET | `GET /cube/template/build/{buildID}/status` | 构建状态 |
| `/api/v1/sdk/templates/compat` | GET | `GET /cube/template/compat` | 兼容性矩阵 |

> WebUI 是自有前端，**不需要 E2B 兼容格式**。CubeOps handler 直接返回 CubeMaster 响应的 JSON（透传 `json.RawMessage`），WebUI 前端按 CubeMaster 的 schema 解析。这比 CubeAPI 的 E2B 翻译层更简单。

### 3.3 cubemaster.Client 扩展

当前 `cubemaster.Client` 已有 `ListSandboxes`、`CreateSandbox`、`DeleteSandbox`、`GetSandbox`、`CreateSnapshot`、`DeleteSnapshot`、`RollbackSandbox`、`UpdateSandbox`、`ConnectSandbox`、`GetTemplate` 等方法。需补充：

```go
// internal/cubemaster/client.go — 新增方法

// ListSnapshots 列出快照。
func (c *Client) ListSnapshots(ctx context.Context, params map[string]string) (json.RawMessage, error) {
    // GET /cube/sandbox?request_id=...&instance_type=...&sandbox_id=...&limit=...
    return c.getWithQuery(ctx, "/cube/snapshot", params)
}

// SetSandboxTimeout 设置沙箱绝对 TTL。
func (c *Client) SetSandboxTimeout(ctx context.Context, body interface{}) (json.RawMessage, error) {
    return c.post(ctx, "/cube/sandbox/timeout", body)
}

// RefreshSandbox 延长沙箱 TTL。
func (c *Client) RefreshSandbox(ctx context.Context, body interface{}) (json.RawMessage, error) {
    return c.post(ctx, "/cube/sandbox/refresh", body)
}

// GetSandboxLogs 获取沙箱日志。
func (c *Client) GetSandboxLogs(ctx context.Context, body interface{}) (json.RawMessage, error) {
    return c.post(ctx, "/cube/sandbox/logs", body)
}

// ListTemplates 列出模板。
func (c *Client) ListTemplates(ctx context.Context, templateID string, includeRequest bool) (json.RawMessage, error) {
    params := map[string]string{}
    if templateID != "" {
        params["template_id"] = templateID
    }
    if includeRequest {
        params["include_request"] = "true"
    }
    return c.getWithQuery(ctx, "/cube/template", params)
}

// CreateTemplateFromImage 从镜像创建模板。
func (c *Client) CreateTemplateFromImage(ctx context.Context, body interface{}) (json.RawMessage, error) {
    return c.post(ctx, "/cube/template/from-image", body)
}

// RedoTemplate 重建模板。
func (c *Client) RedoTemplate(ctx context.Context, body interface{}) (json.RawMessage, error) {
    return c.post(ctx, "/cube/template/redo", body)
}

// DeleteTemplate 删除模板。
func (c *Client) DeleteTemplate(ctx context.Context, body interface{}) (json.RawMessage, error) {
    return c.deleteWithBody(ctx, "/cube/template", body)
}

// GetTemplateBuildStatus 获取构建状态。
func (c *Client) GetTemplateBuildStatus(ctx context.Context, buildID string) (json.RawMessage, error) {
    return c.get(ctx, fmt.Sprintf("/cube/template/build/%s/status", buildID))
}

// StartTemplateBuild 启动构建。
func (c *Client) StartTemplateBuild(ctx context.Context, buildID string, body interface{}) (json.RawMessage, error) {
    return c.post(ctx, fmt.Sprintf("/cube/template/build/%s", buildID), body)
}

// GetTemplateCompat 获取兼容性矩阵。
func (c *Client) GetTemplateCompat(ctx context.Context) (json.RawMessage, error) {
    return c.get(ctx, "/cube/template/compat")
}

// AdoptTemplateCompatBaseline 采纳兼容性基线。
func (c *Client) AdoptTemplateCompatBaseline(ctx context.Context, body interface{}) (json.RawMessage, error) {
    return c.post(ctx, "/cube/template/compat", body)
}
```

### 3.4 SDK Handler 实现示例

```go
// internal/handler/sdk.go

package handler

import (
    "encoding/json"
    "net/http"
    "strconv"

    "github.com/gorilla/mux"
    "github.com/tencentcloud/CubeSandbox/CubeOps/internal/cubemaster"
)

type SDKHandler struct {
    cm *cubemaster.Client
}

func NewSDKHandler(cm *cubemaster.Client) *SDKHandler {
    return &SDKHandler{cm: cm}
}

// ListSandboxes — GET /api/v1/sdk/sandboxes
func (h *SDKHandler) ListSandboxes(w http.ResponseWriter, r *http.Request) {
    ctx := r.Context()

    // 从 query 参数构建 CubeMaster 请求
    body := map[string]interface{}{
        "RequestID":     requestID(),
        "instance_type": "cubebox",
        "start_idx":     1,
        "size":          500,
    }
    if v := r.URL.Query().Get("size"); v != "" {
        if n, err := strconv.Atoi(v); err == nil {
            body["size"] = n
        }
    }

    raw, err := h.cm.ListSandboxes(ctx, body)
    if err != nil {
        writeError(w, http.StatusBadGateway, err.Error())
        return
    }
    writeJSON(w, http.StatusOK, raw)
}

// GetSandbox — GET /api/v1/sdk/sandboxes/{id}
func (h *SDKHandler) GetSandbox(w http.ResponseWriter, r *http.Request) {
    ctx := r.Context()
    sandboxID := mux.Vars(r)["id"]

    raw, err := h.cm.GetSandbox(ctx, sandboxID, "cubebox")
    if err != nil {
        writeError(w, http.StatusBadGateway, err.Error())
        return
    }
    writeJSON(w, http.StatusOK, raw)
}

// DeleteSandbox — DELETE /api/v1/sdk/sandboxes/{id}
func (h *SDKHandler) DeleteSandbox(w http.ResponseWriter, r *http.Request) {
    ctx := r.Context()
    sandboxID := mux.Vars(r)["id"]

    body := map[string]interface{}{
        "RequestID":     requestID(),
        "sandbox_id":    sandboxID,
        "instance_type": "cubebox",
    }
    raw, err := h.cm.DeleteSandbox(ctx, body)
    if err != nil {
        writeError(w, http.StatusBadGateway, err.Error())
        return
    }
    writeJSON(w, http.StatusOK, raw)
}

// PauseSandbox — POST /api/v1/sdk/sandboxes/{id}/pause
func (h *SDKHandler) PauseSandbox(w http.ResponseWriter, r *http.Request) {
    h.sandboxAction(w, r, "pause")
}

// ResumeSandbox — POST /api/v1/sdk/sandboxes/{id}/resume
func (h *SDKHandler) ResumeSandbox(w http.ResponseWriter, r *http.Request) {
    h.sandboxAction(w, r, "resume")
}

func (h *SDKHandler) sandboxAction(w http.ResponseWriter, r *http.Request, action string) {
    ctx := r.Context()
    sandboxID := mux.Vars(r)["id"]

    body := map[string]interface{}{
        "requestID":     requestID(),
        "sandbox_id":    sandboxID,
        "instance_type": "cubebox",
        "action":        action,
    }
    raw, err := h.cm.UpdateSandbox(ctx, body)
    if err != nil {
        writeError(w, http.StatusBadGateway, err.Error())
        return
    }
    writeJSON(w, http.StatusOK, raw)
}

// --- snapshots ---

// ListSnapshots — GET /api/v1/sdk/snapshots
func (h *SDKHandler) ListSnapshots(w http.ResponseWriter, r *http.Request) {
    ctx := r.Context()
    q := r.URL.Query()

    params := map[string]string{
        "request_id":    requestID(),
        "instance_type": "cubebox",
    }
    for _, k := range []string{"sandbox_id", "name", "status", "limit", "next_token"} {
        if v := q.Get(k); v != "" {
            params[k] = v
        }
    }

    raw, err := h.cm.ListSnapshots(ctx, params)
    if err != nil {
        writeError(w, http.StatusBadGateway, err.Error())
        return
    }
    writeJSON(w, http.StatusOK, raw)
}

// CreateSnapshot — POST /api/v1/sdk/sandboxes/{id}/snapshots
func (h *SDKHandler) CreateSnapshot(w http.ResponseWriter, r *http.Request) {
    ctx := r.Context()
    sandboxID := mux.Vars(r)["id"]

    var req map[string]interface{}
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        writeError(w, http.StatusBadRequest, "invalid JSON body")
        return
    }
    req["request_id"] = requestID()
    req["sandbox_id"] = sandboxID

    raw, err := h.cm.CreateSnapshot(ctx, req)
    if err != nil {
        writeError(w, http.StatusBadGateway, err.Error())
        return
    }
    writeJSON(w, http.StatusOK, raw)
}

// RollbackSandbox — POST /api/v1/sdk/sandboxes/{id}/rollback
func (h *SDKHandler) RollbackSandbox(w http.ResponseWriter, r *http.Request) {
    ctx := r.Context()
    sandboxID := mux.Vars(r)["id"]

    var req map[string]interface{}
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        writeError(w, http.StatusBadRequest, "invalid JSON body")
        return
    }
    req["request_id"] = requestID()
    req["instance_type"] = "cubebox"

    raw, err := h.cm.RollbackSandbox(ctx, sandboxID, req)
    if err != nil {
        writeError(w, http.StatusBadGateway, err.Error())
        return
    }
    writeJSON(w, http.StatusOK, raw)
}

// --- templates ---

// ListTemplates — GET /api/v1/sdk/templates
func (h *SDKHandler) ListTemplates(w http.ResponseWriter, r *http.Request) {
    ctx := r.Context()
    templateID := r.URL.Query().Get("template_id")
    includeReq := r.URL.Query().Get("include_request") == "true"

    raw, err := h.cm.ListTemplates(ctx, templateID, includeReq)
    if err != nil {
        writeError(w, http.StatusBadGateway, err.Error())
        return
    }
    writeJSON(w, http.StatusOK, raw)
}

// GetTemplate — GET /api/v1/sdk/templates/{id}
func (h *SDKHandler) GetTemplate(w http.ResponseWriter, r *http.Request) {
    ctx := r.Context()
    templateID := mux.Vars(r)["id"]

    raw, err := h.cm.GetTemplate(ctx, templateID)
    if err != nil {
        writeError(w, http.StatusBadGateway, err.Error())
        return
    }
    writeJSON(w, http.StatusOK, raw)
}

// DeleteTemplate — DELETE /api/v1/sdk/templates/{id}
func (h *SDKHandler) DeleteTemplate(w http.ResponseWriter, r *http.Request) {
    ctx := r.Context()
    templateID := mux.Vars(r)["id"]

    body := map[string]interface{}{
        "RequestID":     requestID(),
        "template_id":   templateID,
        "instance_type": "cubebox",
        "sync":          true,
    }
    raw, err := h.cm.DeleteTemplate(ctx, body)
    if err != nil {
        writeError(w, http.StatusBadGateway, err.Error())
        return
    }
    writeJSON(w, http.StatusOK, raw)
}

// CreateTemplate — POST /api/v1/sdk/templates
func (h *SDKHandler) CreateTemplate(w http.ResponseWriter, r *http.Request) {
    ctx := r.Context()

    var req map[string]interface{}
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        writeError(w, http.StatusBadRequest, "invalid JSON body")
        return
    }
    req["requestID"] = requestID()
    if _, ok := req["instance_type"]; !ok {
        req["instance_type"] = "cubebox"
    }

    raw, err := h.cm.CreateTemplateFromImage(ctx, req)
    if err != nil {
        writeError(w, http.StatusBadGateway, err.Error())
        return
    }
    writeJSON(w, http.StatusOK, raw)
}

// RebuildTemplate — POST /api/v1/sdk/templates/{id} (rebuild)
func (h *SDKHandler) RebuildTemplate(w http.ResponseWriter, r *http.Request) {
    ctx := r.Context()
    templateID := mux.Vars(r)["id"]

    var req map[string]interface{}
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        writeError(w, http.StatusBadRequest, "invalid JSON body")
        return
    }
    req["requestID"] = requestID()
    req["template_id"] = templateID

    raw, err := h.cm.RedoTemplate(ctx, req)
    if err != nil {
        writeError(w, http.StatusBadGateway, err.Error())
        return
    }
    writeJSON(w, http.StatusOK, raw)
}

// --- helpers ---

func requestID() string {
    return fmt.Sprintf("cubeops-%d", time.Now().UnixNano())
}

func writeJSON(w http.ResponseWriter, code int, data json.RawMessage) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(code)
    w.Write(data)
}

func writeError(w http.ResponseWriter, code int, msg string) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(code)
    json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
```

### 3.5 路由注册改动

```go
// internal/server/server.go — buildRouter() 修改

// 旧代码（删除）：
//   handler.InitCubeAPIProxy(s.cfg.CubeAPIURL)
//   ...
//   authed.PathPrefix("/sdk/").HandlerFunc(handler.CubeAPIProxy)

// 新代码：
sdkHandler := handler.NewSDKHandler(s.cm)

// SDK — sandboxes
authed.HandleFunc("/sdk/sandboxes", sdkHandler.ListSandboxes).Methods(http.MethodGet)
authed.HandleFunc("/sdk/sandboxes", sdkHandler.CreateSandbox).Methods(http.MethodPost)
authed.HandleFunc("/sdk/sandboxes/{id}", sdkHandler.GetSandbox).Methods(http.MethodGet)
authed.HandleFunc("/sdk/sandboxes/{id}", sdkHandler.DeleteSandbox).Methods(http.MethodDelete)
authed.HandleFunc("/sdk/sandboxes/{id}/logs", sdkHandler.GetSandboxLogs).Methods(http.MethodGet)
authed.HandleFunc("/sdk/sandboxes/{id}/timeout", sdkHandler.SetSandboxTimeout).Methods(http.MethodPost)
authed.HandleFunc("/sdk/sandboxes/{id}/refreshes", sdkHandler.RefreshSandbox).Methods(http.MethodPost)
authed.HandleFunc("/sdk/sandboxes/{id}/pause", sdkHandler.PauseSandbox).Methods(http.MethodPost)
authed.HandleFunc("/sdk/sandboxes/{id}/resume", sdkHandler.ResumeSandbox).Methods(http.MethodPost)
authed.HandleFunc("/sdk/sandboxes/{id}/connect", sdkHandler.ConnectSandbox).Methods(http.MethodPost)

// SDK — snapshots
authed.HandleFunc("/sdk/snapshots", sdkHandler.ListSnapshots).Methods(http.MethodGet)
authed.HandleFunc("/sdk/sandboxes/{id}/snapshots", sdkHandler.CreateSnapshot).Methods(http.MethodPost)
authed.HandleFunc("/sdk/sandboxes/{id}/rollback", sdkHandler.RollbackSandbox).Methods(http.MethodPost)

// SDK — templates
authed.HandleFunc("/sdk/templates", sdkHandler.ListTemplates).Methods(http.MethodGet)
authed.HandleFunc("/sdk/templates", sdkHandler.CreateTemplate).Methods(http.MethodPost)
authed.HandleFunc("/sdk/templates/{id}", sdkHandler.GetTemplate).Methods(http.MethodGet)
authed.HandleFunc("/sdk/templates/{id}", sdkHandler.RebuildTemplate).Methods(http.MethodPost)
authed.HandleFunc("/sdk/templates/{id}", sdkHandler.DeleteTemplate).Methods(http.MethodDelete)
authed.HandleFunc("/sdk/templates/{id}/builds/{buildID}", sdkHandler.StartTemplateBuild).Methods(http.MethodPost)
authed.HandleFunc("/sdk/templates/{id}/builds/{buildID}/status", sdkHandler.GetTemplateBuildStatus).Methods(http.MethodGet)
authed.HandleFunc("/sdk/templates/compat", sdkHandler.GetTemplateCompat).Methods(http.MethodGet)
```

### 3.6 删除 proxy.go

```bash
# 删除反向代理文件
rm CubeOps/internal/handler/proxy.go
```

`server.go` 中 `handler.InitCubeAPIProxy(s.cfg.CubeAPIURL)` 调用一并移除。

## 4. 请求格式说明

### 4.1 WebUI 不需要 E2B 兼容

CubeAPI 的核心价值是 **E2B SDK 兼容**——它把 E2B SDK 的请求格式翻译成 CubeMaster 内部格式。但 WebUI 是自有前端，不需要 E2B 兼容：

| 调用方 | 格式要求 | 走哪条路 |
|--------|----------|----------|
| 外部 E2B SDK 客户端 | E2B 兼容格式 | CubeAPI `:3000` → CubeMaster |
| WebUI 管理控制台 | CubeMaster 原生格式 | CubeOps `:3010` → CubeMaster（直连） |

CubeOps SDK handler 直接透传 CubeMaster 的 JSON 响应（`json.RawMessage`），WebUI 前端按 CubeMaster schema 解析。无需翻译层。

### 4.2 响应格式

CubeMaster 的 HTTP 响应统一为 `{ret: {ret_code, ret_msg}, ...data}` 信封格式。CubeOps 直接透传：

```json
// CubeOps 返回给 WebUI 的格式（= CubeMaster 原样）
{
  "RequestID": "cubeops-1752060000000000000",
  "ret": { "ret_code": 0, "ret_msg": "ok" },
  "data": [
    { "sandbox_id": "sb-xxx", "status": "running", ... }
  ]
}
```

WebUI 前端检查 `ret.ret_code === 0` 判断成功。

### 4.3 错误处理

| 错误类型 | CubeOps 行为 | HTTP 状态码 |
|----------|-------------|-------------|
| CubeMaster 返回 `ret_code != 0` | 透传错误码和消息 | 502 Bad Gateway |
| CubeMaster 网络不可达 | 返回错误描述 | 502 Bad Gateway |
| 请求参数缺失/非法 | 返回错误描述 | 400 Bad Request |
| JWT 鉴权失败 | 由 auth middleware 拦截 | 401 Unauthorized |

## 5. 与 AgentHub handler 的关系

CubeOps 的 AgentHub handler（`internal/handler/agenthub.go`）**已经在直接调用 CubeMaster** 来创建/删除/暂停/恢复沙箱、创建快照、回滚等。本方案的 SDK handler 与 AgentHub handler 调用的是**同一个 `cubemaster.Client` 实例**，使用同一组 CubeMaster 端点。

区别在于使用场景：

| Handler | 调用方 | 用途 | 额外逻辑 |
|---------|--------|------|----------|
| AgentHub | WebUI 数字助手管理页 | 管理数字助手实例（含 OpenClaw 网关、企业微信、模型配置） | 写 MySQL（AgentHub 表）、调 envd、docker inspect |
| SDK | WebUI 沙箱/模板/快照管理页 | 通用沙箱操作（无 AgentHub 业务逻辑） | 纯透传，无额外副作用 |

两者可以共存，不冲突。

## 6. 配置变更

### 6.1 环境变量

```bash
# .one-click.env

# CUBE_API_URL 从"必需"降级为"可选"
# 仅当 CubeAPI 启用 AUTH_CALLBACK_URL 且需要 CubeOps 校验 session 时保留
# CUBE_API_URL=http://127.0.0.1:3000

# CUBE_MASTER_ADDR 保持不变（CubeOps 直连目标）
CUBE_MASTER_ADDR=http://127.0.0.1:8089
```

### 6.2 CubeOps config.go

```go
// internal/config/config.go

type Config struct {
    // ...
    CubeMasterAddr string
    // CubeAPIURL 保留字段但标记为可选
    // 仅用于 AUTH_CALLBACK_URL 场景下 CubeOps 提供 session 校验端点
    CubeAPIURL string
    // ...
}
```

`Load()` 中 `CUBE_API_URL` 改为纯可选，不设置时默认 `http://127.0.0.1:3000`（仅 fallback，不再用于 proxy）。

## 7. 部署影响

### 7.1 对 cubeops-deployment.md 的修订

原部署方案中以下内容需更新：

| 章节 | 原内容 | 修订 |
|------|--------|------|
| §4.3 启动脚本 | `export CUBE_API_URL=...` | 标记为可选 |
| §5 WebUI 路由 | `/cubeops/api/v1/sdk/*` → CubeAPI proxy | `/cubeops/api/v1/sdk/*` → CubeOps 直连 CubeMaster |
| §7.4 鉴权适配 | `AUTH_CALLBACK_URL` → CubeOps | 保留，但说明不再依赖 proxy |
| 附录 B 环境变量 | `CUBE_API_URL` 标注"SDK 代理目标" | 改为"可选，仅 auth callback 场景" |

### 7.2 systemd 依赖

```
# cube-sandbox-cubeops.service
# 原依赖：
#   After=... cube-sandbox-cube-api.service
#   Wants=... cube-sandbox-cube-api.service
#
# 新依赖（CubeOps 不再依赖 CubeAPI）：
After=network-online.target cube-sandbox-cubemaster.service
Wants=network-online.target cube-sandbox-cubemaster.service
```

CubeOps 只依赖 CubeMaster（HTTP REST）和 MySQL，不再依赖 CubeAPI。

### 7.3 故障隔离

| 场景 | 优化前 | 优化后 |
|------|--------|--------|
| CubeAPI 宕机 | WebUI SDK 操作全部失败 | WebUI SDK 操作正常（直连 CubeMaster） |
| CubeMaster 宕机 | 全部失败 | 全部失败（根源） |
| CubeOps 宕机 | WebUI 运维操作失败 | 同左 |

优化后，CubeAPI 故障不影响 WebUI 管理功能。

## 8. 迁移步骤

### 8.1 代码改动（单个 PR）

1. 新增 `internal/handler/sdk.go`（SDK handler）
2. 扩展 `internal/cubemaster/client.go`（补齐端点方法）
3. 修改 `internal/server/server.go`（路由从 proxy 改为 sdkHandler）
4. 删除 `internal/handler/proxy.go`
5. 更新 `internal/config/config.go`（`CUBE_API_URL` 标为可选）
6. 更新 `CubeOps/README.md`（移除 proxy 相关描述）

### 8.2 验证

```bash
# 1. 编译
cd CubeOps && make build

# 2. 启动（不需要 CubeAPI 运行）
JWT_SECRET=test \
DATABASE_URL=mysql://cube:cube_pass@127.0.0.1:3306/cube_mvp \
CUBE_MASTER_ADDR=http://127.0.0.1:8089 \
./bin/cubeops

# 3. 验证 SDK 端点直连 CubeMaster
# 登录获取 token
TOKEN=$(curl -s -X POST http://127.0.0.1:3010/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"admin"}' | jq -r .accessToken)

# 沙箱列表（直连 CubeMaster，不经过 CubeAPI）
curl -s -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:3010/api/v1/sdk/sandboxes | jq .

# 模板列表
curl -s -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:3010/api/v1/sdk/templates | jq .

# 确认 CubeAPI 未被调用（CubeAPI 停止后上述请求仍成功）
```

### 8.3 回滚

如果发现问题，回滚只需 revert 该 PR。proxy.go 恢复后，`/api/v1/sdk/*` 重新走 CubeAPI 代理。WebUI 前端无感知（路径不变）。

## 9. 未覆盖的端点

以下 CubeAPI 端点目前在 CubeMaster 上**尚无对应 HTTP REST 实现**（见 `cubemaster/mod.rs` 注释标记 ❌）。本方案不处理这些端点，WebUI 如需使用，需先在 CubeMaster 侧补齐：

| CubeAPI 端点 | CubeMaster 状态 | 处理方式 |
|-------------|----------------|----------|
| `PATCH /templates/:id` (update) | 无对应端点 | 暂不支持，WebUI 不提供此功能 |
| `GET /templates/:id/builds/:buildID/logs` | 无对应端点 | 暂不支持 |

这些端点在 CubeAPI 的 `routes.rs` 中存在，但 CubeMaster 侧缺少实现。优化后 CubeOps 也不提供，与 CubeAPI 行为一致。

## 10. 方案优势总结

| 维度 | 优化前 | 优化后 |
|------|--------|--------|
| **网络跳数** | WebUI → nginx → CubeOps → CubeAPI → CubeMaster | WebUI → nginx → CubeOps → CubeMaster |
| **延迟** | +1 跳 HTTP 往返（CubeOps↔CubeAPI） | 直连 |
| **故障面** | CubeAPI 宕机 → WebUI SDK 操作不可用 | CubeAPI 宕机 → WebUI 不受影响 |
| **部署依赖** | CubeOps 依赖 CubeAPI 存活 | CubeOps 仅依赖 CubeMaster + MySQL |
| **代码复杂度** | proxy.go（反向代理 + 前缀剥离） | sdk.go（直接调 CubeMaster，与 agenthub handler 模式一致） |
| **CubeAPI 职责** | 混合（外部 SDK + WebUI 中转） | 纯粹（仅外部 E2B SDK 客户端） |

## 11. 系统配置表重构

### 11.1 问题

CubeOps 从 CubeAPI 剥离时，直接复用了 `t_agenthub_setting` 表作为通用 KV 存储，导致系统级密钥与 AgentHub 业务配置混装在同一张表里：

| `t_agenthub_setting` 中的 key | 实际归属 | 是否合理 |
|-------------------------------|---------|---------|
| `jwt_secret` | 系统级（认证签名） | ✗ |
| `secret_master_key` | 系统级（AES-GCM 加密主密钥） | ✗ |
| `openclaw_api_key` | AgentHub 级（LLM 密钥） | ✓ |
| `openclaw_primary_model` | AgentHub 级（默认模型） | ✓ |
| `openclaw_base_url` | AgentHub 级（LLM 网关地址） | ✓ |

同理，`t_agenthub_user`（admin 账户）也是系统级的，不属于 AgentHub 业务。`t_agenthub_session` 在 CubeOps 中完全未使用（JWT 是无状态的），属于废弃表。

### 11.2 目标拆分

```
优化前（混装）                              优化后（按归属分离）
─────────────────                          ─────────────────
t_agenthub_setting                         t_system_setting (新增)
  ├─ jwt_secret           ──────────────▶    ├─ jwt_secret
  ├─ secret_master_key    ──────────────▶    ├─ secret_master_key
  ├─ openclaw_api_key                        └─ (未来: rate_limit, feature_flag...)
  ├─ openclaw_primary_model               
  └─ openclaw_base_url                     t_agenthub_setting (保留)
                                              ├─ openclaw_api_key
t_agenthub_user                              ├─ openclaw_primary_model
  └─ admin (bcrypt hash)                     └─ openclaw_base_url

                                           t_system_user (新增)
t_agenthub_session (废弃)                     └─ admin (bcrypt hash)
```

### 11.3 新建 migration

在 `cubedb/migrate/migrations/mysql/` 新增时间戳命名的迁移文件：

```sql
-- 20260709204400_system_setting_table.sql

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260709204400_system_setting', 60);

-- 系统级配置表（KV 结构，与 t_agenthub_setting 一致）
CREATE TABLE IF NOT EXISTS `t_system_setting` (
  `setting_key` varchar(128) NOT NULL,
  `setting_value` text DEFAULT NULL,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`setting_key`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 系统用户表（运维系统管理员账户，从 t_agenthub_user 迁移）
CREATE TABLE IF NOT EXISTS `t_system_user` (
  `username` varchar(128) NOT NULL,
  `password` varchar(255) NOT NULL,
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`username`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 数据迁移：将系统级密钥从 t_agenthub_setting 搬到 t_system_setting
INSERT IGNORE INTO `t_system_setting` (`setting_key`, `setting_value`)
SELECT `setting_key`, `setting_value`
FROM `t_agenthub_setting`
WHERE `setting_key` IN ('jwt_secret', 'secret_master_key');

-- 数据迁移：将用户从 t_agenthub_user 搬到 t_system_user
INSERT IGNORE INTO `t_system_user` (`username`, `password`, `created_at`, `updated_at`)
SELECT `username`, `password`, `created_at`, `updated_at`
FROM `t_agenthub_user`;

-- 清理：从 t_agenthub_setting 删除已迁移的系统级 key
DELETE FROM `t_agenthub_setting`
WHERE `setting_key` IN ('jwt_secret', 'secret_master_key');

SELECT RELEASE_LOCK('cubemaster_migration_20260709204400_system_setting');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260709204400_system_setting', 60);

-- 回滚：将系统级 key 搬回 t_agenthub_setting
INSERT IGNORE INTO `t_agenthub_setting` (`setting_key`, `setting_value`)
SELECT `setting_key`, `setting_value` FROM `t_system_setting`
WHERE `setting_key` IN ('jwt_secret', 'secret_master_key');

-- 回滚：将用户搬回 t_agenthub_user
INSERT IGNORE INTO `t_agenthub_user` (`username`, `password`, `created_at`, `updated_at`)
SELECT `username`, `password`, `created_at`, `updated_at` FROM `t_system_user`;

DROP TABLE IF EXISTS `t_system_user`;
DROP TABLE IF EXISTS `t_system_setting`;

SELECT RELEASE_LOCK('cubemaster_migration_20260709204400_system_setting');
```

> **迁移安全**：`cubedb` 的内容指纹层要求已应用的 migration 文件不可修改。此 migration 是新增文件，不影响已有 migration。`INSERT IGNORE` + `DELETE` 的组合是幂等的，多次执行结果一致。

### 11.4 废弃 t_agenthub_session

`t_agenthub_session` 在 CubeOps 中完全未使用（JWT 无状态认证不依赖 DB session）。但不在本 migration 中 DROP 它，保留作为向后兼容缓冲：

- 短期：保留表结构，CubeOps 不读写
- 长期：确认无其他服务依赖后，在后续 migration 中清理

### 11.5 代码改动

#### 11.5.1 `setting.go` — 按归属拆分

```go
// internal/store/setting.go

package store

import (
    "context"
    "database/sql"
    "errors"
)

// ── 系统级配置（t_system_setting）──────────────────────────────

const settingMasterKey = "secret_master_key"

// GetSystemSetting retrieves a system-level setting value by key.
func (s *Store) GetSystemSetting(ctx context.Context, key string) (string, error) {
    var val string
    err := s.db.WithContext(ctx).Raw(
        "SELECT setting_value FROM t_system_setting WHERE setting_key = ? LIMIT 1", key,
    ).Scan(&val).Error
    if errors.Is(err, sql.ErrNoRows) || val == "" {
        return "", nil
    }
    return val, err
}

// GetOrCreateSystemSetting atomically gets an existing system setting or
// creates it with the given value. Uses INSERT IGNORE for concurrency safety.
func (s *Store) GetOrCreateSystemSetting(ctx context.Context, key, value string) (string, error) {
    if err := s.db.WithContext(ctx).Exec(
        "INSERT IGNORE INTO t_system_setting (setting_key, setting_value) VALUES (?, ?)",
        key, value,
    ).Error; err != nil {
        return "", err
    }
    return s.GetSystemSetting(ctx, key)
}

// SetSystemSetting upserts a system-level setting value.
func (s *Store) SetSystemSetting(ctx context.Context, key, value string) error {
    return s.db.WithContext(ctx).Exec(
        "INSERT INTO t_system_setting (setting_key, setting_value) VALUES (?, ?) "+
            "ON DUPLICATE KEY UPDATE setting_value = VALUES(setting_value)",
        key, value,
    ).Error
}

// ── AgentHub 级配置（t_agenthub_setting，保持不变）─────────────

// GetAgentHubSetting retrieves an AgentHub-level setting value by key.
func (s *Store) GetAgentHubSetting(ctx context.Context, key string) (string, error) {
    var val string
    err := s.db.WithContext(ctx).Raw(
        "SELECT setting_value FROM t_agenthub_setting WHERE setting_key = ? LIMIT 1", key,
    ).Scan(&val).Error
    if errors.Is(err, sql.ErrNoRows) || val == "" {
        return "", nil
    }
    return val, err
}

// SetAgentHubSetting upserts an AgentHub-level setting value.
func (s *Store) SetAgentHubSetting(ctx context.Context, key, value string) error {
    return s.db.WithContext(ctx).Exec(
        "INSERT INTO t_agenthub_setting (setting_key, setting_value) VALUES (?, ?) "+
            "ON DUPLICATE KEY UPDATE setting_value = VALUES(setting_value)",
        key, value,
    ).Error
}
```

#### 11.5.2 `db.go` — 改用系统配置表

```go
// internal/store/db.go — 修改 bootstrapMasterKey / BootstrapJWTSecret / seedDefaultAdmin

// bootstrapMasterKey 改为从 t_system_setting 读取
func (s *Store) bootstrapMasterKey(ctx context.Context) error {
    b64, err := s.GetOrCreateSystemSetting(ctx, "secret_master_key", crypto.GenerateMasterKeyB64())
    if err != nil {
        return err
    }
    if err := crypto.InstallMasterKey(b64); err != nil {
        return fmt.Errorf("install master key: %w", err)
    }
    slog.Info("master encryption key installed")
    return nil
}

// BootstrapJWTSecret 改为从 t_system_setting 读取/写入
func (s *Store) BootstrapJWTSecret(ctx context.Context, envSecret string) (string, error) {
    if envSecret != "" {
        return envSecret, nil
    }
    existing, _ := s.GetSystemSetting(ctx, "jwt_secret")
    if existing != "" {
        slog.Info("JWT secret loaded from database (t_system_setting)")
        return existing, nil
    }
    generated := crypto.GenerateMasterKeyB64()
    if err := s.SetSystemSetting(ctx, "jwt_secret", generated); err != nil {
        return "", fmt.Errorf("persist JWT secret: %w", err)
    }
    slog.Info("JWT secret auto-generated and persisted to database (t_system_setting)")
    return generated, nil
}

// seedDefaultAdmin 改为写入 t_system_user
func (s *Store) seedDefaultAdmin(ctx context.Context) error {
    hash, err := crypto.HashPassword("admin")
    if err != nil {
        return fmt.Errorf("hash default password: %w", err)
    }
    result := s.db.WithContext(ctx).Exec(
        "INSERT IGNORE INTO t_system_user (username, password) VALUES (?, ?)",
        "admin", hash,
    )
    if result.Error != nil {
        return fmt.Errorf("seed admin: %w", result.Error)
    }
    return nil
}
```

#### 11.5.3 `setting.go` — 用户操作改为 t_system_user

```go
// internal/store/setting.go — 修改用户查询/更新

// GetUserPassword 从 t_system_user 读取
func (s *Store) GetUserPassword(ctx context.Context, username string) (string, error) {
    var pwd string
    err := s.db.WithContext(ctx).Raw(
        "SELECT password FROM t_system_user WHERE username = ? LIMIT 1", username,
    ).Scan(&pwd).Error
    if errors.Is(err, sql.ErrNoRows) || pwd == "" {
        return "", nil
    }
    return pwd, err
}

// SetUserPassword 更新 t_system_user
func (s *Store) SetUserPassword(ctx context.Context, username, passwordHash string) error {
    result := s.db.WithContext(ctx).Exec(
        "UPDATE t_system_user SET password = ? WHERE username = ?",
        passwordHash, username,
    )
    if result.Error != nil {
        return result.Error
    }
    if result.RowsAffected == 0 {
        return errors.New("user not found")
    }
    return nil
}
```

#### 11.5.4 AgentHub handler — 改用 AgentHub 专用方法

AgentHub handler 中读取 `openclaw_*` 设置的调用从 `GetSetting`/`SetSetting` 改为 `GetAgentHubSetting`/`SetAgentHubSetting`：

```go
// internal/handler/agenthub.go — 所有 openclaw_* 设置操作

// 旧：
//   s.store.GetSetting(ctx, "openclaw_api_key")
//   s.store.SetSetting(ctx, "openclaw_api_key", value)

// 新：
s.store.GetAgentHubSetting(ctx, "openclaw_api_key")
s.store.SetAgentHubSetting(ctx, "openclaw_api_key", value)
```

### 11.6 Key 路由总表

优化后，配置 key 按归属路由到不同表：

| Key | 存储表 | 读写方法 | 说明 |
|-----|--------|----------|------|
| `jwt_secret` | `t_system_setting` | `Get/SetSystemSetting` | JWT 签名密钥 |
| `secret_master_key` | `t_system_setting` | `GetOrCreateSystemSetting` | AES-GCM 加密主密钥 |
| *(未来系统配置)* | `t_system_setting` | `Get/SetSystemSetting` | rate_limit、feature_flag 等 |
| `openclaw_api_key` | `t_agenthub_setting` | `Get/SetAgentHubSetting` | LLM API 密钥 |
| `openclaw_primary_model` | `t_agenthub_setting` | `Get/SetAgentHubSetting` | 默认模型 |
| `openclaw_base_url` | `t_agenthub_setting` | `Get/SetAgentHubSetting` | LLM 网关地址 |
| `admin` (用户) | `t_system_user` | `GetUserPassword/SetUserPassword` | 管理员账户 |

### 11.7 迁移安全性

| 风险 | 应对 |
|------|------|
| migration 执行时 CubeOps/CubeMaster 同时启动 | `cubedb` 集群级锁串行化 migration，`INSERT IGNORE` 幂等 |
| 旧版 CubeOps 仍在读写 `t_agenthub_setting` 的 `jwt_secret` | 不兼容——升级时必须同时更新 CubeOps 二进制和 migration。one-click 部署中 `install.sh` 先替换二进制再启动，migration 在启动时自动执行 |
| `t_agenthub_setting` 中残留已迁移的 key | migration 的 `DELETE` 语句清理，不会残留 |
| 回滚 | migration 的 Down 段将数据搬回原表并 DROP 新表，完全可逆 |

### 11.8 改动清单

| 文件 | 改动类型 | 说明 |
|------|----------|------|
| `cubedb/migrate/migrations/mysql/20260709204400_system_setting_table.sql` | **新增** | 建表 + 数据迁移 |
| `CubeOps/internal/store/setting.go` | **重构** | 拆分为 System/AgentHub 两组方法；用户操作改表 |
| `CubeOps/internal/store/db.go` | **修改** | `bootstrapMasterKey`、`BootstrapJWTSecret`、`seedDefaultAdmin` 改用系统表 |
| `CubeOps/internal/handler/agenthub.go` | **修改** | openclaw 设置操作改用 `Get/SetAgentHubSetting` |
| `CubeOps/internal/handler/helpers.go` | **检查** | 如有调用 `GetSetting`/`SetSetting` 的地方，按 key 归属切换 |
