# 模板可靠性、存储后端与大列表：整体测试指南

> 更新日期：2026-11-05。协议与测试入口已按源码 `3cbc88c1` 核对；执行时必须固定交付 SHA。
> 本文是测试流程，不是集群验收通过报告。
> 当前有两个隔离反例 FAIL 及未入库的验收脚本引用；不得直接进入发布。下表“已有证据”包含历史结果，不代表本次重跑全部通过。
> 已有结果见 [修复报告第 0.7 节](template-reliability-fix-plan.md#07-验证方式与实际结果)；详细集群操作见 [模板/S3 复现手册](../zh/dev/template-s3-reproduction-runbook.md)。修复报告第 1 节以后保留原始设计，不代表当前实现。

## 1. 范围、状态与通过标准

| 范围 | 已有证据 | 本次还需要验收 |
|---|---|---|
| CLI `backend`、`build` 别名 | CLI 整包和新增 backend/pages race 专项通过 | 真实 `xfs`、`s3`、COS/MinIO 配置下创建并运行沙箱 |
| DB/HTTP/CLI 分页 | SQLite 页边界、250 definitions + 2000 jobs、无 COUNT、HTTP 透传、逐页输出和共享超时专项通过 | MySQL/PG 查询计划、深页、首屏耗时、内存、并发更新 |
| fingerprint 重试、BUILT 诊断 | 终态前驱注册、重复回调、诊断和定义替换专项通过 | 双 Master 注册争用、真实数据库事务与失败恢复 |
| TC 对象复用、删除与私盘 GC | Linux 临时目录/SQLite/模拟引用专项通过；reconcile/s3store race 通过 | 真实 S3/COS、双 TC 私盘/共享盘、滚动重启和故障闭环 |
| ext4 直连与短期授权 | 分支已有实现；本指南核对了 Master/TC/Cubelet 专项入口 | 本指南列出的下载专项重跑及真实字节路径、续传与授权负例 |
| 暖 COMMIT | origin 账本、PARTIALLY_READY backfill、防冷镜像退化专项通过 | origin 真实恢复验证；完整暖跨节点仍未接通，不得关闭 #2095 |
| 全套/Rust | Master macOS 全套曾受 gomonkey 可执行内存修改限制；本机缺 cargo/rustfmt | Linux 全包门禁、Rust 编译/测试/格式检查 |

结果只使用以下分类：

- **PASS**：命令成功，目标测试确实运行，业务断言和取证一致。
- **FAIL**：测试/业务断言不符，或出现误删、授权泄露、校验失败仍发布等安全问题。
- **SKIP**：缺工具、权限、存储/节点或测试主动跳过；不计入通过。
- **NOT_RUN**：尚未执行或故障未触发；不计入通过。
- **BLOCKED**：依赖尚未实现的能力，例如普通暖 COMMIT 跨节点。不能用冷重建替代通过。

Go 编译成功、`[no tests to run]`、仅 READY 状态、只有模拟 S3 签名，均不等于整体测试通过。历史专项结果不能替代本次部署版本的验收。

## 2. 环境与安全准备

### 2.1 分层环境

| 层级 | 环境要求 | 用途 |
|---|---|---|
| L0 专项回归 | Go 1.25.7；建议 Linux；Python 3；race 另需 CGO/C 编译器 | SQLite、临时目录、httptest，不连接业务 DB/桶 |
| L1 构建及全包门禁 | 仓库 builder/Docker；Cubelet 所需 cubecow SDK、BPF/proto 生成产物 | Linux 编译、现有单测门禁 |
| L2 正常集成 | 隔离 Master/TC、可用 CubeAPI/CubeOps、两个专用计算节点 A/B、测试数据库与存储 | 创建、分发、运行、直下载 |
| L3 并发/故障 | 两个 Master、两个 TC；独立 MySQL/PG；TC 私盘和共享盘分别验证 | CAS、引用删除、重启收敛、性能 |

三个 Go 模块均声明 `go 1.25.7`。不要在 macOS 上直接运行固定 `GOOS=linux` 的模块 `make test`；交叉编译的测试二进制只能到匹配的 Linux 架构执行。`-race` 不能用 `CGO_ENABLED=0` 替代。

### 2.2 必须确认的安全边界

1. 使用独立测试 DB、bucket/prefix、TC 管理根目录和专用节点，先登记负责人及资源清单。
2. 单测终端不得继承生产连接配置；集成凭证只通过受控环境/Secret 注入，不写进请求示例、报告或 Git。
3. 不关闭鉴权，不使用跳过 TLS 校验来掩盖错误；计算节点 endpoint 必须实际可达。
4. 节点退役、断网、暂停服务、重启和磁盘故障只在确认隔离的环境执行。本文不提供自动操作线上服务或设备的脚本。
5. 不手删业务对象/DB 行、不覆盖同 ID 数据、不为制造冷下载清除活跃缓存；优先使用新计算节点。
6. 证据目录仅测试人员可读。URL 签名、ticket/refresh token、AK/SK、请求中的敏感环境变量必须脱敏；不打印完整服务环境或 Secret。
7. 不删除 `.codebuddy`，不 reset/rebase 现有工作区，不自动提交、推送、部署。

### 2.3 版本记录与执行终端

下面代码块按顺序在**同一个 Bash 会话**执行；macOS/Zsh 用户可先进入 `bash`。命令仅供执行流程使用，编写本文时没有执行它们。

在仓库根目录初始化；证据目录将创建在仓库下：

```bash
set -euo pipefail
umask 077
export ROOT="$(git rev-parse --show-toplevel)"
export RUN_ID="$(date -u +%Y%m%d-%H%M%S)"
export EVIDENCE="$(mktemp -d "$ROOT/template-test-${RUN_ID}.XXXXXX")"
git -C "$ROOT" rev-parse HEAD > "$EVIDENCE/head.txt"
git -C "$ROOT" status --short > "$EVIDENCE/worktree.txt"
git -C "$ROOT" diff --stat > "$EVIDENCE/diff-stat.txt"
go version > "$EVIDENCE/go-version.txt"
go env GOOS GOARCH CGO_ENABLED GOTOOLCHAIN > "$EVIDENCE/go-env.txt"
uname -a > "$EVIDENCE/platform.txt"
```

HEAD 不能代表未提交补丁。另以受控方式保存改动快照和二进制校验值（Linux `sha256sum` / macOS `shasum -a 256`），记录实际 Master、TC、Cubelet、Shim/VMM、S3lvol、SDK 版本与 OCI image digest。`git diff` 不包含未跟踪文件，若它们参与构建必须另行登记；不要将含密钥的完整 diff 上传。

## 3. 专项测试与日志保存

### 3.1 统一执行器

以下函数保留 `go test` 的真实退出码，并检查实际运行/跳过事件，防止错误正则产生假 PASS。参数依次为模块、证据标签、包、测试正则、可选模式 `race`；正则 `-` 表示整包。返回非零后应先定位失败，不继续集成验收。主动跳过会单独报告为 SKIP。

```bash
run_go() {
  local module="$1" label="$2" pkg="$3" pattern="$4" mode="${5:-normal}" rc=0
  local args=(-json -count=1 -timeout=20m -gcflags=all=-l)
  if [ "$pattern" != '-' ]; then args+=(-run "$pattern"); fi
  if [ "$mode" = race ]; then args+=(-race); fi
  printf '%s\n' "module=$module package=$pkg pattern=$pattern mode=$mode" > "$EVIDENCE/$label.command.txt"
  (cd "$ROOT/$module" && go test "${args[@]}" "$pkg") \
    2> "$EVIDENCE/$label.stderr.log" | tee "$EVIDENCE/$label.jsonl" || rc=$?
  printf '%s\n' "$rc" > "$EVIDENCE/$label.exit-code.txt"
  if [ "$rc" -ne 0 ]; then return "$rc"; fi
  python3 - "$EVIDENCE/$label.jsonl" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as stream:
    events = [json.loads(line) for line in stream if line.strip()]
runs = [e for e in events if e.get("Action") == "run" and e.get("Test")]
skips = [e for e in events if e.get("Action") == "skip"]
fails = [e for e in events if e.get("Action") == "fail"]
if fails or not runs:
    print("FAIL: failed events or no matching test executed")
    sys.exit(1)
if skips:
    print("SKIP:", [(e.get("Package"), e.get("Test")) for e in skips])
    sys.exit(2)
print("PASS: test events executed =", len(runs))
PY
}
```

执行器的 `.exit-code.txt` 记录 Go 命令退出码；Python 的无测试/SKIP 判定由终端返回码另行记录到总报告。首次依赖下载可能耗时，stderr 独立保留，不据终端暂时无 JSON 输出判定卡死。

### 3.2 Master 生命周期、定义同步、BUILT 诊断

```bash
run_go CubeMaster master-lifecycle ./pkg/templatecenter \
  '^(TestRetireArtifactFingerprint(RejectsStaleStatus)?|TestRemoteBuildDeferredDiagnosisPreservesResult|TestRegisterRemoteBuiltArtifactAfterTerminalPredecessor|TestCleanupArtifact(KeepsBuiltJobReference|RetiredReferenceDoesNotResurrect|DropsRetiredPlacement|KeepsPlacementBeforeRegistryReady)|TestEnsureTemplateDefinition(ReplacesRebuiltArtifact|SameArtifactPreservesConvergedRequest|FailedReplacementKeepsCache|RejectsUnsafeArtifactReplacement|SerializesRebuiltArtifact))$'

run_go CubeMaster master-resume-lock ./pkg/templatecenter \
  '^(TestAcquireResumeArtifactLock(RemovesEntryAfterRelease|SerializesSameArtifactID|DoesNotSerializeDifferentArtifactIDs))$' race

run_go CubeMaster master-commit-backfill ./pkg/templatecenter \
  '^(TestCommitReplicaSummary|TestListBackfillDefinitionsIncludesPartiallyReady|TestBackfillRejectsWarmCommit)$'
```

断言重点：终态旧 fingerprint 退休但保留清理线索；READY/BUILDING 不被退休；新 UUID 可注册、重复回调幂等；有效 BUILT 不因随意超时被丢弃；替换失败不破坏定义/缓存；最新 COMMIT 不能被历史 CREATE/REDO 覆盖。进程内锁测试不是双 Master 事务验收。

### 3.3 Master/HTTP/CLI 分页及 backend

```bash
run_go CubeMaster master-list ./pkg/templatecenter \
  '^(TestTemplateInfoFromJobBackend|TestListTemplates(LightweightPagination|BoundsHistoricalJobs|WithOptionsUsesDatabasePagination|WithOptionsDefaultsToFinitePage|BackCompatReturnsAll|WithOptionsIgnoresLegacyFullListCache|WithOptionsNotInitialized))$'

run_go CubeMaster master-http-list ./pkg/service/httpservice/cube \
  '^(TestListTemplates(LightweightPaging|RejectsInvalidPagingParams|PaginationAndBatchOrigin|WithoutPagingParamsUsesFiniteDefault|LimitZeroUsesFiniteDefault|BatchFallsBackWhenOriginNodeHasEmptyIP|DegradesWhenBatchReplicaLookupFails))$'

run_go CubeMaster cli-all ./cmd/cubemastercli/commands/cubebox '-'

run_go CubeMaster cli-backend-pages-race ./cmd/cubemastercli/commands/cubebox \
  '^(TestTemplateBackendCLIRequest|TestApplyTemplateBackendFlag|TestWalkTemplatePages(UsesActualPageLength|StopsWithoutProgress|SharesDeadline|RejectsShiftedRows)|TestStreamTemplatePagesFlushesBeforeNextRequest|TestTemplateListQuerySkipsTotalWhenRequested)$' race
```

断言重点：真实 CLI 解析到 HTTP 请求；非法 backend 零请求；JSON backend 不传 flag 时保留、显式 flag 时覆盖；默认有限页、最大 200、轻量模式无精确 COUNT；实际页长度推进、第一页在下一请求前输出、无进展停止、总超时不每页重置。内部 `BackCompatReturnsAll` 不表示 HTTP 默认无限返回。

### 3.4 下载票据、TC 鉴权与 Cubelet 下载

TC/Cubelet 在 Linux 执行；Cubelet 应先完成第 4 节构建前置准备。

```bash
run_go CubeMaster master-download ./pkg/templatecenter \
  '^(TestArtifactDownload(RejectsDeletedReplicaAndForgedToken|AnnotationsModeSnapshot|UsesExistingCredentialOnly)|TestBuildArtifactDownload(TicketForTemplateCenter|RefreshToken|TicketForS3UsesFreshBoundedURL|TicketDoesNotFallBackToStoredS3URL)|TestEnsureArtifactStableLocator.*)$'

run_go CubeMaster master-download-ticket-route ./pkg/service/httpservice/cube \
  '^(TestArtifactDownloadTicketRouteIssuesDirectTemplateCenterTicket|TestCubeRoutesTemplateProxyRemainder|TestTemplateCenterRoutesExcludeWritesAndCachedReads)$'

run_go CubeTemplateCenter tc-download ./pkg/httpservice \
  '^(TestArtifactDownloadHandlerOnlyUsesComponentAuthentication|TestNodeArtifactDownloadHandlerUsesPerNodeTokenNotComponentSecret|TestTicketDownloadIntegration_.*|TestDirectArtifactPathConstrainsFSObjectKey)$'

run_go Cubelet cubelet-download ./internal/cube/server/images/ext4image \
  '^(TestFetchTicketRejectsMasterProxySource|TestFetchTemplateCenterTicketDownloadsDirectly|TestArtifactDownloadResolverRejectsLegacyMasterURL|TestFetchHeaderTicketAndDownload|TestFetchTicketRejectsCredentialInjection|TestArtifactDownloadRejectsCrossOriginRedirect|TestFetchS3TicketDoesNotForwardInternalHeaders|TestFetchArtifactTicketRejects(ExpiredOrWrongIdentity|UnexpectedOrigin)|TestDownloadArtifact(RefreshesTicketAndResumes|StopsOnTicketUnauthorized|RejectsMismatchedContentRange|WithIndependentKernelSource)|TestEnsurePmemRootfsMixedEntrypointsReusePreparedFile|TestRewriteDownloadHost(LeavesS3PresignedURLUntouched|LeavesLegacyMasterDownloadRouteUntouched|RewritesTicketURL|LeavesTemplateCenterURLUntouched|EmptyEndpointPassesThrough)|TestIsS3PresignedURLDetectsSigV4Signature)$'
```

这些测试使用签名替身、SQLite/httptest，不访问真实桶。Master 只签发票据并校验节点 capability、generation 和当前 replica，不提供任何制品字节路由；TC 的节点入口校验节点票据，组件入口只校验组件凭据（仅供 Master 的可服务性 HEAD 探测）。Cubelet 只接受 `s3`/`template_center` 来源票据。TC ETag/If-Range 用例的 fixture 已补 `Ext4SHA256`；该包需在 Linux 原生运行，不能通过删除断言或排除该测试将门禁标为通过。

Cubelet 上述选择器当前匹配 17 个顶层测试。每次升级都要检查实际运行集合，不只检查“至少有一个测试”；proto 的 `artifactticket` 与 demo 还应整包执行 `-race`。

### 3.5 TC 对象复用、引用删除和本地 GC

```bash
run_go CubeTemplateCenter tc-build-gc ./pkg/build \
  '^(TestReusableArtifactDataUsesStoredVersionedKey|TestArtifactDataExists(S3ObjectConfirmed|S3ObjectMissing|S3ErrorFallsBackToLocalDisk|BackendWithoutURL|LocalOnlyArtifact)|TestLocalArtifactSweep(DurableReferences|Safety|RechecksReferencesInsideIdleGate)|TestArtifactPublicationMarkerLifecycle|TestExecutorWithIdleSkipsActiveBuild|TestDeleteLocalExt4(RemovesManagedDir|RefusesPathTraversal)|TestManagedArtifactDir|TestDeleter(PreservesEveryDurableReference|EmptyArtifactID|ValidatesStoreBeforeDeletingAnyData))$'

run_go CubeTemplateCenter tc-reconcile-race ./pkg/reconcile '-' race
run_go CubeTemplateCenter tc-s3store-race ./pkg/s3store '-' race
```

断言重点：按记录的版本化 `ObjectKey` 判断复用；删除 claim 在 artifact 行锁事务内重查持久引用和 placement；GC 只删管理 root 内过期合法孤儿，DB/marker 错误时关闭删除，活跃构建和引用保留，最终复查位于 executor idle gate。s3store 整包是 key 规则测试，不代表真实上传通过。

## 4. 静态检查、Linux 构建与全包门禁

### 4.1 静态检查

不要运行会重写文件的 `make fmt`/`make lint` 来做只读检查。先审阅变更范围，再执行：

```bash
(cd "$ROOT" && git diff --check) > "$EVIDENCE/diff-check.log" 2>&1
(cd "$ROOT/CubeMaster" && go vet ./pkg/templatecenter ./pkg/service/httpservice/cube ./cmd/cubemastercli/commands/cubebox) > "$EVIDENCE/master-vet.log" 2>&1
(cd "$ROOT/CubeTemplateCenter" && go vet ./pkg/build ./pkg/httpservice ./pkg/reconcile ./pkg/s3store) > "$EVIDENCE/tc-vet.log" 2>&1
```

`gofmt -l` 检查本次变更的 Go 文件，非空输出需核对是否本次引入；不要自动格式化不相关文件。TC vet 也应在 Linux 执行。Rust 工具齐备时，在 `CubeAPI` 执行 `cargo fmt --check`、`cargo check --locked`、`cargo test --locked` 并分别保存退出码；缺工具记录 SKIP，不补写 PASS。

### 4.2 Linux 构建

构建不等于安装。原生 Linux 已有构建前置产物时：

```bash
make -C "$ROOT/CubeMaster" build > "$EVIDENCE/master-build.log" 2>&1
make -C "$ROOT/CubeTemplateCenter" build > "$EVIDENCE/tc-build.log" 2>&1
make -C "$ROOT/Cubelet" build > "$EVIDENCE/cubelet-build.log" 2>&1
```

输出为各模块 `build/`。TC Makefile 固定 Linux/amd64，其他架构不能直接运行其产物。Cubelet 缺 cubecow SDK、BPF/proto 产物时，使用根目录 builder 入口准备，不将缺前置依赖误判为下载回归：

```bash
(cd "$ROOT" && make cubemaster) > "$EVIDENCE/builder-master.log" 2>&1
(cd "$ROOT" && make cubetemplatecenter) > "$EVIDENCE/builder-tc.log" 2>&1
(cd "$ROOT" && make cubelet) > "$EVIDENCE/builder-cubelet.log" 2>&1
```

builder 会构建/使用容器镜像、下载依赖并生成文件，产物在 `_output/bin/`；在测试 checkout 中执行。不执行 install、push 或 one-click 部署。上述两条构建路线按环境择一，不必重复。

### 4.3 全包与真实数据库辅助门禁

```bash
(cd "$ROOT" && make cubemaster-test) > "$EVIDENCE/master-unit-gate.log" 2>&1
(cd "$ROOT" && make cubetemplatecenter-test) > "$EVIDENCE/tc-unit-gate.log" 2>&1
(cd "$ROOT" && make cubelet-test) > "$EVIDENCE/cubelet-unit-gate.log" 2>&1
```

这些是仓库现有门禁，不保证覆盖所有集成场景；逐项核对日志中的 SKIP、匹配数和实际包范围。Master 的 gomonkey 测试需要 `-gcflags=all=-l`，现有目标已设置。TC Makefile 中“没有业务测试、仅编译”的旧注释不符合现状，不作为判断依据。

在可用 Docker 的 Linux 测试环境另执行：

```bash
make -C "$ROOT/CubeMaster" test-docker > "$EVIDENCE/master-docker-db.log" 2>&1
make -C "$ROOT/CubeMaster" test-race > "$EVIDENCE/master-race-gate.log" 2>&1
```

`test-docker` 要求 Docker 可达且强制相应数据库测试，不是双 Master/双 TC 端到端测试。不要直接跑未审阅配置的 `testlocal`/`testtt`：它们会使用 `CubeMaster/test/conf.yaml` 连接集成环境。需要 mount/KVM/root 的用例单独登记，不自动启用 privileged 容器。

## 5. 测试集群配置与 CLI 正常闭环

### 5.1 两个 backend 必须独立记录

| 维度 | 配置/取值 | 验收含义 |
|---|---|---|
| 运行时 CoW | CLI `--backend xfs|s3|cos|minio`；COS/MinIO 被规范化为 `s3` | 节点必须实际启用对应 CoW 能力，别名不会配置 endpoint、bucket 或凭证 |
| ext4 artifact | Master/TC `CUBE_ARTIFACT_STORE_BACKEND=s3|fs` | S3-compatible 对象存储或 TC 文件系统；本地盘/CBS/CFS 属于 fs 介质 |
| artifact S3 配置 | `CUBE_S3_ENDPOINT`、`CUBE_S3_NODE_ENDPOINT`、`CUBE_S3_BUCKET`、`CUBE_S3_REGION` | 分别核对控制面与节点可达地址、测试 bucket/region；COS 使用兼容驱动和正确 HTTPS endpoint |
| artifact fs 配置 | `CUBE_ARTIFACT_STORE_FS_ROOT`、`CUBE_TEMPLATE_CENTER_DOWNLOAD_ADDR` | 根目录和真正持有 artifact 的 TC 下载地址 |
| 下载授权 | Master `common.artifact_download_ttl`；组件秘密 `CUBE_TEMPLATE_CALLBACK_TOKEN` | 只有直连：没有 proxy/direct 开关，也没有任何 Master/TC 代理字节的路由。所有制品下载都走短期票据+refresh capability：S3 走 presigned URL，fs/TC 走 TC 的节点直连 `/internal/artifacts/:id/ext4`（节点 token 本地验证，不是组件秘密）。组件秘密用于派生票据签名 key，以及 Master→TC 的内部调用与可服务性 HEAD 探测，从不下发给节点 |

`artifact=s3 + CoW=xfs` 是有效隔离组合，建议先跑。短期票据 TTL 默认 `1h`，合法范围 `1m`–`24h`，可配置 `2h`。`3cbc88c1` 不再使用旧 `CUBE_ARTIFACT_DOWNLOAD_SIGNING_KEY`、`CUBE_ARTIFACT_DOWNLOAD_TTL` / `CUBE_ARTIFACT_DOWNLOAD_TOKEN_TTL` 配置；one-click 中仍有旧 signing key 生成逻辑，不能当作当前协议依据。旧 `/cube/template/artifact/download` 公共路由和其静态下载 token 仍存在，但新建模板的注解永远是票据协议，不会再指向该路由。修改测试服务配置后必须核实进入实际进程；终端 export 不会改变已启动服务。

每模板选择不同 bucket/profile、任意 native 后端、完整 `jfs` 链路目前没有实现。`--backend cos` 被接受不代表已经真实访问 COS。

### 5.2 地址和输入

以下变量必须替换为**已确认隔离的测试环境**；CLI 默认地址 `0.0.0.0:8089` 不用于验收。CLI 当前使用 HTTP，必须从授权管理网络访问，不能把管理端口公开给互联网。

```bash
export MASTER_HOST='<测试Master地址>'
export MASTER_PORT=8089
export CLI="$ROOT/CubeMaster/build/cubemastercli"
export NODE_A='<专用节点A ID>'
export NODE_B='<专用冷节点B ID>'
export OCI_IMAGE='<已验证镜像@sha256:digest>'
cm() { "$CLI" --address "$MASTER_HOST" --port "$MASTER_PORT" --timeout 2m "$@"; }
cm tpl create --help > "$EVIDENCE/cli-create-help.txt"
cm tpl build --help > "$EVIDENCE/cli-build-help.txt"
cm tpl list --help > "$EVIDENCE/cli-list-help.txt"
```

builder 路线将 `CLI` 改为 `$ROOT/_output/bin/cubemastercli`。必须同时使用新 Master 与新 CLI；只换 CLI 不会补齐旧服务端轻量分页。

### 5.3 创建、观测、运行

```bash
cm tpl build --image "$OCI_IMAGE" --writable-layer-size 4Gi \
  --alias "test-$RUN_ID-xfs" --backend xfs --node "$NODE_A" \
  --detach --json > "$EVIDENCE/create.json" 2> "$EVIDENCE/create.stderr.log"
export JOB_ID="$(jq -er '.job.job_id' "$EVIDENCE/create.json")"
export TEMPLATE_ID="$(jq -er '.job.template_id' "$EVIDENCE/create.json")"
cm tpl watch --job-id "$JOB_ID" --interval 2s --json > "$EVIDENCE/create-watch.json" 2> "$EVIDENCE/create-watch.stderr.log"
cm tpl status --job-id "$JOB_ID" --json > "$EVIDENCE/create-status.json"
```

若自定义镜像需要健康探针，使用它真实提供的 `--expose-port`、`--probe`、`--probe-path`，不套用不匹配的端口。watch 失败就停止，不捏造 ID 或继续以失败模板做基线。

按 [复现手册第 3.2 节](../zh/dev/template-s3-reproduction-runbook.md#32-从模板创建并做真实命令检查) 配置 SDK、指定节点并执行命令，检查 exit code、stdout 和文件内容。先验证 A，再通过第 6 节 redo 分发到 B；模板 READY 和沙箱创建成功都不能代替真实命令检查。

### 5.4 backend 验收矩阵

每个合法变体使用不同测试 alias/请求，登记新模板 ID；不要把矩阵做成未审批的批量创建脚本。

| 输入 | 预期 |
|---|---|
| JSON `tpl create --file` 不传 flag | 保留 JSON 已有 backend；缺省按当前默认 xfs |
| JSON 中 xfs，显式 `--backend s3` | 请求 backend 和相关存储注解同步覆盖为 s3 |
| `tpl build` / `create-from-image` + xfs/s3 | 两个命令等价；请求、定义、列表和实际节点运行能力一致 |
| `--backend cos` / `minio` | 请求和列表显示规范值 s3；实际使用节点部署的 S3-compatible 存储 |
| 未知值 `jfs` / `fs` / `other` | 当前 CLI 拒绝且不创建 job；不要误把 artifact fs 当 CoW backend |

真实 COS/MinIO/S3-compatible 验收应分别在对应测试部署配置中运行：构建、冷节点下载、创建沙箱、写文件、暂停/恢复适用的 S3 CoW 操作。记录实际对象存储访问，不仅检查参数名字。新增 native driver 或多 profile 需求记录 BLOCKED。

## 6. 双节点下载、授权与故障验收

沿用 [复现手册 D1–D3](../zh/dev/template-s3-reproduction-runbook.md#d1s3minio-直下载master-不代理-ext4) 的取证方法；使用本次新建模板和未缓存的 B。

```bash
cm tpl redo --template-id "$TEMPLATE_ID" --node "$NODE_B" \
  --detach --json > "$EVIDENCE/redo-B.json" 2> "$EVIDENCE/redo-B.stderr.log"
export REDO_JOB_ID="$(jq -er '.job.job_id' "$EVIDENCE/redo-B.json")"
cm tpl watch --job-id "$REDO_JOB_ID" --interval 2s --json > "$EVIDENCE/redo-B-watch.json"
cm tpl status --job-id "$REDO_JOB_ID" --json > "$EVIDENCE/redo-B-status.json"
```

| ID | 操作 | 必须观察的结果 |
|---|---|---|
| D01 | artifact=s3，先用 CoW=xfs，B 冷下载 | B 向 Master 取票，随后向 S3/MinIO/COS GET；ext4 字节不经 Master，size/SHA256 匹配，B 可执行 |
| D02 | artifact=fs，文件只在 owner TC，Master 不挂目录 | B 持 Master 签发的节点 token，直接向 owner TC 的 `/internal/artifacts/:id/ext4` GET；字节不经 Master；TC 本地校验节点 token（不是组件秘密）；错误 owner 明确失败 |
| D03 | 默认 1h 和 2h TTL；合法 1m TTL 配合断开后重连 | 新请求过期后重新取票、有界续传；在途连接不要求 TTL 到点断开；不能将旧公共路由的静态 token 当短期票据 |
| D04 | 撤销/错误 node、旧 generation、篡改票据 | Master 新取票请求拒绝（有效格式负例 403）；TC 节点 token 校验失败 403；已签 S3 URL 不保证即时撤销 |
| D05 | 受控中断与错误 Content-Range、size/SHA256 | 正常断点恢复；完整性错误不得发布最终文件；已有缓存命中复验另列风险场景 |
| D06 | node endpoint 不可达或 TLS 不可信 | 明确失败并保留原因；不关闭 TLS 校验、不静默换源 |
| D07 | public API auth 开/关，冷节点 | 记录实际可用性；不以暖缓存成功代替冷下载验证 |

Master 取票入口为 `GET /internal/template/artifacts/:artifact_id/download-ticket`；文件字节只由 S3（presigned URL）或 TC 的节点直连入口 `GET/HEAD /internal/artifacts/:artifact_id/ext4` 提供。Master 与 TC 均不再提供任何代理/中转制品字节的路由：Master 的 `ext4` 文件路由和旧公共下载路由 `/cube/template/artifact/download`（含其 S3 代理流）都已删除；Cubelet 拒绝旧格式 URL/token annotation 和 `source=master` 票据。凭证只在授权测试客户端内存中处理，不放在命令参数、shell 历史或报告。GET presigned URL 不能直接拿来证明 HEAD 成功。

取证包括 artifact ID/generation、脱敏 locator、下载时间线、Master/TC/对象存储字节量、B 落地 size/SHA256 和沙箱真实执行。只采网络包元信息，不抓认证 payload。Master/TC 同 IP 时按端口和服务计数区分；对象域名有多个 IP 时漏抓不能算“没有流量”。内部 TC/控制通道认证 header 不得带到 S3。

## 7. 构建重试、引用保护、私盘 GC 与 COMMIT

### 7.1 构建/删除验收矩阵

故障注入前登记动作、隔离对象、恢复办法和最大观察时间。SQL 只读；如需特殊引用夹具，由负责人在空测试 DB 中使用受审阅的测试程序构造，不通过手改线上状态完成。

| ID | 操作 | 通过标准 |
|---|---|---|
| R01 | 失败/ORPHANED/CLEANUP_PENDING/DELETING 的旧 A 后，同规格生成新 UUID B；分别测重复回调 | A fingerprint 唯一退休占位，A 的 ID/对象/清理记录保留；B 注册并继续分发；重复回调幂等，不复活 A |
| R02 | READY/BUILDING A 与新 B 争用 | 不退休有效/活跃 A；B 合理复用或保留 BUILT 等待，阻塞原因持久化、恢复后清除，不任意超时丢弃 |
| R03 | full redo 产生新 artifact，两个 Master 分别读取定义 | definition、request 注解、replica 与冷热缓存统一指向新 ID/SHA；保留 alias/CreatedAt；失败替换保持旧可用引用 |
| R04 | 两 Master 并发注册同 fingerprint、重复发布及服务重启 | 唯一赢家、CAS/事务一致、无丢失引用、无未解释的长期 BUILT；进程内互斥测试不能代替此项 |
| R05 | 已有 rootfs/半成品后重试，以及版本化对象复用 | 不覆盖有效数据、不盲删；按实际 ObjectKey 判定，不误 HEAD 旧 `id.ext4`；恢复后真实运行 |
| G01 | 分别保留 definition、replica、BUILT job、snapshot、placement，触发正常源删除 | Master 与 TC 都保留源对象；含失败/清理态持久引用仍保护；行锁 claim 后不绕过复查 |
| G02 | 依赖 snapshot 存在时删除模板，随后恢复 snapshot | snapshot 可恢复且数据正确；最后引用释放后清理才收敛 |
| G03 | 永久退役节点与临时不可达对照 | 永久退役 placement 可收敛；临时故障不当作退役，不产生虚假 READY；注册表未就绪时保守保留 |
| G04 | 双 TC 独立私盘均有测试构建残留，共享 DB 行按正常流程删除 | 每 TC 独立扫描自己的管理 root，不受共享 sweep leader 限制；共享 sweep 失败不直接阻断独立扫描，但本地 DB 复查失败仍禁止删除 |
| G05 | 私盘/共享盘下，过期孤儿与活跃构建、发布 marker、合法引用混合 | 仅无引用合法过期孤儿删除；任意 artifact 行含软删除、definition/replica/snapshot/placement、PENDING/RUNNING/BUILT job 均保留 |
| G06 | 构建尚未分配 artifact ID、上传/回调长时间进行、executor 忙 | 活跃构建阻止误删；发布 marker 覆盖完整周期并刷新；idle gate 内最终复查；持续忙可保守推迟 GC |
| G07 | DB 不可达、读取 marker 失败、路径/symlink 异常、TC 滚动重启 | 错误时关闭删除；不删 root/越界路径/symlink；重启后引用与合法孤儿行为一致 |

本地 GC 默认只考虑管理 root 下至少两小时的合法 `rfs-*` 目录；临时年龄调整只允许负责人对登记的空测试夹具操作。不得缩短业务 GC 阈值或修改真实制品时间来加速。DB 历史行仍存在的目录保留是当前设计，不判为本补丁失效。

full redo 与仅向 B 分发不是同一个场景；按 [复现手册 R1/R2/R3/N1/L2](../zh/dev/template-s3-reproduction-runbook.md#5-第二批失败重试及引用一致性) 构造各自前置条件。`--failed-only` 只对确实 FAILED 的测试副本执行。删除入口使用正常 API/CLI，不调用内部函数跳过所有权。

### 7.2 COMMIT 单独门禁

只对登记的测试 sandbox 操作：

```bash
export SANDBOX_ID='<本轮测试sandbox ID>'
cm tpl commit --sandbox-id "$SANDBOX_ID" --detach --json > "$EVIDENCE/commit.json"
export BUILD_ID="$(jq -er '.build_id' "$EVIDENCE/commit.json")"
cm tpl build-watch --build-id "$BUILD_ID" --interval 2s --json > "$EVIDENCE/commit-watch.json"
cm tpl build-status --build-id "$BUILD_ID" --json > "$EVIDENCE/commit-status.json"
```

OCI build/redo 使用 `job_id` + `status/watch`；sandbox COMMIT 使用 `build_id` + `build-status/build-watch`，不能混用。COMMIT 的网络覆盖 flags 要求提供完整 `--file` 请求；未提供时拒绝是预期保护。

- **C01 origin**：提交前写 rootfs 文件并设置驻留进程的内存 marker，提交后在支持的 origin 恢复路径验证二者。记录真实 ready/expected/failed：`expected >= ready`，未尝试 peer 不记 FAILED，phase/job 来源真实。
- **C02 防退化**：存在历史 CREATE/REDO 且最新 job 为 COMMIT 时，image backfill 必须按真正最新 job 判定，不从历史冷镜像补暖模板。
- **C03 跨节点**：当前普通暖 COMMIT 尚未完整关联远端暖包、上传完成、兼容性及模板恢复入口，标为 BLOCKED。不能把 B 冷重建、只恢复文件或 origin 成功记为暖跨节点 PASS。
- 后续能力接通后，另验收 S3 暖包上传 DONE、rootfs + memory + metadata、源节点退役、缺包/失败上传、CPU/kernel 不兼容、二次增量提交、并发销毁与重启恢复。XFS 没有等价完整远端暖包能力，不宣称支持。

## 8. 大列表与真实数据库性能验收

### 8.1 有限页、轻量页和遍历

```bash
cm tpl list --json > "$EVIDENCE/list-default.json"
cm tpl list --limit 50 --offset 0 --json > "$EVIDENCE/list-page0.json"
cm tpl list --limit 50 --offset 50 --json > "$EVIDENCE/list-page1.json"
cm tpl list --limit 50 --offset 0 --no-total --json > "$EVIDENCE/list-light.json"
cm tpl list --limit 1000 --offset 0 --no-total --json > "$EVIDENCE/list-clamp.json"
cm tpl list --all > "$EVIDENCE/list-all.txt" 2> "$EVIDENCE/list-all.stderr.log"
jq -e '(.limit == 50) and ((.data // [] | length) <= 50)' "$EVIDENCE/list-default.json"
jq -e '(.total == -1) and (.has_more | type == "boolean") and ((.data // [] | length) <= 50)' "$EVIDENCE/list-light.json"
jq -e '(.limit == 200) and ((.data // [] | length) <= 200)' "$EVIDENCE/list-clamp.json"
```

HTTP 轻量参数为 `skip_total=true`，CLI 是 `--no-total`。`total=-1` 表示未计数，不是错误或真实数量；某些零值字段可能被 JSON omitempty 省略，应按零解释。

### 8.2 必测条件

| ID | 条件 | 预期/证据 |
|---|---|---|
| P01 | 空库、1/49/50/51/200/201 条、最后一页与越界 offset | 默认 50、最多 200、边界 has_more 正确、无重复/漏项；静态数据下页集合与期望一致 |
| P02 | definition 与 job-only、隐藏资源、多个历史 job 混合 | 过滤/去重符合当前列表语义；最新 job-only backend 正确；有关联模板的历史 job 不膨胀 total |
| P03 | 精确 total 与 `skip_total=true` 对照 | 同一静态数据页一致；轻量 SQL 无精确 COUNT，最多 LIMIT+1 个页 ID 判断 has_more，关联读取限本页 |
| P04 | CLI `--all`，让后续页受控延迟 | 第一页在下一页返回之前可见，表格逐页输出；总 deadline 统一，Ctrl+C 停止请求；不以单个最终文件证明首屏时序 |
| P05 | 短页、重复页、不匹配 offset、空页却 has_more=true | 单测/模拟 HTTP 覆盖；按实际页长推进或明确错误停止，不无限循环 |
| P06 | CLI limit=0/负数、offset<0、all 与显式 limit/offset；HTTP 非法 skip_total | CLI/HTTP 按各自协议拒绝；HTTP limit=0 使用有限默认，与 CLI 拒绝 limit=0 不冲突 |
| P07 | 并发新增、删除和状态变更 | 记录 OFFSET 的重复/跳项与异常；不宣称跨请求快照一致；新正确性要求应另用游标/快照方案实现 |

`--all` 不可与显式 `--limit`/`--offset` 同用；表格输出默认跳过精确 COUNT。JSON 遍历默认仍计数，需使用 `--all --json --no-total` 才跳过 COUNT。JSON 遍历为兼容单个响应格式仍聚合全部条目，有内存风险，只在小数据集测兼容性，不作海量场景推荐。

### 8.3 数据规模、计划与指标

在空测试 DB 通过受审阅的夹具工具构造 250 模板/2000 历史 jobs，再按容量选择 1万/10万模板和密集历史 jobs；这些是计划规模，不是已有性能结果。不得循环构建大型镜像仅为造列表数据，不得向生产 DB 填充夹具。

MySQL 和 PostgreSQL 分别测试：冷/热查询、首/中/深页（例如 offset=0/5000/50000，仅在数据足够时）、精确 COUNT 与轻量模式、并发写入。先记录测试 DB 版本、索引、统计信息和资源配置。

Linux 可测全量表格进程耗时/最大 RSS：

```bash
/usr/bin/time -v "$CLI" --address "$MASTER_HOST" --port "$MASTER_PORT" \
  --timeout 2m tpl list --all > "$EVIDENCE/list-all-perf.txt" 2> "$EVIDENCE/list-all-perf.log"
```

另记录首屏到达时间、每页及总耗时、DB CPU/IO/扫描行/排序/锁等待、Master 内存。基线与补丁使用同一固定数据集/硬件，至少重复三轮。`time -v` 不提供首屏指标，应在终端观测或受审阅的时间戳采集器中单独记录；stdout 被阻塞的情况另列，HTTP deadline 不保证解除外部输出阻塞。

从测试环境实际 SQL trace 获取分页及关联查询，脱敏并使用受控客户端的参数绑定执行 `EXPLAIN`；`EXPLAIN ANALYZE` 会实际运行查询，只在隔离测试 DB 的受控预算内使用。LIMIT 限制返回数量，不保证底层没有全局扫描/排序；检查反连接是否在窗口排名前排除了已有 definition 的历史 jobs，不能只看最后有 LIMIT。

验收前由负责人填写目标表，不借用历史性能数字作为已达到的阈值：

| 指标 | 基线 | 目标/预算 | 补丁实测 |
|---|---|---|---|
| 冷/热首屏耗时、单页 p95 | 待填 | 待填 | 待测 |
| 1万/10万条表格遍历耗时 | 待填 | 待填 | 待测 |
| CLI / Master 最大 RSS | 待填 | 待填 | 待测 |
| DB 扫描/排序/锁等待 | 待填 | 待填 | 待测 |
| 统一 timeout/Ctrl+C 退出时间 | 待填 | 待填 | 待测 |

## 9. 证据、测试顺序与报告

### 9.1 推荐执行顺序

1. 冻结版本/补丁、隔离资源、检查工具与配置。
2. 第 3 节专项，下载类在前置产物齐备的 Linux 环境执行；失败先排查，不继续危险场景。
3. 第 4 节静态、构建、全包和真实数据库辅助门禁；SKIP 单独列出。
4. 第 5 节单节点正常闭环与 backend 矩阵。
5. 第 6 节冷节点直下载与授权/续传，分别覆盖对象存储和 fs 介质。
6. 第 7 节双 Master/TC 并发、引用删除、私盘/共享盘 GC 和重启收敛；COMMIT 单独门禁。
7. 第 8 节真实 DB 分页与性能对照。
8. 汇总结果、保留失败资源、审批清理及测试环境恢复。

### 9.2 每个用例的证据

至少包含：RUN_ID、操作者/日期、环境/版本、存储两个维度、用例 ID、前置条件、命令/测试正则、真实退出码、预期/实测、状态、缺口与附件路径。

业务链路使用 `request_id`、template/job/build/artifact ID、generation、节点/TC ID 和时间窗口串联 Master、TC、Cubelet 日志。保存受控只读 SQL 的 definition、replica、artifact、image job、placement 和 snapshot 引用结果；表名和 SQL 示例见 [复现手册第 3.3 节](../zh/dev/template-s3-reproduction-runbook.md#33-db-对照mysql-只读-sql)。PostgreSQL 使用本数据库 JSON 语法，不照抄 MySQL JSON_EXTRACT。

故障未触发记 NOT_RUN；等待未收敛必须附观察窗口和阻塞原因，不能写“看起来成功”。业务 READY/统计值、DB 所有权、实际对象/目录与恢复结果必须一致。

报告直接采用以下表格，不预填 PASS：

| 用例/层级 | 版本/配置 | 操作/选择器 | 退出码 | 预期与实测 | 状态 | 证据 | 剩余边界 |
|---|---|---|---|---|---|---|---|
| CLI backend / L0 | 待填 | 第 3.3 节 | 待填 | 待填 | NOT_RUN | 待填 | 真实存储另验 |
| D01 / L2 | 待填 | S3 冷节点下载 | 待填 | 待填 | NOT_RUN | 待填 | COS/MinIO 分开 |
| G04 / L3 | 待填 | 双 TC 私盘 | 待填 | 待填 | NOT_RUN | 待填 | 共享盘另验 |
| C03 / L3 | 当前分支 | 普通暖 COMMIT 跨节点 | 不适用 | 控制面链路未完整接通 | BLOCKED | 修复报告 0.5 | 不得关闭 #2095 |
| P03/P07 / L3 | 待填 | 轻量分页/并发变更 | 待填 | 待填 | NOT_RUN | 待填 | OFFSET 非快照 |

整体结论分开写“已完成修复项验收”与“全部业务需求验收”。即使其他项通过，暖跨节点、新 native 存储/多 profile 若在需求范围内仍属于未完成，不能描述为整体需求全部交付。

## 10. 清理、故障恢复与交付门禁

1. 先保存脱敏证据、失败资源清单及引用关系；需要排查的资源由负责人标记保留。
2. 根据本轮登记 ID，通过正常 SDK/API 先释放测试 sandbox，再删除不再需要的 snapshot/template；仅对核对过的测试 template 使用 `cm tpl delete --template-id "$TEMPLATE_ID"`。引用保护用例必须先完成恢复断言，不能提前销毁依赖。
3. 等待正常后台清理收敛，核对 DB、placements、对象存储及各 TC 管理目录。不提供删桶、全表删除、管理 root 递归删除或 force 绕过保护的操作。
4. 仅处理本次独立测试 bucket/prefix 中、登记且无持久引用的资源；需人工残留清理时重新审批。证据目录按保留期处理，不删除其他未跟踪文件或 `.codebuddy`。
5. 恢复测试环境 TTL、限速、故障注入和服务配置，核实进程实际配置与健康状态；不触及线上配置。
6. 测试部署回退使用记录的旧二进制/镜像和兼容配置，先停止测试流量。若存在 schema/状态协议变化，必须审阅兼容性并先备份测试 DB，不能仅回退程序或删除新状态行。
7. 合并/发布门禁：专项确实运行、静态/构建通过、关键引用与授权负例通过、对应真实 DB/存储/多副本验收完成、剩余 BLOCKED/SKIP 明示并经负责人确认。本文不会自动进行提交、推送或部署。

## 11. 现有 E2E 可复用部分与补测流程（2026-11-05）

本节是**补测规格，不是新增测试已实现/已通过**。`3cbc88c1` 的两个隔离反例仍 FAIL；TC ETag fixture 已补 `Ext4SHA256`，需在 Linux 原生运行确认。本次没有运行 live E2E、部署或故障注入。

### 11.1 现有覆盖不能直接拼成完整闭环

| 现有入口 | 可复用 | 缺口 |
|---|---|---|
| `tests/e2e/sdk_compat/cases/templates/test_alias.py` | 模板 build/等待 READY、alias、创建参数持久化 | 未用本次新模板创建沙箱执行命令；没有源对象/节点 SHA、冷下载字节路径 |
| `cases/commands/test_run.py`、`cases/filesystem/test_read_write.py` | 真实 exit/stdout/stderr、文件内容断言 | 使用沙箱 fixture，与新模板构建不是同一条用例 |
| `tests/e2e/sdk_compat/conftest.py` 与 `framework/reporting.py` | `--run-e2e` 显式开启、登记/清理 sandbox、失败保留、脱敏报告 | 不自动提供独立 DB/prefix/TC root、A/B 节点；模板直接请求不自动接入完整 trace |
| `framework/build_throttle.py` | 跨 worker 默认串行 build 到 READY | 增加 xdist worker 不等于触发双 Master 注册或迟到回调竞态 |
| `cases/auth/test_api_key.py` | CubeAPI 模板 API 鉴权 | 不是制品票据下载授权；合法凭据“不等于401”不证明业务成功 |

新增场景建议仍放入现有 `sdk_compat` 测试组织，显式使用本次 `template_id` 建沙箱，并登记本用例所有资源。涉及进程重启、存储/数据库故障的系统测试应单独 opt-in，不混入常规 SDK 用例。当前没有找到统一部署级 barrier/failpoint；下面的屏障都是**建议新增**，不能直接照图调用。

### 11.2 补测矩阵：先确定性交错，再真实系统

| ID | 第一层补测 / 确定性前置 | 完整 E2E 与必需断言 | 当前状态 |
|---|---|---|---|
| E01 新模板闭环 | 复用 build/READY；从同一新模板创建沙箱，不用默认预置模板 | 冷下载、DB artifact/replica/placement、对象和节点 SHA/size、命令/file marker、正常删除与引用收敛 | 规格待实现；单专用计算节点可做 |
| E02 票据字节与授权 | 真实路由/中间件下 fs/s3×public auth 开关；虚构凭据捕获日志 | S3 bytes 绕开 Master；FS bytes 直连 owner TC，不经 Master；撤销后新请求拒绝；旧公共入口边界单列 | 规格待实现；不宣称已签 S3 URL即时撤销 |
| E03 到期重连/Range | 合法短 TTL，断开后等到期再重连；不用睡眠碰运气模拟竞态 | 下一轮取票、partial/Content-Range 正确、size/hash一致、最终可运行；不要求在途连接到点中断 | 部分组件回归已有，真实环境待测 |
| E04 迟到 progress / 重复 BUILT | 正式 DB/HTTP 回归：BUILT→claim→迟到 RUNNING/旧 phase→重复 BUILT；建议 B1 claim 后屏障 | 第二 claim 必须 false；实际 CreateImage 不重复，definition/replica/最终制品唯一 | 隔离状态机反例已 FAIL；先修实现 |
| E05 迁移与旧探测 | 阻塞旧 stat/404；建议 B2 old-probe 返回前屏障；完成正常迁移再释放 | 新 locator 仍 READY，下载新对象/owner、hash/size与运行正确，旧缓存不污染 | SQL降级反例已 FAIL；完整交错待测 |
| E06 claim 后崩溃 | B1 claim 后、B3 首副本后、B4 definition 写前；隔离 Master 受控重启 | 持久进度续跑、唯一恢复者、防重复/丢引用；不能仅超时 FAILED | 候选缺口，未复现完整系统；需真实 DB，跨副本需双 Master |
| E07 ETag / If-Range | Linux 原生运行 TC handler 测试 | 正确/错误 If-Range、HEAD/Range 的内容及status；TC 节点直连入口再验证 | fixture 已补 SHA，原生运行尚未执行 |
| E08 session unlock/cancel | MySQL/PG 两会话，取消ctx与解锁失败注入 | 第二会话可得锁，不确定连接不能回池长期持锁 | 候选待测；不需要第二计算节点 |
| E09 同store错locator删除 | 空DB/FS中 A 指向有引用 B；正常删除 A；保留B控制样本 | B对象/hash/引用均保留；多owner/私盘路由再用双TC | 候选待测；现有wrong-root测试不覆盖 |
| E10 upload与GC / 发布失败 | B5 GC最终检查后、B6 upload link/响应前、B7 PUT后；测试控制器精确交错 | 上传成功对应有效文件；callback/CAS失败的对象有持久cleanup账本且收敛 | 候选待测；先单组件，后真实TC/DB/store |
| E11 scan公平性 | 大于batch的不可处理前批+可处理尾批；有限轮sweep | 尾批在约定窗口推进；记录不可处理原因与cursor | 候选待测；先真实DB集成，不靠sleep |
| E12 错误缓存与日志 | 专用临时缓存预置足够大错误文件；全部凭据用虚构值 | 拒绝/重新下载错误缓存；query/嵌套annotations/refresh/组件token无泄漏 | 缓存当前轻量检查；脱敏候选待测 |

E04/E05 的现有反例是 SQLite/Go overlay，不是正式入库测试，更不是线上故障证据。E08–E12 不需要先等齐 A/B 计算节点；它们可先做组件集成。双 TC 私盘、A→B cold redo、源节点失效与双 Master 接管仍需专用真实拓扑。暖 COMMIT 跨节点仍 BLOCKED，不能用 cold redo 或只恢复文件替代。

### 11.3 屏障和故障预算

- 不向生产协议随意添加 pause/crash 接口。优先依赖注入、隔离HTTP/store、进程内channel；集群hook仅测试构建可用，默认不编入生产。
- 每个barrier提供到达确认、有限等待、释放/取消、日志关联ID。未收到到达确认记 NOT_RUN，不能把随意延迟后正常结束算覆盖。
- 每个进程重启/网络故障只作用于登记的隔离实例；事前确认操作者、最大故障时间、恢复步骤、失败资源保留策略。
- `RUN_ID → request_id → template/job/artifact/generation → node/TC ID` 串联请求、DB状态、存储bytes、落盘SHA和沙箱断言。捕获的是脱敏元信息，不记录token原文。

### 11.4 执行门禁

1. 先修E04/E05（E07 fixture 已修）；正式回归必须实际执行。不得删断言、跳过失败用例或把COMPILE_PASS算运行通过。
2. Linux原生与真实DB组件门禁通过后，再新增E01单节点闭环；模板创建成功、READY、health=200分别不等于完整通过。
3. 明确存储类型、TTL和public auth配置；完成E02/E03。已有暖缓存无法作为冷下载证据。
4. 专用A/B、双Master/TC、独立DB/store/root和fault hooks齐备后，执行多副本/冷redo/恢复/GC；资源不足记NOT_RUN，未接通能力记BLOCKED。
5. 清理按本次资源账本执行并验证收敛，不能吞异常后默认通过。保留失败证据后再审批人工清理。

当前handoff的34控制面+216单计算不足以证明多副本/A-B验收环境齐备；34 Cubelet保持mask。本节不会自动开live测试、切换配置或部署。
