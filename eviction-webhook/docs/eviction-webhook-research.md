# Eviction Webhook 调研与实施方案

> 日期：2026-07-20  
> 作者：CubeSandbox 基础架构组

---

## 一、背景与问题

### 1.1 问题现状

Kubernetes 在节点资源压力（内存、磁盘、PID）超过阈值时，kubelet 会自动发起 Pod 驱逐（Eviction）。对于 CubeSandbox 的 sandbox Pod，驱逐意味着底层 MicroVM **被直接销毁**，用户运行状态全部丢失，无法恢复。

### 1.2 目标与边界

**目标**：开发一个独立的 eviction-webhook 组件，在 K8s 层面拦截 sandbox Pod 的驱逐请求，阻止其执行，并将事件上报给 CubeMaster。

**职责边界**：

| 职责 | 归属 |
|------|------|
| 拦截驱逐请求，返回 denied | eviction-webhook |
| 本地持久化记录驱逐事件 | eviction-webhook |
| 向 CubeMaster 上报事件 | eviction-webhook |
| Pause / Resume sandbox | CubeMaster（已有能力，自动处理） |
| 节点压力监控与恢复触发 | CubeMaster（自动处理） |
| 迁移决策 | CubeMaster（自动处理） |

**eviction-webhook 不修改任何现有组件**（CubeMaster、Cubelet、cube-lifecycle-manager、CubeProxy）。

### 1.3 完整流程思路

本方案引入 eviction-webhook 后，系统整体处理"节点内存压力"的完整链路如下：

```
节点内存压力大
      │
      │  kubelet 检测到内存超过驱逐阈值，选中 sandbox Pod
      ▼
拦 截 驱 逐  ◄─────────────────────────────────────────────
      │                                                    │
      │  eviction-webhook：                               │ kubelet 再次
      │  ① 返回 allowed:false，驱逐被阻止                 │ 尝试驱逐时
      │  ② NDJSON 本地落盘                               │ 继续拦截
      │  ③ 调用 CubeMaster 既有接口执行 isolate/pause     │
      │  ④ 可选异步上报 POST /event/eviction              │
      │                                                    │
      ▼                                                    │
sandbox 暂停（CubeMaster 驱动）                            │
      │                                                    │
      │  eviction-webhook 调用 CubeMaster sandbox update   │
      │  → CubeMaster 调用 Cubelet PauseContainer          │
      │  → MicroVM 冻结：CPU 占用归零，内存状态保留        │
      │  → sandbox → Paused                              ─┘
      │
      │  （节点 CPU 压力立即下降；内存仍驻留，
      │   但随其他 sandbox 空闲回收，压力逐步减小）
      ▼
压 力 减 小
      │
      │  其他 sandbox 被正常回收 / 业务请求减少
      │  节点可用内存逐步上升
      │  kubelet MemoryPressure 条件由 True → False
      │  Cubelet 心跳将该状态上报给 CubeMaster
      ▼
sandbox 恢复（CubeMaster 驱动）
      │
      │  CubeMaster 检测到节点压力解除
      │  → 调用 Cubelet ResumeContainer
      │  → MicroVM 解冻，用户进程从挂起点继续运行
      │  → sandbox → Running
      ▼
用户无感知恢复 ✓
```

**各阶段职责归属**：

| 阶段 | 动作 | 负责方 |
|------|------|--------|
| 节点压力大 | kubelet 检测并发起驱逐 | K8s 原生 |
| 拦截驱逐 | 返回 denied + 上报 CubeMaster | **eviction-webhook（本组件）** |
| sandbox 暂停 | PauseContainer | CubeMaster + Cubelet |
| 压力感知 | 心跳上报 MemoryPressure 条件变化 | Cubelet → CubeMaster |
| sandbox 恢复 | ResumeContainer | CubeMaster + Cubelet |

eviction-webhook 是整个链路的**入口触发点**，将"驱逐事件"转化为"CubeMaster 能感知并处理的信号"，Pause / 压力监控 / Resume 均由 CubeMaster 自行驱动，与 eviction-webhook 无关。

---

## 二、K8s Eviction 机制

### 2.1 驱逐本质

kubelet 发起驱逐的实现是向 APIServer **创建一个 Eviction 子资源**：

```
POST /api/v1/namespaces/{namespace}/pods/{pod-name}/eviction
```

APIServer 在执行前会依次调用所有匹配的 Admission Webhook。Webhook 返回 `allowed: false` 时，APIServer 拒绝请求，驱逐不会执行，Pod 继续运行。

### 2.2 ValidatingWebhook 关键参数

| 参数 | 值 | 说明 |
|------|----|------|
| `rules.resources` | `pods/eviction` | 精确匹配驱逐请求 |
| `rules.operations` | `CREATE` | 驱逐本质是创建 Eviction 子资源 |
| `failurePolicy` | `Ignore` | webhook 不可用时降级放行，不阻断集群 |
| `timeoutSeconds` | `5` | APIServer 等待 webhook 的最大超时 |
| `objectSelector` | 见 2.3 | 过滤 CubeSandbox 专属 Pod |

### 2.3 objectSelector 过滤

对 `pods/eviction` 类型的 webhook，`objectSelector` 匹配**被驱逐 Pod 的 labels**（非 Eviction 对象本身）。使用以下配置精确过滤 CubeSandbox sandbox Pod：

```yaml
objectSelector:
  matchExpressions:
  - key: cube.master.instance.type
    operator: Exists
```

### 2.4 AdmissionReview 关键字段

| 字段 | 内容 |
|------|------|
| `request.uid` | 本次 Admission 请求的唯一 ID |
| `request.name` | 被驱逐的 Pod 名称（= SandboxID） |
| `request.namespace` | Pod 所在命名空间 |
| `request.userInfo.username` | kubelet 身份，格式 `system:node:<nodeName>` |

注意：AdmissionReview 中没有完整的 Pod 对象，Pod labels 需通过 Pod Informer 本地缓存查询。

---

## 三、CubeMaster 接口调研

### 3.1 服务结构

**模块路径**：`github.com/tencentcloud/CubeSandbox/CubeMaster`

**HTTP 框架**：`gorilla/mux`，路由注册在 `pkg/server/server.go` 的 `registerHandlers()` 中。

**现有路由前缀**：`/notify`、`/cube`、`/internal`、`/meta`

### 3.2 请求/响应格式

**请求解析**：`common.GetBodyReq(r, &req)`，底层使用 `json-iterator`。

**标准响应**：

```json
{
  "requestID": "xxx",
  "ret": { "ret_code": 200, "ret_msg": "ok" }
}
```

**响应写出**：`common.WriteResponse(w, http.StatusOK, rsp)`

### 3.3 认证机制

**方式**：HMAC-SHA1/SHA256 签名，通过 6 个 HTTP Header 传递。

**签名构造**（对齐 `pkg/base/auth/auth.go`）：

```
to_be_signed = version.userID.timestamp.nonce.sgnMethod
signature    = base64( HMAC-SHA1(secretKey, to_be_signed) )
```

| Header | 说明 |
|--------|------|
| `cube_version` | 协议版本，默认 `"2023"` |
| `cube_user_id` | 调用方标识 |
| `cube_timestamp` | Unix 时间戳 |
| `cube_nonce` | 随机数，防重放 |
| `cube_sgn_method` | 签名算法，`"sha1"` 或 `"sha256"` |
| `cube_signature` | 计算结果 |

**开关**：`config.AuthConf.Enable`，测试环境可关闭。

### 3.4 可选上报接口

eviction-webhook 可选向 CubeMaster 上报审计事件。该接口不是主恢复链路依赖，默认通过
`EVENT_REPORT_ENABLE=false` 关闭；主链路使用 CubeMaster 已有 isolation、sandbox list、
sandbox update 接口完成 isolate/pause/resume/unisolate。

```
POST /event/eviction

请求体：
{
  "requestID":    "string",   // = AdmissionReview.uid
  "podName":      "string",   // = SandboxID
  "namespace":    "string",
  "nodeName":     "string",
  "instanceType": "string",
  "interceptedAt":"string"    // ISO8601
}

响应体：
{
  "requestID": "string",
  "ret": { "ret_code": 200, "ret_msg": "ok" }
}
```

CubeMaster 收到上报后，自行决策后续处理（Pause / Resume / 迁移），eviction-webhook 不关心。

---

## 四、SandboxID 与 Pod 标签调研

### 4.1 SandboxID 获取

**结论：K8s Pod Name 即 SandboxID。**

CubeMaster 在 `sandbox_create.go` 中以 `InsId` 作为 Pod 名称，`/cube/sandbox/update` 的 `sandbox_id` 字段直接对应 Pod Name。

webhook 收到 AdmissionReview 时，`request.Name`（Pod Name）**即是** CubeMaster 能识别的 SandboxID，无需任何转换。

### 4.2 InstanceType 标签

**标签 Key**：`cube.master.instance.type`

CubeMaster 在创建 sandbox 时将 `req.InstanceType` 写入 Pod label，典型值为 `cubebox`。上报 CubeMaster 时需传入此字段，因此 webhook 通过 Pod Informer 从 Pod labels 中读取。

---

## 五、可行性结论

| 验证项 | 结论 | 依据 |
|--------|------|------|
| ValidatingWebhook 能拦截驱逐 | ✅ | K8s 标准能力 |
| objectSelector 精确过滤 sandbox Pod | ✅ | `cube.master.instance.type Exists` |
| webhook 5s 内响应 | ✅ | 同步路径纯内存操作 < 5ms，上报 CubeMaster 异步执行 |
| webhook 故障不阻塞集群 | ✅ | `failurePolicy: Ignore` 降级放行 |
| SandboxID 获取 | ✅ | Pod Name = SandboxID，无需额外查询 |
| InstanceType 获取 | ✅ | Pod Informer 本地缓存，O(1) 查询 |
| 不修改现有组件 | ✅ | eviction-webhook 是独立新组件 |

---

## 六、整体架构

```
┌──────────────────────────────────────────────────────────────────────┐
│                         Kubernetes Cluster                            │
│                                                                       │
│   kubelet（节点资源压力）                                             │
│       └─► POST /api/v1/namespaces/{ns}/pods/{name}/eviction           │
│                           │                                           │
│                   K8s APIServer                                       │
│                           │  ValidatingWebhookConfiguration           │
│                           ▼  objectSelector 匹配通过                  │
│         ┌──────────────────────────────────────────────┐             │
│         │          eviction-webhook（新组件）            │             │
│         │  HTTPS :8443/webhook/eviction                │             │
│         │  ① 解析 AdmissionReview                      │             │
│         │  ② Pod Informer 取 InstanceType              │             │
│         │  ③ NDJSON 落盘（本地审计）                   │             │
│         │  ④ goroutine 调用 CubeMaster 既有恢复接口    │             │
│         │  ⑤ 可选异步上报 CubeMaster 审计事件          │             │
│         │  ⑥ return allowed: false ─────────────────► 驱逐被阻止    │
│         └──────────────────────────────────────────────┘             │
│                           │                                           │
└───────────────────────────┼───────────────────────────────────────────┘
                            │ isolation/list/update（HMAC，可选）
                            ▼
               ┌──────────────────────────────────────┐
               │             CubeMaster                │
               │  执行节点隔离和 sandbox pause/resume  │
               │             ↓                         │
               │  HTTP 200 返回给 eviction-webhook     │
               └──────────────────────────────────────┘
```

**状态汇报说明**：

Pause/Resume 完成后**不会**重新发起 `POST /api/v1/.../eviction`——该端点仅供 kubelet 主动驱逐时使用。恢复动作由 eviction-webhook 的 recovery manager 调用 CubeMaster 既有接口完成；`POST /event/eviction` 仅是可选审计上报。

```
eviction-webhook ──isolate/list/update──► CubeMaster
                 ◄────── HTTP 200 ───────
eviction-webhook 等待 MemoryPressure 解除后 resume/unisolate
```

---

## 七、组件设计：eviction-webhook

### 7.1 目录结构

```
eviction-webhook/
├── cmd/eviction-webhook/main.go       # 入口：TLS HTTP Server + Pod Informer 启动
├── internal/
│   ├── admission/handler.go           # AdmissionReview 处理，返回 allowed:false
│   ├── podinformer/informer.go        # Pod Informer 缓存（取 InstanceType label）
│   ├── reporter/
│   │   ├── reporter.go                # 异步上报 + 指数退避重试
│   │   └── auth.go                    # HMAC-SHA1 签名（对齐 CubeMaster auth）
│   └── store/store.go                 # NDJSON 本地持久化
├── pkg/types/event.go                 # EvictionEvent 类型定义
├── deploy/kubernetes/                 # K8s 部署清单
└── go.mod
```

### 7.2 EvictionEvent 字段

| 字段 | 来源 | 说明 |
|------|------|------|
| `eventId` | AdmissionReview.uid | 全局唯一 ID，用于幂等 |
| `podName` | request.name | = SandboxID |
| `namespace` | request.namespace | 命名空间 |
| `nodeName` | 解析 userInfo.username | 发起驱逐的节点 |
| `instanceType` | Pod label | CubeMaster 处理所需字段 |
| `interceptedAt` | 本地时间 | 拦截时间戳（ISO8601）|

### 7.3 核心处理流程

```
admission.Handler.handle()（同步，目标 < 5ms）

① 解析 AdmissionReview → 取 podName / namespace / nodeName
② podCache.Get(ns, podName) → 取 instanceType        [内存，0 网络调用]
③ store.Save(event)          → 写 NDJSON             [本地文件 append]
④ go reporter.Report(event)  → 启动 goroutine，立即返回
⑤ return AdmissionResponse { allowed: false }
```

### 7.4 Reporter 上报策略

- **异步**：goroutine 执行，不占用 webhook 5s timeout
- **重试**：失败时指数退避（1s → 2s → 4s），最多 3 次
- **去重**：同一 eventId（AdmissionReview.uid）每次 eviction 请求唯一，天然幂等
- **降级**：全部重试失败后写 error log，NDJSON 本地仍有记录，不影响拦截功能

### 7.5 Pod Informer

- 使用 `k8s.io/client-go` in-cluster config 初始化
- 启动时调用 `WaitForCacheSync`，缓存同步完成后再开始接收请求
- 提供 `Get(namespace, name) (*Pod, bool)` 接口，O(1) 内存查询
- 若 Pod 不在缓存（极少情况）：instanceType 留空，仍拒绝驱逐并上报

---

## 八、K8s 部署清单

### RBAC

```yaml
ServiceAccount:   eviction-webhook (cube-system)
ClusterRole:
  rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list", "watch"]   # Pod Informer 最小权限
ClusterRoleBinding: 绑定以上两者
```

### TLS 证书（cert-manager）

```yaml
Certificate:
  secretName: eviction-webhook-tls
  dnsNames:
  - eviction-webhook.cube-system.svc
  - eviction-webhook.cube-system.svc.cluster.local
  issuerRef: cube-ca-issuer (ClusterIssuer)
```

cert-manager 通过 annotation `cert-manager.io/inject-ca-from` 自动向 ValidatingWebhookConfiguration 注入 caBundle。

### Deployment 关键配置

| 项目 | 值 | 说明 |
|------|-----|------|
| 副本数 | 2 | HA，避免 webhook 单点 |
| 镜像 | `ccr.ccs.tencentyun.com/cube/eviction-webhook` | |
| 监听端口 | 8443（TLS） | |
| 健康检查 | `HTTPS GET /healthz` | |
| 凭据注入 | K8s Secret `eviction-webhook-auth` | 不写入镜像 |
| 资源 | requests 50m/64Mi，limits 200m/256Mi | 轻量服务 |

**环境变量**：

| 变量 | 说明 |
|------|------|
| `CUBE_MASTER_URL` | CubeMaster HTTP 地址 |
| `EVENT_REPORT_ENABLE` | 是否启用 `/event/eviction` 辅助事件上报，默认 `false` |
| `CUBE_AUTH_ENABLE` | 是否对 CubeMaster 请求添加 HMAC 认证头 |
| `CUBE_AUTH_USER_ID` | HMAC 认证用户 ID |
| `CUBE_AUTH_SECRET_KEY` | HMAC 认证密钥 |

### ValidatingWebhookConfiguration

```yaml
webhooks:
- name: eviction.cube.tencent.com
  rules:
  - apiGroups:   ["policy", ""]
    apiVersions: ["v1"]
    resources:   ["pods/eviction"]
    operations:  ["CREATE"]
  clientConfig:
    service:
      name:      eviction-webhook
      namespace: cube-system
      path:      /webhook/eviction
      port:      443
  objectSelector:
    matchExpressions:
    - key: cube.master.instance.type
      operator: Exists
  admissionReviewVersions: ["v1"]
  sideEffects: None
  timeoutSeconds: 5
  failurePolicy: Ignore        # webhook 故障时降级放行，不阻断集群
```

---

## 九、风险评估

| 风险 | 等级 | 应对 |
|------|------|------|
| webhook 单点故障阻断集群驱逐 | 高 | `failurePolicy: Ignore` + 2 副本 |
| webhook 5s timeout 超时 | 中 | 同步路径无网络调用 < 5ms；上报完全异步 |
| Pod Informer 冷启动 label 缺失 | 中 | `WaitForCacheSync` 后再接收请求；缺失时 instanceType 留空仍正常拦截 |
| CubeMaster 不可用，上报失败 | 中 | 3 次退避重试；NDJSON 本地留有记录；不影响驱逐拦截本身 |
| auth 密钥泄露 | 中 | K8s Secret 注入，不写入镜像或配置文件 |
| 非 sandbox Pod 误拦截 | 低 | `objectSelector` 精确过滤，完全规避 |

---

## 十、组件变更清单

### 新增组件

| 组件 | 说明 |
|------|------|
| `eviction-webhook/` | 独立新组件，不修改任何现有库 |

### 现有组件变更

**无**。eviction-webhook 默认不要求 CubeMaster 新增 `/event/eviction` 接口；该接口仅在
`EVENT_REPORT_ENABLE=true` 时作为可选审计上报目标。

---

## 十一、工作量拆分

| 阶段 | 内容 | 估时 |
|------|------|------|
| P1 | 组件脚手架（go.mod、main.go、TLS server、/healthz、Makefile） | 0.5d |
| P2 | Pod Informer（in-cluster config、WaitForCacheSync、Get 接口） | 0.5d |
| P3 | Admission Handler（AdmissionReview 解析、返回 allowed:false） | 0.5d |
| P4 | Store（NDJSON 本地持久化） | 0.5d |
| P5 | Reporter（HMAC 签名 + 异步上报 + 指数退避重试） | 0.5d |
| P6 | K8s 清单（rbac、cert、deployment、service、webhook） | 0.5d |
| P7 | 单元测试 + 集成测试 | 1d |
| **合计** | | **~4d** |

---

## 十二、前置条件与依赖

| 条件 | 说明 |
|------|------|
| cert-manager | 集群需已安装，且存在 `cube-ca-issuer` ClusterIssuer |
| CubeMaster `/event/eviction` 接口 | 可选；仅在 `EVENT_REPORT_ENABLE=true` 时用于辅助事件上报，主恢复链路不依赖该接口 |
| CubeMaster auth 配置 | 若 `AuthConf.Enable=true`，需在 SecretKeyMap 中添加 `eviction-webhook → <secretKey>` |
| Pod label 存量确认 | 确认所有 sandbox Pod 均携带 `cube.master.instance.type` label |
| 镜像仓库权限 | 构建后推送到 `ccr.ccs.tencentyun.com/cube/eviction-webhook` |
