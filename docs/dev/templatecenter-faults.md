# CubeTemplateCenter 故障清单与回归验证

本文档登记模板中心（CubeTemplateCenter, TC）拆分过程中发现并修复的全部故障，每条对应一个可执行的回归验证。验证入口：`scripts/verify_template_faults.py`。

四套易混淆的状态机（大量故障源于把它们当一套）：

| 状态机 | 字段 | 取值 |
|---|---|---|
| 构建任务 | `template_image_jobs.status` | PENDING → RUNNING → BUILT → READY，或 FAILED |
| 构建阶段 | `template_image_jobs.phase` | PULLING/UNPACKING/BUILDING_EXT4/…/DISTRIBUTING/READY |
| 产物 | `rootfs_artifacts.status` | PENDING → BUILDING → READY / FAILED / CLEANUP_PENDING / ORPHANED |
| 模板定义/副本 | `template_definitions.status` | PENDING → CREATING → READY / FAILED |

注意：`job` 没有 `BUILDING` 状态，`BUILDING` 只是 `artifact` 的状态。删除拦截看 `job.status ∈ {PENDING,RUNNING}` 和 `definition.status ∈ {PENDING,CREATING}`。

---

## 一、ext4 转换链路故障

ext4 生成路径：拉镜像(skopeo) → umoci 解包 → envd 沙箱执行 → 写 ext4（Phase-1 umoci+mkfs，或 Phase-2 loop-mount 流式）→ 算 sha256/size → 存库 READY → 分发 → Cubelet 下载。

### F1. 半截 ext4 被当完成品复用 — major，已修复
- **现象**：崩溃发生在 `mkfs.ext4` 中途，留下完整尺寸的稀疏文件。stat 通过、sha256 也能算，reuse 路径误判为完成品，分发出损坏的 rootfs。
- **根因**：构建直接写最终文件名，无「完成」与「半成品」的区分。
- **修复**：`BuildExt4` 写临时文件 `<id>.ext4.tmp.<pid>`，成功后 `os.Rename` 原子发布。最终名存在 = 构建完成。
- **锚点**：`CubeMaster/pkg/templatecenter/image/ext4.go`（`os.Rename`、`.tmp.`）
- **验证**：静态断言原子 rename 存在；`go test ./pkg/templatecenter/image/`

### F2. 跨进程清理误删在写产物 — 已修复（b6000546）
- **现象**：GC/清理跨进程 `RemoveAll` 共享产物目录，把另一进程（TC）正在 mkfs 的目录删掉，构建报「prefetched layer ... no such file」。
- **修复**：`.cube-artifact-build-in-progress` 跨进程标记（pid + 2h TTL）；构建打标记、清理拒删、复用不复用。
- **锚点**：`CubeMaster/pkg/templatecenter/image/build_marker.go`
- **验证**：`go test ./pkg/templatecenter/image/ -run BuildMarker`

### F3. READY 行但文件丢失，永不自愈 — 已修复（4c2825c3，#852）
- **现象**：产物目录丢失（emptyDir 重启/人为清理），DB 行仍 READY。reuse 跳过构建、分发门禁只查 DB、下载端点 500 不改行——每次重试都死在同一处。
- **修复**：`artifact_presence.go` 三值判定（Present/Missing/Unknown），在 reuse/分发/下载/redo 四处探测；Missing 且本节点持有则降级为 FAILED，下次创建原地重建。
- **锚点**：`artifact_presence.go`（`probeArtifactPresence`、`resolveMissingArtifact`）
- **验证**：`go test ./pkg/templatecenter/ -run 'ArtifactPresence|ReadyArtifactUsable|RootfsArtifact'`

### F4. 多 master 下到旧版本，sha256 不匹配 — 已修复（#1005）
- **现象**：`master_node_ip` 存的是下载 base URL（来自 Host 头，HA 下常为 LB），Cubelet 又改写 host 到 MetaServer endpoint，请求落到未构建该产物的节点，下到旧文件。
- **修复**：`resolveMissingArtifact` 区分 Demoted（本节点持有、文件真没了）vs Foreign（文件在另一台）。Foreign 不降级、不本地重建（重建会覆写共享行 token/sha），显式报错 `ErrRootfsArtifactForeign`。
- **锚点**：`artifact_presence.go`（`artifactAuthoritativeHere`、`ErrRootfsArtifactForeign`）
- **验证**：`go test ./pkg/templatecenter/ -run 'Foreign|ServedLocally'`

### F5. redo 复用判定叠加后 nil panic — blocker，已修复
- **现象**：rebase 合并丢了「非重建 resume 路径加载 artifact」分支。resumePhase=DISTRIBUTING/SNAPSHOTTING（产物完好，最常见 redo）时 `artifact` 为 nil，`artifact.ImageConfigJSON` 空指针；裸 goroutine 无 recover，panic crash 整个 CubeMaster。
- **修复**：`redo.go` 补回 else 分支 `getRootfsArtifactByID` 加载。
- **锚点**：`redo.go`（else 分支加载 artifact）
- **验证**：`go test ./pkg/templatecenter/ -run 'TestRunRedoTemplateImageJob'`（`JobPhaseSnapshotting` + READY artifact 用例）

### F6. reuse 判定丢 size 校验 + Unknown 误判可复用 — major，已修复
- **现象**：合并把 master 的 `validateReusableRootfsArtifactFile` 换成 v2 判定，丢了 size 匹配；Unknown（另一进程在写）被判可复用，进程内锁管不到跨进程 TC。
- **修复**：`rootfsArtifactReuseVerdict` 补 size 匹配；Unknown 落回 claim 路径（DB 行锁跨进程串行）。
- **锚点**：`artifact_presence.go`（`rootfsArtifactSizeMatches`）
- **验证**：`go test ./pkg/templatecenter/ -run 'RootfsArtifactReuseVerdict'`

---

## 二、控制面与状态流转故障

### F7. 分发全节点失败 → redo 永久死锁 — 已修复（421974f4）
- **现象**：门禁 `expected>0 && ready==0` 在 `expected==0`（单节点/未触达）时被绕过；全节点失败后 redo 无法重试。
- **修复**：门禁改 `ready==0`；纯函数 `distributionFailure` 区分「未触达」与「全节点失败」。
- **验证**：`go test ./pkg/templatecenter/ -run DistributionOutcome`

### F8. resume goroutine 静默失败 + panic — 已修复（baf8153a）
- **现象**：回调 goroutine 无 `recover()`，panic crash；丢 RequestTrace。
- **修复**：加 `recover()`+`debug.Stack()`，job 留 BUILT 由 reconciler 重放。
- **锚点**：`CubeMaster/pkg/service/httpservice/cube/template_job_callback.go`（`recover()`）
- **验证**：静态断言 recover 存在。

### F9. TC 的 BUILT 报告被 finalize 覆盖 — 已修复（454f5c39）
- **现象**：finalize 覆盖 result_json，丢 TC 上报的 BUILT 细节，无法判断产物是否真经远程构建。
- **修复**：`mergeRemoteBuildReport` 折叠进 `remote_build_report` 子键。
- **验证**：`go test ./pkg/templatecenter/ -run RemoteBuildReport`

### F10. 快照创建 request_id 幂等永不命中 — 未修（#1105，社区 PR #1253 在 review）
- **现象**：指纹在注入随机 snapshotID 后计算；重试路径用空注解重算，左右恒不等；`resumeSnapshotCreateJob` 成死代码。
- **状态**：不在本次范围（只关心模板链路）。**不验证**，仅登记。

### F11. redo 销毁快照模板产物 — 已修复（bed9248d，#1159）
- **现象**：redo 走 from-image 语义，快照模板无 `source_image_ref`，但校验在破坏性 cleanup 之后——产物先删、再报错，可恢复模板被做成永久死。
- **修复**：守卫前移，cleanup 之前先查 source_image_ref。
- **锚点**：`redo.go`（`source_image_ref` 守卫在 `prepareRootfsArtifactForRedoBuild` 前）
- **验证**：静态断言守卫在 cleanup 调用之前。

### F12. PENDING/RUNNING 模板删不掉、无 force — 已修复（5b381db9）
- **现象**：只能等 `failStaleRunningJobs` 超时。
- **修复**：`DeleteTemplateOptions{Force}`，先终结在途 job 再清理。**关键**：不绕过 in-use 检查——force 若先标 definition=FAILED 会静默跳过 in-use（`shouldCheckInUse` 对 FAILED 返回 false），顺序是易错点。
- **锚点**：`delete.go`（in-use 检查在 `failActiveTemplateWork` 之前）
- **验证**：`go test ./pkg/templatecenter/ -run 'DeleteTemplateWithTargets'`

### F13. 迁移指纹误杀 — 已修复（e01bc307）
- **现象**：指纹预检把「内容漂移」（真危险）与「版本缺失」（库比二进制新，安全）混为同一致命错误，多组件共享 CubeDB 全部起不来。
- **修复**：缺失只告警（`logAbsentVersions`），内容漂移仍致命。
- **验证**：`go test ./CubeDB/migrate/ -run PreflightFingerprints`

---

## 三、部署与配置故障

| # | 故障 | 修复 | 验证 |
|---|---|---|---|
| F14 | TC conf 用 Helm 模板顶包，裸机全是 `{{ }}` 占位符 | 补 `configs/single-node/templatecenter.yaml` + install.sh 替换 | 静态：模板无 `{{`，含 `__CUBE_SANDBOX_` 占位符 |
| F15 | chart 无法真正启用 TC | 渲染 `templatecenter_enabled` + 注入地址 env | 静态：conf.yaml 含该键、master.yaml 含 `CUBE_TEMPLATE_CENTER_ADDR` |
| F16 | 配置双源头/字符串枚举 | 单布尔 `templatecenter_enabled` + 地址入 env | 静态：无 `template_build_mode`/`template_route_mode` 残留 |
| F17 | TC 无 ENTRYPOINT 容器 exit 0 | Dockerfile 有 ENTRYPOINT（线上是旧镜像） | 静态：Dockerfile 含 `ENTRYPOINT` |
| F18 | Terraform TC 守护成本过高 | 移除，收敛 Helm/one-click | 静态：terraform 目录无 `templatecenter.tf` |

---

## 四、验证脚本

`scripts/verify_template_faults.py`，三层：

- **static**：grep 代码断言关键不变量（guard/recover/原子 rename/无残留键），无需环境，随时可跑。
- **unit**：对每条故障调用对应的 `go test -run`（需 Linux，`image` 包依赖 `unix.F_SETPIPE_SZ`）。
- **e2e**：复用 `scripts/e2e_templatecenter.py` 跑真实双通道（需运行中的 CubeMaster/TC）。

```bash
python3 scripts/verify_template_faults.py --layer static           # 无环境，最快
python3 scripts/verify_template_faults.py --layer unit             # 需 Linux
python3 scripts/verify_template_faults.py --layer e2e \
    --master http://127.0.0.1:8089 --tc http://127.0.0.1:8090      # 需真实部署
python3 scripts/verify_template_faults.py --layer all ...          # 全部
```

## 五、仍开放（不验证，仅登记）

| 项 | 状态 |
|---|---|
| 快照链路 #1105 幂等 | 社区 PR #1253 在 review，不重复改 |
| Cubelet 侧 `FileExistAndValid` 只看 size、`rewriteDownloadHost` 改写到 LB（#1005 根因另一半） | 按约定只改 CubeMaster+TC，未动 |
| 失败副本自动重试（K5 `PARTIALLY_READY` 无 reconciler） | 遗留 |
| Go 单测未在 Linux 实跑 | 待办（macOS 无 docker） |
