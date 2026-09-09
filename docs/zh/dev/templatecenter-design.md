# CubeTemplateCenter 设计

CubeTemplateCenter（TC）是从 CubeMaster 拆分出来的独立模板构建服务。本文描述已实现架构；代码注释以 `design §x.y` 引用本文章节。

## 1. 概述

历史上 CubeMaster 在进程内完成模板 ext4 构建：拉取源 OCI 镜像、解包 layer、执行 `mkfs.ext4`、上传 artifact、分发到节点——全部发生在对外提供沙箱管控面的同一进程里。TC 接管其中的数据面工作。

拆分的三个理由：

- **权限隔离。** 构建需要 root（`umoci unpack` 保留 uid/gid、`mkfs.ext4`、loop 设备）。把这些移出应答公开 API 的进程可以缩小爆炸半径。
- **资源隔离。** 构建会瞬时打满 CPU/IO；管控面的时延 SLO 不应受并发构建数影响。
- **独立生命周期。** TC 是带节点本地状态的单例；CubeMaster 无状态、可水平扩展（见 §9）。

## 2. 进程拓扑

### 2.1 进程

- **CubeMaster** —— 管控面：持久化 job 行、向 TC 转发构建、接收状态回调、向节点分发 artifact、服务所有模板写操作与带缓存的读操作。
- **CubeTemplateCenter** —— 数据面：拉镜像、构建 ext4、上传 S3、回传状态。永远只有一个实例（§9.1）。

### 2.2 通信

- CubeMaster → TC：内部 HTTP `POST /tc/api/v1/build`（提交）、`POST /tc/api/v1/artifact/delete`（物理删除）。
- TC → CubeMaster：`POST /internal/template/jobs/:job_id/status`（状态回调，带认证——§6.1）。
- 两者共享 CubeDB 与 artifact 目录（§9.7）。

### 2.3 所有权划分

CubeMaster 拥有全部业务状态写入（job 行、definition、replica、别名）与全部 cubelet RPC。TC 只拥有 artifact 的物理数据（本地 ext4 文件、S3 对象）。TC 从不初始化 worker（cubelet）grpc 连接池，因此任何可能发起 cubelet RPC 的 handler——快照创建、删除、redo 续跑——都由 CubeMaster 服务，且根本不在 TC 上注册路由（见 §4）。

## 3. API 与 job 模型

### 3.1 别名语义

模板别名是可选的稳定名字（`[a-z0-9-]`，最长 64 字符），沙箱创建请求可以用它代替生成的 `tpl-*` id。`PUT /cube/template/:template_id/alias` 传 absent / null / `""` 表示清除别名。

### 3.2 构建 job 状态

`t_cube_template_image_job.status`：PENDING → RUNNING → BUILT | FAILED。`phase` 跟踪流水线步骤（PULLING / BUILDING / UPLOADING / DISTRIBUTING）。BUILT 是构建本身的终态；随后 resume 流水线（§3.5）注册 artifact 并分发。

### 3.3 错误映射

HTTP 层把领域错误映射为 API 错误码：`ErrTemplateIDRequired`、`ErrDuplicateTemplate`、`ErrNoTemplateNodes` → 参数错误；`ErrTemplateStoreNotInitialized` → DB 错误；not-found → 130404。渐进式拆分会把更多错误翻译逐步下沉到 store 层。

### 3.4 Redo

`POST /cube/template/redo` 续跑 FAILED 的 job。分发阶段失败但 artifact 已为 READY 的 job 会复用 artifact 而不是重建（复用 PENDING/BUILDING 的 artifact 会读到写了一半的 ext4）。

### 3.5 Resume 流水线

收到 BUILT 回调后 CubeMaster 执行三步 resume：（1）注册远端构建的 artifact，（2）向目标节点分发，（3）写 job 终态行。分发解析不到目标节点时以 `ErrNoTemplateNodes` 失败，并且有意不写 definition/replica 行。

### 3.6 Job 幂等

不变量 **I1**：同一模板规格最多存在一个活跃（PENDING/RUNNING）job。首次创建 job 的 JSON 与 REDO job 的 RequestJSON 与状态翻转在同一个 DB 事务里写入，崩溃不会留下与负载不一致的行。COMMIT / SNAPSHOT_* / LEGACY job 不在 from-image 幂等窗口内。

## 4. 路由划分

CubeMaster 上的公开 `/cube/template*` 路由：

| 路由 | 服务方 | 原因 |
| --- | --- | --- |
| POST `/cube/template`、`/from-image`、`/redo`；DELETE；GET；PUT alias | CubeMaster（本地） | 涉及 cubelet RPC、进程内缓存或转发构建的环境变量 |
| GET `/cube/template/build/:id/status`、GET `/from-image` | CubeMaster（本地） | master 自有 job 行的纯 DB 读；反代会在 TC 不可用时出现 create 200/轮询 502 |
| GET/POST `/cube/template/compat` | 反代到 TC | 无缓存的 DB 读写 |
| GET/HEAD `/cube/template/artifact/download` | 反代到 TC | 文件服务 / S3 重定向 |
| GET `/cube/template/artifact/...`（rootfs、CA） | CubeMaster（本地） | cubelet 经 master 从共享盘拉取 |

`RegisterTemplateRoutes`（TC 进程）只注册无缓存纯 DB 与文件服务的子集。两份清单由合同测试钉住（`TestCubeRoutesTemplateWritesStayOnCubeMaster`、`TestCubeRoutesTemplateProxyRemainder`、`TestTemplateCenterRoutesExcludeWritesAndCachedReads`）。

## 5. 配置

### 5.1 命名

TC 读取的变量一律是 `CUBE_TEMPLATE_CENTER_*`。CubeMaster 读取 `CUBE_TEMPLATE_CENTER_ADDR`（构建提交地址）与 `CUBE_TEMPLATE_CALLBACK_TOKEN`（§6.1）。

### 5.2 地址接线

- Helm：`cube.templateCenterEndpoint` 把集群内 ClusterIP 地址渲染进 master 的 `CUBE_TEMPLATE_CENTER_ADDR` 环境变量和 conf.yaml 的 `template_center_addr`。TC 侧以同样方式得到 `CUBE_MASTER_ADDR`。
- one-click：`cubemaster-start.sh` 把 `CUBE_TEMPLATE_CENTER_ADDR` 默认设为 `http://127.0.0.1:8090`；TC 的 conf.yaml 由安装期解析 `__CUBETEMPLATECENTER_*__` 占位符生成。

### 5.3 向后兼容窗口

改名前的拼写（`CUBE_TC_*`、`CUBE_MASTER_*`、`CUBEMASTER_*`）仍作为 fallback 生效，并记录弃用提示（可通过 `tcconfig.Warnings()` 获取）。保留窗口是因为未同步更新的部署脚本否则会静默把 artifact 写到错误目录，直到很久以后下载 404 才暴露。新部署应只使用 `CUBE_TEMPLATE_CENTER_*` 命名。

## 6. 安全

### 6.1 回调认证

状态回调的 payload 被 resume 流水线整体信任——伪造的 BUILT 上报里的 artifact id/sha 会成为节点启动用的 rootfs。因此该端点要求共享密钥：TC 发送 `X-Cube-Template-Callback-Token`，CubeMaster 用常量时间与 `CUBE_TEMPLATE_CALLBACK_TOKEN` 比较，不匹配返回 401。当 CubeMaster 上该变量未设置时端点保持开放（打印一次警告），以便滚动升级期间旧版 TC 继续工作；Helm（自动生成 `cube-template-callback-token` Secret key）、one-click（生成进 `.one-click.env`）与 terraform（`random_password`）都会默认接好。

### 6.2 TC 构建端点

TC 的内部 `/tc/api/v1/*` 端点不带认证，必须保持在集群内部 / VPC 内部地址（chart 把可选 CLB 渲染为仅内网；one-click 默认把 TC 绑到 loopback）。

## 7.  reconcile（对账）

### 7.1 进度快照

实时拉取进度写 Redis；持久化的终态快照通过状态回调落库。

### 7.2 停滞构建检测

TC 侧的 reconciler（pkg/reconcile）周期性扫描进度上报停止的 job 并标记 FAILED。

### 7.3 被遗弃的构建

TC 重启会丢掉内存中的构建状态。reconciler 按停滞阈值清扫卡在 RUNNING 的 job（默认 10 分钟一轮，可用 `CUBE_TEMPLATE_CENTER_RECONCILE_*` 调整）并置为 FAILED；客户端必须重试（存在 READY artifact 时 redo 会复用，§3.4）。该机制是强制的而非可选：没有它，一次崩溃的构建会借助不变量 I1（§3.6）把模板永远卡死。

## 8. 健康与就绪

- CubeMaster：`GET /notify/health`——不依赖 DB 或 nodemeta。
- TC：`GET /health`——store 已连接且 `nodemeta` 已初始化才算就绪。`nodemeta.Ready()` 表示"Init 已完成"，而不是"release manifest 存在"：manifest（`release-manifest.json`）只有 one-click 包会安装，CubeMaster 在 Kubernetes 里没有它也能正常运行，因此缺失不应导致就绪失败。声明的组件版本只是兼容扫描的输入。

## 9. 存储与并发

### 9.1 单例

TC 永远只有一个实例。artifact 存放在 ReadWriteOnce PVC 后的节点本地盘上；第二个副本既读不到第一个的 ext4 也接不了它的构建，两个副本还会在同一 artifact 目录上竞争。要扩容请扩 CubeMaster（无状态的一半）。

### 9.2 DB 锁

跨进程互斥（如构建时的 artifact 认领）使用 MySQL `GET_LOCK` 命名锁，取代无法跨越 CubeMaster 与 TC 的进程内 `sync.Map[artifactID]*sync.Mutex`。锁名会做归一化以保持在 MySQL 64 字符限制内。

### 9.3 孤儿 GC

CubeMaster 上的后台清扫移除引用计数归零（模板删除、构建失败）且超过 GC 期限的 artifact，执行与在线删除相同的三阶段清理（§9.5）。

### 9.4 Job 归属

job 表没有 owner 列：TC 重启后任何 TC 实例（也只有一个，§9.1）都可以对账所有停滞 job。进度上报会刷新停滞时钟。

### 9.5 Artifact 删除

三阶段最后属主清理（`cleanupArtifactFully`）：Phase 1（短事务，行 FOR UPDATE）统计剩余引用并把行标记为 CLEANUP_PENDING；Phase 2（无锁、幂等）销毁各放置节点上的 ext4；Phase 3（短事务）复查引用与状态、删除放置行，然后通知 TC 移除物理数据。只有 TC 删除 S3 对象 / 本地文件 / artifact 行——过去由 CubeMaster 直接删会泄漏 S3 对象，且没有 CLEANUP_PENDING 行留给兜底清扫。

### 9.6 节点放置

`ArtifactNodePlacement` 行记录哪些节点持有副本，同时驱动 Phase 2 删除与下载就近性。

### 9.7 共享 artifact 存储

TC 写 ext4，CubeMaster 通过 `/cube/template/artifact/download` 把它提供给 cubelet——两个进程必须看到同一个目录。one-click：同机文件系统。Helm：同一个 PVC（ReadWriteOnce 是节点级约束，chart 用 required podAffinity 把 TC 钉在 master 所在节点；`colocateWithMaster=false` 要求真正共享的 ReadWriteMany 卷）。terraform：启用 CFS 时共享，否则单副本 pod 本地 emptyDir 共置。因为目录是共享的，构建期记录的路径对 reconciler 始终有效，即使 TC 中途重启过。

## 10. 已知限制与后续项

以下问题随当前版本发布时已知，逐项列出后续计划。

- **37 天预签名 URL 不刷新。** 上传到 S3 的 artifact 在构建时记录长时效预签名 URL；没有刷新任务，超过预签名时效的模板需要 redo 来生成新 URL。
- **`hasActiveJob` 不含 BUILT。** 幂等窗口（§3.6）覆盖 PENDING/RUNNING；已 BUILT 但 resume 流水线仍在分发的 job 在窗口之外，此时重复创建会发起第二次分发而不是挂到第一次上。
- **envd 时代的 redo 直接 FAILED。** 续跑由旧版进程内构建器产出的模板时没有可复用的 artifact 记录，会快速失败而不是重建；这类模板请从源镜像重新创建。
- **TC 仍依赖 CubeMaster 包。** TC 复用 CubeMaster 的 config loader、log、recov、nodemeta 与 templatecenter store 包，而没有抽成共享库，这就是 TC 的 go.mod 带 CubeMaster `replace` 的原因。把共享部分抽到 `pkgs/` 是后续工作。
