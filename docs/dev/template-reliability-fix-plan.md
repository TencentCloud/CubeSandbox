# 模板可靠性与 ext4 直连下载：整体清单、修复进度与验证报告

> 更新日期：2026-11-04。
> 工作分支：`fix/artifact-download-s3-direct`；本轮开始时 HEAD 为 `6e7cbcee`。
> 状态：已有实现及本轮专项修复，尚未完成全部集成验收；本轮未提交、未推送、未部署。
> 安全边界：不删除业务桶对象、不修改线上配置，不将暖状态提交降级为冷镜像重建。
> 下文第 1 节以后的原始设计保留用于追溯；其“当前”“待实施”指原设计时点。实际实施状态以第 0 节为准。
> 整体测试命令、集群验收矩阵、证据与清理流程见 [整体测试指南](template-reliability-test-guide.md)；新增指南不代表已执行全部集成验收。

## 0. 本轮实施总览

### 0.1 功能、问题与状态矩阵

| 项目 | 原因/目标 | 当前实现 | 验证与剩余边界 |
|---|---|---|---|
| 模板 `backend` 参数 | 将模板 CoW 后端传给 CubeMaster | 分支已有 API、Go/Node/Python SDK、文档与测试；GitHub #2039 已合并相应功能，本轮仅统一纯空白为省略 | Rust 测试未运行；本机无 cargo。旧 #1983 与上游功能重叠，不应原样重复合并 |
| R1/R2 ext4 直连与短期授权 | Master 不搬运 ext4 字节，定位稳定、授权短期有效 | 分支已有 `f80e1c2c`：稳定定位/迁移、Master ticket、TC 下载、Cubelet 直接拉取及授权刷新 | 不是本轮新实现；需在集群验证 S3/MinIO 和本地/CBS/CFS 场景 |
| R3 模板数据库分页 | 避免全量读取再切片，以及逐页等待全量输出 | 已有 DB LIMIT/OFFSET；本轮新增 skip_total（LIMIT+1）、窗口计算前排除已有模板历史 job、CLI --all 逐页 flush/共享截止时间/实际页长度推进 | SQLite 250 模板 + 2000 历史作业、has_more、无 COUNT、真实 CLI HTTP 与 race 专项通过；生产 DB EXPLAIN、深 offset 与并发更新仍需验收 |
| CLI 存储后端选择 | JSON 模板创建缺 backend flag；希望接受 COS/MinIO 名称 | `tpl create` 支持 --backend；`tpl build` 为 create-from-image 别名；xfs/s3/cos/minio 在发送前校验，cos/minio 规范为 s3 | 真实 cli.App→HTTP 参数测试通过；COS/MinIO 使用节点已有 S3-compatible 配置，不是每请求切换不同 bucket，也不代表独立 native COS 后端 |
| R4 / #2063 redo 旧产物引用 | 新 UUID 产物生成后 definition/request/cache 仍指向旧产物 | `6e7cbcee` 已做事务内替换与缓存同步，保留 alias/CreatedAt 并拒绝不安全替换 | `TestEnsureTemplateDefinition*` 专项通过；不等于覆盖同 ID 篡改 SHA 或完整双 Master redo 压测 |
| R5 / #1965 退役节点与清理引用 | 已退役节点、残留引用使删除不收敛或状态失真 | 分支已有注册状态感知、当前地址解析、引用保护等改动 | 退役 placement、注册表未就绪及 BUILT 引用保护专项通过；真实节点 RPC/快照删除验收仍需执行 |
| #2078 相同 fingerprint 构建重试 | 终态旧 A 占唯一 fingerprint，新 B 的 BUILT 回调无法注册 | 本轮增加终态 fingerprint 退休、保留旧清理行与对象位置；新 B 可注册；活跃 BUILDING 仍等待 | 实际注册状态矩阵和重复回调幂等专项通过；不将有效 BUILT 按随意超时丢弃，MySQL/PG 并发验收待执行 |
| TC 版本化对象误判不存在 | UploadVersioned 写 `id-SHA.ext4`，复用却 HEAD 历史 `id.ext4` | 本轮改用 `StatStored(ObjectKey, ArtifactID)` | 在 34 的 Linux 执行实际 fs blobstore 发布/复用回归通过；未访问真实桶 |
| #2110 TC 私有盘残留 | 一个 TC 删掉共享 artifact 行后，其他 TC 私有盘无法从 DB 扫到文件 | 本轮新增每副本独立本地 sweep，位于共享锁之外；executor idle gate、完整发布 marker、最终 DB 引用复查 | 孤儿删除、DB 失败关闭、活跃构建、marker、近期目录、symlink 与锁内复查专项通过；真实多副本/共享盘滚动升级验收待执行 |
| #2095 暖 `tpl commit` 跨节点 | origin-only 注册未接通暖包远端状态、导入与模板调度 | 本轮只修 expected>=ready、未尝试 peer 不记 FAILED、origin phase/job 来源；image backfill 纳入 PARTIALLY_READY 并明确拒绝 COMMIT | 账本与防冷启动退化测试通过；完整暖模板跨节点能力仍未接通，不能关闭该 issue |
| 其他存储与 `jfs` | 前次上游检查提到 jfs 支持范围；当前工作分支 Master/Cubelet CoW 链路只支持 xfs/s3 | 未擅自把 jfs/fs/未知字符串映射为运行时 S3；COS/MinIO 仅在 CLI 作为兼容别名 | 其他 S3-compatible 对象存储使用既有 s3 驱动与部署配置；fs 制品存储和运行时 CoW 是独立维度。新增 native 后端、多存储 profile 或 jfs 需完整实现与验收 |

GitHub 信息沿用前次核对的 master `434d6d2c` 与 issue 快照，不等同于发布状态。本轮工作区基于旧功能分支，未自动 rebase/merge 最新 master，避免覆盖既有改动。集成 PR 前必须重新核对上游冲突及功能重叠。

### 0.2 本轮代码落点

- `CubeAPI/src/services/templates.rs`：先 trim 再判空；覆盖空串、空格、制表/换行。
- `CubeMaster/pkg/templatecenter/artifact_lifecycle.go`：终态清理释放唯一 reuse key；退休记录不因残留引用恢复 READY。
- `CubeMaster/pkg/templatecenter/remote_build_resume.go`：补偿历史终态占位；BUILT 阻塞原因持久化；恢复分发时清空诊断。
- `CubeMaster/pkg/templatecenter/template_commit.go`：origin-only 暖提交的真实计数和副本来源。
- `CubeMaster/pkg/templatecenter/replica_backfill_reconciler.go`：部分就绪 image-backed 模板纳入扫描；按真正最新 job 判定来源（不预先滤掉 COMMIT），暖 COMMIT 不得回退到历史 CREATE/REDO 的冷 rootfs 分发。
- `CubeTemplateCenter/pkg/build/deleter.go`：TC 源删除前在 artifact 行锁事务内复查全部 definition/replica/BUILT job/snapshot 引用和 node placements，不能绕过 Master 保护。`deleter_test.go` 在真实 SQLite 下覆盖每类引用与无引用成功清理；`go.mod/go.sum` 仅增加所需测试依赖。
- `CubeTemplateCenter/pkg/build/build.go`：版本化存储 key 复用；构建、上传、回调完整周期发布保护。
- `CubeTemplateCenter/pkg/build/local_gc.go`：保守私盘清理与发布 marker。
- `CubeTemplateCenter/pkg/build/executor.go`：`WithIdle` 同锁保护 Submit 与最终引用复查/删除。
- `CubeTemplateCenter/pkg/reconcile/reconciler.go` 和 `cmd/templatecenter/app/main.go`：每副本独立 sweep 接入，共享 sweep 失败不阻断本地 sweep。
- 对应 `artifact_lifecycle_test.go`、`template_commit_test.go`、`replica_backfill_reconciler_test.go`、`build_test.go`、`local_gc_test.go`：专项回归。

### 0.3 fingerprint 退休协议

现有 SQL schema 是 `template_spec_fingerprint NOT NULL UNIQUE`，不能直接设置 NULL，也不能让多个旧记录共用空串。

- 对 FAILED / ORPHANED / CLEANUP_PENDING / DELETING 的旧 artifact，以状态与旧 fingerprint 为 CAS 条件改为 `retired:<SHA256(artifact_id)>`，长度固定且每行唯一。
- 正常最后引用释放时，状态与 fingerprint 在同一清理事务中更新；历史终态记录在新注册时补偿退休。
- 旧 artifact ID、状态、ObjectKey、StorageRef、Ext4Path、placements 等清理线索不删除，新构建使用新 UUID 正常占用原 fingerprint。
- READY / BUILDING 不退休；无法取得安全状态时保留 BUILT 重试并暴露具体原因。
- 原始 fingerprint 的构建溯源仍可从 image job 与构建结果取得；退休 artifact 表字段此后是不可复用占位 key，不再是原始构建 fingerprint。
- 多数据库并发、遗留软删除 READY 记录、READY 但对象确实丢失的 phantom winner，仍需独立验收/修复，不能把本补丁描述为任意异常均已收敛。

### 0.4 本地 GC 的删除边界

只扫描管理 root 下单层合法 `rfs-*` 目录，目录年龄至少两小时。实际删除前，在本副本 executor 的 idle 临界区里重新验证：

1. 任意 artifact 行（含软删除）存在则保留。
2. 任意 definition、replica、snapshot 或 node placement 仍引用则保留。
3. PENDING / RUNNING / BUILT job 引用则保留；尚未分配 artifact ID 的活跃构建也阻止扫描删除。
4. mkfs marker 或完整发布 marker 仍活跃则保留；任意 DB/目录/marker 读取错误关闭删除。
5. 拒绝 root 本身、符号链接目录、非法 ID 或改变过的 root。

本地 sweep 不写共享 DB、不删 S3、不发 node RPC；共享删除仍由既有三阶段清理处理。发布 marker 覆盖 BuildExt4 后的上传/回调，并周期续 mtime。持续构建的副本会保守延迟本地 GC。仍有数据库历史记录的目录不会被本补丁主动裁剪。

### 0.5 #2095 必须另做的暖状态能力

不能用“查询 PARTIALLY_READY 后从祖先 ext4 再建一次”代替暖提交：那会丢掉运行时内存和 rootfs 变更。

- S3 已有 rootfs + memory + metadata UUID、远端上传状态、兼容性判定和 sandbox 私有 import 路径；当前普通 COMMIT 模板未把这些控制面事实接通。
- 需要持久关联暖包、origin/版本/CPU/kernel、上传 DONE 状态、模板恢复入口及删除所有权。
- XFS 目前没有等价远端完整暖包传输，不能声明跨节点已支持。
- 必须做双节点真实 S3 验收：内存 marker、rootfs 修改、源节点退役、导出未完成/失败、缺盘、二次增量提交、并发销毁与重启恢复；导入盘属于 sandbox，不能把它写成永久模板 READY 副本。

### 0.6 CLI 后端与大列表使用方式

必须使用本分支重新编译的 CLI 和 Master；只更新 CLI 不会让旧服务端获得 DB 分页/skip_total 能力。

```bash
# 在 Linux 仓库根目录编译，输出到临时目录；不自动替换线上程序
(cd CubeMaster && go build -o /tmp/cubemastercli ./cmd/cubemastercli)

# 检查参数，不创建模板
/tmp/cubemastercli tpl create --help
/tmp/cubemastercli tpl build --help
/tmp/cubemastercli tpl list --help

# 从已有请求 JSON 创建；不传 --backend 时保留 JSON 的已有选择
/tmp/cubemastercli tpl create --file /path/to/request.json --backend s3

# OCI 镜像创建；替换真实镜像并确认节点配置后再执行
/tmp/cubemastercli tpl build --image registry.example.com/app:tag --writable-layer-size 20Gi --backend cos --detach

# 单页：缺省 50，服务端最多 200；下一页 offset=50
/tmp/cubemastercli tpl list --limit 50 --offset 0
/tmp/cubemastercli tpl list --limit 50 --offset 50

# 大数据推荐：不执行精确 COUNT，通过 has_more 提示翻页
/tmp/cubemastercli tpl list --limit 50 --offset 0 --no-total
/tmp/cubemastercli tpl list --limit 50 --offset 50 --no-total

# 表格遍历：每页直接输出、默认跳过 COUNT，共享 2 分钟预算，可 Ctrl+C
/tmp/cubemastercli --timeout 2m tpl list --all
```

CLI 的 `--backend cos` / `--backend minio` 规范化为运行时 `s3`，使用 Cubelet 已启用的 S3 CoW 存储，不会自动设置 COS endpoint、凭证或 bucket；两者不是不同的原生 CoW driver。模板制品位置由 Master/TC 的 `CUBE_ARTIFACT_STORE_BACKEND` 与 `CUBE_S3_*`/fs 配置独立决定。COS 制品使用 s3 driver + COS S3-compatible HTTPS endpoint。若要求每个模板同时从多个 bucket/profile 中选择，当前补丁没有实现此协议，不能用 alias 冒充。

列表返回默认精确 `total`，保持兼容。`skip_total=true` 时 `total=-1` 表示未计数，`has_more` 由最多 LIMIT+1 个 ID 计算。普通 `tpl list --all` 不保存全部条目；`--all --json` 为保持单个 JSON 响应格式仍聚合全部条目，可能耗内存，不推荐用于海量结果。遍历按实际返回条数推进，服务端不推进/重复/空页却 has_more=true 则报错停止；请求总预算不再每页重置。表格逐页 flush 可能使不同页的列宽略有不同。

OFFSET 分页不是跨请求一致性快照；并发更新可能重复/跳项。LIMIT 限制的是返回条数，不意味着数据库永不遍历/排序全部可见 ID。实际慢 SQL、索引和深页性能仍需在生产数据库用 EXPLAIN 核对。本轮已把有 definition 的历史 job 排除在全局窗口计算之外，不宣称任意规模下“永不卡”。

### 0.7 验证方式与实际结果

本轮专项验证使用 Go 1.25.7 自动工具链。Linux 用例是在本地交叉编译后到 34 执行测试二进制，数据来自 testing 临时目录/SQLite/模拟引用，不是生产桶数据。

```bash
# 在仓库根目录执行；macOS 可运行这些无 gomonkey 依赖的专项
(cd CubeMaster && go test ./pkg/templatecenter -count=1 \
  -run '^(TestRegisterRemoteBuiltArtifactAfterTerminalPredecessor|TestRetireArtifactFingerprint|TestRemoteBuildDeferredDiagnosis|TestCommitReplicaSummary|TestListBackfillDefinitions|TestBackfillRejectsWarmCommit|TestEnsureTemplateDefinition|TestCleanupArtifact)')

(cd CubeTemplateCenter && go test -race ./pkg/reconcile ./pkg/s3store -count=1)

# CLI 包全套，以及新增命令/分页的 race 专项
(cd CubeMaster && go test ./cmd/cubemastercli/commands/cubebox -count=1)
(cd CubeMaster && go test -race ./cmd/cubemastercli/commands/cubebox -count=1 \
  -run '^(TestTemplateBackendCLIRequest|TestApplyTemplateBackendFlag|TestWalkTemplatePages|TestStreamTemplatePages|TestTemplateListQuery)')
(cd CubeMaster && go test ./pkg/templatecenter -count=1 \
  -run '^(TestListTemplates|TestTemplateInfoFromJobBackend)')
(cd CubeMaster && go test ./pkg/service/httpservice/cube -count=1 \
  -run '^(TestListTemplatesLightweightPaging|TestListTemplatesRejectsInvalidPagingParams|TestListTemplatesPaginationAndBatchOrigin|TestListTemplatesWithoutPagingParamsUsesFiniteDefault)$')

# 在 Linux 的仓库根目录执行以下 TC 专项
(cd CubeTemplateCenter && go test ./pkg/build -count=1 \
  -run '^(TestReusableArtifactData|TestLocalArtifactSweep|TestArtifactPublicationMarker|TestExecutorWithIdle|TestArtifactDataExists|TestDeleteLocalExt4|TestManagedArtifactDir|TestDeleter)')

# 格式/空白验证
git diff --check
```

- CLI 包全套、新增分页/后端 race、SQLite 数据库分页和 HTTP 轻量参数专项通过；新 CLI Linux 编译与 34 上帮助/测试二进制验证通过，未向线上 Master 发送创建模板请求。
- Master 定义同步/引用保护、终态注册/重复回调、诊断、暖提交账本及 image backfill 专项通过。
- TC build 指定专项在 34 Linux 通过；TC 程序 Linux 交叉编译通过。
- TC reconcile/s3store race 回归通过（macOS linker 有 warning，不影响退出码）。
- 本机无 cargo/rustfmt，Rust 编译与格式验证未执行。
- Master 全套 macOS 测试先前遇到 gomonkey 修改可执行内存失败；不能算全套通过。
- 多副本真实 MySQL/PostgreSQL、共享存储与真实暖状态跨节点集成测试尚未完成。不执行提交、推送或线上部署。

## 1. 需求总览与边界（原始设计）

| 编号 | 需求 | 完成标准 |
|---|---|---|
| R1 | 所有模板 ext4 记录稳定远端信息，Cubelet 直连 S3/MinIO 或 TC 下载 | 新链路的 ext4 字节不经过 CubeMaster；本地盘、CBS、CFS 由 TC HTTP 服务提供 |
| R2 | 下载授权由 CubeMaster 签发，TTL 默认 1h，可配置为 2h 等合法时长 | 不再使用长期 artifact token 或默认七天签名；支持过期重新授权与有界重试 |
| R3 | `cubemastercli tpl ls/list` 改为真正数据库分页 | 查询与关联聚合均有边界，CLI 默认有限页，不再全表加载后切片 |
| R4 | 修复已有 template rootfs 导致创建/redo 不可重试 | 区分完整对象、半成品、引用和并发；安全复用或新 generation 发布，不盲删 |
| R5 | 修复退役节点副本阻塞模板/快照删除及虚假 READY（#1965） | 永久退役与暂时不可达可区分；当前地址清理；部分失败可收敛；状态真实 |

### 1.1 必须区分的两个 backend

- 创建模板的 `backend=xfs|s3` 是沙箱 CoW 后端，决定运行、暂停及快照的数据存储行为。
- 本文的 artifact 存储是构建出来的 ext4 放在哪里：已有 `RootfsArtifact.StorageBackend` 使用 `s3`、`fs` 或空值（历史记录）。
- 本地盘、CBS、CFS 都可以作为 TC 的文件存储介质，统一通过 TC 下载；不要把它们重新塞进 CoW backend 枚举。
- S3/MinIO 的 artifact 存储已配置，不代表节点 CubeS3lvol 已启用；这不是同一个配置。

### 1.2 明确不做

- 不新增 `proxy/direct/auto`、`CUBE_ARTIFACT_DOWNLOAD_REDIRECT` 或 `redirect=1` 之类行为开关。
- 不在直连失败后静默回落为 Master 中转，也不把 S3 上传失败伪装为本地存储成功。
- 不向 Cubelet 下发 S3 AK/SK、Master↔TC 构建/删除共享密钥。
- 不要求 Cubelet 挂载 CBS/CFS，也不直接暴露 TC 文件系统路径。
- 不通过改双 `tpl-` 前缀、全量删除同名对象、手工删 DB 行来替代幂等和引用一致性修复。
- 不新增独立 prune 命令或 force 开关来绕过正常删除流程。
- `BUILDING_EXT4` 长步骤心跳问题是关联风险，不属于本次五项需求，不能宣称已被直连下载解决。

## 2. 已核实的现状与交接风险

### 2.1 下载与模型

- 当前下载入口在 `CubeMaster/pkg/service/httpservice/cube/template_from_image.go`。
- 本地文件分支由 Master 开文件后 `http.ServeContent` 返回；不是 Master 从 TC 拉流再转发。现有部署依赖 Master/TC 共享 artifact 目录。
- 对象存储分支仍可能由 Master 代理 HTTP 流或调用 blobstore 读取后返回，造成控制面承担大文件传输。
- `CubeMaster/pkg/base/db/models/template_image.go` 的 `RootfsArtifact` 已有 `StorageBackend`、`ObjectKey`、`ArtifactURL`、`DownloadToken`、大小和 SHA256 字段。应增量扩展，不能重复新增同义模型。
- `ArtifactURL` 历史上保存构建时的 presigned URL；`artifact_url_refresh.go` 提供签名刷新及 blobstore 访问。
- `Cubelet/internal/cube/server/images/ext4image/utils.go` 已发送 `X-Cube-Artifact-Token`，但 `rewriteDownloadHost` 当前会改写非 S3 签名 URL 的 host；TC URL 若沿用该逻辑，会被错误改回 Master。

### 2.2 列表并非完全没有分页

- CLI `TemplateListCommand` 已支持 `list`/`ls`、`--limit`、`--offset`，但默认 `limit=0` 表示全量。
- HTTP 已接受分页参数；`ListTemplatesWithOptions` 实际先读全量缓存或 DB，再执行 `sliceTemplateInfos`。
- 冷缓存的 `listTemplatesFromDB` 全量读取 definitions、CREATE/REDO 历史 jobs、隐藏快照关系，合并后再缓存。
- 返回集合还包括没有 definition 的 job-only 模板。只给 definition 查询加 Limit 会漏掉这些条目，并导致 total、排序不一致。
- HTTP 副本来源查询已有批量查询实现，不应未经证据将问题归因于 N+1 或全量 Preload。

### 2.3 rootfs 与删除

- `cowTemplateRootfsName` 使用 `tpl-%s-rootfs`，template ID 自带 `tpl-` 时形成双前缀；这不等于冲突根因。
- `CommitTemplateRootfs` 对已存在 snapshot 主动拒绝，已有测试断言不能覆盖删除旧对象；需保留其保护有效数据的意图。
- 当前模板/artifact 清理已有三阶段与 `CLEANUP_PENDING`，不是从零建立 GC。
- 已读取路径仍有优先使用历史 NodeIP 的逻辑；不能假定已实现退役节点过滤和当前地址优先。

### 2.4 分支上有未完成且不符合最终要求的改动

核实工作区有两个未提交修改文件：

- `CubeMaster/pkg/service/httpservice/cube/template_from_image.go`。
- `CubeMaster/pkg/templatecenter/artifact_url_refresh.go`。

草稿新增 S3 重定向、`CUBE_ARTIFACT_DOWNLOAD_REDIRECT`、`redirect=1` 和代理回落。它既未覆盖 TC 直连，也未实现短 TTL 与重新授权。实施者应有针对性地替换这些草稿，不做整仓 reset，不丢弃其他用户改动；保留原分支的 backend API 工作。

## 3. R1：ext4 数据流与控制流分离

### 3.1 目标链路

| 存储场景 | 目标数据路径 | 部署及定位要求 |
|---|---|---|
| TC 本地盘 | `Cubelet → 持有 artifact 的 TC HTTP 下载接口` | 记录实例/存储身份；路由到确实持有文件的 TC |
| TC 使用 CBS | `Cubelet → 挂载 CBS 的 TC HTTP 下载接口` | 不多节点直接挂载普通 ext4；重挂载后更新 TC 服务位置 |
| TC 使用 CFS/NFS | `Cubelet → 可读取该共享存储的 TC HTTP 下载接口` | 相同 storage_ref 的健康 TC 可提供读取，无需 Cubelet 挂载共享盘 |
| S3/MinIO | `Cubelet → 对象存储签名 URL` | endpoint 对节点可达，TLS 证书可信；不能改写已签名 host |

CubeMaster 只负责节点鉴权、artifact 状态/权限校验、定位解析、临时授权及分发编排。TC HTTP 直连的收益是剥离 Master 的长连接和字节搬运，不是承诺减少同机场景网络跳数或提高磁盘上限。

TC 构建与下载仍可能竞争 CPU/磁盘/网卡；需下载并发限额、流式读取和资源隔离。CFS 可以按共享存储扩展下载实例，本地盘/CBS 不能随意加无数据副本后负载均衡。

### 3.2 稳定定位数据

推荐语义如下；字段名为建议，落地时结合已有模型和迁移规范确定：

| 字段/实体 | 含义 |
|---|---|
| `storage_backend` | 复用已有 artifact 类型 `s3`/`fs`；历史空值做明确兼容解析 |
| `storage_ref` | 存储实例或配置标识，解决多存储、配置变化和跨实例定位；不是凭证 |
| `object_key` | 复用已有稳定对象 key；TC 文件也使用受控相对 key |
| `bucket` / endpoint 引用 | S3 对象所在桶与节点可达 endpoint；关联稳定存储描述，不只依赖进程当前全局配置 |
| `owner_tc_id` | 本地盘/CBS artifact 的持有者身份；共享文件存储按 storage_ref 解析可读 TC |
| TC 实例注册记录 | 实例 ID、节点可达下载地址、storage_ref、健康/租约状态；地址可更新 |
| 大小、SHA256、generation | 产物完整性及不可变版本；复用已有完整性字段 |

DB 记录实际已发布的远端事实。临时 URL、token、expires_at 是签发结果，不是稳定定位来源；不把 AK/SK 或私钥写进 artifact 行。

已有 `Ext4Path` 可继续作为 TC 内部文件定位或历史兼容字段，但不得直接作为 Cubelet 下载地址。不要继续用 `MasterNodeIP` 冒充 TC 持有者。

### 3.3 构建发布与位置变化

1. TC 在临时目录构建、校验大小/SHA256，产物以不可变 generation 发布。
2. 文件存储采用同文件系统原子发布；S3 上传成功并确认对象可用后发布定位元数据。
3. TC 上报 READY/BUILT 的同时提交 storage_ref、object key、持有者及完整性信息；不可先把未发布产物标 READY。
4. TC 重启/CBS 重挂载后，健康注册更新地址，Master 从存储关系解析当前位置，不重复修改 artifact 的不可变内容身份。
5. 本地盘持有者永久丢失且无副本时，明确产物不可用并重建；不能随机换一个无文件 TC。
6. 选定远端存储的上传/发布失败时，明确失败，不静默转换另一种介质。

### 3.4 Master 下载授权接口（建议协议，尚未实现）

建议使用已鉴权的内部 `POST /internal/template/artifacts/:artifact_id/download-ticket`，或等价的现有节点 RPC。最终接入点需核实并复用当前节点身份体系，不能用知道 artifact_id 就能匿名取票的接口。

请求：artifact ID、预期 generation、下载用途；node_id 必须来自可信调用身份，不相信 body 自报身份。

响应：

| 内容 | 说明 |
|---|---|
| artifact ID / generation | 下载期间绑定同一份不可变产物 |
| source type | `tc` 或 `s3`，用于明确决定目标请求的鉴权方式 |
| URL | TC 只读接口地址或 S3 presigned GET URL |
| TC ticket | 仅 TC 下载时提供；S3 使用 URL 自身签名 |
| expires_at / server_time | 实际有效期和签发时间 |
| SHA256 / size | 端到端完整性与长度约束 |

创建/redo 分发优先由 Master 下发以上结构；过期时 Cubelet 用节点身份重新申请。普通模板列表/详情不能顺带暴露 ticket、签名 URL 或 DownloadToken。

### 3.5 TC 只读接口

建议 `GET/HEAD /tc/api/v1/artifacts/:artifact_id/download`：

- ticket 校验失败返回 401/403，不采用 HTTP 200 包裹"下载失败"造成 Cubelet 写入错误文件。
- 只访问 READY 的指定 generation；拒绝已撤销或待清理产物。
- 支持 Range、If-Range、206/416、HEAD；正确返回长度与强 ETag，完整性 SHA256 与 ETag 语义清晰。
- artifact ID/key 从可信元数据解析；禁止任意路径、目录遍历、符号链接逃逸。
- 不开放目录列表、写入、构建、上传或删除权限；内部控制 API 仍使用独立鉴权。
- 下载入口需 TLS/可信内网入口、网络白名单；只开放下载路由，不因下载把全部内部 API 暴露给计算节点。
- 流式发送、不读完整文件入内存；连接头/空闲超时、并发与带宽资源有边界，避免健康大文件被短总超时截断。
- 同机 one-click 可使用部署生成的可达地址；Kubernetes 可用 Service/内网 LB，但 ClusterIP 是否对 Cubelet 网络可达必须实际验证，不能直接当作默认事实。

### 3.6 Cubelet 改造

- 新协议按 source type 直接请求 TC/S3，不能对所有非 SigV4 URL 统一改写 host。
- 新 TC URL 从授权结果使用原始地址；历史 Master URL 的 host rewrite 仅适用于识别出的旧下载协议/路径。
- TC 请求带 `X-Cube-Artifact-Token`；S3 请求不带 TC ticket、Master 授权 header。
- 优先显式取票后直接 GET，而非依赖跨 host 自动重定向；历史入口兼容时必须控制 header 转发，防止凭证泄漏。
- 写临时文件，校验大小/SHA256 后原子替换；并发下载复用节点内去重机制。
- 弱网重试有指数退避、最大次数和整体任务预算；不能无限循环。
- 续传校验 generation/ETag 与 Content-Range。远端忽略 Range 返回 200 时重新开始，不把完整响应追加到半文件；generation 改变不得拼接旧数据。
- S3 GET 签名不能拿去 HEAD。以授权元数据提供大小/SHA256，必要时 Range GET 探测，或扩展独立 HEAD 签名能力；不得直接重定向 HEAD 到 GET 签名。

## 4. R2：短期下载授权

### 4.1 TTL 配置

建议统一配置名 `CUBE_ARTIFACT_DOWNLOAD_TOKEN_TTL`，默认 `1h`，支持 `2h` 及满足后端限制的合法正 duration。此名称为新方案建议，并非现有配置。

- Master 签发时统一读取和验证；非法、零或负值启动/配置加载时报错，不能静默永久有效。
- S3 签名传入相同 TTL，但临时 STS 凭证可能更早到期；返回实际可保证的 expires_at，超过协议/凭证限制明确处理，不能谎报有效期。
- TC 校验票内 expires_at，不根据自身默认 TTL 再延长；容忍小范围时钟偏差，节点/服务端保持时钟同步。
- 数据定位与凭证寿命分离；不周期性给 DB 全表重写 token，不继续签默认七天下载授权。

### 4.2 TC ticket 设计

推荐复用现有可用签名设施；若无合适实现，采用标准库/成熟库的非对称签名票据（例如 Ed25519），Master 持有私钥，TC 只持有验签公钥。不要自创加密算法或把构建共享密钥复用为节点下载权限。

必要 claims：artifact_id、generation、storage_ref、audience、允许 GET/HEAD、iat、exp、kid；jti 可用于审计/撤销。若绑定 node_id，要以 mTLS/可信入口等校验真实节点身份，仅写 claim 不能证明请求者就是该节点。

- key 由部署 Secret 管理；多副本 Master 使用一致签发体系，TC 公钥分发可信。
- 轮换期间保留旧公钥至其票据 TTL 到期；私钥/票据不能出现在日志或普通 API 响应。
- ticket 是短期 bearer 授权，可在有效期内用于重试/Range，不称为"一次使用即失效"。
- 取票时验证节点是否仍被允许分发该 artifact；TC 每次读请求验证产物生命周期，删除/撤销能阻止后续新读取。
- 健康传输开始时校验到期时间，不因流式下载跨过 exp 主动截断；重连/续传需要有效新票。
- 活跃下载与物理 GC 之间要有安全保护，避免边下载边删造成平台差异或失败。

### 4.3 重新授权与错误分类

| 错误 | 行为 |
|---|---|
| TC 明确 ticket 过期 / S3 明确签名过期 | 向 Master 重新取票，在整体预算内续传 |
| 未授权、票据篡改、节点已退役 | 终止；不能靠不停取票绕过权限 |
| 普通 S3 403 | 区分时钟偏差、签名/region/凭证错误；不能一律当过期 |
| 网络超时、连接断开、可重试 5xx | 同一 generation 下有界重试 |
| 404、产物已撤销、generation 冲突 | 明确报错；由控制面确认缺失/重建，不静默切换来源 |
| 长度或 SHA256 不匹配 | 丢弃不可信半成品并报完整性错误，重试有上限 |

日志和 trace 仅记录 artifact/node ID、generation、source、耗时、重试数及错误类别；TC header 和 S3 query 签名都脱敏。标准 presigned URL 仍是敏感 bearer 凭证，不是"query 符合规范就不存在泄漏风险"。

## 5. R3：模板列表数据库分页

### 5.1 修复核心

不是再加一遍 `--limit/--offset`，而是把 `ListTemplatesWithOptions → 全量缓存/全量 DB → 内存切片` 换成有限查询，并保留历史列表的资源语义。

### 5.2 查询设计

1. 先定义统一可见模板集合：definition 为主，加入尚无 definition 的 CREATE/REDO job-only 模板；每个 template_id 只出现一次。
2. 删除/删除中的隐藏快照、pause 内部产物、权限/租户过滤在分页和 count 之前完成，不能分页后在 Go 中过滤导致短页/错误 total。
3. 使用统一排序字段及 template_id 唯一 tie-breaker。可优先保留 `updated_at DESC, template_id ASC`，但 definition/job-only 必须用同一规则。
4. DB 对集合做 Limit/Offset；通过兼容 MySQL/PostgreSQL 的 UNION/子查询/窗口或等价方案实现，按支持的数据库版本选择，不硬编码单一方言。
5. 只按本页 template IDs 聚合关联数据。最新 CREATE/REDO job 在 DB 按 attempt_no DESC、id DESC 选一条，不能读取每个模板全部历史 jobs 后再挑。
6. 精简 Select，仅读取显示必需字段；若 image_info 需从 JSON 提取，限制为本页，不加载全表大 ResultJSON/RequestJSON。
7. 复用已有批量副本查询并应用 R5 的同一可用性投影；节点注册表不可用时明确状态未知，不伪装所有节点退役。
8. total 为同一可见集合的 COUNT，不靠 len(全量列表)。为关联与排序条件补索引，并用 EXPLAIN 验证。
9. 移除列表请求对全量聚合缓存的依赖；如需要缓存，按查询/页粒度限容，并检查后台刷新是否仍周期性全表加载。

### 5.3 CLI / API 约定

- `tpl ls` 与 `tpl list` 共用逻辑；建议默认 `--limit 50 --offset 0`，最大页大小 200，最终常量与现有 API 约定统一。
- API 缺省及 limit=0 也必须有限返回；CLI 对负数、非法值不能悄悄省略参数退化为全量。
- 保留已有 data/total 响应字段，新增 limit、offset、has_more 等信息；打印翻页提示。
- 如提供 `--all`，它是显式 CLI 便利功能，不是下载模式开关。按页读取并立即输出；JSON 使用流式序列化或明确 NDJSON 格式，不能累积全部响应。
- 默认由全量变分页是可见行为变化。更新 CubeAPI/SDK/内部调用方，避免旧消费者误把第一页当全集；不能以兼容为理由保留无上限全量入口。
- 稳定排序保证静态数据翻页一致；并发更新时 Offset 分页本身可能漂移，不能承诺跨请求强一致。需要无重复遍历时，配套 keyset cursor/固定读取快照，不能只宣称 ORDER BY 即可。
