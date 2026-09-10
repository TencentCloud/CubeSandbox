# 验收测试记录 / Acceptance Test Results — 2026-08-07（最近一次更新：2026-09-07）

环境：本地部署 CubeSandbox（dev-env QEMU 虚拟机，CubeAPI @127.0.0.1:13000），
基线 Go envd 0.5.13（`ghcr.io/tencentcloud/cubesandbox-base:2026.16`），
cube-envd 0.1.0（`make cube-envd` 产物，含独立评审整改，见 §2b）。

## 1. 单元测试

`make cube-envd-test` → **45 passed, 0 failed**（含 Connect envelope 编解码、
proto3 JSON 映射、路径/用户解析、降权执行、错误映射、进程组信号、句柄化进程表
防 PID 复用、常量时间令牌比较）。clippy `-D warnings` 通过。

**2026-08-31 重跑**（阶段 1 项 1.7 /init 幂等落地 + 复审整改后）：
`cargo test --locked` → **75 passed, 0 failed**；
`cargo clippy --release --all-targets --locked -- -D warnings` 与
`cargo fmt --check` 均干净。新增 9 个用例覆盖 `/init` body-token 生命周期
四分支（含错误文案逐字节）、空 token 在解码层被拒、RFC3339 解析（合法/非法各
十余例、闰日、i64 纳秒范围越界 → 400）、timestamp 闸门（`utils.AtomicMax` 语义）
与 `defaultUser`/`defaultWorkdir` 的空值不覆盖规则。

## 2. 一致性对拍（cube-envd vs Go envd 0.5.13）

同一镜像起两个容器（cube-envd 经 `ENVD_BIN` 开关注入）逐报文对比。
**2026-09-02 重跑（88 个协议场景，全新容器全量重录）**：

```
PASS 78  FAIL 0  DECLARED-DIFF 10  SKIP 0  MISSING 0
```

较上版变化：① `fs_stat_symlink_probe` 移出 allowlist——服务层 `entry_info`
按上游 `GetEntryInfo` 对齐后，悬垂链接（探测目标 `zz_probe_gone` 显式不存在）
两侧给出完全一致的 entry；② 新增 `fs_stat_suid`/`fs_stat_sticky` 锁定
Go `FileMode.String()` 的 `u`/`g`/`t` 前缀权限格式（4755 → `urwxr-xr-x`、
1777 文件 → `trwxrwxrwx`，数值 `mode` = `Perm()` 不含特殊位）；③ `cap_fs`
改为自建 `base_a.txt`/`base_b.bin`（不再依赖先跑 rest 组，消除对裸容器录制
时 Stat/Move 黄金路径录成 404 的假通过）；④ 新增 `fs_makedir_on_file`——
MakeDir 到已存在文件：上游按 `os.Stat` 的 `isDir` 分支给
`invalid_argument`（`path already exists but it is not a directory`），
cube-envd 原给 `already_exists`，已对齐。

**2026-08-31 重跑（71 个协议场景）**：

```
PASS 60  FAIL 0  DECLARED-DIFF 11  SKIP 0  MISSING 0
```

> 注：`rest_init_timestamp_out_of_range` 为手写解析器换 `time` crate + 越界改 400
> 重构后的实测（本行上方数字为 2026-08-31 重跑实测值，71 场景全录）。

其中 11 项 DECLARED-DIFF 均为设计声明的 MVP 差异（PTY、watch 家族、
/files/compose、gzip 编码、嵌套 selector 宽容性、解析器错误措辞、
符号链接 lstat vs follow、以及越界 timestamp 的 400 vs 204），allowlist 见 `conformance.py`
`DECLARED_DIFFERENT`——本次 allowlist 11 条全部命中（gzip 下载场景
`rest_files_gzip_accept` 已作为第 10 条进入命中集，越界 timestamp 为第 11 条）。

**2026-08-31 新增的 22 个 `/init` 生命周期场景全部 PASS（未进 allowlist）**：
首设放行 / 匹配放行 / 已设而 body 不带 → 401 `access token reset not
authorized` / 不匹配 → 401 `access token validation failed` / 新 timestamp
生效 / 旧 timestamp 被丢弃且不再校验 token / 非法 timestamp → 400 / 无
timestamp 恒生效 / `defaultUser` 影响后续 `/files` 的用户解析 /
`defaultWorkdir` 顶替空 path / 以及收尾的 `/envs` 断言（被拒的三次 /init
的 envVars 均未落库）。

**2026-08-31 复审后补的 5 个场景（其中 1 个进 allowlist）**：越界 timestamp（年 > 2262）
——上游 `UnixNano()` 溢出回绕成负值、被闸门当旧请求丢弃 → 204 不落库不动水位；
cube-envd 把它当调用方 bug 直接 400（DECLARED-DIFF 第 11 条），同样不落库、不动水位
（紧随其后的 `rest_init_after_out_of_range` 用普通 timestamp 证明水位没被顶死，两侧
`/envs` 键集完全一致）；空 `accessToken`（→ 400，上游 `*SecureToken.UnmarshalJSON`
在解码层就拒空串）、无时区的带小数秒 timestamp（→ 400，RFC3339 zone 必选）、
日历上不存在的日期如 2023-02-31（→ 400，Go 报 `day out of range`）。三者原先在
cube-envd 上是 204 且**会落库**：空 token 会被存下，此后任何带真实 token 的 `/init`
都只能 401（SDK/Cubelet 这类只发真实 token 或不发头的调用方全部被挡），改不回来。

> 注（2026-08-31 实测修正了此前对上游的两处误读）：① 相等 timestamp
> **放行**（`utils.AtomicMax.SetToGreater` 只在严格更小时拒绝）；② timestamp
> 闸门在 token 校验**之前**，旧 timestamp 的 /init 直接 204、不会 401。
> 另：`/init` 在上游位于鉴权白名单，**不校验** `X-Access-Token` 头，
> token 语义完全由 body 决定——cube-envd 已按此对齐（此前会做 header 预检）。

历史基线（2026-08-07，49 个场景）：`PASS 40  FAIL 0  DECLARED-DIFF 9`。

### 2a. CORS 对照（阶段 1 项 1.5，2026-08-31 新增）

同镜像双容器，逐个请求比对 `Access-Control-*` 与 `Vary`（大小写无关）：

| 请求 | 结果 |
|---|---|
| `OPTIONS /health` + Origin + `ACRM: POST` | 一致（204 + ACAO `*` + ACAM 回显 `POST` + Max-Age 7200 + 预检 Vary）|
| 同上 + `ACRH: content-type, x-access-token` | 一致（额外回显 ACAH）|
| `OPTIONS` + `ACRM: TRACE`（不在允许方法集）| 一致（仅 Vary，无 ACAO）|
| `OPTIONS` + ACRM 但无 Origin | 一致（仅 Vary）|
| `GET /health` + Origin | 一致（ACAO `*` + Expose-Headers 六项 + `Vary: Origin`）|
| `GET /health` 无 Origin | 一致（仅 `Vary: Origin`）|
| `POST /envs` + Origin | 一致 |
| `GET /files?path=...` + Origin | **CORS 头一致**；`Vary` 不同（Go `Accept-Encoding`，cube-envd `Origin`）|
| `OPTIONS /health` + Origin、无 `ACRM` | 一致（405 + ACAO `*` + Expose-Headers）——review 补测 |

最后一行是项 1.3 的既有缺口（上游 `download.go:118` 设置 `Vary: Accept-Encoding`，
cube-envd 的 download 尚未发该头），不是 CORS 差异：上游 CORS 中间件在 handler
**之前**写 `Vary`，会被 handler 自己的 `Set` 覆盖，故 1.3 落地后 cube-envd 同样
只剩 `Accept-Encoding`——`cors.rs` 的 `apply()` 已按此语义实现（响应已有 `Vary`
则不动）。`Vary` 不在 `conformance.py` 的 `HEADERS_KEPT` 比对面内，不影响对拍结论。

**Review 补测（2026-08-31）**：`OPTIONS /x`（无 `ACRM`）对 rs/cors 是 actual 请求，
`isMethodAllowed` 对 OPTIONS 恒放行（cors.go:490-492）——cube-envd 原实现把
OPTIONS 排除在方法集外，该形状只回 `Vary: Origin`，与上游（405 + ACAO +
Expose-Headers）不一致；已修（`is_method_allowed` 对 OPTIONS 恒 true）并新增
conformance 场景 `rest_cors_options_actual` 锁定。

对拍侧新增 7 个 CORS 场景；`conformance.py` 的 `HEADERS_KEPT` 已扩展纳入五个
`Access-Control-*` 头（`Vary` 因 1.3 缺口仍不在比对面），CORS 头差异从此会被
fixture 对拍自动抓到。全新容器重录后 **PASS 47 / FAIL 0 / DECLARED-DIFF 10**
（57 场景）。

## 2b. 独立评审整改（三个独立 sub-agent 复核）

代码评审 / 协议一致性 / E2E 三路独立 agent 复核后发现并已修复的缺陷：

| 编号 | 缺陷 | 修复 |
|---|---|---|
| C1 | 未建进程组，`kill_pid` 注释谎称"组长"，超时/信号只杀直接子进程、泄漏孙进程 | pre_exec 中 `setpgid(0,0)`；`kill_process_group` 对 `-pid` 发信号，整组回收（相对 Go 泄漏为有意改进，已文档化）|
| C2 | `child.id().unwrap_or_default()` 可返回 pid=0 | 显式取 pid，spawn 失败按缺失二进制事件流处理 |
| C3/C4 | 进程表以 OS pid 为键，PID 复用时误删/误杀 | 引入单调 `ProcHandle`，表以句柄为键，`find_pid` 取最新句柄 |
| C5 | multipart 上传无大小上限 | `multer` `Constraints::size_limit`，超限→413 |
| S2 | chown 跟随符号链接 | 改用 `libc::lchown` |
| S3 | access token 非常量时间比较 | `constant_time_eq` |
| R1 | `lock().unwrap()` 遇毒锁 panic | `unwrap_or_else(PoisonError::into_inner)` 恢复 |
| F1 | proto3 零值未省略（size/mode）；`.current_dir()` 以 root 身份先 chdir；无效 cwd 静默降到 `/` | 零值 `skip_serializing_if`；chdir 移入 pre_exec 且在降权之后；无效 cwd 返回 `invalid_argument`（不再静默成功）|
| F3 | 嵌套 selector 被展开，畸形 SendSignal 可误杀存活进程 | 嵌套 selector 解析为空 → `not_found`，无副作用 |
| F6 | not_found 措辞与 Go 不一致 | 按 pid/tag 逐字对齐 Go 文案 |

以上均在活体对拍中逐条对 Go 基线复验通过（空文件省 size、mode-000 省 mode、
无效 cwd 返回字节级一致的 `invalid_argument`、嵌套 selector 双方均不动进程）。

覆盖 issue #1227 要求的五类路径：成功 / 错误 / 超时（`Connect-Timeout-Ms`
到期杀进程 + `deadline_exceeded`）/ 取消（断连后进程存活）/ 大输出（2 MiB
字节级一致）。

### 2c. legacy SDK（User-Agent `connect-python`）对照（阶段 1 项 1.6，2026-09-01 新增）

同镜像双容器全新重录（83 个场景）：

```
PASS 71  FAIL 0  DECLARED-DIFF 12  SKIP 0  MISSING 0
```

较 1.6 之前（78 场景，`PASS 67 / DECLARED-DIFF 11`）净增 5 个 legacy 场景，
其中 4 个 PASS、1 个进 allowlist。逐场景实测：

| 场景 | 断言 | 结果 |
|---|---|---|
| `fs_legacy_stat` | Stat 200：`entry` 收窄为 `{name,type,path}`（size/mode/permissions/owner/group/modifiedTime/symlinkTarget 全丢）+ `X-E2B-Legacy-SDK: true` | 一致 |
| `fs_legacy_listdir` | ListDir 200：`entries` 每个元素同样收窄 + 头 | 一致 |
| `fs_legacy_remove` | Remove 200：`{}`（本来就空）+ 头 | 一致 |
| `fs_legacy_stat_missing` | Stat 404：**不**收窄、**不**带 `X-E2B-Legacy-SDK`（上游 `WrapUnary` 先返回 err，走不到 `shouldHideChanges`）| 一致 |
| `fs_legacy_stat_symlink` | 符号链接 Stat：跟随链接 → 目标类型（`FILE_TYPE_FILE`）| 一致 |
| `fs_legacy_stat_symlink_dir` | 指向目录的链接 → `FILE_TYPE_DIRECTORY`（follow 语义）| 一致 |
| `fs_legacy_stat_symlink_dangling` | 悬垂链接 → `FILE_TYPE_UNSPECIFIED`（proto3 零值，`type`/`mode` 键省略，Stat 仍 200）| 一致 |

**2026-09-02 服务层对齐后**：legacy 对拍另有独立目录（`fixtures-go-legacy` /
`fixtures-rust-legacy`，7 个 legacy 场景，`--which fs-legacy` 可单独重跑；全量
`all` 录制的 87 个 fixture 里同样包含这 7 个），并补
`fs_legacy_stat_symlink_dir`（链接→目录）与 `fs_legacy_stat_symlink_dangling`
（悬垂链接）两个形状。两者最初都 FAIL（cube-envd 一律收窄成 `FILE`）——
根因不在 legacy 收窄层，而在服务层条目语义：cube-envd 的 `entry_info` 已按上游
`shared GetEntryInfo`（`entry.go:19-68`）对齐——链接的 type/mode 取跟随目标、
悬垂目标 → `UnknownFileType`（零值省略）、`permissions` 按 Go `FileMode.String()`
（`L…`、setuid/setgid/sticky 为 `u`/`g`/`t` 前缀）、`symlinkTarget` 按
`EvalSymlinks` 语义；`ListDir` 同步改为 `filepath.WalkDir` 的 DFS 序。
`narrow_entry` 不再做任何类型映射（服务层不再产出 `FILE_TYPE_SYMLINK`）。
`fs_legacy_stat_symlink` 从 `DECLARED_DIFFERENT` 移除；重录后：

```
PASS 7  FAIL 0  DECLARED-DIFF 0  SKIP 0  MISSING 0
```

全量对拍（88 场景）同步重录：`PASS 78 / FAIL 0 / DECLARED-DIFF 10`，
`fs_stat_symlink_probe` 移出 allowlist（悬垂链接两侧给出完全一致的 entry），
新增 `fs_stat_suid`/`fs_stat_sticky` 锁定 `u`/`g`/`t` 前缀格式，新增
`fs_makedir_on_file` 锁定 MakeDir 到已存在文件的 `invalid_argument` 对齐。

单元测试 `cargo test --locked` → **93 passed, 0 failed**（legacy 用例之外含新增
symlink 语义覆盖：链接三形状的 `entry_info`、Go `FileMode.String` 特殊位前缀、
`ListDir` 根跟随链接但不进入链接子目录、悬垂链接作根 → 404 而非 400、DFS 完整
序列含第三层）；`cargo clippy
--all-targets --locked -- -D warnings` 与 `cargo fmt --check` 均干净。

> Review 修正（2026-09-01）：首版 legacy 场景复用了 `cap_fs` 的残留路径
> （`base_a.txt` / `zz_link`），`--which fs-legacy` 单独跑时两个前置都不存在，
> 4 个 fixture 里有 3 个录成 404 `not_found`——对拍照样"通过"却什么也没证明，
> allowlist 里的 symlink 差异更是没有实测依据。已改为自带 fixtures（自建文件、
> 自建目录、自建符号链接，跑完自清），并补 `fs_legacy_listdir` 覆盖 `entries`
> 分支。

## 3. SDK 端到端（三大验收场景）

模板 `tpl-49213eb35f7a44f89f42995c`（基于含 §2b 全部整改的 cube-envd 镜像
`create-from-image` 创建）；Python SDK（`sdk/python`）经
CubeProxy 访问。**19 passed, 0 failed**。

| 场景 | 断言 |
|---|---|
| 1 健康检查 | 沙箱达到 RUNNING（就绪探测 :49983/health 通过）、基础命令往返 |
| 2 命令执行 | stdout/stderr 分流、退出码、env 注入、用户切换、cwd、大输出管道、超时强制生效（2s 抛错） |
| 3 文件读写 | 文本/二进制写读一致、list/stat/make_dir/rename/remove、缺失文件报 404 |
| 回滚验证 | Go envd 模板 `tpl-72f50185f0c8428a99620480`（`ENVD_BIN=/usr/bin/envd`）命令 + 文件 smoke 通过 |

## 4. 性能对比（同镜像同宿主，`perf.py` 实测）

| 指标 | Go envd 0.5.13 | cube-envd 0.1.0 | 变化 |
|---|---|---|---|
| 稳态 RSS | 16.1 MiB | 2.3 MiB | −86% |
| 冷启动至 /health 204（均值，10 次） | 38.9 ms | 13.2 ms | −66% |
| `echo hi` 端到端延迟 P50 / P95（100 次） | 6.3 / 8.3 ms | 4.3 / 5.6 ms | −32% / −33% |
| 静态二进制体积 | 10.5 MB | 2.6 MB | −75% |

## 5. item 1.8 cgroup 真机验证（2026-09-02，feat/cube-envd-cgroup-1.8）

cgroup 行为不经 envd RPC 暴露，conformance 套件无法对拍，故以真机实测
覆盖 init() 正向路径与 spawn 落位。

### 环境矩阵落点

| 环境 | 矩阵行 | 实测结果 |
|---|---|---|
| dev 机宿主真根（WSL2，cgroup v2，systemd 已启用 controller） | 第 1 行（一致） | cube-envd init() 非 Noop：subtree_control 幂等追加成功（宿主已含 cpuset cpu io memory hugetlb pids rdma）；`/sys/fs/cgroup/{user,ptys}` 建出；`user/memory.max=12404305920`（meminfo 算得）、`cpu.max="max 100000"`；envd 自身不迁移（仍在 `0::/init.scope`） |
| 普通 docker 容器（私有 cgroupns，ns root 承载容器进程） | 第 3 行（同结局 → Noop） | subtree_control enable 失败（cgroup v2 no-internal-process 规则，EIO）→ Err → Noop；1.8 对拍全程 Noop 下运行 |
| `--privileged` 容器 + PID1 自移子 cgroup（构造"容器节点空"拓扑） | 嵌套可写根（第 2 行效果） | init() 非 Noop：subtree_control = "cpu memory"；start 的 sleep 进程落在 `user/`（`cgroup.procs` 含 pid，`/proc/<pid>/cgroup = 0::/user`）——验收标准① |

### A1 探针（exec.rs `#[ignore]`）

`sudo cargo test -- --ignored spawn_lands_child_in_its_cgroup`（宿主真根）→ ok：真实
cgroup dir fd 经 pre_exec `openat` 写入 `cgroup.procs`，子进程 `/proc/<pid>/cgroup`
落在 `cube-a1-<pid>` 子树；测试自清理，host 无残留。

### 单测（变基到 1.6 之后实测）

`cargo test` → **110 passed, 0 failed, 1 ignored**（ignored 为 A1 探针）；
`cargo clippy --all-targets` 与 `cargo fmt --check` 均干净。较 1.6 的 93 增加 17
（本分支新增用例，含 1 个 ignored 探针）。

### 对拍（2026-09-03 变基后重录）

`proc_missing_cmd` 出 allowlist 的依据（本分支实测）：wrapper 之后 missing cmd 的
stderr 与上游字节一致 ——
`/usr/bin/nice: '/no/such/bin': No such file or directory`。

本分支变基到 1.6 之后 fresh 容器重录（同镜像起两个容器，`capture.py all` 两侧各
90 fixture，`conformance.py` 退出码 0）。三行口径不同，前两行为各自分支当时的历史
实测，第三行为本分支当前实测：

| 口径 | 场景 | PASS | FAIL | DECLARED-DIFF | allowlist |
|---|---|---|---|---|---|
| 1.8 首测（1.6 前，80 fixture） | 80 | 70 | 0 | 10 | 11 → 10（移除 `proc_missing_cmd`） |
| 1.6 重录（§2c） | 88 | 78 | 0 | 10 | 11 → 10（移除 `fs_stat_symlink_probe`） |
| **1.8 变基后重录（2026-09-03）** | 90 | 81 | 0 | 9 | 9（`proc_missing_cmd` 与 `fs_stat_symlink_probe` 均已移除） |

场景数 90 是 1.6 合并时套件即有的规模（base 与本分支的 `capture.py` 完全相同，
本分支未动）——§2c 记录的 88 是 1.6 开发中更早的时点。`conformance.py` 的
`DECLARED_DIFFERENT` 现 9 条且全部命中，无孤儿条目；FAIL 恒 0。

## 6. item 1.3 Range / 条件请求下载（2026-09-06，feat/cube-envd-fs-1.3）

> 目标：GET /files 下载对齐上游 `download.go` + Go `net/http` `ServeContent`（identity 路径）：
> Accept-Encoding 双 406 → Vary → Range/206/416 → Last-Modified → 304/412 → If-Range，顺序与
> 头集合逐字节一致（决策：导师 2026-09-05「完全兼容做」）。实施见
> `docs/cube-envd/item-1.3-implementation-plan.md`。

### 单测

`cargo test` → **230 passed / 1 ignored**（新增 55 个纯函数单测：encoding/ranges/httpdate/
content_disposition/preconditions 五模块；15 个 handler 级集成测试直驱真实下载路径，覆盖
206 字节精确、416 两型、304 头集合、IMS 回灌 round-trip、空值条件头、If-Range 等）。clippy
`-D warnings` 与 fmt 干净。`time` crate 开启既有依赖的 `formatting` feature（无新依赖）。

### 对拍（双端 fresh 容器全量重录）

```
PASS 104  FAIL 0  DECLARED-DIFF 8  SKIP 0  MISSING 0   （112 场景）
```

allowlist 8 条全部命中、无孤儿条目（watch/compose/gzip/nested-selector×3/JSON 措辞/
init 越界时间戳——均为既有登记差异）。新增内容：

- **`capture.py` 新组 `cap_files_negotiation()`（15 场景）**：单/开区间/后缀 Range→206 +
  `Content-Range: bytes a-b/N` 字节精确；越界 vs 语法错误 → 416 两型（`bytes */N` 有无、
  body 文本 `invalid range…`）；空文件 Range→200；IMS 回灌 304（两段式取真实 Last-Modified）；
  过期 IMS→200；INM `*`→304 / 具体 etag→200（并跳过 IMS）；If-Match→412；If-Range etag
  失配丢 Range→200；AE 双 406（identity 门带 `Vary: Accept-Encoding`，parse 失败只带 CORS
  的 `Vary: Origin`）。
- **`conformance.py`**：`HEADERS_KEPT` +5（Vary/Accept-Ranges/Content-Range/
  Content-Disposition/Last-Modified）；Last-Modified 值（RFC 1123，双端容器 mtime 必异）
  归一 `<time>`——存在性比较、秒粒度由 httpdate.rs 单测覆盖。
- **wire 抽查（两侧 normalize 后逐字节 identical）**：206 `bytes 2-11/20`；416 无
  Last-Modified（serveError 在 setLastModified 后跑仍删之——与上游一致）；304 头集合
  `{Vary, Content-Disposition, Last-Modified}` 无 CT/CL/CE；412 裸空 body 保留
  Vary/CD/Last-Modified；406 消息文本逐字节一致。

### 录制中发现并修复（capture 层）

rust hyper 在 wire 上以小写发送 header 名（`last-modified:`），Go net/http 用 canonical
（`Last-Modified:`）——两者均合法（RFC 7230 §3.2 字段名不区分大小写），conformance
normalize 已 title-case 抹平，不构成对拍差异；但 capture.py 的 IMS 回灌按固定大小写取
头会崩 → 新增 `header_get()` 大小写不敏感查找，已在重录中使用。

### 审查修复后回归（2026-09-07，四轮）

PR #13 四轮评审整改（全部落地于单一整改提交）后全量回归：

**第一轮整改（4 项发现）**：① `httpdate.rs` 手写实现对齐 Go `http.ParseTime`
三格式（IMF-fixdate/RFC850/asctime，含大小写、空格 run、星期不校验、小数秒、
RFC850 时区 token 等全部 quirk），`format_http_date` 去 panic（年 10000 文件
曾 500）；② mtime 按 fs.go `isZeroTime` 先判精确 epoch 再截断，亚秒/负 mtime
保留（floor 语义），checkIfRange 无零时间门；③ ETag 列表改 fs.go
scan-and-resume 循环（`"a"garbage, *` → If-Match 412 / If-None-Match 200）；
④ stat-then-open 经查上游 `download.go:82/:138/:172` 同构，保留。
验证：`cargo test` **239 passed / 1 ignored**（+8：httpdate 边界金标准、
mtime/If-Range 边界、畸形 ETag、modtime 单元测试）；另做 **719 例随机+变异
差分 dump vs go1.26.5 `http.ParseTime`，零分歧**。

**第二轮整改**：① [P2] obs-text 头（0x80-0xFF）曾被 `to_str()` 失败重分类为
absent——改为 lossy UTF-8，坏字节→U+FFFD，在全部解析器中与 Go 原始字节行为
同构（+4 handler 测试：Range`\xFF`→416、INM`\xFF`+新 IMS→200 非 304、
If-Range`\xFF`→200 非 206、AE`identity;q=0,\xFF`→406）；② 上传上限与
`connect::MAX_ENVELOPE_SIZE` 解耦为 `MAX_UPLOAD_SIZE = 256 MiB`（对齐 proxy
层上限，覆盖原 64-256 MiB 功能回归区间）；③ lchown 失败告警、rename 后目录
fsync、ranges 改 ASCII trim、httpdate 重复测试行清理。
`cargo test` → **242 passed / 1 ignored**；clippy/fmt 干净。

**第三轮整改（round-2 复审结论：其余全部确认通过）**：① 空文件 GET 补
`Content-Length: 0`（Go ServeContent 按 Seek(End) 定长；此前真空文件发
chunked，conformance normalize 抹平 framing 故不可见——按 sniff 首读是否
0 字节区分真空文件与 /proc 伪文件，后者保持 chunked 为既有有意差异）；
② 上传 Content-Type 改 lossy 读取（obs-text boundary 不再误路由 raw 路径）。
`cargo test` → **244 passed / 1 ignored**（+2：空文件 CL:0、/proc 伪文件
无 CL）；clippy/fmt 干净。

**第四轮（round-3 复审通过后的实证新发现）**：go run 直接驱动 `ServeContent`
实证：Go 的响应 size 一律来自打开句柄的 `Seek(End)`（sizeFunc），而非 path
stat——`/dev/zero`/`/dev/urandom` SeekEnd=0 → **200 CL:0 空体**（此前
chunked 无限流）；`/proc/*` SeekEnd 报 EINVAL → sizeFunc 失败 →
**500 `"seeker can't seek\n"`**（fs.go errSeeker 路径；此前"200 chunked 流
真实内容"建立在"Go 会截断"的错误假设上，从未对拍过）。改为统一 SeekEnd
模型：sniff 后 `file.seek(End)`，size 取代 path-stat 用于
Range/CL/Content-Range（顺带关闭 stat-vs-handle race 的 size 侧），失败走
`plain_error(500)`（形状经 fs.go serveError + http.Error 核对一致：
Vary/Disposition 保留、LM 删除、text/plain+nosniff、`"text\n"`）；200 流
limit = size（Go CopyN(sendSize)）。此前 round-3 的 sniffed_empty 分支被
该统一模型取代而删除。`capture.py` 新增 2 场景：
`rest_files_proc_seeker`（500）与 `rest_files_devzero`（CL:0）。
`cargo test` → **245 passed / 1 ignored**；clippy/fmt 干净。

**第五轮（round-5，2026-09-08 复审 cd985b7e）**：仅一个真 bug——`skip_frac_second`
用 `filter` 统计了小数点后**所有**数字而非紧邻的连续数字（Go 是 `for ;
isDigit(value, n); n++`），导致 asctime 年份 / 数字时区被吞掉（`"07:00:00.5 2026"`
解析失败 → IMS/IUS/If-Range 全部退化为 200；Go 分别是 304/412/206）；且按字节
数 `&rest[n..]` 切片会落在 UTF-8 字符中间（obs-text 头 → panic → 请求级 500，
虽被 CatchPanicLayer 捕获）。改为 `take_while` 只吃紧邻数字 + `rest.get(n..)`
边界安全；新增 5 个用例（IMF/RFC850/asctime 小数秒、数字后跟年份/时区、obs-text
边界、以及三布局 × 任意 obs-text 尾的**不 panic 组合扫描**）+ 2 个 handler 测试；
`capture.py` 新增 `rest_files_cond_ims_fraction` /
`rest_files_cond_ims_asctime_fraction`（双端均 304，抽查已确认非"都是 200"蒙混）。
`cargo test` → **250 passed / 1 ignored**；clippy/fmt 干净。

**对拍（各轮均全新容器双端全量重录）**：

```
前三轮 PASS 104  FAIL 0  DECLARED-DIFF 8  SKIP 0  MISSING 0   （112 场景）
第四轮（+2 Seek(End) 场景）PASS 106  FAIL 0  DECLARED-DIFF 8  SKIP 0  MISSING 0   （114 场景）
第五轮（+2 小数秒场景）PASS 108  FAIL 0  DECLARED-DIFF 8  SKIP 0  MISSING 0   （116 场景）
```

存量场景零回归。已知残留（有意声明）：multipart 非 UTF-8 part filename 受
multer str API 限制给 400（Go 会写文件）；Content-Disposition 非 UTF-8
basename 回退 `download`（query 强制 UTF-8，实际不可达）。评审建议的
obs-text fixture 待 capture 客户端支持非 ASCII 头后补录。

## 7. PR-A 错误面对齐 + 阻塞池（2026-09-08，feat/cube-envd-fs-errno，#14 已合并）

新增 6 个非 ENOENT 错误路径场景（`fs_stat_enotdir` / `fs_stat_enametoolong` /
`fs_makedir_through_file` / `fs_move_into_newdir` / `fs_listdir_eloop` /
`fs_listdir_on_file`），错误文案与 code 全部按 go1.26 实测对齐（`go_compat/errno`
逐字表）。EACCES/EROFS 系列有意未加：harness 双侧 root（CAP_DAC_OVERRIDE）分支
不可达，由单测覆盖。全新容器全量重录：

```
PASS 114  FAIL 0  DECLARED-DIFF 8  SKIP 0  MISSING 0   （122 场景）
```

较第五轮基线（116 场景 PASS 108）+6 场景全 PASS，DECLARED-DIFF 8 条不变。

## 8. PR-B WatchDir 家族（2026-09-08，feat/cube-envd-fs-errno）

流式 `WatchDir`（含真递归：合成 Create 事件、cookie 配对改名、子 watch 路径前缀
替换）+ pull watcher 三兄弟（CreateWatcher/GetWatcherEvents/RemoveWatcher）。
事件映射按 e2b fsnotify 源码逐条核对——关键：`IN_MOVED_TO` 是 **CREATE** 而非
RENAME，目录内一次改名产生 RENAME(old)+CREATE(new) 两条。单测 287 passed
（+25：解析含匿名事件不截断批次/展开次序/递归含符号链接不注册/go_rel/断连回收/
缓冲上限/Q_OVERFLOW fatal/keepalive 事件后重置/三兄弟并发竞态）；clippy `-D warnings` 与
fmt 干净。`capture.py` 新增独立组 `--which watch`（9 场景，**不混入 `all`**，
独立灰度，单独统计）：create / write / remove / rename（两帧断言）/
chmod（经沙箱内进程触发 IN_ATTRIB）/ recursive（mkdir -p 两级合成事件）/
disconnect（Start 帧后断连）/ keepalive（`Keepalive-Ping-Interval: 1` 头，
静默窗口恰两帧，节奏机制显式化）/ trio（按上游 watcher_test 全流程）。watcherId
随机值与 not-found 消息内嵌 id 均已归一化。

```
watch 组（双端 fresh 容器） PASS 9  FAIL 0  DECLARED-DIFF 0   （9 场景）
all 全量（同容器序）        PASS 118 FAIL 0  DECLARED-DIFF 4   （122 场景）
```

`fs_watch_unary_probe` 由 DECLARED-DIFF 转 PASS（三兄弟已实现，allowlist 同 PR
移除孤儿条目）；`connect_stream` 的帧读取抽取为
`_read_stream_frames` 供 watch 复用，`all` 存量场景零回归。资源泄漏门：
同一容器连跑 3 遍 watch 组（24 个 watcher 建断，含断连场景）后
`/proc/<pid>/fd` 计数回到基线 10、inotify watch 数 0；`max_user_watches` 触顶
路径显式报错（单测覆盖）。

**复审轮（2026-09-09，4 项发现全部处置）**：
1. `ProcessSelector` 的 `deny_unknown_fields` 删除——注释声称"上游直接拒绝该
   形状"被自家 fixture 证伪：connect-go JSON 解码 `DiscardUnknown` 丢弃未知
   `selector` 键，空 selector 走上游 default 分支。删除后我方
   `validated_selector` 的 (None,None) 分支产出**逐字节相同**的
   `unimplemented` 文案，`proc_sendinput_probe` / `proc_connect_missing` /
   `proc_sendsignal_nested_probe` 三条转 PASS，allowlist 7 → 4（无孤儿）；
   混合形状 `{"selector":…,"pid":8}` 与上游一样按 flat pid 生效（单测锁定）。
2. watch.rs 拆分触发条件命中（非测试代码 >800 行）：按 §10 归合作者重构，
   时序在 PR-B 之后，本 PR 不拆。
3. keepalive 节奏由不可观测变为显式：新增 `watch_keepalive` 场景（1s 头，
   双端各两帧，逐字节一致）；30s 默认与 90s 的差异无法低成本 fixture
   （需静默 30s+），以 README 已知差异条目为声明载体。
4. pull 缓冲上界同样无法低成本 fixture（上游会返回两万条事件的巨型 body），
   声明载体同为 README 已知差异表。

**PR #16 live 评审轮（2026-09-09，chaojixinren 于 QEMU/OpenCloudOS 真沙箱
Rust+Go 对照，3 项发现全部修复）**：
1. **[P1] 递归目录改名后直属文件事件静默丢失**——`IN_MOVE_SELF` 分支在递归
   子目录判断之前就删除 wd 映射并 `inotify_rm_watch`；树内目录 a→b 改名时
   MOVED_TO 已重写路径，MOVE_SELF 又把该目录的 watch 拆掉，后续事件全部丢失
   而两侧仍返回成功。fsnotify 对递归子目录是提前返回、**保留 watch**（inode
   跟随，父目录 MOVED_TO 重写路径）。恢复该顺序；回归测试重放改名并断言
   `b/f` 事件以根相对名继续上报。
2. **[P2] WatchDir 忽略 `Connect-Timeout-Ms`**——泵未消费请求 deadline，超时
   后仍持续发 keepalive。deadline 现在纳入泵 select 生命周期，到期发送
   `deadline_exceeded "context deadline exceeded"` EndStream 错误帧并释放
   inotify fd（单测断言帧形状与流终止）。
3. **[P2] `watch_recursive` 假阳性**——sweep 删 RW 后只重建 W，递归 watch
   对不存在的目录 404，两侧同样失败仍被差分判 PASS，递归验收从未真正执行。
   RW 现于开 watch 前创建，fixture 携带真实序列（start + CREATE/CHMOD a +
   CREATE/CHMOD a/b）。

修复后 fresh 容器复采：watch 组 **9/9 PASS**（含真实递归与断言序列），
289 单测 / clippy `-D warnings` / fmt 全绿。

已知有意偏离（PR 描述同步）：pull watcher 事件缓冲设上界（上游无界累积）；
keepalive 沿用进程流的 30s 默认（上游文件 watch 为 90s，同 LB 空闲超时理由），
`Keepalive-Ping-Interval` 头覆盖语义一致。**注意**：`watch` 组须在未跑
`init_token` 的实例上采集（令牌闸门设置后所有无令牌请求 401，两侧措辞不同）。
## 9. PR-C 数据面（2026-09-09，feat/cube-envd-dataplane）

上传**原地流式写**（对齐上游 `upload.go:68` 的 `O_WRONLY|O_CREATE|O_TRUNC`，
放弃 MVP 期自加的 temp+rename 原子性——三仓 issue 考古零需求、HA failover
重启模型使原子性保护窗口失去意义）。单测 293 passed（基线 9aa30f49 为 289：
+4 数据面测试，−2 旧 write_file 测试改造；复审轮 +2 回归测试。初稿的 263 是
变基前基线 02e9f7e9 的口径）；clippy `-D warnings` 与 fmt 干净。

**RSS 门（实测）**：256MiB 流式上传（1MiB 客户端分块）0.2-0.5s 完成，
daemon RSS 峰值增量 **0-6 MiB**——修复前为 +256MiB 级整包缓冲。

**吞吐门（实测，100MiB 下载 loopback）**：本分支 848-895 MiB/s，与改前基线
（worktree 构建 02e9f7e9 实测 866-899 MiB/s）持平——"下载单任务化"尝试
（通道+专用读任务，256KiB 块）实测仅 535-670 MiB/s，**被测量否决并回退**，
回退理由记录于 `reader_stream` 注释；Go 对照 6333-6969 MiB/s（loopback 上限，
非实现间可比瓶颈）。`all` 全量双录（#16 合入后的 base）**118 PASS / 0 FAIL /
DIFF 4**，上传/下载/条件请求场景零回归。注意：对拍须在**全新容器**上采集——
同容器重复跑 `all` 时，上一轮 `init_token` 的令牌闸门会让下一轮全 401，
perf 制品文件也会污染 ListDir。

落位语义变更（README 已知差异表同步）：上传中并发读可见部分内容、失败留
截断文件、不再 fsync、符号链接跟随（写穿至目标）——全部为上游既有行为。

**复审轮（2026-09-09，chaojixinren 4 项发现全部处置）**：
1. **[High] multipart 写任务未等待**——part 读错误经 `?` 提前返回，写任务
   脱管、结果被丢弃。修复：读错误转发为 writer 终态（与 raw 路径同构），
   writer 必等待；回归测试 = field 1 完整上传 + field 2 数据中途流错误，
   断言 400 + 两个文件的落盘终态。
2. **[Medium] 符号链接属主**——容器探针证实（target=root:root、link 被摸成
   user:user，Go 侧 target=user:user）：lchown 是 temp+rename 时代的产物。
   修复：chown 跟随（对齐上游 `os.Chown`），探针复测双侧一致
   （target=user:user、link=user:user）；单测锁定"内容跟随 + 链接存活"。
3. **[Low] MAX_UPLOAD_SIZE 注释过时**——已改写为流式口径。
4. **[Low] 测试数口径**——见本节开头（263 = 变基前基线，293 = PR head）。

另：harness 修复一处固有 flaky——`VOLATILE_KEYS` 原按 JSON 类型名归一
（Go 整数渲染 vs Rust 浮点），rest_metrics 在宿主负载凑整时必挂；改为
值无关的 `<volatile>`。

## 复现

见 [README.md](README.md)。E2E 需要本地部署环境与两个模板：

```bash
cubemastercli tpl create-from-image --image <cube-envd 镜像> --expose-port 49983 ...
CUBE_API_URL=... CUBE_PROXY_NODE_IP=... TEMPLATE_CUBE=<tpl> TEMPLATE_GO=<tpl> \
  python3 e2e_sdk.py
```
