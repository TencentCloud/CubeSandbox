# 模板可靠性修复：AI 远程部署与 E2E 验收任务书

> 本文可交给另一个 AI 作为验收计划；当前不得直接发布。
>
> 更新日期：2026-11-05。
> 已核对源码：`3cbc88c1`，父提交 `12405579`。执行前设置开发方明确交付的 `CODE_BASE`，不得自动使用漂移的分支 HEAD。
> 当前提交不包含 `tests/e2e/template_center/verify-reliability.sh`；不得尝试执行或假定其存在。
> 本次实际结果与阻塞项见 [`template-reliability-cross-check-20261105.md`](template-reliability-cross-check-20261105.md)。文档本次修改尚未提交/推送，不代表远端文件已更新。
> 测试分支：`test/template-reliability-318ee986`。
> 完整场景定义与判定标准见 [`template-reliability-test-guide.md`](template-reliability-test-guide.md)。
> 本文只规定远程拓扑、发布顺序、操作门禁和 E2E 执行流程，不代表这些场景已经通过。

## 0. 任务目标与固定拓扑

| 角色 | 地址 | 允许执行的操作 |
|---|---|---|
| 构建机兼控制面 | `9.135.79.34` | 在隔离 worktree 中测试/构建；部署 CubeMaster、CubeMaster CLI、CubeTemplateCenter；34 的 Cubelet 保持 mask |
| 计算节点 | `9.135.10.216` | 部署 Cubelet；执行模板分发、下载和沙箱运行验证 |

SSH 端口与认证方式使用已有可用配置，**不能假定 34 与 216 使用相同端口**。

本次目标包括：

1. 从指定提交统一构建 Master、CLI、TC、Cubelet。
2. 完成组件专项、构建门禁和版本取证。
3. 备份、原子替换、逐组件重启，并提供明确回滚路径。
4. 验证 CLI backend、数据库分页、TC 构建/回调、Cubelet artifact 下载、真实沙箱和 SDK E2E。
5. 对当前拓扑无法覆盖的双节点、双 Master、双 TC、双数据库场景明确报告 `BLOCKED`，不能用单节点结果冒充通过。

结果状态只能使用：

- `PASS`：目标测试确实运行，退出码和业务证据均符合预期；包通过不代表包内所有用例均执行，须单独记录 `SKIP`。
- `COMPILE_PASS`：仅交叉编译成功，不是测试运行通过，不能作为发布或 E2E 门禁。
- `FAIL`：测试、发布、业务断言或安全约束不符合预期。
- `SKIP`：有明确前置条件缺失或测试主动跳过。
- `NOT_RUN`：尚未执行或故障条件没有触发。
- `BLOCKED`：能力未实现或当前拓扑不具备验证条件。

## 1. 强制安全边界

1. 不得在业务数据库、业务 bucket/prefix 或业务节点执行故障注入和批量夹具创建。
2. 无法确认测试资源隔离时，只执行只读预检并停止，相关场景报告 `BLOCKED`。
3. 不得输出 `.one-click.env`、Secret、数据库密码、S3 AK/SK、ticket、refresh token 或带签名 URL。
4. 不得 `reset`、`rebase` 或修改两台机器原有源码工作区；构建必须使用新的临时 worktree。
5. 不得删除 `.codebuddy`、业务对象、业务 DB 行或现有活跃 rootfs 缓存。
6. 不得解除 34 上已 mask 的 Cubelet 来凑第二个计算节点。
7. 发布前必须备份真实运行二进制并记录 SHA256；任何组件失败立即回滚，不继续后续步骤。
8. 不得将服务 `active`、TC `/health=200` 或模板 `READY` 单独作为功能通过证据。
9. 普通暖 COMMIT 跨节点能力仍未完整接通，必须报告 `BLOCKED`，不得用冷镜像重建冒充暖恢复。
10. `cos/minio` 只是 CLI 到现有 S3-compatible backend 的别名，不代表自动切换 endpoint、bucket、凭证或支持每模板存储 profile。

## 2. 发布测试分支

本轮由开发机提交并以非 force 方式推送到 `test/template-reliability-318ee986`；验证 AI 不再提交或推送。交付消息中的提交 SHA 是唯一验收目标，不得自动验收之后发生漂移的分支 HEAD。

在验证环境设置并核对：

```bash
export CODE_BASE='<交付消息提供的提交SHA>'
export BRANCH=test/template-reliability-318ee986
```

第 4 节会 fetch、检查祖先关系并将 worktree 固定到 `CODE_BASE`。若交付未推送、SHA 无法解析或脚本缺失，停止并报告 `BLOCKED`。

## 3. 两台机器只读预检

先设置本地连接变量，端口由执行环境填写：

```bash
export BUILD_HOST=9.135.79.34
export CONTROL_HOST="$BUILD_HOST"
export COMPUTE_HOST=9.135.10.216
export BUILD_SSH_PORT='<34实际SSH端口>'
export CONTROL_SSH_PORT="$BUILD_SSH_PORT"
export COMPUTE_SSH_PORT='<216实际SSH端口>'
```

### 3.1 构建/控制面主机 34：仓库与构建环境

```bash
ssh -p "$BUILD_SSH_PORT" "root@$BUILD_HOST" '
  set -e
  hostname
  uname -a
  git -C /root/CubeSandbox status --short
  git -C /root/CubeSandbox rev-parse --short HEAD
  docker info --format "{{.ServerVersion}}"
  df -h /tmp /root
'
```

### 3.2 构建/控制面主机 34：在线服务

```bash
ssh -p "$CONTROL_SSH_PORT" "root@$CONTROL_HOST" '
  set -e
  hostname
  uname -a
  systemctl show \
    cube-sandbox-cubemaster.service \
    cube-sandbox-cube-templatecenter.service \
    -p ActiveState -p SubState -p MainPID -p ExecStart --no-pager
  ss -lntp | grep -E ":(3000|8089|8090)\\b" || true
  curl -sS -o /dev/null -w "TC health=%{http_code}\\n" \
    --connect-timeout 2 --max-time 5 \
    http://127.0.0.1:8090/health
'
```

通过 `ExecStart` 和启动脚本确认真实二进制路径。不要先入为主地替换某个文件。

### 3.3 计算节点 216

```bash
ssh -p "$COMPUTE_SSH_PORT" "root@$COMPUTE_HOST" '
  set -e
  hostname
  uname -a
  systemctl show cube-sandbox-cubelet.service \
    -p ActiveState -p SubState -p MainPID -p ExecStart --no-pager
  systemctl is-enabled cube-sandbox-cubelet.service || true
  ps -ef | grep -E "[c]ubelet|[c]ube-shim|[c]ube-vs"
  df -h
  free -h
'
```

部署 Cubelet 前必须确认 216 是专用测试节点、没有业务 sandbox，且允许安全重启。

## 4. 34 创建隔离 worktree

在 34 执行：

```bash
set -euo pipefail

REPO=/root/CubeSandbox
BRANCH=test/template-reliability-318ee986
: "${CODE_BASE:?请先设置交付消息提供的提交SHA}"
RUN_ID="$(date -u +%Y%m%d-%H%M%S)"
WORKTREE="/tmp/CubeSandbox-template-reliability-$RUN_ID"
EVIDENCE="/root/template-reliability-evidence-$RUN_ID"

install -d -m 0700 "$EVIDENCE"

git -C "$REPO" fetch origin \
  "refs/heads/$BRANCH:refs/remotes/origin/$BRANCH"

git -C "$REPO" merge-base --is-ancestor "$CODE_BASE" "refs/remotes/origin/$BRANCH"
SOURCE_HEAD="$(git -C "$REPO" rev-parse "$CODE_BASE^{commit}")"
test ! -e "$WORKTREE"

git -C "$REPO" worktree add --detach "$WORKTREE" "$SOURCE_HEAD"

test -z "$(git -C "$WORKTREE" status --short)"
test -f "$WORKTREE/docs/dev/template-reliability-test-guide.md"
test -f "$WORKTREE/CubeTemplateCenter/pkg/build/local_gc.go"

grep -n "req.Header = make(http.Header)" \
  "$WORKTREE/Cubelet/internal/cube/server/images/ext4image/utils.go"

git -C "$WORKTREE" rev-parse HEAD > "$EVIDENCE/source-head.txt"
git -C "$WORKTREE" status --short > "$EVIDENCE/source-status.txt"
uname -a > "$EVIDENCE/platform.txt"
```

如果代码基线不存在、工作树不干净或关键文件缺失，立即停止。

## 5. 34 组件门禁

`3cbc88c1` 未包含文档先前引用的 `tests/e2e/template_center/verify-reliability.sh`，也没有可核验的 `--full` 实现。不得用文档中的脚本描述代替交付文件或验收结果。

当前使用 [测试指南第 3 节](template-reliability-test-guide.md#3-专项测试与日志保存) 的人工专项命令；先核对隔离环境、测试集合和实际退出码。复现报告中的两个反例及 TC ETag 夹具问题未解决前，停止发布。

逐项保留 `PASS`、`FAIL`、`SKIP`、`COMPILE_PASS` 和 `NOT_RUN`；原生 Linux 实际运行 TC/Cubelet 测试，macOS 的交叉编译不得计为运行通过。远程 E2E 状态必须另行记录，不能由组件单测推出。

发布门禁仍需使用仓库 builder，避免误用 34 上旧 Go：

```bash
set -euo pipefail
cd "$WORKTREE"

make cubemaster-test \
  > "$EVIDENCE/cubemaster-test.log" 2>&1

make cubetemplatecenter-test \
  > "$EVIDENCE/cubetemplatecenter-test.log" 2>&1

make cubelet-test \
  > "$EVIDENCE/cubelet-test.log" 2>&1
```

任何命令退出非零都停止发布。检查日志中是否存在 `SKIP`、`FAIL`、`panic` 或 `[no tests to run]`，并记录实际测试数。

Cubelet ext4 下载至少必须覆盖以下专项：

```bash
cd "$WORKTREE"

make builder-image
make builder-run BUILDER_CMD='
  cd /workspace &&
  IN_CUBE_SANDBOX_BUILDER=1 make cubecow-sdk &&
  cd /workspace/CubeNet/cubevs &&
  make gen &&
  cd /workspace/Cubelet &&
  go mod download &&
  make proto &&
  go test -count=1 \
    ./internal/cube/server/images/ext4image \
    -run "^(TestFetchMasterTicketUsesReachableMaster|TestFetchHeaderTicketAndDownload|TestFetchTicketRejectsCredentialInjection|TestArtifactDownloadRejectsCrossOriginRedirect|TestFetchS3TicketDoesNotForwardInternalHeaders|TestFetchArtifactTicketRejects(ExpiredOrWrongIdentity|UnexpectedOrigin)|TestDownloadArtifact(RefreshesTicketAndResumes|StopsOnTicketUnauthorized|RejectsMismatchedContentRange|WithIndependentKernelSource)|TestRewriteDownloadHost(LeavesS3PresignedURLUntouched|RewritesCubeMasterRoute|RewritesTicketURL|LeavesTemplateCenterURLUntouched|EmptyEndpointPassesThrough)|TestIsS3PresignedURLDetectsSigV4Signature)$"
' > "$EVIDENCE/cubelet-ext4-test.log" 2>&1
```

按 `3cbc88c1` 预期匹配 17 个顶层测试，无 panic、FAIL、SKIP；必须核对实际集合。本次 macOS 只交叉编译该包，不代表这些用例已在 Linux 运行。

## 6. 34 统一构建

```bash
set -euo pipefail
cd "$WORKTREE"

make cubemaster \
  > "$EVIDENCE/build-master.log" 2>&1

make cubetemplatecenter \
  > "$EVIDENCE/build-tc.log" 2>&1

make cubelet \
  > "$EVIDENCE/build-cubelet.log" 2>&1

for binary in cubemaster cubemastercli templatecenter cubelet; do
  test -x "$WORKTREE/_output/bin/$binary"
done

sha256sum \
  "$WORKTREE/_output/bin/cubemaster" \
  "$WORKTREE/_output/bin/cubemastercli" \
  "$WORKTREE/_output/bin/templatecenter" \
  "$WORKTREE/_output/bin/cubelet" \
  | tee "$EVIDENCE/binaries-built.sha256"

cp "$EVIDENCE/binaries-built.sha256" \
  /tmp/template-reliability-318ee986.sha256
```

四个组件必须来自同一个 worktree 和同一次代码版本，不能在 34 与 216 分别从不同仓库构建。

## 7. 暂存控制面产物并分发 Cubelet

在 34 执行。控制面就在本机，因此直接复制到权限受限的暂存目录；只向 216 传输 Cubelet：

```bash
set -euo pipefail

STAGE=/tmp/template-reliability-318ee986
install -d -m 0700 "$STAGE"

install -m 0755 "$WORKTREE/_output/bin/cubemaster" "$STAGE/cubemaster"
install -m 0755 "$WORKTREE/_output/bin/cubemastercli" "$STAGE/cubemastercli"
install -m 0755 "$WORKTREE/_output/bin/templatecenter" "$STAGE/templatecenter"
cp /tmp/template-reliability-318ee986.sha256 "$STAGE/"

ssh -p "$COMPUTE_SSH_PORT" "root@$COMPUTE_HOST" \
  'install -d -m 0700 /tmp/template-reliability-318ee986'

scp -P "$COMPUTE_SSH_PORT" \
  "$WORKTREE/_output/bin/cubelet" \
  /tmp/template-reliability-318ee986.sha256 \
  "root@$COMPUTE_HOST:/tmp/template-reliability-318ee986/"
```

34 和 216 分别重新计算 SHA256 并与构建清单相应行匹配；不匹配则停止。

## 8. 34 发布前基线与路径确认

继续在 34 执行。以下路径是常见 one-click 布局，必须先根据实际 `ExecStart` 验证：

```bash
set -euo pipefail

RUN_ID="$(date -u +%Y%m%d-%H%M%S)"
EVIDENCE="/root/template-reliability-control-$RUN_ID"
BACKUP="/usr/local/services/cubetoolbox/backups/template-reliability-$RUN_ID"
STAGE=/tmp/template-reliability-318ee986

TC_BIN=/usr/local/services/cubetoolbox/CubeTemplateCenter/bin/templatecenter
MASTER_BIN=/usr/local/services/cubetoolbox/CubeMaster/bin/cubemaster
CLI_BIN=/usr/local/services/cubetoolbox/CubeMaster/bin/cubemastercli

install -d -m 0700 "$EVIDENCE" "$BACKUP"

for path in "$TC_BIN" "$MASTER_BIN" "$CLI_BIN"; do
  test -x "$path"
done

systemctl show \
  cube-sandbox-cubemaster.service \
  cube-sandbox-cube-templatecenter.service \
  -p ActiveState -p SubState -p MainPID -p ExecStart --no-pager \
  > "$EVIDENCE/services-before.txt"

sha256sum "$TC_BIN" "$MASTER_BIN" "$CLI_BIN" \
  > "$EVIDENCE/binaries-before.sha256"

curl -sS -o /dev/null -w '%{http_code}\n' \
  --connect-timeout 2 --max-time 5 \
  http://127.0.0.1:8090/health \
  > "$EVIDENCE/tc-health-before.txt"

test "$(cat "$EVIDENCE/tc-health-before.txt")" = 200
```

确认当前没有正在运行的模板 build、redo、commit 或 delete。无法确认时暂停发布。

## 9. 34 部署 TC

```bash
set -euo pipefail

cp -a "$TC_BIN" "$BACKUP/templatecenter"
sha256sum "$TC_BIN" > "$BACKUP/templatecenter.sha256"

"$STAGE/templatecenter" -v \
  > "$EVIDENCE/tc-staged-version.txt" 2>&1 || true

install -m 0755 "$STAGE/templatecenter" "${TC_BIN}.new"
mv -f "${TC_BIN}.new" "$TC_BIN"

systemctl restart cube-sandbox-cube-templatecenter.service

code=000
for _ in $(seq 1 30); do
  code="$(curl -sS -o /dev/null -w '%{http_code}' \
    --connect-timeout 2 --max-time 4 \
    http://127.0.0.1:8090/health || true)"
  [ "$code" = 200 ] && break
  sleep 2
done

test "$code" = 200
systemctl is-active --quiet cube-sandbox-cube-templatecenter.service

TC_PID="$(systemctl show \
  cube-sandbox-cube-templatecenter.service \
  -p MainPID --value)"

readlink -f "/proc/$TC_PID/exe" > "$EVIDENCE/tc-process-exe.txt"
sha256sum "$TC_BIN" > "$EVIDENCE/tc-after.sha256"
"$TC_BIN" -v > "$EVIDENCE/tc-version-after.txt" 2>&1 || true

journalctl \
  -u cube-sandbox-cube-templatecenter.service \
  --since '10 minutes ago' --no-pager \
  > "$EVIDENCE/tc-after-deploy.log"
```

失败立即回滚：

```bash
systemctl stop cube-sandbox-cube-templatecenter.service
cp -a "$BACKUP/templatecenter" "$TC_BIN"
systemctl start cube-sandbox-cube-templatecenter.service
curl -fsS --max-time 5 http://127.0.0.1:8090/health
```

回滚后停止整个发布流程并报告 `FAIL`。

## 10. 34 部署 Master 与 CLI

TC 健康后执行：

```bash
set -euo pipefail

cp -a "$MASTER_BIN" "$BACKUP/cubemaster"
cp -a "$CLI_BIN" "$BACKUP/cubemastercli"
sha256sum "$MASTER_BIN" "$CLI_BIN" \
  > "$BACKUP/master-cli.sha256"

install -m 0755 "$STAGE/cubemaster" "${MASTER_BIN}.new"
install -m 0755 "$STAGE/cubemastercli" "${CLI_BIN}.new"

mv -f "${MASTER_BIN}.new" "$MASTER_BIN"
mv -f "${CLI_BIN}.new" "$CLI_BIN"

systemctl restart cube-sandbox-cubemaster.service
systemctl is-active --quiet cube-sandbox-cubemaster.service

MASTER_PID="$(systemctl show \
  cube-sandbox-cubemaster.service \
  -p MainPID --value)"

readlink -f "/proc/$MASTER_PID/exe" \
  > "$EVIDENCE/master-process-exe.txt"

sha256sum "$MASTER_BIN" "$CLI_BIN" \
  > "$EVIDENCE/master-cli-after.sha256"

"$MASTER_BIN" -v > "$EVIDENCE/master-version-after.txt" 2>&1 || true
"$CLI_BIN" --version > "$EVIDENCE/cli-version-after.txt" 2>&1 || true

journalctl \
  -u cube-sandbox-cubemaster.service \
  --since '10 minutes ago' --no-pager \
  > "$EVIDENCE/master-after-deploy.log"
```

失败立即回滚：

```bash
systemctl stop cube-sandbox-cubemaster.service
cp -a "$BACKUP/cubemaster" "$MASTER_BIN"
cp -a "$BACKUP/cubemastercli" "$CLI_BIN"
systemctl start cube-sandbox-cubemaster.service
systemctl is-active --quiet cube-sandbox-cubemaster.service
```

## 11. 34 只读 Smoke

```bash
set -euo pipefail

CLI="$CLI_BIN"

cm() {
  "$CLI" \
    --address 127.0.0.1 \
    --port 8089 \
    --timeout 2m "$@"
}

cm tpl create --help > "$EVIDENCE/cli-create-help.txt"
cm tpl build --help > "$EVIDENCE/cli-build-help.txt"
cm tpl list --help > "$EVIDENCE/cli-list-help.txt"

grep -q -- '--backend' "$EVIDENCE/cli-create-help.txt"
grep -q -- '--backend' "$EVIDENCE/cli-build-help.txt"
grep -q -- '--no-total' "$EVIDENCE/cli-list-help.txt"
grep -q -- '--all' "$EVIDENCE/cli-list-help.txt"

cm tpl list \
  --limit 50 --offset 0 --no-total --json \
  > "$EVIDENCE/list-light.json"

jq -e '
  (.total == -1) and
  (.has_more | type == "boolean") and
  ((.data // [] | length) <= 50)
' "$EVIDENCE/list-light.json"

cm tpl list \
  --limit 1000 --offset 0 --no-total --json \
  > "$EVIDENCE/list-clamp.json"

jq -e '
  (.limit == 200) and
  ((.data // [] | length) <= 200)
' "$EVIDENCE/list-clamp.json"

cm tpl list --json > "$EVIDENCE/list-default.json"

jq -e '
  (.limit == 50) and
  ((.data // [] | length) <= 50)
' "$EVIDENCE/list-default.json"
```

不要在海量结果上执行 `--all --json`。

## 12. 216 发布 Cubelet

先根据 `ExecStart` 确认真实路径。以下是常见路径，不能未核对直接使用：

```bash
set -euo pipefail

RUN_ID="$(date -u +%Y%m%d-%H%M%S)"
EVIDENCE="/root/template-reliability-compute-$RUN_ID"
BACKUP="/usr/local/services/cubetoolbox/backups/template-reliability-$RUN_ID"
STAGE=/tmp/template-reliability-318ee986
CUBELET_BIN=/usr/local/services/cubetoolbox/Cubelet/bin/cubelet

install -d -m 0700 "$EVIDENCE" "$BACKUP"

test -x "$CUBELET_BIN"
test -x "$STAGE/cubelet"

systemctl show cube-sandbox-cubelet.service \
  -p ActiveState -p SubState -p MainPID -p ExecStart --no-pager \
  > "$EVIDENCE/cubelet-before.txt"

sha256sum "$CUBELET_BIN" > "$EVIDENCE/cubelet-before.sha256"
cp -a "$CUBELET_BIN" "$BACKUP/cubelet"

# 再次确认节点无业务 sandbox 后才能继续。
install -m 0755 "$STAGE/cubelet" "${CUBELET_BIN}.new"
mv -f "${CUBELET_BIN}.new" "$CUBELET_BIN"

systemctl restart cube-sandbox-cubelet.service
systemctl is-active --quiet cube-sandbox-cubelet.service

CUBELET_PID="$(systemctl show \
  cube-sandbox-cubelet.service \
  -p MainPID --value)"

readlink -f "/proc/$CUBELET_PID/exe" \
  > "$EVIDENCE/cubelet-process-exe.txt"

sha256sum "$CUBELET_BIN" > "$EVIDENCE/cubelet-after.sha256"
"$CUBELET_BIN" -v > "$EVIDENCE/cubelet-version-after.txt" 2>&1 || true

journalctl \
  -u cube-sandbox-cubelet.service \
  --since '10 minutes ago' --no-pager \
  > "$EVIDENCE/cubelet-after-deploy.log"
```

失败立即回滚：

```bash
systemctl stop cube-sandbox-cubelet.service
cp -a "$BACKUP/cubelet" "$CUBELET_BIN"
systemctl start cube-sandbox-cubelet.service
systemctl is-active --quiet cube-sandbox-cubelet.service
```

在 34/CubeOps 验证 216 节点在线、心跳正常、地址正确，且未被错误标记为退役。

## 13. E2E 前置条件

在 34 设置：

```bash
export NODE_216='<216对应的真实节点ID>'
export OCI_IMAGE='<固定测试镜像@sha256:digest>'
export CUBE_API_URL='<34上实际CubeAPI地址>'
export CUBE_PROXY_NODE_IP='<实际测试CubeProxy地址>'
export RUN_ID="$(date -u +%Y%m%d-%H%M%S)"
```

必须确认：

1. 216 是专用节点且允许创建/销毁测试 sandbox。
2. 使用独立测试 bucket/prefix。
3. 数据库是测试实例，或当前操作已获明确测试许可。
4. 216 能访问 34 的 Master/TC 及对象存储 node endpoint。
5. 运行时 CoW backend 和 artifact 存储是两个独立维度。
6. S3 CoW 场景开始前，216 的 CubeS3lvol 必须处于 ready。

## 14. xfs 模板闭环

在 34 执行：

```bash
set -euo pipefail

ALIAS_XFS="e2e-$RUN_ID-xfs"

cm tpl build \
  --image "$OCI_IMAGE" \
  --writable-layer-size 4Gi \
  --alias "$ALIAS_XFS" \
  --backend xfs \
  --node "$NODE_216" \
  --detach --json \
  > "$EVIDENCE/create-xfs.json"

JOB_ID="$(jq -er '.job.job_id' "$EVIDENCE/create-xfs.json")"
TEMPLATE_XFS="$(jq -er '.job.template_id' "$EVIDENCE/create-xfs.json")"

cm tpl watch \
  --job-id "$JOB_ID" \
  --interval 2s --json \
  > "$EVIDENCE/create-xfs-watch.json"

cm tpl status \
  --job-id "$JOB_ID" --json \
  > "$EVIDENCE/create-xfs-status.json"
```

通过标准不止是 `READY`：

- TC 构建和 Master 回调成功；
- definition、replica、artifact ID/generation 一致；
- 216 完成下载、校验和加载；
- 从模板创建 sandbox 后能执行真实命令；
- 文件写入、读取和销毁正常；
- 再次从同一模板创建仍成功。

为确保新 artifact，使用新的 alias/规格或新镜像 digest；不得删除 216 的活跃缓存制造冷下载。

## 15. SDK 交叉 E2E

在 34 或可访问 34 CubeAPI 的隔离 runner 执行：

```bash
cd "$WORKTREE/tests/e2e/sdk_compat"

python3 -m venv .venv
. .venv/bin/activate
pip install -r requirements.txt

export CUBE_API_URL='<34实际CubeAPI地址>'
export CUBE_TEMPLATE_ID='<符合E2E要求的READY模板ID>'
export CUBE_PROXY_NODE_IP='<实际CubeProxy地址>'

pytest --run-e2e \
  --sdk-e2e-backends=cubesandbox \
  cases/templates/test_alias.py

pytest --run-e2e \
  --sdk-e2e-backends=cubesandbox \
  -m 'smoke or p0'
```

整套 smoke/p0 所用模板需要满足 README 中 envd/Code Interpreter 端口要求；不满足时相关用例报告 `SKIP`，不能通过关闭前置检查强跑。

## 16. backend 矩阵

逐项验证并使用不同 alias/template ID：

| 输入 | 预期 |
|---|---|
| 不传 `--backend` | JSON 已有值保留；缺省行为按 xfs |
| `--backend xfs` | 请求、列表、定义及节点实际运行一致 |
| `--backend s3` | 仅在 216 已启用 CubeS3lvol 时运行 |
| `--backend cos` | CLI 规范为 s3；实际仍依赖部署好的 S3-compatible 配置 |
| `--backend minio` | CLI 规范为 s3；实际仍依赖部署好的 S3-compatible 配置 |
| `--backend fs/jfs/other` | CLI 拒绝，且没有创建 job |

`cos/minio` 参数解析通过不能代替真实 COS/MinIO endpoint 验收。若没有对应隔离配置，真实存储场景报告 `SKIP` 或 `BLOCKED`。

## 17. proxy/direct 下载链路与授权

默认模式为 `proxy`；仅在隔离环境中按实际配置启用 `direct` 并分别验收。`direct` 不等于 FS 字节绕过 Master：

| 模式/产物 | 正确链路 | 鉴权边界 |
|---|---|---|
| 默认 proxy | 216 → Master 旧模板下载路由 → TC 或对象存储 | 保持旧兼容路径 |
| direct + FS | 216 向 Master 领票 → 同一可达 Master 的 artifact `/ext4` → owner TC | Master 校验节点 capability；转发 TC 时剥离节点票据，仅带既有组件认证 |
| direct + S3 | 216 向 Master 领票 → 对象存储签名 URL | URL 不改写；不能携带内部组件、节点或 refresh 认证 header |

专项验证：

1. refresh 每次重查 artifact/节点/replica 归属；伪造、过期、错误节点和撤销 placement 均不得绕过。
2. capability 签名密钥由 Master 既有组件秘密派生；历史下发的下载 token 不能作为签名秘密。
3. 即使票据宣告不可达 Master A，216 从可达 Master B 领票后也从 B 的固定兄弟 `/ext4` 路由下载；仅允许合法 artifact 路由，拒绝任意路径、错误 ID、query、fragment、userinfo、RawPath。S3 签名 URL 不参与此改写。
4. Range/Content-Range、过期重取和续传、401 停止、size/SHA256 均应有实际字节证据。
5. 大文件慢流超过普通 API WriteTimeout 仍应下载完整；只对文件 GET/HEAD 清除 write deadline，不对普通写 API 全局解除。
6. 控制面探活使用组件认证 HEAD 或 S3 Stat，不要求节点副本、不签发虚拟节点票据；404 与 401/503/网络错误分别判定 Missing/Unknown。
7. 下载完成后沙箱必须真实可执行，不能只检查 READY。

只采网络连接元数据，不抓取认证 payload 或带签名 URL。FS 链路经过 Master 是正确实现，不能按旧版“216 直接从 TC、Master 不传大流”的标准判失败。

无凭证负例在 34 执行（artifact ID 和节点 ID 必须是本轮合法测试夹具）：

```bash
export ARTIFACT_ID='<本轮artifact ID>'

curl -sS -o "$EVIDENCE/ticket-denied.json" \
  -w '%{http_code}\n' \
  "http://127.0.0.1:8089/internal/template/artifacts/$ARTIFACT_ID/download-ticket" \
  -H "X-Cube-Node-ID: $NODE_216"

curl -sS -o /dev/null \
  -w '%{http_code}\n' \
  "http://127.0.0.1:8089/internal/template/artifacts/$ARTIFACT_ID/ext4" \
  -H "X-Cube-Node-ID: $NODE_216"

curl -sS -o /dev/null \
  -w '%{http_code}\n' \
  "http://127.0.0.1:8090/tc/api/v1/artifacts/$ARTIFACT_ID/ext4"
```

前两项应为 403，TC 无组件凭证应为 401。TC 接口不校验节点 capability；不能用节点票据代替组件认证。

## 18. 分页 E2E

```bash
cm tpl list --limit 50 --offset 0 --json \
  > "$EVIDENCE/page-0.json"

cm tpl list --limit 50 --offset 50 --json \
  > "$EVIDENCE/page-50.json"

cm tpl list --limit 50 --offset 0 --no-total --json \
  > "$EVIDENCE/page-light.json"

cm tpl list --all \
  > "$EVIDENCE/list-all.txt" \
  2> "$EVIDENCE/list-all.stderr.log"
```

静态数据集下验证：

- 默认 50，服务端最多 200；
- 相邻页无重复和遗漏；
- `total=-1` 只表示未计数；
- job-only 模板存在时不被丢弃；
- 同模板历史 jobs 不膨胀 total；
- `--all` 表格逐页输出；
- Ctrl+C 和统一 timeout 可停止遍历。

`--all` 已对跨页重复 ID 和空 ID 返回错误，遇到漂移应重试而非静默去重；用 `[A,B]` 后 `[B]` 验证失败退出。OFFSET 仍不是并发写入下的一致性快照，这只能检测部分漂移，不能承诺并发零遗漏。MySQL/PG 的 1万/10万数据、深 offset、EXPLAIN 和并发写入只能在独立测试 DB 执行；否则报告 `BLOCKED`。

## 19. COMMIT

对专用测试 sandbox 操作：

```bash
export SANDBOX_ID='<本轮测试sandbox ID>'

cm tpl commit \
  --sandbox-id "$SANDBOX_ID" \
  --detach --json \
  > "$EVIDENCE/commit.json"

BUILD_ID="$(jq -er '.build_id' "$EVIDENCE/commit.json")"

cm tpl build-watch \
  --build-id "$BUILD_ID" \
  --interval 2s --json \
  > "$EVIDENCE/commit-watch.json"

cm tpl build-status \
  --build-id "$BUILD_ID" --json \
  > "$EVIDENCE/commit-status.json"
```

验证 origin 上 rootfs 文件、可观测内存 marker、真实副本计数和最新 job 来源。普通暖 COMMIT 跨节点固定报告 `BLOCKED`。

## 20. 当前拓扑的预期 BLOCKED 项

| 场景 | 原因 |
|---|---|
| A 节点创建、B 节点冷分发 | 当前仅确认一个计算节点 216 |
| 永久退役节点与临时不可达对照 | 缺第二个专用计算节点或不能安全故障注入 |
| 普通暖 COMMIT 跨节点 | 控制面能力尚未完整接通 |
| 双 TC 私盘 GC | 34 若只有一个 TC 实例则无法验证 |
| 双 Master 并发 fingerprint 注册 | 34 若只有一个 Master 实例则无法验证 |
| MySQL/PG 双数据库对照 | 需要两套独立测试数据库 |
| 真实 COS、MinIO、S3 分别验收 | 需要对应的独立 endpoint/bucket/prefix 配置 |

不得把 34 控制面当作第二个计算节点，也不得启用 34 的 Cubelet 来填补拓扑。

### 20.1 遗留问题与验证优先级

以下是交接时未闭环项，不应因为专项回归通过就标记已解决：

| 优先级/场景 | 交接状态 | 另一 AI 需要验证的断言 |
|---|---|---|
| P1 snapshot-only redo 领票授权 | `NOT_RUN`，授权闭环待确认 | 只有 snapshot 引用、无普通 replica 的恢复是否有合法下载授权；不得绕过 artifact/node 归属检查 |
| P1 普通 restore 丢失 rootfs 缓存 | `NOT_RUN`，refresh 凭据补发待确认 | 仅对专用测试缓存制造冷恢复，检查是否能补发凭据、下载并运行；不能用其他 build 重建代替 restore |
| P1 普通暖 COMMIT 跨节点 | `BLOCKED`，能力尚未完整接通 | 保持明确阻断；不能用镜像冷启动冒充内存/文件暖恢复 |
| P1 多节点冷分发与永久退役对照 | 当前拓扑 `BLOCKED` | 需第二个获批专用计算节点，34 Cubelet 保持 mask |
| P1 真实存储迁移/复用/GC | 远程 `NOT_RUN`；无隔离资源则 `BLOCKED` | FS→S3、跨 TC owner、旧 READY 确认 Missing/Unknown、迁移 CAS 失败保留源、共享引用拒绝误删 |
| P2 双 Master/TC 与 MySQL/PG | 当前拓扑 `BLOCKED` | READY ABA、generation 并发改变、fingerprint 竞争、双 TC 私盘回收、真实数据库分页性能 |
| P2 OFFSET 并发写入 | 已知一致性限制 | 重复/空 ID 必须失败，但没有 snapshot/cursor 协议，仍可能漏项；报告限制而非宣称完整一致 |
| P2 COS/MinIO/S3 分别运行 | 无对应隔离配置则 `BLOCKED` | 参数别名解析与实际 endpoint/bucket 验收分开；不动态切换生产凭证 |
| Linux TC/Cubelet 原生运行与完整包回归 | 非 Linux 交付机只能 `COMPILE_PASS` | 在 Linux builder 实际运行，报告测试数量、skip、失败和依赖；不能使用 `-exec=true` 冒充执行 |
| 远程部署、慢流故障注入、真实 sandbox/SDK E2E | 本轮 `NOT_RUN` | 依本文门禁执行，保存字节/SHA、业务输出和回滚证据 |

### 20.2 本轮修复需重点复核

- CLI backend 贯穿请求；`cos/minio` 仍是现有 s3 backend 别名。
- artifact ID 保留完整 UUID、短 fingerprint 输入不 panic；已发布 ext4 不被构建覆盖，TC 同内容上传幂等、异内容返回 409。
- TC 复用先校验 backend/locator/key 归属；旧 READY 仅在确认 Missing 后 CAS 转 FAILED，Unknown 保持原数据并返回可重试错误；同 artifact 的 BUILT 重放保留既有下载 token。
- 迁移采用独立对象 key 和原 generation/status/locator/owner/摘要 CAS；跨 TC 更新实际目标 owner 和稳定 `file://` locator；CAS 失败不删除源文件。
- 下载模式、票据秘密分离、可信领票端点、组件鉴权 HEAD、Range 续传和慢流标准以第 17 节为准。

本轮没有执行 SSH、远程部署、真实 DB/bucket 操作或 sandbox 创建。下一 AI 若发现问题，记录最小复现、脱敏请求元数据、实际版本和失败日志；停止对应场景，不自行修改代码后继续宣称原提交通过。

## 21. 清理

1. 先保存脱敏证据和失败资源清单。
2. 释放本轮测试 sandbox。
3. 确认不再需要 snapshot 后，再删除本轮测试 template。
4. 等待正常后台清理，核对 DB、对象存储及 TC 管理目录。
5. 不执行删桶、全表删除、管理 root 递归删除或 force 绕过保护。
6. 保留发布前二进制备份，直到验收完成并得到人工确认。

模板删除示例：

```bash
cm tpl delete --template-id "$TEMPLATE_XFS"
```

只允许删除本轮已登记的 template ID。

## 22. 最终报告模板

最终报告必须明确：

```text
构建机兼控制面：9.135.79.34
计算节点：9.135.10.216
交付提交 CODE_BASE：<交付消息中的SHA>
实际构建 HEAD：<填写，必须等于 CODE_BASE 解析出的提交>
专项脚本证据目录：<填写>
Linux 原生运行/交叉编译：<逐项区分 PASS、COMPILE_PASS、SKIP、NOT_RUN>
```

| 场景 | 状态 | 版本/配置 | 证据 | 说明 |
|---|---|---|---|---|
| Master/TC/Cubelet 组件门禁 |  |  |  | 实际运行数量、SKIP |
| TC 发布及回滚检查 |  |  |  | SHA256、PID、health |
| Master/CLI 发布及回滚检查 |  |  |  | SHA256、PID、日志 |
| 216 Cubelet 发布 |  |  |  | SHA256、PID、心跳 |
| CLI backend xfs/s3/cos/minio |  |  |  | 区分参数与真实存储 |
| artifact S3/fs 下载 |  |  |  | 网络路径、size/SHA |
| 下载授权/续传 |  |  |  | 状态码、重取、Range |
| fingerprint 重试 |  |  |  | artifact A/B、DB/job |
| 删除引用与本地 GC |  |  |  | definition/replica/job/snapshot/placement |
| COMMIT origin |  |  |  | build ID、文件/内存 |
| COMMIT 跨节点 | `BLOCKED` | 当前实现 | 修复报告 | 不得冒充通过 |
| 分页 SQLite/MySQL/PG |  |  |  | EXPLAIN、耗时、RSS |
| SDK E2E |  |  |  | pytest 日志 |

另外列出：

- 构建产物与目标机实际二进制 SHA256；
- 发布前备份位置；
- 服务最终状态；
- 所有 job/build/template/artifact/sandbox ID；
- 所有 `FAIL/SKIP/NOT_RUN/BLOCKED` 项；
- 是否触发过回滚；
- 已清理和保留的测试资源。

执行过程中不要提交、推送或修改新代码。遇到新的代码错误时，保存证据并停止对应场景，等待人工决策。
