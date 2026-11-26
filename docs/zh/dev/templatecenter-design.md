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

节点持续从节点视图中缺失满 15 分钟（取异步操作超时；只看是否缺失，不看健康位）时，Phase 2 跳过它，避免失联节点让 artifact 永远停在 `CLEANUP_PENDING`。该节点的行会保留并写入 `deferred_at`，其余行照常删除并通知 TC。每轮 artifact GC 会对节点已回到视图的 deferred 行，用节点当前地址重试销毁，确认成功后才删行；节点一直不回来则该行一直保留。历史回填的 `ip:` 前缀 id 不会被判为缺失。

**范围与限制**：
- 本机制覆盖 `ArtifactNodePlacement` 行的延迟清理。
- crashed/unhealthy-but-registered 节点不在范围：只有完全从节点视图消失才计为缺失，健康位被忽略。
- 历史回填的 `ip:` 前缀 id 来自 20260623145000 迁移中无 `node_id` 的 replica 行，不会被判为缺失。若 artifact 在已退役节点上只剩 `ip:` 行，Phase 2 会持续向旧 IP 重试，artifact 永远停在 `CLEANUP_PENDING`。此类行为封闭的历史存量，可用 `SELECT COUNT(*) FROM t_cube_artifact_node_placement WHERE node_id LIKE 'ip:%'` 确认，必要时手动清理。
- 用户可见延迟：15 分钟宽限期 + 最多 10 分钟 GC 周期，即节点消失后最多 25 分钟 artifact 才终结。

deferred 行不会被自动回收：该行是"节点上可能仍有文件"的唯一记录，按时间删除会在节点回归时让这些文件静默泄漏。确认节点已永久下线后，运维可执行 `DELETE FROM t_cube_artifact_node_placement WHERE deferred_at > 0 AND node_id = '<node-id>'` 释放其行。**建议先备份，通过受控 CLI 执行，并关注相关 metrics。**

缺失起点只保存在进程内存中：Master 重启会重置计时，HA 各副本独立计时。defer 是幂等的（`deferred_at = 0` 保护），副本间判定时刻不同不会写坏数据；重启只是让收敛最多推迟一个宽限期。defer 日志会记录缺失起点和已缺失时长，便于排障。

**升级顺序**：`deferred_at` 列由迁移 `20261112071208_artifact_placement_deferred_at.sql` 添加。必须先执行迁移，再滚动升级 CubeMaster / CubeTemplateCenter；否则 GC 扫描和 TC backstop 会因列不存在而报错。`CUBE_AUTO_MIGRATION=false` 时，需先手动执行迁移 SQL，再启动新版本。

### 9.7 共享 artifact 存储

TC 写 ext4，CubeMaster 通过 `/cube/template/artifact/download` 把它提供给 cubelet——两个进程必须看到同一份 artifact 字节，或者都能通过同一套 S3 路径 / presigned URL 找到它。

- one-click：同机文件系统共享
- Helm：同一个 PVC（本地盘模式时通过同节点亲和保证 master / TC 共置）
- `s3Backed=true`：由对象存储统一承载 artifact 字节

因为 artifact 存储是共享的 / 可寻址的，构建期记录的路径与对象引用对 reconciler 始终有效，即使中途发生 TC 重启也不需要把 job “绑死”在某个实例上。

当对象存储才是持久副本时，TC 写出的 ext4 并不是副本，只是构建中间产物。因此 TC
在构建收尾阶段就把它删掉，构建节点不会再“每构建一次就多留一个 artifact”：

- 上传成功（`s3` 后端）：在 BUILT 上报成功后立即删除本地副本。这类 artifact 由
  CubeMaster 从对象存储流式读取，本地文件此后不会再被任何流程读到。
- 上传失败：构建上报 FAILED，本地副本同样删除。旧行为是照样上报 BUILT、不写
  storage backend，此时 artifact 由 TC 的磁盘提供——只有 Master 恰好共享该副本的
  artifact 目录时才成立，换成远端 Master 或第二个 TC 副本就取不到了。

两种情况会保留本地文件：`CUBE_ARTIFACT_STORE_BACKEND=fs` 时存储根可能就是同一个
目录，文件本身就是持久副本；以及上传成功但没能记录下载 URL 时，Master 对这类记录
返回 502（并不会回退读 TC 的磁盘），保留字节是此时唯一的可恢复副本。

### 9.8 直连下载的灰度开关与存储身份

artifact 行上记录 `storage_backend` / `bucket` / `object_key` 三元组，作为 artifact
字节的稳定身份；`bucket` 列由迁移
`20261119140000_rootfs_artifact_store_identity.sql` 添加。历史行**故意**不回填
`bucket`：它们的真实 bucket 只能由当初上传的那套部署决定，用当前默认值回填会把
"身份未知"伪造成"身份已确认"。因此复用与删除逻辑把空字段当作"未知"处理，既不会
因为比不上而强制重建，也不会因为比不上而永远删不掉。

**直连注解是追加的，不是替换的**。每次 per-node `CreateImage` 只会选出一个*直连*源，
但它是和稳定的 CubeMaster URL **一起**下发，而不是取而代之：旧的那对注解始终存在，选出
直连源时再附加直连的那对。新版 Cubelet 优先用直连、失败回退旧的；老版 Cubelet 只看得见
旧的那对。正是这份冗余让下面的回退和混版本灰度成为可能。

因此 capability **不是** per-node 凭据：HMAC 消息只覆盖 artifact id 和过期时间，同一秒内
下发的所有节点拿到的 token 是完全相同的。签发动作仍放在 per-node 循环内，因为分发并发度
受 `common.distribution_workers` 限制——排在队尾的节点若用分发开始时签的那一个 token，拿到
手时可能已经接近或超过 TTL。签名是纯 CPU（约 1.4us、不走网络），按节点签不产生可测量开销。

**直连失败回退**：直连端点是优化，永远不是拿到字节的唯一途径。当直连失败的原因可归因于端点
本身时，Cubelet 会用同一份 spec 里的 legacy CubeMaster URL 重试一次：

- 连接/DNS 失败，或 HTTP 401、403、404、408、429 以及任意 5xx；
- `expires_at` 已过期的 capability——在发请求**之前**就识别出来，免得过期凭据表现为一个
  难以归因的 403，或者错误页被当成 artifact 字节后报成大小不符。

完整性失败（sha256 或大小不符）**故意不回退**：端点已经应答了，换地方重试只会掩盖一个损坏
的 artifact。凭据跟随所选源，所以 Cube capability 绝不会被发给 S3。

没有这套回退，节点到 S3/TC 的出口被封、混版本下的 404、或者 TTL 过期，任何一个都会把一套
原本可用的部署变成"模板创建失败"，且只能靠回滚解决。

**运维前置条件**：有三个环境事实决定直连路径是否*可用*。它们都不是正确性的前提——每一条
缺失只是让优化失效，因为代理路径始终有效、上面的回退会兜住失败。把它们当作"下载还在走
CubeMaster 时该查什么"，而不是安装阻塞项：

| 事实 | 缺失时的后果 |
| --- | --- |
| CubeMaster 持有 artifact bucket 的 `CUBE_S3_*` 凭据 | Master 签不出 presign，S3 直连永不被选中。TC 上报的归一化降级为告警而不是让 job 失败，构建仍然成功（这也是旧的"Master 必须持有 S3 凭据"要求不再成立的原因）。 |
| 每个 Cubelet 都能访问 TC 的 node-facing 地址（`common.template_center_node_addr` / `CUBE_TEMPLATE_CENTER_NODE_ADDR`） | TC 端点被下发但不可达；每个节点失败一次后回退到 CubeMaster。loopback/通配地址会被提前识别，根本不会下发 TC 直连。 |
| Master 与 TC 在**两边都配了 S3** 时 `CUBE_S3_*` 必须一致 | 上报的 bucket 与 Master 配置的 bucket 不同属于确定性冲突，仍然拒绝该回调。只有这一种情况是致命的；**无法验证**的 bucket（Master 没配 S3）按上报值记录。 |

**两条下载路由接受的凭据不同**，因为它们位于用户 API-key 中间件的两侧：

| 路由 | 挂载位置 | 接受的凭据 |
| --- | --- | --- |
| `/cube/template/artifact/download` | API-key 鉴权之内 | 长期 `DownloadToken` 或 capability |
| `/cube/template/artifact/direct-download` | API-key 鉴权之外 | 仅 capability |

长期 `DownloadToken` 是一个没有有效期的 bearer 凭据：明文存在 artifact 行上、发给过
每一个拉取过该 artifact 的 Cubelet、只有重新 finalize 才会轮换。在 API-key 中间件后
面这是可以接受的，因为调用方已经通过鉴权。但在 `direct-download` 上没有第二因子，接受
它意味着一次泄漏就等于永久拿到 artifact 字节，因此该路由拒绝它——
`CubeMaster/pkg/service/httpservice/cube/routes.go` 把请求标记为 capability-only，
`validRootfsArtifactDownloadToken` 据此判定。CubeTemplateCenter 复用同一个注册函数，
所以直连路径的两端都生效。这不会造成回退：capability 签不出来时 CubeMaster 会返回
`ArtifactSourceLegacy` 并清掉全部直连注解，Cubelet 根本不会拿长期 token 去打
`direct-download`。

**构建竞争败者的对象回收**：输掉 fingerprint 竞争的那次构建已经用自己的 artifact id
上传了对象，而 adopt 胜者意味着这个 id 永远不会有行。artifact GC 是按行扫描的，没有
行的对象永远不会成为 GC 候选——每输一次竞争就泄漏一整个 rootfs。因此 Master 只在
adopt 之后请求 TC 删除上报的那个对象。几道判断故意是不对称的，因为误删会毁掉一个在用
的模板，而漏删只是浪费存储：胜者自己的字节绝不碰（同一 artifact id 或同一 object
key）、上报里存储身份不完整就当作"没上传过"、artifact id 仍存在任何状态的行就交给
拥有它的 GC。回收失败只记录 object key 供人工清理，绝不让 job 失败。

`common.artifact_download_direct_enabled`（默认 true，可由
`CUBEMASTER_ARTIFACT_DOWNLOAD_DIRECT_ENABLED` 覆盖）是直连下载的灰度总开关。置为
false 时 CubeMaster 既不签发 presigned URL 也不下发 TC/Master 直连端点，只写旧的
代理注解，等价于本特性之前的行为，cubelet 侧无需感知。由于代理路径始终可用，关掉
它只是用 Master 的带宽换安全性，不会导致模板创建失败。开关对重启后新分发的
artifact 生效，已经下发出去的 URL 不受影响。

## 10. 已知限制与后续项

以下问题在当前版本里仍然存在：

- ~~**37 天预签名 URL 不刷新。**~~ 已解决：cubelet 下载的不再是构建时记录的那个 URL。每次 `CreateImage` 都会按 `common.artifact_download_capability_ttl` 重新签发短时效 presigned URL，模板再老也不需要 redo 换 URL。artifact 行上保存的是 `storage_backend` / `bucket` / `object_key` 这组稳定身份，而不是凭据。
- **`hasActiveJob` 不含 `BUILT`。** 幂等窗口覆盖 `PENDING` / `RUNNING`；已 `BUILT` 但 resume 流水线还在分发时，重复创建可能发起第二次分发，而不是挂到第一次上。
- **envd 时代的 redo 直接 `FAILED`。** 对旧版进程内构建器产出的模板，若缺少可复用 artifact 记录，redo 会快速失败而不是自动重建；这类模板请从源镜像重新创建。
- **TC 仍依赖 CubeMaster 包。** TC 复用 CubeMaster 的 config loader、log、recovery、nodemeta 与 templatecenter store 包，所以当前 `go.mod` 里仍需要 `replace`。把共享部分抽到 `pkgs/` 仍是后续工作。
