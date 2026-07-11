# CubeMaster HTTP API Gin 重构方案

> **版本**: v2 (经对抗性评审修正)
> **Gin 版本要求**: `github.com/gin-gonic/gin >= v1.7.0`（需要 static-priority-over-param 路由特性）

## 1. 背景与目标

CubeMaster 的 HTTP API 当前使用 `gorilla/mux` 作为路由器，但所有 `/cube/*` 路由统一指向 `cube.HttpHandler`——一个 25+ case 的 `switch r.URL.Path` 巨型分发器。`inner`、`notify` 包有同样的模式。这导致：

- **双重路由**：mux 已匹配路由，handler 内部再 switch 一次路径
- **dispatcher 内手写路径匹配**：`isSandboxRollbackResourcePath` 手动 split path
- **响应写入不一致**：`WriteResponse` 存在 header 顺序 bug，streaming handler 用 nil-return 协议绕过
- **中间件臃肿**：logging + auth + panic recovery + mock 全在一个函数

**目标**：迁移到 Gin 框架，**消除 dispatcher 层的手写路由分发 switch**（cube.HttpHandler / inner.HttpHandler / notify.HttpHandler），用 Gin 原生的路由树替代 dispatcher。

**明确不在本次范围内**：
- handler 内部的路径解析 helper（`resourceIDFromPath`、`sandboxIDFromRollbackPath` 等）——这些是 handler 业务逻辑的一部分，通过 `r.URL.Path` 操作，Gin 不修改此值，继续正常工作
- handler 内部的 method 分发（`handleSandboxAction` 的 `switch r.Method`）——Gin 按方法注册路由后这些变为防御性代码，保留不动以最小化变更面

**原则**：
- 不破坏任何外部 API 契约（路径、响应格式、错误码、认证方式）
- 不改变 handler 业务逻辑
- 渐进式迁移，保留现有单元测试
- 不过度设计

---

## 2. 当前架构分析

### 2.1 路由注册（server.go:75-127）

```
gorilla/mux Router
├── /metrics                          → promhttp.Handler
├── /notify/*  (Subrouter)            → notify.HttpHandler (switch r.RequestURI)
│   ├── POST /notify/host
│   └── GET  /notify/health
├── /cube/*    (Subrouter)            → cube.HttpHandler (switch r.URL.Path, 25+ cases)
│   ├── POST/DELETE /sandbox
│   ├── POST         /sandbox/preview
│   ├── GET/POST     /sandbox/list
│   ├── ... (20+ more)
│   └── GET/HEAD     /ca/{filename}
├── /internal/* (Subrouter)
│   ├── GET  /node                    → inner.HttpHandler (switch r.URL.Path)
│   ├── POST /fake_create             → inner.HttpHandler (注: switch 内无 case，走 default→404)
│   ├── *    /ws                      → inner.HttpHandler (websocket)
│   └── *    /query                   → inner.HttpHandler
└── /internal/meta/*
    ├── GET    /readyz                → meta.ReadyzHandler (直接注册)
    ├── POST   /nodes/register        → meta.RegisterNodeHandler
    ├── GET    /nodes/{node_id}       → meta.GetNodeHandler (mux.Vars)
    └── ... (5 more, 3 use mux.Vars)
```

### 2.2 核心问题

| 问题 | 位置 | 影响 |
|------|------|------|
| 巨型 switch 路由分发 | `cube.go:55-132`, `inner.go:33-64`, `notify.go:36-64` | mux 已匹配路由，handler 内部重新匹配，完全冗余 |
| `mux.Vars` 直接依赖 | `meta.go:118,132,169,185`, `template_commit.go:172` | 强耦合 gorilla/mux，5 处零测试覆盖 |
| `WriteResponse` header bug | `res.go:20-25` | `WriteHeader` 在 `Header().Set` 之前，Content-Type 实际靠 Go 的 content-type sniffing 生效 |
| nil-return 流式协议 | `cube.go:120-131` | handler 返回 nil 表示"已自行写入响应"，dispatcher 靠检查 nil 决定是否 WriteResponse——隐式协议 |
| 双 JSON 解码路径 | `common.GetBodyReq` vs `utils.DecodeHttpBody` | 同一包内两种 body 解析方式，jsoniter 配置不同 |
| 中间件臃肿 | `middleware.go:31-106` | logging + panic + auth + mock 全在一个 defer 里 |

---

## 3. 向后兼容性约束（MUST NOT change）

以下契约已被外部客户端（Cubelet、cube-lifecycle-manager、cubemastercli）硬编码，**任何变更都会导致静默故障**：

### 3.1 响应信封格式

```json
{
  "requestID": "uuid-string",
  "ret": {
    "ret_code": 200,
    "ret_msg": "success"
  }
}
```

- JSON key 名 (`ret`, `ret_code`, `ret_msg`, `requestID`) 不可改变
- `ret_code == 200` 表示成功（客户端硬编码检查此值）
- `ret_code` 具体数值不可改变（如 `130401`=AuthFailed, `130483`=NotFound, `130490`=AlreadyInState）

**已存在的例外（本次迁移不改变，仅记录）**：
- `meta.go` 的 `writeErr` 在 body 解码失败时返回 HTTP 400（meta.go:97, 115, 172）
- `inner/stat.go` 的 `handleQuery` 在序列化失败时返回 HTTP 500 + 非 JSON 信封格式（stat.go:77）
- 这些是 **pre-existing behavior**，迁移后行为完全一致

### 3.2 HTTP 状态码

cube/notify 包的 handler **恒返回 HTTP 200**——即使认证失败、panic、业务错误。客户端只检查 body 中的 `ret_code`。

**例外**：
- `meta` 包的 `writeErr` 有选择地使用 400/200（已有行为，保留）
- 未匹配路由：当前 mux 返回 HTTP 404 plain text；迁移后 Gin 的 NoRoute handler 返回 HTTP 200 + JSON（**这是有意的改进**，统一错误格式）
- 未匹配方法：当前 mux 返回 HTTP 405 plain text；迁移后 Gin 的 NoMethod handler 返回 HTTP 200 + JSON（同上）

### 3.3 URL 路径

以下路径被客户端字符串拼接硬编码，必须原样保留：
`/cube/sandbox`, `/cube/sandbox/update`, `/cube/sandbox/info`, `/cube/sandbox/list`, `/cube/sandbox/preview`, `/cube/sandbox/commit`, `/cube/sandbox/rollback`, `/cube/sandbox/timeout`, `/cube/template`, `/cube/template/from-image`, `/cube/template/redo`, `/cube/template/build/{id}/status`, `/cube/snapshot`, `/cube/snapshot/{id}`, `/cube/snapshot/storage`, `/cube/operation/{id}`, `/cube/listinventory`, `/internal/meta/readyz`, `/internal/meta/nodes/register`, `/internal/meta/nodes/{id}/status`, `/internal/node`, `/internal/query`

### 3.4 JSON 序列化

当前使用 `jsoniter.Config{EscapeHTML:false, UseNumber:true, MarshalFloatWith6Digits:true, ObjectFieldMustBeSimpleString:true}`。

- **UseNumber=true 是 load-bearing**：松散类型的 `interface{}` 字段解码为 `json.Number` 而非 `float64`
- **不可使用 Gin 默认的 `c.JSON()`**（底层是 `encoding/json`，行为不同）
- 必须继续通过 `common.WriteResponse` 使用 `FastestJsoniter` 序列化

### 3.5 认证机制

- 6 个 header：`cube_version`, `cube_user_id`, `cube_timestamp`, `cube_nonce`, `cube_sgn_method`, `cube_signature`
- HMAC-SHA1/256 签名，magic version `"2023"`
- 可通过 `AuthConf.Enable=false` 全局关闭
- 认证失败返回 HTTP 200 + `ret_code=130401`

### 3.6 请求 body 回退查询参数

`handleInfoAction`, `handleListAction`, `handleSandboxLogsAction` 在 body 解码失败（`io.EOF`）时回退到 query 参数。handler 不变，此行为自动保留。

### 3.7 Trailing Slash

当前 gorilla/mux **不**自动重定向 trailing slash（`/cube/sandbox/` → 404）。

**迁移决策**：设置 `engine.RedirectTrailingSlash = false`，与 mux 行为一致。

---

## 4. 目标架构

### 4.1 整体结构

```
gin.Engine (gin.New(), RedirectTrailingSlash=false, HandleMethodNotAllowed=true)
├── Middleware: GinRequestMiddleware (trace + auth + mock + recovery)
├── NoRoute  handler (HTTP 200 + JSON ret_code=-1)
├── NoMethod handler (HTTP 200 + JSON ret_code=MasterParamsError)
├── GET /metrics
├── Group /notify
│   ├── POST /host     → notify.hostChangeGinHandler
│   └── GET  /health   → notify.healthCheckGinHandler
├── Group /cube
│   ├── POST   /sandbox                    → cube.Adapt(handleSandboxAction)
│   ├── DELETE /sandbox                     → cube.Adapt(handleSandboxAction)
│   ├── POST   /sandbox/preview             → cube.Adapt(handleSandboxPreviewAction)
│   ├── ... (每条路由直接映射到具体 handler 函数)
│   ├── POST   /sandbox/:sandbox_id/rollback → cube.Adapt(handleSandboxRollbackAction)
│   ├── GET    /snapshot/:snapshot_id       → cube.Adapt(handleSnapshotAction)
│   ├── DELETE /snapshot/:snapshot_id       → cube.Adapt(handleSnapshotAction)
│   ├── GET    /operation/:operation_id     → cube.Adapt(handleSnapshotOperationAction)
│   ├── GET    /template/build/:build_id/status → cube.Adapt(handleTemplateBuildStatusAction)
│   ├── GET/HEAD /ca/:filename              → cube.Adapt(handleCADownloadAction)
│   └── GET/HEAD /template/artifact/download → cube.Adapt(handleTemplateArtifactDownloadAction)
├── Group /internal
│   ├── GET  /node   → inner.nodeGinHandler
│   ├── GET  /ws     → inner.websocketGinHandler
│   └── GET  /query  → inner.queryGinHandler
│   (注: /internal/fake_create 当前为 dead route — HttpHandler switch 无对应 case，
│    迁移后不注册，由 NoRoute handler 处理，行为与当前 default 分支一致)
└── Group /internal/meta
    ├── GET    /readyz              → meta.readyzGinHandler
    ├── POST   /nodes/register      → meta.registerNodeGinHandler
    ├── GET    /nodes               → meta.listNodesGinHandler
    ├── GET    /version-matrix      → meta.versionMatrixGinHandler
    ├── GET    /nodes/:node_id      → meta.getNodeGinHandler
    ├── POST   /nodes/:node_id/status → meta.updateNodeStatusGinHandler
    ├── POST   /nodes/:node_id/labels → meta.updateNodeLabelsGinHandler
    └── DELETE /nodes/:node_id/labels → meta.deleteNodeLabelGinHandler
```

### 4.2 核心设计决策

#### 决策 1：Handler 签名不变

cube 包 handler 函数保持 `func(w http.ResponseWriter, r *http.Request, rt *CubeLog.RequestTrace) interface{}` 签名。

**理由**：
- 15+ 单元测试直接调用这些函数并断言返回值——改签名 = 全部改测试
- handler 内部通过 `r.URL.Path` 解析路径参数，Gin 不修改 `r.URL.Path`，现有解析逻辑继续工作
- 业务逻辑零变更 = 零 regression 风险

#### 决策 2：Adapter 模式

创建两个薄 adapter 函数（不需要 AdaptStream——流式 handler 的成功路径返回 nil，错误路径返回 `*types.Res`，`Adapt` 统一处理两种情况）：

```go
// cube/gin_adapter.go

// Adapt 包装返回 interface{} 的 handler，用 WriteResponse 写响应。
// 流式 handler 成功时返回 nil（已自行写入 c.Writer），错误时返回 *types.Res。
func Adapt(fn func(http.ResponseWriter, *http.Request, *CubeLog.RequestTrace) interface{}) gin.HandlerFunc {
    return func(c *gin.Context) {
        rt := CubeLog.GetTraceInfo(c.Request.Context())
        rsp := fn(c.Writer, c.Request, rt)
        if rsp == nil {
            return // 流式 handler 已自行写入响应
        }
        common.WriteResponse(c.Writer, http.StatusOK, rsp)
    }
}

// AdaptList 同上，但用 WriteListResponse（用于 list 端点，走 bufferpool）
func AdaptList(fn func(http.ResponseWriter, *http.Request, *CubeLog.RequestTrace) interface{}) gin.HandlerFunc {
    return func(c *gin.Context) {
        rt := CubeLog.GetTraceInfo(c.Request.Context())
        rsp := fn(c.Writer, c.Request, rt)
        if rsp == nil {
            return
        }
        common.WriteListResponse(c.Writer, http.StatusOK, rsp)
    }
}
```

**nil-return 语义说明**：流式 handler（如 `handleCADownloadAction`）成功时直接通过 `http.ServeContent(w, r, ...)` 写入响应并返回 nil；失败时返回 `*types.Res`。`Adapt` 检测 nil 跳过 WriteResponse，非 nil 时正常写——**与现有 dispatcher 的 nil-return 协议完全一致**，只是从 dispatcher switch 内迁移到每个路由的 adapter 中。

#### 决策 3：meta 包 handler 改为 `*gin.Context`

meta 包的 8 个 handler 当前签名是 `func(w http.ResponseWriter, r *http.Request)`，其中 4 个用 `mux.Vars(r)["node_id"]`。这些 handler **没有单元测试**，改签名无 regression 风险。

改为：
```go
func getNodeGinHandler(c *gin.Context) {
    nodeID := c.Param("node_id")
    // ... 业务逻辑不变 ...
    common.WriteResponse(c.Writer, http.StatusOK, rsp)
}
```

**注意**：meta 的 `writeErr(w, http.StatusBadRequest, err)` 必须保留 HTTP 400 状态码——迁移时不能"修正"为 200。使用 `c.Writer.WriteHeader(http.StatusBadRequest)` + 手动写 JSON body，或改为 `c.JSON(400, ...)`（meta 不使用 FastestJsoniter，用标准 json 即可——与当前 `writeErr` 行为一致）。

#### 决策 4：mux.Vars 替换

| 位置 | 当前 | 迁移后 |
|------|------|--------|
| meta.go × 4 | `mux.Vars(r)["node_id"]` | `c.Param("node_id")` (handler 改为 `*gin.Context`) |
| template_commit.go:172 | `mux.Vars(r)["build_id"]` | 从 `r.URL.Path` 手动提取（handler 签名不变） |

template_commit.go 替换为：
```go
// TODO: [shortcut] 临时手解析路径参数，因 handler 签名保持 (w,r,rt) 不变以兼容单元测试。
// 未来 handler 签名迁移到 *gin.Context 后可用 c.Param("build_id") 替代。
func buildIDFromPath(path string) string {
    prefix := actionURI(TemplateBuildStatusAction) + "/"  // /cube/template/build/
    trimmed := strings.TrimPrefix(path, prefix)
    return strings.TrimSuffix(trimmed, "/status")
}
```

#### 决策 5：中间件保持为单一函数

当前 `MiddlewareLogging` 是一个函数做五件事。不拆分——拆分是过度设计。适配为 gin 中间件格式：

```go
// middleware/gin_middleware.go

func GinRequestMiddleware() gin.HandlerFunc {
    return func(c *gin.Context) {
        start := time.Now()
        rt := &CubeLog.RequestTrace{
            Action:         c.Request.Method,
            CallerIP:       c.Request.RemoteAddr,
            Caller:         getCaller(c.Request),
            Callee:         constants.CubeMasterServiceID,
            CalleeAction:   c.Request.URL.Path,
            CalleeEndpoint: "localhost",
        }

        // Context 设置（与现有逻辑完全一致）
        ctx := getHTTPUA(c.Request.Context(), c.Request)
        if callerHostIP := getCallerHostIP(c.Request); callerHostIP != "" {
            ctx = constants.WithHostIP(ctx, callerHostIP)
        }
        ctx = CubeLog.WithRequestTrace(ctx, rt)
        ctx = log.WithLogger(ctx, CubeLog.WithContext(ctx))
        c.Request = c.Request.WithContext(ctx)

        var dump []byte
        if log.IsDebug() {
            dump, _ = httputil.DumpRequest(c.Request, config.GetConfig().Common.DebugDumpHttpBody)
        }

        defer func() {
            if err := recover(); err != nil {
                log.G(ctx).Fatalf("HandlerFunc panic:%s", string(debug.Stack()))
                common.WriteResponse(c.Writer, http.StatusOK, &types.Res{
                    Ret: &types.Ret{
                        RetCode: -1,
                        RetMsg:  http.StatusText(http.StatusInternalServerError),
                    },
                })
                c.Abort()
                return
            }
            rt.Cost = time.Since(start)
            select {
            case <-ctx.Done():
                if errors.Is(ctx.Err(), context.Canceled) {
                    rt.RetCode = int64(errorcode.ErrorCode_ClientCancel)
                }
            default:
            }
            CubeLog.Trace(rt)
            if log.IsDebug() {
                log.G(ctx).WithFields(map[string]interface{}{
                    "CallerIP":  c.Request.RemoteAddr,
                    "RequestId": rt.RequestID,
                }).Debugf("http_request_coming: %s", string(dump))
            }
        }()

        // Mock 模式
        if config.GetConfig().Common.MockHttpDirect {
            common.WriteResponse(c.Writer, http.StatusOK, &types.Res{
                Ret: &types.Ret{
                    RetCode: int(errorcode.ErrorCode_Success),
                    RetMsg:  errorcode.ErrorCode_Success.String(),
                },
            })
            time.Sleep(time.Duration(1+rand.Intn(2)) * time.Millisecond) // 保留：模拟处理延迟
            c.Abort()
            return
        }

        // 认证
        if err := checkAuth(ctx, c.Request); err != nil {
            status, _ := ret.FromError(err)
            rt.RetCode = int64(status.Code())
            common.WriteResponse(c.Writer, http.StatusOK, &types.Res{
                Ret: &types.Ret{
                    RetCode: int(status.Code()),
                    RetMsg:  status.Message(),
                },
            })
            c.Abort()
            return
        }

        c.Next()
    }
}
```

**与现有代码的差异**（完整列表）：
1. `next.ServeHTTP(w, r.WithContext(ctx))` → `c.Next()`
2. mock/auth 失败后增加 `c.Abort()`（防止 gin 继续执行后续 handler）
3. defer 内 panic 恢复增加 `c.Abort()` + `return`（防止 defer 后继续执行）
4. 其余完全一致（**包括 mock 模式的 `time.Sleep`**）

#### 决策 6：保留 `common.WriteResponse`，修复 header bug

```go
// 修复前 (res.go:20-25)
func WriteResponse(w http.ResponseWriter, code int, data interface{}) {
    w.WriteHeader(code)           // BUG: headers frozen after this
    w.Header().Set("Content-Type", "application/json")
    d, _ := FastestJsoniter.Marshal(data)
    w.Write(d)
}

// 修复后
func WriteResponse(w http.ResponseWriter, code int, data interface{}) {
    w.Header().Set("Content-Type", "application/json")  // 先设 header
    w.WriteHeader(code)                                   // 再写 status
    d, _ := FastestJsoniter.Marshal(data)
    w.Write(d)
}
```

同样修复 `WriteListResponse`。行为无变化——Go 的 content-type sniffing 当前已正确识别 JSON。

---

## 5. 详细实施步骤

### 步骤 1：添加 Gin 依赖

```bash
cd CubeMaster && go get github.com/gin-gonic/gin@latest
```

确保 `go.mod` 中 gin 版本 >= v1.7.0（需要 static-priority-over-param 路由特性）。

### 步骤 2：修改 `pkg/server/server.go`

**变更**：
1. `internalHttp` 结构体：`router *mux.Router` → `engine *gin.Engine`
2. `NewInternalHttp`：`mux.NewRouter()` → `gin.New()` + 配置
3. `registerHandlers()` → `registerRoutes()`：用 Gin 路由组注册
4. 添加 `NoRoute` / `NoMethod` handler
5. import 替换

```go
type internalHttp struct {
    *http.Server
    engine *gin.Engine
}

func NewInternalHttp(ctx context.Context, cfg *config.Config) (*internalHttp, error) {
    if cfg == nil || cfg.Common == nil {
        return nil, errors.New("config is nil")
    }

    engine := gin.New()
    engine.RedirectTrailingSlash = false  // 与 mux 行为一致
    engine.HandleMethodNotAllowed = true

    // NoRoute: 未匹配路由返回 HTTP 200 + JSON（统一错误格式）
    engine.NoRoute(func(c *gin.Context) {
        rt := CubeLog.GetTraceInfo(c.Request.Context())
        if rt != nil {
            rt.RetCode = -1
        }
        common.WriteResponse(c.Writer, http.StatusOK, &types.Res{
            Ret: &types.Ret{
                RetCode: -1,
                RetMsg:  http.StatusText(http.StatusNotFound),
            },
        })
    })

    // NoMethod: 路由匹配但方法不匹配，返回 HTTP 200 + JSON
    engine.NoMethod(func(c *gin.Context) {
        rt := CubeLog.GetTraceInfo(c.Request.Context())
        if rt != nil {
            rt.RetCode = int64(errorcode.ErrorCode_MasterParamsError)
        }
        common.WriteResponse(c.Writer, http.StatusOK, &types.Res{
            Ret: &types.Ret{
                RetCode: int(errorcode.ErrorCode_MasterParamsError),
                RetMsg:  http.StatusText(http.StatusMethodNotAllowed),
            },
        })
    })

    s := &internalHttp{
        Server: &http.Server{
            Addr:         fmt.Sprintf("%s:%d", cfg.Common.HttpBind, cfg.Common.HttpPort),
            ReadTimeout:  time.Second * time.Duration(cfg.Common.ReadTimeout),
            WriteTimeout: time.Second * time.Duration(cfg.Common.WriteTimeout),
            IdleTimeout:  time.Second * time.Duration(cfg.Common.IdleTimeout),
            Handler:      engine,
        },
        engine: engine,
    }

    s.registerRoutes()
    return s, nil
}

func (s *internalHttp) registerRoutes() {
    r := s.engine

    // 全局中间件（与当前 r.Use(middleware.MiddlewareLogging) 等价）
    r.Use(middleware.GinRequestMiddleware())

    // Metrics（也走中间件，与当前行为一致）
    r.GET("/metrics", gin.WrapH(promhttp.Handler()))

    // Notify 路由组
    notifyG := r.Group(notify.NotifyURI())
    {
        notifyG.POST(notify.HostChangeNotifyAction, notify.HostChangeGinHandler)
        notifyG.GET(notify.HealthCheckAction, notify.HealthCheckGinHandler)
    }

    // Cube 路由组
    cubeG := r.Group(cube.CubeURI())
    cube.RegisterCubeRoutes(cubeG)

    // Internal 路由组
    innerG := r.Group(inner.InnerURI())
    inner.RegisterInnerRoutes(innerG)
    // 注: /internal/fake_create 当前为 dead route（HttpHandler switch 无 case），
    // 迁移后不注册，由 NoRoute handler 处理——行为与当前 default 分支一致。

    // Meta 路由组
    metaG := r.Group(metahttp.MetaURI())
    meta.RegisterMetaRoutes(metaG)
}
```

### 步骤 3：创建 `pkg/service/httpservice/cube/gin_adapter.go`

实现 `Adapt`, `AdaptList`（见决策 2）。

### 步骤 4：创建 `pkg/service/httpservice/cube/routes.go`

```go
package cube

func RegisterCubeRoutes(g *gin.RouterGroup) {
    // Sandbox CRUD
    g.POST(SandboxAction, Adapt(handleSandboxAction))     // 内部按 method 分发 POST→create, DELETE→delete
    g.DELETE(SandboxAction, Adapt(handleSandboxAction))
    g.POST(SandboxPreviewAction, Adapt(handleSandboxPreviewAction))
    g.POST(SandboxCommitAction, Adapt(handleSandboxCommitAction))
    g.POST(SandboxRollbackAction, Adapt(handleSandboxRollbackAction))
    g.POST(SandboxAction+"/:sandbox_id/rollback", Adapt(handleSandboxRollbackAction))
    g.POST(SandboxUpdateAction, Adapt(handleUpdateAction))
    g.POST(SandboxTimeoutAction, Adapt(handleSandboxTimeoutAction))
    g.POST(SandboxRefreshAction, Adapt(handleSandboxRefreshAction))
    g.POST(SandboxExecAction, Adapt(handleExecAction))     // 仅 POST
    g.GET(SandboxInfoAction, Adapt(handleInfoAction))
    g.POST(SandboxInfoAction, Adapt(handleInfoAction))
    g.GET(SandboxListAction, AdaptList(handleListAction))
    g.POST(SandboxListAction, AdaptList(handleListAction))
    g.GET(SandboxLogsAction, Adapt(handleSandboxLogsAction))
    g.POST(SandboxLogsAction, Adapt(handleSandboxLogsAction))

    // Image
    g.POST(ImageAction, Adapt(handleImageAction))
    g.DELETE(ImageAction, Adapt(handleImageAction))

    // Snapshot（注意：DELETE /snapshot 集合级不注册——当前 mux 也不注册）
    g.POST(SnapshotAction, Adapt(handleSnapshotAction))
    g.GET(SnapshotAction, Adapt(handleSnapshotAction))
    g.GET(SnapshotStorageAction, Adapt(handleSnapshotStorageAction))
    g.GET(SnapshotAction+"/:snapshot_id", Adapt(handleSnapshotAction))
    g.DELETE(SnapshotAction+"/:snapshot_id", Adapt(handleSnapshotAction))
    g.GET(OperationAction+"/:operation_id", Adapt(handleSnapshotOperationAction))

    // Template
    g.POST(TemplateAction, Adapt(handleTemplateAction))
    g.GET(TemplateAction, Adapt(handleTemplateAction))
    g.DELETE(TemplateAction, Adapt(handleTemplateAction))
    g.GET(TemplateCompatAction, Adapt(handleTemplateCompatAction))
    g.POST(TemplateCompatAction, Adapt(handleTemplateCompatAction))
    g.POST(TemplateRedoAction, Adapt(handleRedoTemplateAction))
    g.GET(TemplateBuildStatusAction+"/:build_id/status", Adapt(handleTemplateBuildStatusAction))
    g.GET(TemplateFromImageAction, Adapt(handleTemplateFromImageAction))
    g.POST(TemplateFromImageAction, Adapt(handleTemplateFromImageAction))
    g.GET(TemplateArtifactDownloadAction, Adapt(handleTemplateArtifactDownloadAction))
    g.HEAD(TemplateArtifactDownloadAction, Adapt(handleTemplateArtifactDownloadAction))

    // Artifact / CA download
    g.GET(CADownloadActionPrefix+":filename", Adapt(handleCADownloadAction))
    g.HEAD(CADownloadActionPrefix+":filename", Adapt(handleCADownloadAction))
    g.GET(RootfsArtifactAction, Adapt(handleRootfsArtifactAction))

    // Inventory
    g.POST(ListInventoryAction, Adapt(handleListInventoryAction))
}
```

**路由逐条验证**（diff vs server.go:86-110）：

| server.go 注册 | 方法 | 路由 | routes.go | ✓/✗ |
|---|---|---|---|---|
| :86 | POST,DELETE | /sandbox | g.POST + g.DELETE | ✓ |
| :87 | POST,DELETE | /image | g.POST + g.DELETE | ✓ |
| :88 | GET,POST | /sandbox/list | g.GET + g.POST (AdaptList) | ✓ |
| :89 | GET,POST | /sandbox/info | g.GET + g.POST | ✓ |
| :90 | POST | /sandbox/exec | g.POST only | ✓ |
| :91 | POST | /sandbox/update | g.POST | ✓ |
| :92 | POST | /sandbox/timeout | g.POST | ✓ |
| :93 | POST | /sandbox/refresh | g.POST | ✓ |
| :94 | POST | /sandbox/commit | g.POST | ✓ |
| :95 | POST | /sandbox/rollback | g.POST | ✓ |
| :96 | POST | /sandbox/preview | g.POST | ✓ |
| :97 | POST | /sandbox/{id}/rollback | g.POST("/:sandbox_id/rollback") | ✓ |
| :98 | GET,POST | /snapshot | g.GET + g.POST | ✓ |
| :99 | GET,DELETE | /snapshot/{id} | g.GET + g.DELETE("/:snapshot_id") | ✓ |
| :100 | GET | /operation/{id} | g.GET("/:operation_id") | ✓ |
| :101 | GET,POST,DELETE | /template | g.GET + g.POST + g.DELETE | ✓ |
| :102 | GET,POST | /template/compat | g.GET + g.POST | ✓ |
| :103 | POST | /template/redo | g.POST | ✓ |
| :104 | GET | /template/build/{id}/status | g.GET("/:build_id/status") | ✓ |
| :105 | GET,POST | /template/from-image | g.GET + g.POST | ✓ |
| :106 | GET,HEAD | /template/artifact/download | g.GET + g.HEAD | ✓ |
| :107 | GET,HEAD | /ca/{filename} | g.GET + g.HEAD("/:filename") | ✓ |
| :108 | GET | /rootfs-artifact | g.GET | ✓ |
| :109 | POST | /listinventory | g.POST | ✓ |
| :110 | GET,POST | /sandbox/logs | g.GET + g.POST | ✓ |

**snapshot/storage 路由**：server.go 中未在 cubeGroup 注册（当前通过 HttpHandler switch case 处理，cube.go:59），routes.go 中独立注册 `g.GET(SnapshotStorageAction, ...)`。 ✓

### 步骤 5：修改 `pkg/service/httpservice/cube/cube.go`

- **删除** `HttpHandler` 函数（55-132 行）——整个 switch 不再需要
- **删除** `isSandboxRollbackResourcePath` 函数（134-138 行）——Gin 路由 `/sandbox/:sandbox_id/rollback` 替代
- **保留** `getCaller` 函数（140-145 行）——middleware 中的 `getCaller` 是同名但属于 middleware 包的独立函数
- **保留** 所有 URI 常量和 `actionURI` 函数——handler 内部仍通过 `actionURI()` 比较 `r.URL.Path`
- 删除不再需要的 import（`strings`, `path/filepath` 如果仅被 HttpHandler 使用）

### 步骤 6：修改 `pkg/service/httpservice/cube/template_commit.go`

替换 `mux.Vars(r)["build_id"]`（见决策 4）。

### 步骤 7：修改 `pkg/service/httpservice/inner/`

创建 `routes.go`：

```go
package inner

func RegisterInnerRoutes(g *gin.RouterGroup) {
    g.GET(NodeAction, nodeGinHandler)
    // /ws 和 /query 当前不限制 HTTP 方法。
    // 实际只被 GET 请求（cubemastercli 用 GET），注册为 GET 即可。
    // 未来如需支持其他方法，可添加 g.POST/g.PUT 等。
    g.GET(StateWs, websocketGinHandler)
    g.GET(StateQuery, queryGinHandler)
}

func nodeGinHandler(c *gin.Context) {
    ctx := c.Request.Context()
    rt := CubeLog.GetTraceInfo(ctx)
    req := &types.GetNodeReq{}
    querys := c.Request.URL.Query()
    req.RequestID = querys.Get("requestID")
    req.HostID = querys.Get("host_id")
    if ss := querys.Get("score_only"); ss == "true" {
        req.ScoreOnly = true
    }
    rt.RequestID = req.RequestID
    rsp := getNodeInfo(ctx, req)
    common.WriteResponse(c.Writer, http.StatusOK, rsp)
}

func websocketGinHandler(c *gin.Context) {
    handleWebsocket(c.Writer, c.Request)
}

func queryGinHandler(c *gin.Context) {
    handleQuery(c.Writer, c.Request)
}
```

- **删除** `inner.HttpHandler` switch（inner.go:33-64）
- **保留** `handleWebsocket`, `handleQuery`, `getNodeInfo` 函数不变
- **不注册 `/internal/fake_create`**——当前为 dead route（switch 内无 case），不注册行为一致

### 步骤 8：修改 `pkg/service/httpservice/notify/`

创建 `routes.go`：

```go
package notify

func RegisterNotifyRoutes(g *gin.RouterGroup) {
    g.POST(HostChangeNotifyAction, hostChangeGinHandler)
    g.GET(HealthCheckAction, healthCheckGinHandler)
}

func hostChangeGinHandler(c *gin.Context) {
    ctx := c.Request.Context()
    rt := CubeLog.GetTraceInfo(ctx)
    req := &types.HostChangeEvent{}
    if err := common.GetBodyReq(c.Request, req); err != nil {
        rt.RetCode = int64(errorcode.ErrorCode_MasterParamsError)
        common.WriteResponse(c.Writer, http.StatusOK, &types.Res{
            Ret: &types.Ret{
                RetCode: int(errorcode.ErrorCode_MasterParamsError),
                RetMsg:  err.Error(),
            },
        })
        return
    }
    rt.RequestID = req.RequestID
    ctx = log.WithLogger(ctx, log.G(ctx).WithFields(map[string]any{"RequestId": req.RequestID}))
    rsp := hostChangeNotify(ctx, req)
    rt.RetCode = int64(rsp.Ret.RetCode)
    common.WriteResponse(c.Writer, http.StatusOK, rsp)
}

func healthCheckGinHandler(c *gin.Context) {
    rsp := healthCheck(c.Writer, c.Request)
    common.WriteResponse(c.Writer, http.StatusOK, rsp)
}
```

- **删除** `notify.HttpHandler` switch（notify.go:36-64）
- **保留** `hostChangeNotify`, `healthCheck` 业务函数不变

### 步骤 9：修改 `pkg/service/httpservice/meta/`

创建 `routes.go`，将 8 个 handler 改为 `*gin.Context`：

```go
package meta

func RegisterMetaRoutes(g *gin.RouterGroup) {
    g.GET(readyzAction, readyzGinHandler)
    g.POST(registerNodeAction, registerNodeGinHandler)
    g.GET(nodesAction, listNodesGinHandler)
    g.GET(versionMatrixAction, versionMatrixGinHandler)
    g.GET(nodeAction, getNodeGinHandler)
    g.POST(nodeStatusAction, updateNodeStatusGinHandler)
    g.POST(nodeLabelsAction, updateNodeLabelsGinHandler)
    g.DELETE(nodeLabelsAction, deleteNodeLabelGinHandler)
}
```

每个 handler 中 `mux.Vars(r)["node_id"]` → `c.Param("node_id")`。

**关键保留点**：meta 的 `writeErr(w, status, err)` 函数（meta.go:203-214）在 body 解码失败时用 HTTP 400，在业务逻辑错误时用 HTTP 200。这个 dual-status 行为必须原样保留。改为 gin handler 后：
```go
// body decode 失败：HTTP 400（与当前一致）
c.JSON(http.StatusBadRequest, &types.Res{...})
// 业务逻辑错误：HTTP 200（与当前一致）
common.WriteResponse(c.Writer, http.StatusOK, &types.Res{...})
```

route 常量格式变更：
```go
// 修改前 (gorilla/mux 格式)
nodeAction       = "/nodes/{node_id}"
nodeStatusAction = "/nodes/{node_id}/status"
nodeLabelsAction = "/nodes/{node_id}/labels"

// 修改后 (gin 格式)
nodeAction       = "/nodes/:node_id"
nodeStatusAction = "/nodes/:node_id/status"
nodeLabelsAction = "/nodes/:node_id/labels"
```

### 步骤 10：创建 `pkg/service/httpservice/middleware/gin_middleware.go`

实现 `GinRequestMiddleware()`（见决策 5）。

**删除** `middleware.go` 中的 `MiddlewareLogging` 函数——所有路由通过 gin engine，不再有 net/http 级别的 handler chain。保留 `getCaller`, `getCallerHostIP`, `getHTTPUA`, `checkAuth`, `lookupSecretKeyByUserID` 等 helper 函数（`GinRequestMiddleware` 依赖它们）。

### 步骤 11：修复 `pkg/service/httpservice/common/res.go`

修复 `WriteResponse` 和 `WriteListResponse` 的 header 顺序（见决策 6）。

### 步骤 12：修改 `pkg/server/server_test.go`

```go
// 修改前：使用 mux.Router.Match()
s := &internalHttp{router: mux.NewRouter()}
s.registerHandlers()
req := httptest.NewRequest("GET", "/cube/ca/cube-root-ca.crt", nil)
var match mux.RouteMatch
assert.True(t, s.router.Match(req, &match))

// 修改后：通过 gin.Engine 发送实际请求
gin.SetMode(gin.TestMode)
s := &internalHttp{engine: gin.New()}
s.registerRoutes()

// 验证 GET 和 HEAD 都能路由到 CA download
for _, method := range []string{http.MethodGet, http.MethodHead} {
    w := httptest.NewRecorder()
    req := httptest.NewRequest(method, "/cube/ca/cube-root-ca.crt", nil)
    s.engine.ServeHTTP(w, req)
    // 路由匹配成功（不返回 gin 默认 404）
    assert.NotEqual(t, http.StatusNotFound, w.Code)
}
```

### 步骤 13：删除 gorilla/mux 依赖

```bash
cd CubeMaster && go mod tidy
```

确认 `gorilla/mux` 不再出现在 `go.mod` 的 require 块中。

---

## 6. 文件变更清单

| 文件 | 操作 | 变更内容 |
|------|------|---------|
| `go.mod` | 修改 | 添加 `gin-gonic/gin >=v1.7.0`，删除 `gorilla/mux`（tidy 后自动） |
| `pkg/server/server.go` | 修改 | mux.Router→gin.Engine，重写路由注册，添加 NoRoute/NoMethod |
| `pkg/server/server_test.go` | 修改 | 适配 gin.Engine，保留 GET+HEAD 断言 |
| `pkg/service/httpservice/cube/gin_adapter.go` | **新建** | Adapt/AdaptList |
| `pkg/service/httpservice/cube/routes.go` | **新建** | RegisterCubeRoutes |
| `pkg/service/httpservice/cube/cube.go` | 修改 | 删除 HttpHandler switch + isSandboxRollbackResourcePath |
| `pkg/service/httpservice/cube/template_commit.go` | 修改 | mux.Vars→buildIDFromPath 手动解析 |
| `pkg/service/httpservice/inner/routes.go` | **新建** | RegisterInnerRoutes + gin handler |
| `pkg/service/httpservice/inner/inner.go` | 修改 | 删除 HttpHandler switch |
| `pkg/service/httpservice/notify/routes.go` | **新建** | RegisterNotifyRoutes + gin handler |
| `pkg/service/httpservice/notify/notify.go` | 修改 | 删除 HttpHandler switch |
| `pkg/service/httpservice/meta/routes.go` | **新建** | RegisterMetaRoutes |
| `pkg/service/httpservice/meta/meta.go` | 修改 | handler 改 *gin.Context，mux.Vars→c.Param，route 常量格式 |
| `pkg/service/httpservice/middleware/gin_middleware.go` | **新建** | GinRequestMiddleware |
| `pkg/service/httpservice/middleware/middleware.go` | 修改 | 删除 MiddlewareLogging，保留 helper |
| `pkg/service/httpservice/common/res.go` | 修改 | 修复 header 顺序 bug |

**不变的文件**（零 regression 风险）：
- 所有 `cube/sandbox_*.go`（create, delete, timeout, refresh, logs 等）
- 所有 `cube/snapshot.go`（path 解析 helper 不变）
- 所有 `cube/template*.go`（除 template_commit.go 的 mux.Vars 替换）
- 所有 `*_test.go`（除 server_test.go）
- 所有 `integration/` 测试

---

## 7. 风险分析与缓解

### 7.1 Gin 路由冲突

**风险**：Gin 使用 radix tree router（httprouter fork），静态路由和参数化路由在同一层级可能冲突。

**正确原理**：Gin v1.7.0+ 支持**静态路由优先于参数化路由**（static-priority-over-param）。同层级同深度的静态路由（如 `/snapshot/storage`）和参数化路由（如 `/snapshot/:snapshot_id`）可以共存——Gin 先尝试静态匹配，不中再尝试参数化匹配。

**本项目涉及的 static-param 同级路由**（全部已验证无冲突）：

| 静态路由 | 参数化路由 | HTTP方法 | 冲突 |
|----------|-----------|---------|------|
| `/snapshot/storage` (GET) | `/snapshot/:snapshot_id` (GET, DELETE) | GET 树内 static-priority | ✓ 无冲突 |
| `/sandbox/list` (GET,POST) | `/sandbox/:sandbox_id/rollback` (POST) | 不同深度，无冲突 | ✓ |
| `/nodes/register` (POST) | `/nodes/:node_id` (GET) | 不同方法，无冲突 | ✓ |

**缓解**：步骤 4 完成后立即 `go build`——Gin 注册冲突路由时会 panic。

### 7.2 Trailing Slash 行为变更

**风险**：Gin 默认 `RedirectTrailingSlash=true`，会将 `/cube/sandbox/` 301 重定向到 `/cube/sandbox`。当前 mux 不做此重定向（返回 404）。

**缓解**：设置 `engine.RedirectTrailingSlash = false`（见步骤 2）。

### 7.3 NoRoute / NoMethod HTTP 状态码变更

**风险**：当前 mux 对未匹配路由返回 HTTP 404 plain text，对未匹配方法返回 HTTP 405 plain text。迁移后 NoRoute/NoMethod handler 返回 HTTP 200 + JSON body。

**评估**：这是**有意的改进**——统一错误响应格式。当前没有任何客户端依赖 mux 的 404/405 plain text 响应（所有客户端检查 body 中的 ret_code）。风险极低。

### 7.4 Websocket + Gin 中间件交互

**风险**：Gin 中间件中的 panic recovery 在 websocket upgrade 后可能尝试写入已 hijack 的 ResponseWriter。

**缓解**：
- `gin.ResponseWriter` 实现了 `http.Hijacker`，`gorilla/websocket` 的 `upgrader.Upgrade(c.Writer, c.Request, nil)` 正常工作
- websocket handler 内部不 panic
- Gin middleware 的 defer/recovery 在 `c.Next()` 返回后执行——此时 websocket goroutine 已独立运行

### 7.5 `http.NewResponseController` 兼容性

`extendSnapshotWriteDeadline(w)`（snapshot.go:239-244）使用 `http.NewResponseController(w).SetWriteDeadline(deadline)`。

Gin 的 `ResponseWriter` 包装了 `http.ResponseWriter`。`http.NewResponseController` 通过 `Unwrap()` 方法层层解包。Gin v1.9.0+ 的 `ResponseWriter` 实现了 `Unwrap() http.ResponseWriter`。如果 gin 版本不够新，`SetWriteDeadline` 会返回 `http.ErrNotSupported`——代码已优雅处理此情况（仅打 warn 日志，不 panic）。

### 7.6 mock 模式行为

**风险**：mock 模式的 `time.Sleep` 如果丢失会改变测试时序。

**缓解**：已在决策 5 的代码中明确保留 `time.Sleep`。与现有代码完全一致。

### 7.7 Content-Type Header 变化

修复 `WriteResponse` 的 header 顺序后，Content-Type 从隐式 sniffing 变为显式设置。行为一致——所有 handler 输出 JSON body，sniffing 也会识别为 `application/json`。

---

## 8. 测试策略

### 8.1 单元测试（不变）

以下测试 handler 函数签名的单元测试**无需修改**：
- `sandbox_create_test.go`, `sandbox_preview_test.go`, `snapshot_test.go`, `template_test.go`, `template_commit_test.go`
- `cubeboxutil_test.go`, `affinityutil_test.go`（纯函数测试）
- `middleware_test.go`（测试 `getCallerHostIP` helper）

### 8.2 单元测试（需修改）

- `server_test.go` — 重写为 gin.Engine 适配（见步骤 12），保留 GET+HEAD 断言

### 8.3 新增测试（迁移守卫）

**路由集对等测试**——在 `server_test.go` 中新增，断言 gin 注册的路由集合与 mux 原始路由集合完全一致（method × path）：

```go
func TestRouteParity(t *testing.T) {
    expected := []struct {
        method string
        path   string
    }{
        {"GET", "/metrics"},
        {"POST", "/cube/sandbox"},
        {"DELETE", "/cube/sandbox"},
        {"POST", "/cube/sandbox/preview"},
        // ... 完整列表从 server.go:86-110 提取
    }

    gin.SetMode(gin.TestMode)
    s := &internalHttp{engine: gin.New()}
    s.registerRoutes()

    routes := s.engine.Routes()
    routeSet := make(map[string]bool)
    for _, r := range routes {
        routeSet[r.Method+" "+r.Path] = true
    }

    for _, e := range expected {
        key := e.method + " " + e.path
        assert.True(t, routeSet[key], "missing route: %s", key)
    }
}
```

**Static-priority 测试**——验证 Gin 正确区分静态路由和参数化路由：
```go
func TestStaticPriorityRouting(t *testing.T) {
    // GET /cube/snapshot/storage 应路由到 handleSnapshotStorageAction，不是 handleSnapshotAction(id="storage")
    w := httptest.NewRecorder()
    req := httptest.NewRequest("GET", "/cube/snapshot/storage", nil)
    s.engine.ServeHTTP(w, req)
    // 验证响应是 storage 数据格式，不是 "snapshot not found"
}
```

### 8.4 集成测试（不变但关键）

`integration/` 目录的 7 个测试文件通过真实 HTTP client 测试完整请求链路。它们是**迁移的主要安全网**：
- `inner_test.go` — GET /internal/node
- `image_test.go` — POST /cube/image
- `cubebox_helpers_test.go` — sandbox CRUD lifecycle
- `notify_test.go` — POST /notify/host
- `main_test.go` — 服务启动 + readiness

### 8.5 验证步骤

1. `go build ./...` — 编译通过
2. `go vet ./...` — 静态分析通过
3. `go test ./pkg/server/ ./pkg/service/httpservice/...` — 单元测试通过
4. `go test ./integration/ -v` — 集成测试通过（需要 redis/DB mock 环境）
5. 手动 curl 验证关键端点的响应格式

---

## 9. 迁移执行顺序

```
1. go get gin → 添加依赖（确保 >= v1.7.0）
2. common/res.go → 修复 header bug（独立变更，先做）
3. middleware/gin_middleware.go → 新建 Gin 中间件
4. cube/gin_adapter.go → 新建 adapter
5. cube/routes.go → 新建路由注册
6. cube/cube.go → 删除 HttpHandler switch
7. cube/template_commit.go → 替换 mux.Vars
8. inner/routes.go + inner.go → 迁移 inner 包
9. notify/routes.go + notify.go → 迁移 notify 包
10. meta/routes.go + meta.go → 迁移 meta 包
11. server.go → 替换 mux→gin.Engine + NoRoute/NoMethod
12. server_test.go → 适配 gin + 路由对等测试
13. go mod tidy → 清理依赖
14. go build + go test → 全量验证
```

步骤 2 可独立先行。步骤 3-10 可并行开发（各包独立），但步骤 11 依赖全部完成。步骤 12-14 在最后统一执行。
