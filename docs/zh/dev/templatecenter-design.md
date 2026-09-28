# CubeTemplateCenter 设计

CubeTemplateCenter（TC）是从 CubeMaster 拆分出来的独立模板构建服务。本文描述**当前已实现架构**；代码注释里出现的 `design §x.y` 引用本文章节。

## 1. 概述

历史上 CubeMaster 在进程内完成模板 ext4 构建：拉取源 OCI 镜像、解包 layer、执行 `mkfs.ext4`、上传 artifact、分发到节点——全部发生在对外提供沙箱管控面的同一进程里。TC 接管其中的数据面工作。

拆分的主要理由：

- **权限隔离。** 构建需要 root（`umoci unpack` 保留 uid/gid、`mkfs.ext4`、loop 设备）。把这些操作移出公开 API 进程，可以缩小爆炸半径。
- **资源隔离。** 构建会瞬时打满 CPU / IO；管控面的时延 SLO 不应直接受并发构建影响。
- **独立生命周期。** CubeMaster 主要承载 API、状态与编排；TC 主要承载构建、artifact 落盘 / 上传、对账。两者可以独立升级和排障。

除了 `from-image` 构建链路，当前版本还支持把**历史本地模板 artifact** 迁进 TC 存储。对应 API 是 `/cube/template/migrate`，CLI 命令名是 `tpl merge`。

## 2. 进程拓扑

### 2.1 进程

- **CubeMaster** —— 管控面：持久化 job 行、暴露模板 API、向 TC 提交构建 / migrate 请求、接收状态回调、向节点分发 artifact、编排删除与 redo。
- **CubeTemplateCenter** —— 数据面：拉镜像、构建 ext4、上传 S3、接收 `tpl merge` 上传的 ext4、提供 artifact 下载 / 302、回传构建状态。

TC **不是语义上的“永远单例”**：

- 使用**本地盘**时，通常保持 **1 副本**，因为每个副本只看得到自己的本地 artifact。
- 使用 **S3-backed** 或 **ReadWriteMany 共享存储** 时，可以部署 **多副本 TC**。

### 2.2 通信

- CubeMaster → TC：
  - `POST /tc/api/v1/build`（提交 `from-image` 构建）
  - `POST /tc/api/v1/artifact/upload`（`tpl merge` 上传本地 ext4）
  - `POST /tc/api/v1/artifact/delete`（物理删除 artifact 数据）
- TC → CubeMaster：
  - `POST /internal/template/jobs/:job_id/status`（构建状态回调，带认证——见 §6.1）
- 存储共享：
  - 共享 CubeDB
  - artifact 字节可以落在同机共享目录、ReadWriteMany 卷，或 S3 / MinIO

### 2.3 所有权划分

CubeMaster 拥有全部**业务状态写入**（job 行、definition、replica、alias、compat）与全部 cubelet RPC。TC 只拥有 artifact 的**物理数据**（本地 ext4 文件、S3 对象）以及与之直接相关的对账 / 删除。

TC 不初始化 worker（cubelet）grpc 连接池，因此任何可能发起 cubelet RPC 的 handler——快照创建、删除、redo 续跑、模板分发——都仍由 CubeMaster 服务，且不在 TC 上注册写路由。

## 3. API 与 job 模型

### 3.1 别名语义

模板别名是可选的稳定名字（`[a-z0-9-]`，最长 64 字符），沙箱创建请求可以用它代替生成的 `tpl-*` id。`PUT /cube/template/:template_id/alias` 传 absent / null / `""` 表示清除别名。

### 3.2 `from-image` 构建 job

`from-image` job 由 CubeMaster 创建 / 复用并持久化，TC 负责真正执行构建。job 会携带进度字段（`phase`、`progress`、pull bytes / layers、distribution counters），便于 CLI 和 UI 轮询展示。

构建链路大致分为：

1. 拉取 / 解包源镜像
2. 生成 ext4 rootfs
3. 计算模板规格指纹与 artifact 元数据
4. 回调 CubeMaster，触发 resume 流水线
5. 由 CubeMaster 注册 artifact、向节点分发、写最终模板 / job 状态

### 3.3 `tpl merge` / migrate job

`POST /cube/template/migrate` 会提交一个 **migrate job**。CLI 命令名保持用户习惯上的 `tpl merge`，但后端 API 路径明确使用 `migrate`，强调它做的是**artifact 存储迁移**，不是“模板请求 merge”。

语义如下：

- 输入是一个已经 `READY` 的模板。
- 如果 artifact 仍在 CubeMaster 本地磁盘：
  - 优先迁到 S3（若已配置）
  - 否则上传到 TC 自己的 artifact store（`/tc/api/v1/artifact/upload`）
- 如果 artifact 已经是 S3-backed：
  - 做幂等检查
  - 尝试清理遗留的本地 ext4 副本

对**存量镜像制作出来的历史模板**，运维文档应统一采用以下口径：**`tpl merge` 解决历史 artifact 的存储收敛问题，`tpl redo` 解决节点侧重新分发 / 必要时重建问题。**

典型场景是：模板最初的 artifact 仍保存在 CubeMaster 本地盘，后续集群开启了 `s3Backed=true`，需要将这批历史 artifact 从本地盘迁移到 **S3 托管存储**。在这个场景下，应先执行 `tpl merge` 完成存储迁移；若同一次运维还需要让模板重新覆盖目标节点，再继续执行 `tpl redo`。

> **高亮提醒**
> 在默认共盘 / 共享存储拓扑下，未执行 `tpl merge` 并不意味着现有 `READY` 模板会立即失去下载能力；真正的问题是历史 artifact 仍未完成从**本地盘到 S3 托管存储**的收敛。
>
> - **存储侧**：开启 `s3Backed=true` 后，旧模板不会自动补做迁移。
> - **恢复侧**：如果本地 ext4 先丢了，再跑 `tpl merge` 也无法补救，因为已经没有可上传的文件；这时只能对可重建的 `from-image` 模板执行 `tpl redo`，回退到重建流程。

migrate job 有自己独立的状态读取路径：`GET /cube/template/migrate?job_id=...`。

### 3.4 错误映射

HTTP 层把领域错误映射为 API 错误码：

- `ErrTemplateIDRequired`、`ErrDuplicateTemplate`、`ErrNoTemplateNodes` → 参数错误
- `ErrTemplateStoreNotInitialized` → DB / store 初始化错误
- not-found → `130404`
- `ErrTemplateNotReady` → conflict

渐进式拆分会把更多错误翻译逐步下沉到 store 层。

### 3.5 Redo

`POST /cube/template/redo` 用于续跑失败的模板 job。分发阶段失败但 artifact 已为 `READY` 的 job 会复用 artifact，而不是重建；复用 `PENDING` / `BUILDING` 的 artifact 会读到半成品 ext4，因此是禁止的。若 artifact 不再可复用，redo 会退回到从 `source_image_ref` 做 full rebuild（该构建由 TC 执行，而不是依赖先前 `merge` 的结果）。

在运维文档中，`tpl merge` 应描述为**历史 artifact 从本地盘迁移到 S3 托管存储**的存储收敛动作，`tpl redo` 应描述为**节点侧重新分发 / 必要时重建**动作。只有在同一次运维同时涉及历史 artifact 迁移和节点重新覆盖时，才需要按 **先 `merge`、后 `redo`** 的顺序执行。

### 3.6 Resume 流水线

TC 上报 `BUILT` / 构建完成后，CubeMaster 执行 resume：

1. 注册远端构建产出的 artifact
2. 向目标节点分发 artifact
3. 写 template / replica / job 终态

当解析不到目标节点时，resume 会以 `ErrNoTemplateNodes` 失败，并有意不写 definition / replica 行。

### 3.7 Job 幂等

不变量 **I1**：同一模板规格最多存在一个活跃（`PENDING` / `RUNNING`）`from-image` job。

对 migrate 也有类似约束：同一模板在同一时刻最多存在一个活跃 migrate job；跨副本并发提交时，后写入者会在 DB 中让位给“更早创建的赢家”并复用其 job id，避免多个副本同时上传同一个 ext4。

## 4. 路由划分

CubeMaster 上对外暴露的 `/cube/template*` 路由：

| 路由 | 服务方 | 原因 |
| --- | --- | --- |
| POST `/cube/template`、`/from-image`、`/redo`、`/migrate`；DELETE；GET；PUT alias | CubeMaster（本地） | 涉及 job 持久化、状态编排、cubelet RPC、缓存或向 TC 转发请求 |
| GET `/cube/template/build/:id/status` | CubeMaster（本地） | `tpl commit` 的 build-status / build-watch 读取的是 master 本地 job 行 |
| GET `/cube/template/from-image?job_id=...` | CubeMaster（本地） | `from-image` job 轮询读取 master 持久化状态 |
| GET `/cube/template/migrate?job_id=...` | CubeMaster（本地） | migrate job 轮询读取 master 持久化状态 |
| GET / POST `/cube/template/compat` | 反代到 TC | 无缓存的 DB 读写 |
| GET / HEAD `/cube/template/artifact/download` | 反代到 TC | 文件服务 / S3 重定向 |
| GET `/cube/rootfs-artifact?...` 等元数据接口 | CubeMaster（本地） | 返回的是 master 维护的 artifact 行与模板元数据 |

`RegisterTemplateRoutes`（TC 进程）只注册**无缓存纯 DB 路由**与**artifact 下载 / 内部 API**子集。两份清单由合同测试钉住，保证“模板写路由不漂移到 TC”。

## 5. 配置

### 5.1 命名

TC 读取的变量一律是 `CUBE_TEMPLATE_CENTER_*`。CubeMaster 主要读取：

- `CUBE_TEMPLATE_CENTER_ADDR`：提交构建 / artifact 上传 / 删除的内部地址
- `CUBE_TEMPLATE_CALLBACK_TOKEN`：TC 回调认证令牌（见 §6.1）

### 5.2 地址接线

- **Helm**：`cube.templateCenterEndpoint` 把集群内 Service 地址渲染进 master 的 `CUBE_TEMPLATE_CENTER_ADDR` 环境变量和 `conf.yaml` 的 `template_center_addr`。TC 侧以同样方式获得 `CUBE_MASTER_ADDR`。
- **one-click**：`cubemaster-start.sh` 默认把 `CUBE_TEMPLATE_CENTER_ADDR` 设为 `http://127.0.0.1:8090`；TC 的 `conf.yaml` 则在安装期由 `__CUBETEMPLATECENTER_*__` 占位符渲染得到。

### 5.3 向后兼容窗口

改名前的拼写（`CUBE_TC_*`、`CUBE_MASTER_*`、`CUBEMASTER_*`）仍作为 fallback 生效，并记录弃用提示（可通过 `tcconfig.Warnings()` 获取）。保留窗口的原因是：未同步更新的部署脚本否则可能静默把 artifact 写到错误目录，最后只在下载 404 时暴露。

新部署应只使用 `CUBE_TEMPLATE_CENTER_*` 命名。

## 6. 安全

### 6.1 回调认证

状态回调的 payload 会被 resume 流水线整体信任——伪造的 `BUILT` 上报里的 artifact id / sha 可能最终成为节点启动使用的 rootfs。因此该端点要求共享密钥：

- TC 发送 `X-Cube-Template-Callback-Token`
- CubeMaster 用常量时间与 `CUBE_TEMPLATE_CALLBACK_TOKEN` 比较
- 不匹配返回 `401`

为了兼容滚动升级，若 CubeMaster 未设置该变量，端点会暂时保持开放并打印一次警告。Helm、one-click 与 terraform 都会默认接好该密钥。

### 6.2 TC 内部端点

TC 的 `/tc/api/v1/*` 端点不带认证，必须保持在集群内部 / VPC 内部地址上；不要把它们直接暴露到公网。chart 默认渲染成内网 Service / 内网 LB；one-click 默认绑定 loopback。

## 7. Reconcile（对账）

### 7.1 进度快照

实时进度会写 Redis；持久化终态快照通过状态回调落库。

### 7.2 停滞构建检测

TC 侧 reconciler（`pkg/reconcile`）周期性扫描进度上报停止的 job，并把长期停滞的 job 标记为 `FAILED`。

### 7.3 被遗弃的构建

TC 重启会丢失内存中的构建状态。reconciler 按停滞阈值清扫卡在 `RUNNING` 的 job（默认 10 分钟一轮，可通过 `CUBE_TEMPLATE_CENTER_RECONCILE_*` 调整）并置为 `FAILED`；客户端需要重试。若已经存在 `READY` artifact，redo 会优先复用。

该机制是强制的：没有它，一次崩溃的构建会借助不变量 I1 把模板永久卡死在“已有活跃 job”的状态里。

## 8. 健康与就绪

- **CubeMaster**：`GET /notify/health` —— 不依赖 DB 或 nodemeta。
- **TC**：`GET /health` —— store 已连接且 `nodemeta` 已初始化才算就绪。

`nodemeta.Ready()` 表示“初始化已完成”，而不是“release manifest 文件存在”。`release-manifest.json` 只有 one-click 包会安装；Kubernetes 部署里没有它也不应导致 TC 就绪失败。

## 9. 存储与并发

### 9.1 副本模型与 artifact 存储

TC 的副本策略取决于 artifact 字节存在哪里：

- **本地盘 / RWO PVC**：推荐 **1 副本**。否则不同副本彼此看不到对方构建出的 ext4，请求被打到非构建副本时会 404。
- **ReadWriteMany 共享卷**：可多副本，所有副本能看到同一份 ext4 字节。
- **S3-backed**：可多副本；artifact 下载统一走 presigned URL / 302。

因此，正确结论不是“TC 永远单例”，而是：**本地盘模式通常单副本；共享存储或 S3 模式支持多副本。** CubeMaster 仍然是无状态、最适合水平扩展的一侧。

### 9.2 DB 锁

跨进程互斥（如 artifact 认领、并发迁移、对账互斥）使用 MySQL `GET_LOCK` 命名锁，取代无法跨越 CubeMaster 与 TC 的进程内 `sync.Mutex`。锁名会做归一化，以保持在 MySQL 64 字符限制内。

### 9.3 孤儿 GC

CubeMaster 上的后台清扫会移除引用计数归零（模板删除、构建失败）且超过 GC 期限的 artifact，并执行与在线删除相同的三阶段清理。

### 9.4 Job 归属

job 表没有 owner 列：TC 重启后，任何仍然存活的 TC 副本都可以对账停滞 job。进度上报会刷新停滞时钟，因此对账依赖的是共享 DB / 状态时间戳，而不是“某个固定副本拥有某个 job”。

### 9.5 Artifact 删除

删除采用三阶段最后属主清理（`cleanupArtifactFully`）：

1. **Phase 1（短事务）**：统计剩余引用，把行标为 `CLEANUP_PENDING`
2. **Phase 2（无锁、幂等）**：删除各放置节点上的 ext4
3. **Phase 3（短事务）**：复查引用与状态、删除 placement 行，并通知 TC 删除物理数据

只有 TC 删除 S3 对象 / 本地文件 / artifact 行。过去由 CubeMaster 直接删会泄漏 S3 对象，而且没有 `CLEANUP_PENDING` 行留给兜底清扫。

### 9.6 节点放置

`ArtifactNodePlacement` 行记录哪些节点持有副本，同时驱动删除 Phase 2 与下载就近性。

### 9.7 共享 artifact 存储

TC 写 ext4，CubeMaster 通过 `/cube/template/artifact/download` 把它提供给 cubelet——两个进程必须看到同一份 artifact 字节，或者都能通过同一套 S3 路径 / presigned URL 找到它。

- one-click：同机文件系统共享
- Helm：同一个 PVC（本地盘模式时通过同节点亲和保证 master / TC 共置）
- `s3Backed=true`：由对象存储统一承载 artifact 字节

因为 artifact 存储是共享的 / 可寻址的，构建期记录的路径与对象引用对 reconciler 始终有效，即使中途发生 TC 重启也不需要把 job “绑死”在某个实例上。

## 10. 已知限制与后续项

以下问题在当前版本里仍然存在：

- **37 天预签名 URL 不刷新。** 上传到 S3 的 artifact 在构建时记录长时效 presigned URL；没有单独的刷新任务，超过时效后需要 redo 或重新 merge / rebuild 才会得到新 URL。
- **`hasActiveJob` 不含 `BUILT`。** 幂等窗口覆盖 `PENDING` / `RUNNING`；已 `BUILT` 但 resume 流水线还在分发时，重复创建可能发起第二次分发，而不是挂到第一次上。
- **envd 时代的 redo 直接 `FAILED`。** 对旧版进程内构建器产出的模板，若缺少可复用 artifact 记录，redo 会快速失败而不是自动重建；这类模板请从源镜像重新创建。
- **TC 仍依赖 CubeMaster 包。** TC 复用 CubeMaster 的 config loader、log、recovery、nodemeta 与 templatecenter store 包，所以当前 `go.mod` 里仍需要 `replace`。把共享部分抽到 `pkgs/` 仍是后续工作。
