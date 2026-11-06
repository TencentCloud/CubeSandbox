# 模板可靠性、存储后端与大列表：整体测试指南

> 更新日期：2026-11-06。执行前锁定本次待验提交，核实部署的 Master、TC、Cubelet、CLI 实际 SHA；不能以分支名、旧版测试结果或构建成功代替版本确认。
> 本文是测试计划，不是集群验收通过报告。每次回归和真实数据流结果均须由执行者按实际 SHA 填报，不预置 PASS/FAIL。
> 详细部署操作见 [模板/S3 复现手册](../zh/dev/template-s3-reproduction-runbook.md)。旧版实施记录只供历史追溯，不作为本次验收证据。

## 1. 范围、状态与通过标准

| 范围 | 已有证据 | 本次还需要验收 |
|---|---|---|
| CLI `backend` / `build` | CLI 支持 `xfs|s3|jfs`；CubeAPI 当前只接受 `xfs|s3` | 本提交真实 CLI 参数矩阵；fs/S3 artifact × xfs/S3 CoW 的真实构建、冷下载、沙箱执行；JFS 单列验证 |
| DB/HTTP/CLI 分页 | 历史 SQLite/httptest 覆盖有限页与 CLI 遍历 | MySQL 与 PostgreSQL 上真实 Master/CLI；超过 20 万可见 ID、深 offset、COUNT、首屏/RSS 和预算 |
| 远端构建回调 / V01 | 历史有状态机和 callback 专项，不等于当前提交运行通过 | 真实 TC→Master 回调顺序、迟到 RUNNING/status-less progress、重复 BUILT；同 DB 观察唯一分发/副本 |
| artifact locator / V02 | 历史有 SQLite/存储 stub；旧 locator 竞态未由 stub 验证 | Master 可读源文件、真实 merge、旧探测 barrier、CAS 前后 DB 状态及新 locator 冷下载 |
| 票据、TC 直连与 Cubelet 下载 | 各组件有 httptest/跨包测试，不访问生产等价存储 | 真实 S3/fs 字节路径、节点/代际绑定、Range/过期刷新、授权负例和落盘 SHA |
| TC 对象复用、删除与私盘 GC | 历史临时目录/模拟引用测试 | MySQL/PG、真实隔离 store、双 TC 私盘/共享盘、滚动重启和持久引用闭环 |
| COMMIT 与 SDK E2E | 有 origin/backfill 专项；新模板 lifecycle E2E 已实现 | 当前提交在目标 Linux 的全包/真实 COMMIT origin 恢复及实际 live E2E 执行；暖跨节点仍 BLOCKED |
| 全套/Rust | macOS 对 Linux 专用包不构成运行验证 | Linux 全包、race/Docker DB、Rust gate；逐项记录执行数/SKIP 和环境阻塞 |

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

以下函数保留 `go test` 的真实退出码，并检查实际运行/跳过事件，防止错误正则产生假 PASS。参数依次为模块、证据标签、包、测试正则、可选模式 `race`；正则 `-` 表示整包。返回非零后应先定位失败，不继续集成验收。主动跳过会单独报告为 SKIP。每条测试矩阵必须另外记录 `PASS/FAIL/SKIP/NOT_RUN/BLOCKED`、执行环境、提交 SHA、接口请求 ID 和证据文件；Go 测试通过不能自动把对应 live 场景标为 PASS。

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

run_go CubeMaster master-redo-artifact-lock ./pkg/templatecenter \
  '^(TestRunRedoTemplateImageJob(ReloadsArtifactAfterBuildLock|FailsOnArtifactReloadError|StopsOnArtifactCleanupFailure)|TestArtifactStatusReusableForRedo)$'

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

这些测试使用签名替身、SQLite/httptest，不访问真实桶。Master 只签发票据并校验节点 capability、generation 和当前 replica，不提供制品字节路由；TC 的节点票据路径供 Cubelet 直接下载，组件认证路径也注册 GET/HEAD，Master 当前用它做服务性 HEAD 探测。两条 TC 路径的认证方式不同，不能把组件秘密传给节点。Cubelet 只接受 `s3`/`template_center` 来源票据。TC ETag/If-Range 用例需在 Linux 原生运行；fixture 有 SHA 不代表用例或 live 路径已验证。不得删断言或排除用例来制造 PASS。

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
| 运行时 CoW | CLI `--backend xfs|s3|jfs` | 节点必须实际启用对应 CoW 能力；使用 COS/MinIO 时配置节点的 S3-compatible endpoint、bucket 和凭证；JFS 需独立的共享存储验收 |
| ext4 artifact | Master/TC `CUBE_ARTIFACT_STORE_BACKEND=s3|fs` | S3-compatible 对象存储或 TC 文件系统；本地盘/CBS/CFS 属于 fs 介质 |
| artifact S3 配置 | `CUBE_S3_ENDPOINT`、`CUBE_S3_NODE_ENDPOINT`、`CUBE_S3_BUCKET`、`CUBE_S3_REGION` | 分别核对控制面与节点可达地址、测试 bucket/region；COS 使用兼容驱动和正确 HTTPS endpoint |
| artifact fs 配置 | `CUBE_ARTIFACT_STORE_FS_ROOT`、`CUBE_TEMPLATE_CENTER_DOWNLOAD_ADDR` | 根目录和真正持有 artifact 的 TC 下载地址 |
| 下载授权 | Master `common.artifact_download_ttl`；独立签名密钥 `CUBE_ARTIFACT_DOWNLOAD_SIGNING_KEY`；组件认证 `CUBE_TEMPLATE_CALLBACK_TOKEN` | 无 proxy/direct 开关。所有下载由实际 storage owner 提供字节：S3 使用 presigned URL，fs/TC 使用 TC 节点票据入口 `/internal/artifacts/:id/ext4`。TC 还有仅内部服务调用的组件认证 GET/HEAD 路径；Master 在其中使用 HEAD 做可服务性探测。两个密钥用途隔离，均不得下发节点 |

`artifact=s3 + CoW=xfs` 是有效组合，仅能验证 S3 artifact 与 XFS runtime CoW 的这一个组合。当前 `common.artifact_download_ttl` 默认 `1h`，允许 `1m`–`24h`，例如可设 `2h`。票据签名使用独立的 `CUBE_ARTIFACT_DOWNLOAD_SIGNING_KEY`（Master 与 TC 均需配置，缺失时拒绝签发/验签），不复用 `CUBE_TEMPLATE_CALLBACK_TOKEN`；不要设置历史 `CUBE_ARTIFACT_DOWNLOAD_TTL` 或 `CUBE_ARTIFACT_DOWNLOAD_TOKEN_TTL` 并期待生效。Master 不再注册旧公共 `/cube/template/artifact/download` GET/HEAD 路由；历史 token annotation 不是可用路由/凭据，单列兼容负例。更新配置后须重启或滚动服务并核对运行进程真实配置，终端 export 不会改变已启动服务。

每模板选择不同 bucket/profile、任意 native 后端仍需单独实现；`jfs` 参数已被 CLI 接受，但完整构建、共享存储与跨节点链路必须独立验证。CLI 不接受 `--backend cos|minio`；真实 COS/MinIO 访问取决于节点的 S3-compatible 存储配置。

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
| JSON `tpl create --file` 不传 flag | 保留 JSON 已有 backend；JSON 未提供时记录字段仍缺省/由服务端既有逻辑处理，不由 CLI 擅自补 xfs |
| JSON 中 xfs，显式 `--backend s3` | 请求 backend 和相关存储注解同步覆盖为 s3 |
| `tpl build` / `create-from-image` + xfs/s3 | 两个命令等价；请求、定义、列表和实际节点运行能力一致 |
| `--backend jfs` | CLI 接受；需 JFS 节点与共享存储完成独立真实链路验证，不能只凭请求成功记 PASS |
| `--backend cos` / `minio` / `fs` / `other` | CLI 拒绝且不创建 job；不要误把 artifact fs 或 S3-compatible 服务商名称当 CoW backend |

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

Master 取票入口为 `GET /internal/template/artifacts/:artifact_id/download-ticket`；S3 artifact 字节由对象存储 presigned GET 提供，fs artifact 字节由 TC 节点直连入口 `GET/HEAD /internal/artifacts/:artifact_id/ext4` 提供。TC 另注册组件认证的内部 GET/HEAD，用于 Master 预检；它不是 Cubelet 下载凭据。CubeMaster 不提供 artifact 字节代理路由，旧 Master 公共下载路由 `/cube/template/artifact/download` 不应注册；本测试 checkout 的 TC 节点路由仍提供受票据保护的 fs 字节，不能笼统称 TC 没有下载路由。Cubelet 应拒绝过期、错误 generation/节点/用途的票据和不支持的 `source=master`；只有在当前代码存在相应拒绝断言时才作为门禁，否则记为额外安全测试。凭证仅在授权测试客户端内存处理，不放命令参数、shell 历史或报告。S3 GET presigned URL 不证明 HEAD 成功。

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
| P05 | 短页、重复页、不匹配 offset、空页却 has_more=true | 真实 Master 的固定数据集仅验证正常分页；错误响应属于 CLI 容错专项，须经批准在隔离链路用受控代理修改指定响应。代理场景只证明 CLI 的错误处理，不计入真实 DB 分页/性能 PASS；未配置代理记 NOT_RUN |
| P06 | CLI limit=0/负数、offset<0、all 与显式 limit/offset；HTTP 非法 skip_total | CLI/HTTP 按各自协议拒绝；HTTP limit=0 使用有限默认，与 CLI 拒绝 limit=0 不冲突 |
| P07 | 并发新增、删除和状态变更 | 记录 OFFSET 的重复/跳项与异常；不宣称跨请求快照一致；新正确性要求应另用游标/快照方案实现 |

`--all` 不可与显式 `--limit`/`--offset` 同用；表格输出默认跳过精确 COUNT。JSON 遍历默认仍计数，需使用 `--all --json --no-total` 才跳过 COUNT。JSON 遍历为兼容单个响应格式仍聚合全部条目，有内存风险，只在小数据集测兼容性，不作海量场景推荐。

### 8.3 数据规模、计划与指标（交给目标环境验证模型执行，不是 Go 单测）

**先划清两条独立的数据流**：功能链路使用小而干净的隔离功能库和公开 CLI/SDK：构建真实模板→Master→TC→制品→节点副本→显式创建沙箱→命令/文件操作；迁移、COMMIT 单独登记真实资源。列表性能链路使用**另一隔离负载库**：批量造数只产生列表负载，然后由真实 CLI/HTTP→真实 Master→负载库→真实分页响应。`tpl list` 本身不经过 TC/Cubelet，不能要求其 trace 出现 TC/节点；EXPLAIN、直接 SQL、单测或 mock 均不能替代入口响应。两个库禁止共享 Master 实例或改已启动进程的 DB 配置；若无法分库分实例则列表性能记 BLOCKED，不混测功能时延或 backfill。

**写入门禁**：另一模型先列出两套实例的实际 DB 地址/库名、运行中进程配置/版本、迁移版本、Redis、存储 owner/节点、权限、资源预算、回滚与清理方案，交环境负责人审批。负载库必须与业务数据隔离、可丢弃，访问账号仅有该库写权限；绝不在生产业务库中造数、重置或清理数据。身份/隔离/授权不明则停止写入记 BLOCKED；脚本不得全局 DELETE/TRUNCATE/DROP。生产拓扑若不能做到这种隔离，只有业务库的只读证据，不等于完成 22 万+ 验收。

1. 固定代码 SHA、数据库/迁移版本，先记录表结构、索引、统计、DB CPU/内存/磁盘配额和超时，再分批装载 1 万→5 万→10 万→**至少 220001 个唯一可见 template_id**。测试数据统一 `tpl-perf-<RUN_ID>-<序号>` 前缀（留意 MySQL 128 字节限制），definition 使用 `kind=template`、实际列表允许的 status、`request_json='{}'`、`deleted_at=NULL`；为部分记录设置相同 `updated_at`。这些只是**列表投影**，并无真正 artifact/replica，禁止对其执行创建/迁移/删除 API；另一功能库中创建的真模板不得混进此库。
2. 如需测历史作业影响，再分档加入 1 万个 job-only template_id（每 ID 两个不同 attempt）、最多 5 万条已有 definition 的历史 job；每条 job 的 `(job_id)`、`(template_id, attempt_no)`、`(request_id, operation)` 必须各自满足现有唯一索引。再以小比例加入软删 definition、pause snapshot、DELETED/DELETING snapshot 和不同状态，**分别计算物理行数与 SQL 可见唯一 ID**；`total` 必须与可见唯一 ID 对照，不应预设等于 220001 或 DB 行数。留存每档/每类数量和生成规则，不让假数据进入业务建沙箱路径。
3. 不提供可直接对业务库执行的裸 `INSERT` 示例。另一模型应先读取目标数据库的当前迁移 DDL，给出**仅针对隔离库**的参数绑定、批次≤1000、唯一键处理、配额/速率限制和前缀校验方案，经环境负责人复核后造数；逐档核对 COUNT、唯一 ID 和磁盘/锁等待，超预算即停。装载后在隔离库更新统计信息；后台 backfill 可能反复扫到无 artifact 的假 READY 并影响测量，应监测此干扰、隔离其资源，无法消除则本档性能结论标记 INCONCLUSIVE，而非据此优化索引或宣称 PASS。
4. 固定数据后先由真实 CLI/SDK 向本版 Master 发起列表请求；再从这批请求的 trace/DB 慢查询日志提取**实际执行的参数化 SQL**，针对对应 COUNT、排序 ID 页和页内 job 回查做 `EXPLAIN`（MySQL 用 `EXPLAIN FORMAT=JSON`，PG 用 `EXPLAIN (FORMAT JSON)`），记录扫描行、窗口排名、反连接、临时表、filesort、关联查询及是否用到索引。受控的 `EXPLAIN ANALYZE` 会执行查询，只在隔离库加超时且低负载时运行。`type=ALL`/`Using filesort` 是风险线索而非单独的 FAIL 判据；`UNION ALL` 的全局排序不一定因单加 `updated_at` 索引消失。**先取同数据真实请求基线再决定是否需前向迁移索引、改写窗口/COUNT 或提供游标分页；不得未经实测直接给生产库建索引。**
5. 由**负载库对应的真实 Master**接收 CLI/HTTP 请求，固定数据上分别测 `limit=50/200`、`offset=0/5000/50000/100000/200000/实际末页/越界`，`total` 与 `skip_total=true`（CLI `--no-total`）至少各三轮，记录入口 request ID、HTTP 状态、业务码、耗时分布、Master/DB CPU/IO/RSS、COUNT 独立耗时与查询计划；同时间戳按 `(sort_ts DESC,template_id ASC)`、job-only 最新 attempt、可见 total、末页 has_more 和静态数据跨页无重漏逐项核对。深页 OFFSET 的静态正确性不能推断并发写场景有一致性保证；并发场景只能在**另一个小型真实功能库**通过合法 API 创建/删除专用资源做独立观察，不在装有假 READY 的负载库中建沙箱。22 万档先做**有限页**，`--all`/全量导出仅经内存/时间预算审批后运行；`--all --json` 不用于海量档。

**并发与恢复扩展**：先测单客户端冷/热基线，再按负责人批准的 `1→5→10→目标并发` 阶梯，每级固定 1–5 分钟，混合首屏/深页与 `skip_total`/精确 COUNT，设置服务端/客户端 QPS、连接数、查询 timeout 与 DB CPU/IO 停止线；不对生产流量做压测。每档记录请求成功率、业务 ret_code 分布、P50/P95/P99、DB pool 等待/慢查询和错误后恢复时间。只在测试负载 Master 上注入一次短暂 DB 连接中断/查询取消：请求必须明确失败（不得返回伪成功/部分页），恢复 DB 后后续同一合法列表请求成功且 pool 无连接耗尽；该故障演练和静态性能数据分开记，不把被注入档计入延迟基线。

6. 22 万+ 场景用于暴露/量化性能风险；除非核对本次实际 diff 并复测，不得写成“列表 SQL/索引已修复”，也不能仅凭 EXPLAIN 的 filesort 宣称 FAIL。只有真实入口的正确性及事先约定的耗时/资源预算都达标，才能记录“当前版本该数据分布下可接受”；否则附真实请求、脱敏 SQL/EXPLAIN 和资源数据，区分 COUNT、排序、窗口/反连接、深 OFFSET 瓶颈，另行做针对性修复后在**同一隔离数据集**复测。缺实际库/授权/预算记 BLOCKED，不替其他功能链路作结论。

**全量遍历独立门禁**：先在小档验证 `tpl list --all` 的逐页输出和中断，再评估 22 万档的响应时间/内存、DB 资源与目标审批预算；无预算或超预算即不跑 `--all`，记 NOT_RUN，不用默认 `--timeout 2m` 假设海量全表可完成。若获批，另记录每页/首屏时间、退出码、最大 RSS 和资源上限；`/usr/bin/time -v` 只记录进程资源，不证明首屏到达时序。静态性能基线与补丁须在相同隔离数据集/硬件上对照，不将并发修改时的 OFFSET 漂移当稳定分页承诺。

从测试环境实际 SQL trace 获取分页及关联查询，脱敏并使用受控客户端的参数绑定执行 `EXPLAIN`；`EXPLAIN ANALYZE` 会实际运行查询，只在隔离测试 DB 的受控预算内使用。LIMIT 限制返回数量，不保证底层没有全局扫描/排序；检查反连接是否在窗口排名前排除了已有 definition 的历史 jobs，不能只看最后有 LIMIT。

验收前由负责人填写目标表，不借用历史性能数字作为已达到的阈值：

| 指标 | 基线 | 目标/预算 | 补丁实测 |
|---|---|---|---|
| 冷/热首屏耗时、单页 p95 | 待填 | 待填 | 待测 |
| 1万/5万/10万/22万+ 档有限页耗时；全量遍历仅获批后 | 待填 | 待填 | 待测 |
| CLI / Master 最大 RSS | 待填 | 待填 | 待测 |
| DB 扫描/排序/锁等待 | 待填 | 待填 | 待测 |
| 统一 timeout/Ctrl+C 退出时间 | 待填 | 待填 | 待测 |

### 8.4 本轮缺陷的真实链路验收（另一模型执行）

先核对正在运行的 Master、TC、Cubelet、CLI 实际 SHA/进程配置、**同一功能库**及存储 owner、专用测试节点、Redis 和授权/回滚方案；未授权不得替换服务或故障注入。先用公开 SDK/CLI 完成正常闭环：真实镜像构建→BUILT→READY→节点副本→显式创建沙箱→命令与文件读写→按 ID 清理；只有这条基线 PASS，才对**专用 job/artifact**做 V01/V02 的时序注入。每个场景用对应服务的 request/job/artifact ID 和时间窗拼接 HTTP、DB、Master/TC/节点日志，不能假设所有请求共用一个 trace；回调密钥、票据、签名 URL 不写入报告。无法提供功能隔离或故障注入控制则相关场景 BLOCKED，编译/单测不抵消。

| 问题 | 目标环境操作 | PASS 判据 |
|---|---|---|
| V01 迟到进度重开分发 | **仅在获批的隔离功能链路**正常构建专用 job；TC 的 `RUNNING/BUILDING_EXT4` 上报是同步的，不能扣住 HTTP 响应等待 BUILT。若能部署按 job_id 定向的代理，代理应先**向 TC 立即 ACK 并安全留存原始已认证请求**（不在报告泄露凭据），使正常 BUILT 到达 Master；确认该 job 已由 Master claim 为 `RUNNING/DISTRIBUTING` 后，再向 Master 重放原有 RUNNING/无状态进度及 BUILT。无代理到达确认/安全重放能力则 V01 记 BLOCKED，不声称覆盖交错 | DB phase 不回退、原 artifact/result 不被覆盖、BUILT 仅一次分发 claim；核对节点分发 RPC、READY replica 和 job 终态，**构建本身不会创建用户 sandbox，不以沙箱数作判据**。迟到 `FAILED` 当前不受同一 RUNNING phase 限制，另列未覆盖风险，不能用本场景宣称所有迟到状态安全 |
| V02 旧 locator 的 404 | 先确认专用 READY 模板有**Master 可读的本地 legacy ext4 源文件**、可迁移目标 TC/隔离桶及与探测一致的旧 owner；`tpl merge` 是本地源→TC/S3 的异步迁移，**不是两 TC 私盘间复制**。若来源不满足，不能靠手工改业务 DB 或删除真实对象硬造条件，记 BLOCKED。满足前置条件时，在隔离链路只对该 artifact 的旧 fs HEAD 或旧 S3 Stat 设置有到达确认/超时保护的受控探测屏障，保持本地源可读；通过公开 `tpl merge`/status 确认同一 artifact 的 locator 已真实改变，再放行旧探测返回 404。要分别记录探测、迁移行 CAS、降级 CAS 的发生顺序 | 原旧 404 不降级新 locator 的 READY 行、`last_error` 不写入旧结果；新 locator 须实际重新探测，按正常分发流程让**无缓存的专用节点**冷下载并核对 SHA，不能只用已有副本创建沙箱来证明；另对新 locator 真缺失按独立测试资源验证降级。不具备源文件/屏障/冷节点，V02 竞态未验证 |
| ISSUE-1 分页 HTTP 契约 | 对已部署 Master 真实 GET `/cube/template?limit=abc`、`?skip_total=xx`、`?offset=-1`，并对照合法分页；留存响应正文 | 当前 CubeMaster 契约为 HTTP 200 且 `ret.ret_code=MasterParamsError`、`ret.ret_msg` 非空；不能只根据 HTTP 200 判成功或擅自改变为 4xx；`limit=0` 按现有协议用默认 50 |
| ISSUE-2 COMMIT 与测试夹具分开 | Linux 全包中五个 commit-action 测试不再因未配置 Redis panic，只算**测试可运行性**。业务另在已配置 Redis 的隔离功能链路，真实创建并定位本轮沙箱→写文件和内存 marker→`tpl commit --sandbox-id` 得到 build_id→`tpl build-watch/build-status`→Master host lookup→Cubelet CommitSandbox→生成的新 template_id/origin READY 或 PARTIALLY_READY→仅在支持的 **origin 恢复路径**显式建沙箱验证文件与内存；清理专用资源 | 测试夹具和业务链路分别给结论。不要把生成了 build_id 当作完成、重新提交生成新 job 当同一 job 幂等、进程崩溃后 COMMIT 自动续跑或跨节点暖恢复当已有能力；相关场景若未接通标 BLOCKED |
| ISSUE-3 新模板 SDK E2E | 使用实际有 envd 服务的镜像，验证 49983/49999 暴露与 49999 `/` probe；执行 `pytest --run-e2e --sdk-e2e-backends cubesandbox` 对应单文件，留存新模板→READY→真实沙箱→`commands.run` 与文件往返→清理的事件 | 用例 PASS、命令 rc=0、文件读写一致；不应只看到模板 READY 就判通过；404 时区分端口、路由、镜像服务和产品路径 |
| 22 万+ 列表负载 | **独立的**获批负载库造 220001+ 可见 ID，运行本版真实 CLI/HTTP→Master→DB→响应；先固定数据验证分页及 COUNT，再按请求 trace 做 EXPLAIN/计时，有限页优先，全量另过资源门禁 | 分别提交唯一可见 ID、物理行数、非法/合法 HTTP 响应、每页/COUNT 耗时和资源预算、SQL 计划；仅造数/EXPLAIN/单测不算真实请求 PASS，且本轮代码未对列表 SQL/索引做性能修复 |

**交接反馈**：另一模型逐项给出实际执行时间、环境/版本、原始响应、DB 查询及失败堆栈；开发侧先 CR 核对是代码缺陷、环境缺口还是测试配置，再按可重放失败项修改；修改后换新的 SHA 在相同隔离数据和时序上复跑。禁止向生产业务库植入模拟 job、手工改生产 artifact locator 或在业务节点进行故障注入。

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

| 用例/层级 | 实测版本（各组件 SHA） | 真实入口 / 数据集 | 退出码与请求结果 | 断言/实测指标 | 状态 | 证据路径 | 未覆盖边界 |
|---|---|---|---|---|---|---|---|
| E01/E17 / L2 新模板 SDK | 待填 | pytest + 本轮 template_id | 待填 | rc、marker、文件往返、清理 | NOT_RUN | 待填 | 冷字节路径另测 |
| E02/D01–D06 / L2 下载 | 待填 | xfs/s3 × fs/s3 + cold node | 待填 | ticket/auth、Range、source bytes、SHA | NOT_RUN | 待填 | 已签 S3 URL撤销边界 |
| E07–E08 / L3 回调 | 待填 | 真 TC callback + 两 Master/共享 DB | 待填 | 同一 job phase、claim 数、副本数 | NOT_RUN | 待填 | late FAILED 单列 |
| E09–E10 / L3 locator | 待填 | tpl merge + barrier + cold node | 待填 | locator/status/error/SHA 时序 | NOT_RUN | 待填 | 无屏障记 BLOCKED |
| E11 / L2 CLI backend | 待填 | 真 create/build request + 节点矩阵 | 待填 | JSON/file override、非法值、无 job | NOT_RUN | 待填 | CLI 单测非 live |
| E12–E14 / L3 列表 | 待填 | MySQL/PG 负载 Master + 22万可见ID | 待填 | ID集合、页/COUNT/P95/RSS/EXPLAIN | NOT_RUN | 待填 | 性能需预算 |
| E15 / L3 引用与GC | 待填 | artifact/引用/双TC私盘或共享盘 | 待填 | 保留和删除收敛时序 | NOT_RUN | 待填 | 拓扑不足 BLOCKED |
| E16 / L2 COMMIT origin | 待填 | API 固定 request ID + origin sandbox | 待填 | build/job/template/replica/restore | NOT_RUN | 待填 | 暖跨节点仍 BLOCKED |

整体结论分开写“已完成修复项验收”与“全部业务需求验收”。即使其他项通过，暖跨节点、新 native 存储/多 profile 若在需求范围内仍属于未完成，不能描述为整体需求全部交付。

## 10. 清理、故障恢复与交付门禁

1. 先保存脱敏证据、失败资源清单及引用关系；需要排查的资源由负责人标记保留。
2. 根据本轮登记 ID，通过正常 SDK/API 先释放测试 sandbox，再删除不再需要的 snapshot/template；仅对核对过的测试 template 使用 `cm tpl delete --template-id "$TEMPLATE_ID"`。引用保护用例必须先完成恢复断言，不能提前销毁依赖。
3. 等待正常后台清理收敛，核对 DB、placements、对象存储及各 TC 管理目录。不提供删桶、全表删除、管理 root 递归删除或 force 绕过保护的操作。
4. 仅处理本次独立测试 bucket/prefix 中、登记且无持久引用的资源；需人工残留清理时重新审批。证据目录按保留期处理，不删除其他未跟踪文件或 `.codebuddy`。
5. 恢复测试环境 TTL、限速、故障注入和服务配置，核实进程实际配置与健康状态；不触及线上配置。
6. 测试部署回退使用记录的旧二进制/镜像和兼容配置，先停止测试流量。若存在 schema/状态协议变化，必须审阅兼容性并先备份测试 DB，不能仅回退程序或删除新状态行。
7. 合并/发布门禁：专项确实运行、静态/构建通过、关键引用与授权负例通过、对应真实 DB/存储/多副本验收完成、剩余 BLOCKED/SKIP 明示并经负责人确认。本文不会自动进行提交、推送或部署。

## 11. 现有 E2E 可复用部分与补测流程（2026-11-06）

本节区分可复用组件测试与需真实环境执行的缺口。旧版本的 V01/V02 结果仅适用于当时的 SHA；本次待验版本的 V01/V02 在执行前均为 NOT_RUN。TC ETag fixture 的修正同样不代表 Linux 原生测试或部署 GET/HEAD/Range 已通过。

### 11.1 现有覆盖不能直接拼成完整闭环

| 现有入口 | 当前已有动作 | 还不能证明什么 |
|---|---|---|
| `tests/e2e/sdk_compat/cases/templates/test_new_template_sandbox_lifecycle.py` | 新建模板、配置 `exposed_ports=[49983,49999]` 和 `probe_port=49999`；用同一个 `template_id` 创建沙箱，跑命令和文件往返，按 fixture 清理 | live 是否运行尚未知；没有断言 cold node、磁盘落地 SHA、票据授权、对象 store 字节量 |
| `cases/templates/test_alias.py` | 模板 READY、alias 及请求参数持久化 | 不是上述新模板端到端的替代；也不证明冷下载 |
| `cases/commands/test_run.py`、`cases/filesystem/test_read_write.py` | 真实 exit/stdout/stderr、文件内容断言 | 各自使用独立 sandbox fixture，未与模板构建串成同一个生命周期 |
| `tests/e2e/sdk_compat/conftest.py` 与 `framework/reporting.py` | `--run-e2e` opt-in、资源登记/清理、失败保留和报告脱敏 | 不提供 DB/prefix/TC root、A/B 冷节点或人工批准的故障屏障 |
| `framework/build_throttle.py` | xdist 下默认串行 build 到 READY | 增加 worker 不会自动制造两个 Master 的 claim race 或迟到回调 |
| `cases/auth/test_api_key.py` | CubeAPI API-key 鉴权 | 不测内部 ticket/refresh token/node binding；也不证明业务成功 |

场景分层运行：已有 SDK lifecycle 直接按其真实命令运行并记录 NOT_RUN/PASS；下载票据须用冷节点和抓取的服务/对象元信息另验；并发回调/locator 测试必须有受控屏障及到达确认；库压测用独立的负载 Master/DB。系统重启、DB/对象 store 故障、删除和 timeout 只在独立 opt-in 拓扑执行。不得把合并几个不共享资源、没有同一个 template/artifact ID 的 E2E 文件称作端到端闭环。

### 11.2 扩充后的真实数据流矩阵：每项单独记状态

现有专项命令覆盖的是局部组件行为。下表把输入、时序、线上可观察量与边界拆开；先确保主链路正常，再逐项添加一个变量。凡“受控注入”均需独立授权/隔离实例和到达确认，不可在业务实例做。

| ID | 前置 / 操作（必须由入口流量完成，除造负载外） | PASS 的可核对证据 | 当前执行状态 |
|---|---|---|---|
| E01 新模板正常闭环 | 运行已有 `pytest --run-e2e --sdk-e2e-backends cubesandbox -q tests/e2e/sdk_compat/cases/templates/test_new_template_sandbox_lifecycle.py`；确认测试用镜像真实监听 probe port；从 CLI/SDK build 开始，使用响应中的同一 `template_id` 建 sandbox，跑唯一 run marker、写文件/读回/`cat`，最后清理 | pytest 实际执行且 exit 0；模板/job/artifact/replica/placement 关联到同一 ID；命令 rc=0 且 marker 精确命中；文件 API 读回与 guest `cat` 字节一致；清理 ID 与本次登记一致 | 用例已在当前 checkout；live 未执行填 NOT_RUN。它不覆盖 cold download bytes/票据，须另跑 D 项 |
| E02 下载拓扑交叉矩阵 | artifact store `fs|s3` × runtime CoW `xfs|s3` 四格分别创建独立 template；每格以从未下载过的新节点 B 做 redo/distribution，再真实创建并运行 sandbox。先记录节点 S3 CoW 能力和 TC owner | definition 中 artifact storage 与 runtime backend 分别匹配；B 对源字节的实际请求是 owner TC 或对象存储，CubeMaster 发 ticket 但未传 ext4 body；落盘 size/SHA 与 artifact row 和源对象一致；guest run marker 成功。缺任一 backend 配置不得把别格结果代替 | 四格 live 都未执行则分别 NOT_RUN/BLOCKED；不合并报告为“fs/S3 已通过” |
| E03 正常 ticket / replica binding | 由真实 redo 为 B 建立 replica；记录 artifact ID、generation、node ID；运行 Cubelet 真正取票并下载。再给无 replica 的 C 发起新的取票请求 | 已授权 B 获 fresh ticket，响应无缓存；C 不能获取有效票据；B 的 TC/S3 请求头不含 Master/TC component secret；获取的字节摘要可重算一致 | httptest 不能证明部署配置、DB replica 一致性或真实网络路径 |
| E04 ticket 身份/生命周期负例 | 在隔离客户端依次测试：缺 node/token header、改 node ID、换 artifact ID、download token 当 refresh token、过期 refresh、伪签名、撤销/删除 B replica、非 READY artifact、错误 generation、ticket 到期后重新 refresh；每次只改一项。S3 已签 URL 另记其签名有效期，不声称即时撤销 | 分层记录预期状态：字段缺失在 TC 节点入口 400，ticket 不授权 403，未知 artifact 404，非 READY 410；token 不应跨 node/artifact/generation/purpose 重用；删除 replica 后再取票拒绝。保存 response code/header 与 DB 行前后值，不保存 token | 每个负例单独 PASS/FAIL；没能撤销已签 S3 URL不算代码失败，若产品要求立即撤销须另立协议需求 |
| E05 GET/HEAD/Range/ETag | 用同一 FS artifact 经节点票据入口执行 HEAD、全量 GET、`Range: bytes=0-1023`、尾部 Range、越界 Range、matching/mismatching `If-Range`；再对 S3 使用实际 GET presigned URL；断开一次传输后等待合法短 TTL 后重新取票续传 | HEAD 无 body 且 size/ETag 合理；206 的 Content-Range 与返回字节完全吻合；越界返回 416；matching If-Range 给 partial，validator 不匹配走完整响应；重新取票只继续使用合法区间，最终 SHA/size 一致。不得拿 S3 GET URL 的 HEAD 成功代替 GET | TC ETag fixture 有 SHA 不代表这些部署场景已过；未能构造自然到期/断点条件记 NOT_RUN |
| E06 Master 旧入口/字节路径 | 经服务路由分别 GET/HEAD 旧 `/cube/template/artifact/download` 与 Master 普通 API；同时抓取合法 S3/TC 请求的状态、字节数及服务端 access log | Master 旧 artifact download route 不注册；Master 请求只签票，不产生 ext4 bytes；TC 组件认证 GET/HEAD 是 TC 内部路径，Cubelet FS GET 用独立 node-ticket 路径。按 trace、端口/服务和对象请求记录来源，不能因 Master 与 TC 同机就合并计数 | 非目标版本/旧进程测试结果无效；禁止将共享 IP 的总字节量直接当 Master 代理 |
| E07 V01：迟到 RUNNING/status-less progress | 用正常真实构建产生 job；受控代理先确认 TC 原回调已收到并快速 ACK，再等待 DB 显示 `RUNNING/DISTRIBUTING` 后向 Master 重放 job 的旧 `RUNNING/BUILDING_EXT4`、status-less progress。随后并发/重复投递完全相同和 artifact/result 不同的 BUILT；每次保存 before/after job row | phase 不回退；迟到 progress 不覆盖 artifact/result；BUILT 后分发 claim 只有一个赢家，只有一条有效 CreateImage/分发链，唯一 replica/最终 definition；HTTP 重放为预期幂等响应。比较两个 Master 时必须共享同一真实 SQL DB，而非各自 SQLite | 没有 job 定向屏障/到达确认则 BLOCKED；当前代码未对迟到 FAILED 作同样保证，它必须单列风险，不推断已覆盖 |
| E08 V01：失败与响应丢失边界 | 真实隔离 job 上分别制造（a）BUILT 写库成功但 HTTP response 丢失；（b）注册 artifact DB 临时错误使 Master 返回 5xx；（c）TC retry 同一 BUILT；（d）重复 READY/FAILED 终态 callback；（e）BUILT 与 FAILED 交错。不要同时注入两种故障 | (a) retry 后只有一次 continuation；(b) job 保持可重放状态、成功 retry 后恢复；终态不可被旧非终态复活；FAILED 与 RUNNING 结果按当前实现逐项记录。对状态不变量失败即报 bug，不能仅凭 HTTP 200 判定落库成功 | 这组揭示代码当前未覆盖的迟到 FAILED 行为；该场景通过也不自动扩大当前补丁承诺 |
| E09 V02 locator 变化 / stale 404 | 准备 Master 真正可读的 legacy ext4 源和隔离目的 TC/S3；确认旧 HEAD/Stat 请求到达屏障后发真实 `tpl merge`，等待同一 artifact ID locator 改变、迁移 job 完成，再释放旧探测为 404；原文件/source保持到 CAS 完成 | generation/status 相同但 locator 任一字段变化，旧 probe CAS 的 rows affected 为 0，不改新行状态/error；新 locator 再探测；新的 cold B 按新地址下载且 SHA/size 一致。通过归属正确的 MySQL/PG 行快照证据关联 migration job 与 artifact ID | 无 legacy 源/可控旧请求/可观测 CAS 或冷 B 时 BLOCKED；两个 TC 私盘互相拷贝不是 `tpl merge` 的前置/能力 |
| E10 V02 当前 locator 结果分类 | 分别对当前 locator 产生真实 404、403、timeout/503、DB error；同一 locator 成功两次后切换 storage_ref/object_key/owner URL，再请求一次 | 仅当前 locator 的权威 not-found + READY 行完整匹配才按规则 demote；未知网络/鉴权/DB 故障不得写 FAILED；切换 locator 后 positive cache 不沿用旧结论，新的 404 不能污染别的 generation/owner | 每种状态需独立执行；用 stub 返回序列仅算组件测试 |
| E11 CLI backend/JSON override | 对真实 `tpl create --file` 测 JSON=`xfs`、`s3`、未给 backend 三种，再显式覆盖；`tpl build` 各测 xfs/s3；输入 cos/minio/fs/other 和大小写变体；单列 jfs | 未传 flag 保留 file JSON 值；显式 `xfs|s3|jfs` API payload/注解一致；非法值在发送 HTTP 前失败且 DB 无新 job；s3/jfs 均须核对节点实际能力，不能仅靠参数 echo | 现有 httptest 对 create/build 显式传 backend，不能证明 `--file` 未传 flag 的真实 request；无对应节点能力时真实组合 BLOCKED |
| E12 真实 DB 列表小/中档 | 由负载库真实 Master 入口，先 0/1/49/50/51/199/200/201 visible ID，再混 definitions/job-only/多 attempt/hidden snapshot/同 timestamp；CLI JSON 逐页抓同一 requestID 和响应 | 按 SQL 可见规则对照独立预期 ID 集；精确 total 等于唯一 visible ID；(sort_ts DESC,template_id ASC) 稳定；job-only 只出最高 attempt；`skip_total` 页内容与 count 页相同，total=-1、has_more 真实；页间静态集合无重复/漏项 | SQLite/httptest 不算 MySQL/PG；ID 具体排序 tie 必须按 SQL 而不是 DB 默认顺序 |
| E13 真实 DB 列表 220001+ | 按 §8.3 建独立负载库，至少 220001 可见唯一 ID；真实 CLI/HTTP 分 limit 50/200、offset 0/5000/50000/100000/200000/末页/越界和 total/no-total 冷热请求；核对唯一 ID，不用行数代替可见数 | 返回 ID 正确且静态无重漏；记录 status+ret_code、requestID、首屏/P50/P95、COUNT/页查询耗时、Master RSS/DB CPU/IO/扫描与 EXPLAIN；指标达到预审批预算才 PASS。该提交未修改查询/索引，性能结果只评本环境可接受度 | 没独立 DB/隔离 Master/预算记 BLOCKED；无真实 entry request 只为 NOT_RUN，不能用 EXPLAIN PASS |
| E14 CLI --all、取消和异常分页 | 小规模固定集真实 CLI `--all` 表格（逐页 flush）与 `--all --json`（聚合）对比；代理仅在获批测试链路分别延迟下一页、重复页、错 offset、空页+has_more，发 SIGINT/timeout | 表格第一屏确实早于后续页；JSON 总数/顺序正确且 RSS 在预算内；重复/错位响应明确退出而不死循环；Ctrl+C/timeout 在边界内停止，记录退出码和最后请求 ID | 代理注入仅证明 CLI 协议容错，不计数据库性能 PASS；没有代理记 NOT_RUN。22 万全量 JSON 未经内存预算不跑 |
| E15 删除/引用/GC 生命周期 | 真实建 artifact 后分别保留 definition、replica、placement、snapshot、BUILT job 的引用，按公开删除；释放最后引用后按实际 GC 期限等待；TC 私盘与共享 storage 分开跑 | 有任一 durable ref 对象和行均保留；最后引用释放后进入预期 cleanup 状态且在规定窗口收敛；失败保留 retry/error，而不是丢 locator；TC 本地 GC 不删 active/publish marker/path traversal | 删除、缩短 GC TTL 或构造 orphan 需专用环境/审批；无等待窗口记 NOT_RUN。区分 Master artifact GC 和 TC 文件 GC |
| E16 COMMIT origin | 真实 sandbox 写 rootfs 文件并起驻留进程 marker；使用 API client 固定 request ID 调 commit，跟踪 build status 到 origin READY/PARTIALLY_READY，在有支持的 origin 路径恢复并核对文件和内存 marker | build_id/template_id/request/job/replica 的状态相符；源沙箱仍可用；原点恢复完整验证数据；重复 request 的幂等必须客户端重放完全相同 request ID/payload，而不是重复 CLI（CLI 默认生成 UUID） | 未接通 origin restore 标 BLOCKED；跨节点暖包恢复仍 BLOCKED，不能以 cold redo 替代 |
| E17 新模板 SDK 生命周期 | E01 的现有 pytest 测试需确认实际收集并执行；在结果报表保留测试名、结尾清理和 failure artifact | 断言 sandbox 用的是新 `template_id`，probe 真实健康；run marker/命令 rc/文件内容都满足；后续 D01/D02 冷下载需另有独立证据 | `py_compile`/收集测试不算 live PASS；缺 endpoint/image/backend 记具体 BLOCKED/SKIP |

旧报告/旧 checkout 中的 FAIL 只表示当时 SHA。本次待验版本的 V01/V02 及 SDK 用例只按本矩阵真实执行结果更新；不得预先写“已修复/已通过”。E01/E17 是同一现有用例，不重复计数。

### 11.3 屏障和故障预算

- 不向生产协议随意添加 pause/crash 接口。优先依赖注入、隔离HTTP/store、进程内channel；集群hook仅测试构建可用，默认不编入生产。
- 每个barrier提供到达确认、有限等待、释放/取消、日志关联ID。未收到到达确认记 NOT_RUN，不能把随意延迟后正常结束算覆盖。
- 每个进程重启/网络故障只作用于登记的隔离实例；事前确认操作者、最大故障时间、恢复步骤、失败资源保留策略。
- `RUN_ID → request_id → template/job/artifact/generation → node/TC ID` 串联请求、DB状态、存储bytes、落盘SHA和沙箱断言。捕获的是脱敏元信息，不记录token原文。

### 11.4 执行门禁

1. 先核对本次部署 SHA 与运行配置；清单中的旧 FAIL/SKIP 不转抄成本次结果。目标提交每项回归重新执行，失败保持原始证据。
2. 运行 Linux 原生与真实 MySQL/PG 组件门禁；检查实际运行的测试名、数量、skip、race、DB 容器和退出码。编译通过不能标测试通过。
3. 先跑 E01 真实新模板单节点闭环；之后按 artifact store × runtime backend 的独立配置跑 E02–E06。READY、health=200 或暖副本命中不能替代冷下载字节与 SHA 证据。
4. 再跑 V01/V02 屏障交错、真实数据库 22 万列表入口和分页协议；缺授权/屏障/独立负载 Master 时逐项记 BLOCKED/NOT_RUN，不用 SQLite 或直 SQL补位。
5. GC、COMMIT、双 Master/TC 故障依照隔离资源等级单独执行；暖 COMMIT 跨节点明确 BLOCKED，不把未实现能力标成 PASS。
6. 所有结果按 RUN_ID 与 resource ID 核对；先封存脱敏证据，人工审批后清理本次资源并确认收敛，不吞异常、不清理用户数据。

运行前由环境负责人填写实际控制面/计算节点清单、服务角色和隔离关系；节点数量本身不等于覆盖能力，必须证明测试 job 可落到目标节点且可观察下载来源。本指南不会自动启动 live 测试、切换配置或部署。
