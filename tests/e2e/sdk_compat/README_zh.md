# SDK 兼容性 E2E 测试

本目录包含 Python SDK 兼容性在线端到端测试。相同的后端无关测试用例可
分别通过以下 SDK 执行：

- `cubesandbox`：`sdk/python` 中的 CubeSandbox Python SDK；
- `e2b`：`e2b-code-interpreter` 或 `e2b` Python SDK，连接兼容 CubeSandbox
  的后端。

测试套件默认不执行在线测试。未指定 `--run-e2e` 时，pytest 只进行安全的
收集：所有在线用例都会被跳过，仅运行标记为 `framework` 的纯逻辑单元测试。
默认后端是 `cubesandbox`；使用
`SDK_E2E_BACKENDS=e2b,cubesandbox` 执行双后端兼容性测试。

相关文档：

- [English README](README.md)
- [English framework design](docs/framework-design.md)
- [中文框架设计](docs/zh/framework-design.md)
- [English case authoring guide](docs/case-authoring.md)
- [中文用例编写指南](docs/zh/case-authoring.md)
- [English test coverage and improvement plan](docs/test-coverage.md)
- [中文测试覆盖盘点与优化建议](docs/zh/test-coverage.md)

## Backend 环境变量

`cubesandbox` 后端：

- `CUBE_API_URL`：CubeAPI 控制面地址，默认
  `http://127.0.0.1:3000`；
- `CUBE_TEMPLATE_ID`：用于创建 sandbox 的 READY 模板 ID；
- `CUBE_API_KEY`：目标 CubeAPI 需要认证时使用；
- `CUBE_PROXY_NODE_IP`：runner 无法解析 sandbox wildcard DNS 时使用。

`e2b` 后端：

- `SDK_E2E_BACKENDS=e2b` 或 `SDK_E2E_BACKENDS=e2b,cubesandbox`：启用 E2B
  后端；
- `E2B_API_KEY`：E2B SDK 使用的 API key，必须显式设置；
- `CUBE_API_URL`：兼容 E2B 的 CubeSandbox 控制面地址，adapter 会显式传给
  E2B SDK；
- `SSL_CERT_FILE`：自托管 HTTPS sandbox endpoint 使用的本地 CA 文件。

E2B 后端不会关闭 TLS 证书校验。自托管 HTTPS 环境必须配置
`SSL_CERT_FILE` 或系统 trust store。

## 准备模板

执行在线 E2E 前，需要准备支持 Code Interpreter 的模板，并暴露 envd
(`49983`) 和 Jupyter/Code Interpreter (`49999`)：

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
```

将生成的 template ID 设置为 `CUBE_TEMPLATE_ID`。

`cases/network/test_mask_request_host.py` 会额外临时创建一个也暴露 `8765`
端口的模板（在 `CUBE_PROXY_NODE_IP=127.0.0.1` 走跨节点映射路径时需要）。
可用 `SDK_E2E_MASK_HOST_TEMPLATE_IMAGE` 或 `CUBE_TEMPLATE_E2E_IMAGE` 覆盖镜像。
整套 suite 的 preflight 仍需要一个已 READY 的 `CUBE_TEMPLATE_ID`。

## 快速开始

```bash
cd tests/e2e/sdk_compat
python3 -m venv .venv
. .venv/bin/activate
pip install -r requirements.txt

export CUBE_API_URL=http://10.0.1.5:3000
export CUBE_TEMPLATE_ID=tpl-xxxxxxxxxxxxxxxxxxxxxxxx
export CUBE_PROXY_NODE_IP=10.0.1.2

pytest --run-e2e
```

显式指定 CubeSandbox 后端：

```bash
pytest --run-e2e --sdk-e2e-backends=cubesandbox
```

## 执行范围

```bash
# 快速环境 smoke
pytest --run-e2e -m smoke

# PR gate：稳定的 CubeSandbox 后端测试
pytest --run-e2e -m "smoke or p0" --sdk-e2e-backends=cubesandbox

# 每日双 SDK 兼容性回归
SDK_E2E_BACKENDS=e2b,cubesandbox pytest --run-e2e -m "p0 or p1"

# Volume Plugin 回归（需手动部署并配置插件；cubesandbox >= 0.6.0）
SDK_E2E_VOLUME_PLUGIN=true pytest --run-e2e -m volume --sdk-e2e-backends=cubesandbox

# 更广泛的回归
SDK_E2E_BACKENDS=e2b,cubesandbox \
pytest --run-e2e -m "p0 or p1 or p2"
```

运行单个测试文件、测试函数或参数化后端：

```bash
# lifecycle 文件
pytest --run-e2e cases/lifecycle/test_pause_resume.py

# 单个测试函数
pytest --run-e2e \
  cases/lifecycle/test_pause_resume.py::test_pause_sets_state_paused

# 指定后端
pytest --run-e2e \
  --sdk-e2e-backends=cubesandbox \
  cases/lifecycle/test_pause_resume.py::test_pause_sets_state_paused[cubesandbox]

# 按关键字选择
pytest --run-e2e -k "pause and resume"

# 查看参数化测试的 node ID
pytest --collect-only -q cases/lifecycle/test_pause_resume.py
```

### 平台生命周期测试

自动 pause、auto-resume 和 auto-kill 依赖 CubeProxy、Redis、
cube-lifecycle-manager、CubeMaster 和 Cubelet：

```bash
export SDK_E2E_PLATFORM_LIFECYCLE=true
export CUBE_PROXY_NODE_IP=<cube-proxy-node-ip>

pytest --run-e2e --sdk-e2e-trace \
  cases/lifecycle/test_auto_lifecycle.py
```

如果没有设置 `SDK_E2E_PLATFORM_LIFECYCLE=true`，这些测试会被跳过。目标
计算节点上还需要使用 `READY` 模板。

运行单个生命周期测试：

```bash
pytest --run-e2e --sdk-e2e-trace \
  cases/lifecycle/test_auto_lifecycle.py::test_lifecycle_auto_resume_preserves_state
```

### E2B 双后端

```bash
pip install e2b-code-interpreter
export E2B_API_KEY=<e2b-api-key>
export SSL_CERT_FILE=/root/.local/share/mkcert/rootCA.pem
export SDK_E2E_BACKENDS=e2b,cubesandbox
pytest --run-e2e
```

## E2B SDK 兼容性

同一个环境里同一个包只能装一个版本，因此上面任何一种执行方式验证的都只是"当前恰好装
着的那个"版本。`e2b-versions.txt` 记录本套件实际验证过的版本，使用者读它即可判断某个
CubeSandbox 版本预期能配合哪些 SDK 版本，而不必等线上故障才发现。

验证某个版本时，装上它再照常执行。下面的命令假定"快速开始"里那套 E2B 后端环境变量已经
导出——如果是从 `e2b-versions.txt` 直接跳到这一节，请先设置，否则 preflight 会在创建任何
沙箱之前直接结束整个 session（`e2b backend requires E2B_API_KEY`，退出码 2）：

```bash
export E2B_API_KEY=<e2b-api-key>
# 自建 HTTPS 端点还需要本地 CA：
export SSL_CERT_FILE=/root/.local/share/mkcert/rootCA.pem

# 完整覆盖：配对安装 interpreter 包，run_code 用例依赖它。
# 挑一个 pip 能直接解出来的组合：`e2b-code-interpreter` 声明 `e2b>=2.26.0,<3.0.0`，
# 因此 `e2b-versions.txt` 里 2.21.0 那一行只有覆盖约束才装得上（该行自己有说明），
# 不适合作为第一次执行时照抄的命令。
pip install 'e2b==2.26.0' 'e2b-code-interpreter==2.8.1'
pytest --run-e2e --sdk-e2e-backends=e2b -m "smoke or p0"

# 只装核心 SDK 时**必须**排除依赖 interpreter 的用例：e2b 后端无条件声明了
# run_code capability，不排除就会被收集、在纯 SDK 上调用 Sandbox.run_code，
# 最终报成失败——而那只是缺包，不是真的不兼容。
#
# 注意选择范围，它决定这个排除条件到底起不起作用：在 `smoke or p0` 之内，依赖 interpreter
# 的用例只在 cases/run_code/ 下，而它们两个 marker 都带，所以两种写法选出的是同一组用例，
# 怎么写都一样。（这里刻意不写具体数量——想知道当前值，就在这个范围上跑下面那条 diff。）
# 一旦范围放宽就不一样了——只装核心 SDK 跑 `p0 or p1`（也就是上
# 面那套双后端回归）会收集到 p1 的 lifecycle 用例，只有 `requires_code_interpreter` 排得掉。
#
# 要按 `requires_code_interpreter` 排除，而不是按 `run_code` marker。后者只存在于
# cases/run_code/ 下，而依赖 interpreter 的用例在 cases/lifecycle/ 里同样有（写作时是
# test_pause_resume.py、test_auto_lifecycle.py 与 test_rollback_clone.py），它们带
# `requires_code_interpreter` 但没有 run_code marker。`requires_code_interpreter`
# 才是不依赖"用例放在哪个文件"的那个开关——这正是关键，因为用例还在不断新增。
#（它是由 CODE_INTERPRETER capability 控制的专用 marker。写成通用的
# `requires_capability(CODE_INTERPRETER)` 效果等价，但本套件统一用专用的那个：
# skip 信息更清楚，上面的 -m 表达式也是按它来选的。）
#
# 想知道两种写法当前差多少，请自己跑，而不要相信这里写死的数字（数字会腐烂，本注释
# 之前就带过一个过期的）：
#
#   diff <(pytest --collect-only -q -m "(p0 or p1) and not run_code") \
#        <(pytest --collect-only -q -m "(p0 or p1) and not requires_code_interpreter")
#
# 差集里也不是每个都会因缺包而失败：对 e2b 后端，auto_lifecycle 那几个还要求
# platform_lifecycle capability、rollback_clone 那两个要求 rollback_clone，而后端都没有
# 声明，fixture 会先跳过。按 `requires_code_interpreter` 排除则一次覆盖全部。
pip install 'e2b==2.29.5'
pytest --run-e2e --sdk-e2e-backends=e2b -m "(smoke or p0) and not requires_code_interpreter"
```

每个版本在独立虚拟环境中重复一次，发版时把结果记入 `e2b-versions.txt`。

几条让这份清单不只是版本号的说明：

- **版本号本身不是兼容性信号。** `e2b` 2.26/2.29 的 `commands.run` 在 v0.5.1-rc5 的
  envd 上静默挂死（SDK 自带的 timeout 不生效），在 v0.5.1 正式版上正常——而两个构建里
  envd 自报的版本都是 `0.5.11`。所以兼容性必须按 CubeSandbox 版本声明，不能从 envd
  版本推导。
- **`e2b-code-interpreter` 有自己的版本序列。** 它的 2.9.0 要求
  `e2b>=2.26.0,<3.0.0`，因此固定较低 `e2b` 版本的组合无法同时使用当前的 interpreter
  包，该组合下 `run_code` 用例不可用。
- **依赖 interpreter 的用例。** 未安装 `e2b-code-interpreter` 的组合应按 capability
  marker 排除（`-m "(smoke or p0) and not requires_code_interpreter"`），而不是把
  "缺包"报成失败。只排除 `run_code` marker 不够：它仅覆盖 `cases/run_code/`，而
  `cases/lifecycle/` 下也有带 `requires_code_interpreter`、但没有 `run_code` marker 的依赖
  interpreter 用例。注意本套件用的是专用的 `requires_code_interpreter`，而不是通用的
  `requires_capability(CODE_INTERPRETER)`——两者等价，`-m` 表达式按前者来写。

## 环境变量

测试会自动加载 `tests/e2e/sdk_compat/.env`。已经在 shell 中导出的变量
优先级高于 `.env`：

```bash
cp env.example .env
```

内置 `.env` loader 比较轻量：只支持每行一个 `KEY=VALUE`，以及简单的
单引号/双引号值；不支持多行 quoted value。多行 secret、private key 或
复杂配置建议直接在 shell 中 export，不要写入 `.env`。

必需变量：

- `CUBE_API_URL`：CubeAPI 地址；
- `CUBE_TEMPLATE_ID`：用于创建 sandbox 的 READY 模板 ID。

常用可选变量：

- `SDK_E2E_BACKENDS`：后端列表，默认 `cubesandbox`；
- `CUBE_API_KEY`：目标环境需要 API key 时使用；
- `E2B_API_KEY`：运行 E2B 后端时需要；
- `SDK_E2E_E2B_VALIDATE_API_KEY`：启用 E2B SDK 的客户端 `e2b_*` key
  格式检查；自托管环境默认 `false`，服务端鉴权不受影响；
- `CUBE_PROXY_NODE_IP`：无法解析 wildcard sandbox DNS 时使用；
- `CUBE_PROXY_PORT_HTTP`：默认 `80`；
- `CUBE_SANDBOX_DOMAIN`：默认 `cube.app`；
- `SDK_E2E_DEFAULT_TIMEOUT`：显式 connect、cleanup resume 等操作的默认
  超时，默认 `120` 秒；
- `SDK_E2E_API_TIMEOUT`：CubeAPI 控制面请求超时，用于 preflight、诊断
  和清理，默认 `5` 秒；
- `SDK_E2E_CREATE_TIMEOUT`：创建超时，默认 `120` 秒；
- `SDK_E2E_CREATE_CAPACITY_RETRIES`：当调度器瞬时返回 `no more resource`
  （错误码 `130597`）时，额外重试创建 sandbox 的次数，给刚释放的节点留出回收
  时间，默认 `5`；设为 `0` 可关闭重试、遇到容量错误即失败；
- `SDK_E2E_CREATE_CAPACITY_BACKOFF`：容量重试的基础退避秒数，按次指数增长，
  并加入完全抖动（full jitter），避免并行 worker 同步重试，默认 `2`；
- `SDK_E2E_CREATE_CAPACITY_BACKOFF_MAX`：容量重试退避的上限秒数，默认 `30`；
  取值 `<= 0` 表示关闭单次退避上限（退避将增长至内置的 `3600` 秒上限），
  并非“无退避”，除非确实需要无上限增长，否则请保持为正值；
- `SDK_E2E_CREATE_CAPACITY_BUDGET`：单次创建在所有容量重试中累计的**休眠**时间
  上限（秒），默认 `90`；设为 `0` 可关闭、仅依赖 `RETRIES`。它仅约束累计退避
  休眠，不约束 `create()` 调用本身：每次尝试仍可耗时至多 `SDK_E2E_CREATE_TIMEOUT`，
  因此在调度器缓慢拒绝时，单个用例最坏可耗时约
  `(RETRIES + 1) × CREATE_TIMEOUT + BUDGET`。在快速拒绝路径（立即返回 HTTP 500）
  下，该预算可有效约束单次创建的重试时长；
- `SDK_E2E_COMMAND_TIMEOUT`：命令超时，默认 `30` 秒；
- `SDK_E2E_RUN_CODE_TIMEOUT`：代码执行超时，默认 `60` 秒；
- `SDK_E2E_NETWORK_PROBE_TIMEOUT`：network policy 用例中的 TCP socket
  探测超时，默认 `5` 秒；
- `SDK_E2E_TCP_TARGET_IP`：公网 TCP 探测地址，默认 `8.8.8.8`；
- `SDK_E2E_TCP_TARGET_PORT`：公网 TCP 探测端口，默认 `53`；
- `SDK_E2E_ALTERNATE_TCP_TARGET_IP`：备用公网 TCP 探测地址，默认
  `1.1.1.1`；
- `SDK_E2E_PUBLIC_ACCESS_PORT`：限制公开访问入站用例访问的已暴露 HTTP
  端口，默认 `49983`；
- `SDK_E2E_PUBLIC_ACCESS_PATH`：限制公开访问入站用例访问的路径，默认
  `/health`；
- `SDK_E2E_PUBLIC_ACCESS_EXPECTED_STATUS`：限制公开访问入站用例期望的
  成功 HTTP 状态码，默认 `204`；
- `SDK_E2E_PUBLIC_ACCESS_EXPECTED_BODY`：限制公开访问入站用例期望的
  成功响应 body，默认空字符串；默认公网 URL 使用 HTTP，traffic access token
  会以明文 header 发送，跨网络或多租户 CI 环境应使用 HTTPS endpoint；
- `SDK_E2E_KEEP_SANDBOX_ON_FAILURE`：仅保留 setup/call 失败的 sandbox；
- `SDK_E2E_TRACE`：输出每次 SDK adapter 操作；
- `SDK_E2E_SKIP_INTERNET_TESTS`：当 runner 或环境没有稳定公网出站时，
  跳过 `requires_internet` 测试，默认 `false`；
- `SDK_E2E_REPORT_DIR`：JSONL 报告目录；
- `SDK_E2E_WORKERS`：`--run-e2e` 的 pytest-xdist worker 数量。并行需显式开启，
  未设置（或 `0`/`1`/`no`/`off`）时串行运行，避免压垮同机的控制面；传整数、
  `auto` 或 `logical` 才会并行；显式 `-n`/`--numprocesses`（或 `-p no:xdist`）
  优先。不带 `--run-e2e` 时忽略，因此 hermetic `framework` gate 仍为串行；
- `SDK_E2E_TEMPLATE_BUILD_CONCURRENCY`：xdist 各 worker 间并发的 live template
  build 上限，默认 `1`（完全串行，使结果与串行运行一致）；小于 `1` 或非整数回退
  为 `1`；当取值不小于 worker 数时跳过节流；仅 POSIX（无 `fcntl` 时为 no-op）；
  节流按 UID 而非按 run 命名：同一用户在同一主机上并发的两个 `--run-e2e` 任务会
  共享 slot 并相互串行化其构建。这是有意为之——两个任务都在争用同一台共享构建
  主机——且 `SDK_E2E_TEMPLATE_BUILD_WAIT` 上限会限制任务在降级为不节流前的等待
  时长；
- `SDK_E2E_TEMPLATE_BUILD_WAIT`：等待 build slot 的单前驱上限（秒），超时后该
  worker 放弃节流直接构建，避免某个卡死的 worker 拖垮整个套件；实际等待时间按
  worker 数缩放；默认 `1800`；`<= 0` 表示无限等待；
- `SDK_E2E_PLATFORM_LIFECYCLE`：启用平台生命周期测试；
- `SDK_E2E_PLATFORM_LIFECYCLE_IDLE_TIMEOUT`：平台空闲超时，默认 `30` 秒；
- `SDK_E2E_PLATFORM_LIFECYCLE_WAIT_MARGIN`：额外等待时间，默认 `20` 秒；
- `SDK_E2E_PLATFORM_LIFECYCLE_POLL_TIMEOUT`：轮询窗口，默认 `45` 秒；
- `CUBE_PROXY_ADMIN_PORT`：CubeProxy admin 端口，默认 `8082`；
- `SDK_E2E_VOLUME_PLUGIN`：启用 Volume Plugin 用例（CRUD 与 sandbox
  `volumeMounts` 绑定/解绑），默认 `false`；
- `SDK_E2E_VOLUME_DRIVER`：`POST /volumes` 使用的 driver，默认 `cos`；
- `SDK_E2E_VOLUME_REFCOUNT_WAIT`：等待绑定中删除 `409` / 解绑后 `204`
  的秒数，默认 `60`。

### 失败时保留 sandbox

默认情况下，测试结束后会清理主 sandbox。调试 setup 或测试主体
（call phase）失败时，可以显式保留实例：

```bash
export SDK_E2E_KEEP_SANDBOX_ON_FAILURE=true
pytest --run-e2e -vv cases/lifecycle/test_auto_lifecycle.py
```

该选项只保留 setup/call 失败的主 sandbox，便于通过 `info()`、CubeAPI、
CubeProxy 和 lifecycle-manager 日志排查问题。以下情况仍会清理：

- 测试通过或被跳过；
- 只有 teardown 阶段失败；
- 测试额外创建的 peer/control sandbox（必须由测试自己的 context manager
  或 `finally` 清理）。

保留实例会占用资源，不应作为常规运行配置。排查完成后应手动终止实例，
或恢复默认配置：

```bash
unset SDK_E2E_KEEP_SANDBOX_ON_FAILURE
```

自托管 HTTPS 环境优先使用本地 CA：

```bash
export SSL_CERT_FILE=/root/.local/share/mkcert/rootCA.pem
```

E2B 后端不会关闭 TLS 证书校验。自托管环境需要通过 `SSL_CERT_FILE` 或
系统 trust store 信任本地 CA。

## Preflight

启用 `--run-e2e` 后，session 级 preflight 会检查：

- `CUBE_TEMPLATE_ID` 或 `--cube-template-id` 是否存在；
- CubeAPI `/health` 是否可访问；
- 目标模板是否存在；
- 模板状态是否为 ready-like：`ready`、`active` 或 `available`。

启用平台生命周期时，如果设置了 `CUBE_PROXY_NODE_IP`，还会检查
CubeProxy admin heartbeat。

## 报告和 Trace

JSONL 报告写入 `SDK_E2E_REPORT_DIR`。串行运行写入单个 `events.jsonl`；启用
pytest-xdist 后，每个 worker 各自写入 `events-gw0.jsonl`、`events-gw1.jsonl`……
以避免行交错，因此应读取或聚合 `events*.jsonl` 而非固定的 `events.jsonl`：

```text
SDK_E2E_REPORT_DIR/events.jsonl        # 串行
SDK_E2E_REPORT_DIR/events-gw0.jsonl    # xdist worker
```

主要事件包括：

- `preflight_passed` / `preflight_failed`；
- `sandbox_created`；
- `sandbox_cleanup` / `sandbox_kept`；
- `test_result`。

生成 HTML 报告：

```bash
pytest --run-e2e -m lifecycle \
  --html=reports/sdk-dual/report.html \
  --self-contained-html
```

生成 CI 使用的 JUnit XML 报告：

```bash
pytest --run-e2e -m lifecycle \
  --junit-xml=reports/sdk-dual/junit.xml
```

失败结果会包含错误、sandbox 信息和最近的 SDK 操作 trace。trace 会对
API key、环境变量等敏感值脱敏，并截断过大的字符串和集合。文件内容只
记录长度，不记录明文或内容 hash。

启用实时 trace：

```bash
pytest --run-e2e --sdk-e2e-trace \
  cases/lifecycle/test_pause_resume.py::test_pause_sets_state_paused

SDK_E2E_TRACE=true pytest --run-e2e -m lifecycle
```

Trace 模式可能在 terminal 中输出非敏感的命令或代码结果；JSONL 报告仍会
执行脱敏。

## 目录结构

```text
tests/e2e/sdk_compat/
  adapters/       SDK adapter 和 tracing proxy
  framework/      配置、preflight、capability、清理、报告
  cases/          按 capability domain 划分的后端无关用例
  docs/           框架设计、用例编写、覆盖盘点与优化建议
  reports/        本地 JSONL 报告
  e2b-versions.txt  本套件已验证过的 E2B SDK 版本
  README.md
  README_zh.md
```

当前测试域：

- `cases/lifecycle/`：创建、info、connect、create options、pause/resume、
  kill、auto-pause、auto-resume、auto-kill；
- `cases/commands/`：stdout、stderr、退出码、环境变量、特殊字符、多行
  输出和缺失命令；
- `cases/filesystem/`：读写、覆盖、多行内容、文件 API 与 shell 互操作；
- `cases/run_code/`：表达式结果、stdout、kernel 状态和 Python 错误；
- `cases/network/`：创建时的 allow/deny 和公网出站策略，以及运行中沙箱的策略原地
  更新（含存量连接重判，见 `test_policy_update.py`，仅 CubeSandbox）；
- `cases/concurrency/`：同时运行多个 sandbox 时的数据隔离；
- `cases/host-mount/`：宿主目录挂载扩展——happy path，以及创建时校验、
  运行期 bind-mount 失败和跨 sandbox 共享等边界用例。
- `cases/volume/`：Volume Plugin CRUD、sandbox `volumeMounts` 绑定/解绑，以及每个沙箱挂载点的只读约束（需 `SDK_E2E_VOLUME_PLUGIN=true`；仅 CubeSandbox）。插件需手动部署并配置，且要求 `cubesandbox` >= 0.6.0。
- `cases/auth/`：`CUBE_API_KEY` 简单密钥鉴权，针对 CubeAPI 控制面——
  `X-API-Key`/`Bearer` 通过、错误/缺失返回 401、`/health` 豁免（仅 CubeSandbox）。
  仅当服务端以 `CUBE_API_KEY` 启动且 runner 导出相同 key 时才运行，否则跳过。

新增测试应保持后端无关，通过 capability marker 表达后端差异。

## Marker 和 Capability

优先级 marker：

- `smoke`：最小在线环境检查；
- `p0`：稳定 PR gate 覆盖；
- `p1`：每日兼容性回归；
- `p2`：更广或更慢的每周覆盖；
- `p3`：发布准入与长时间运行覆盖；
- `slow`：超过普通 PR 时间预算的用例。

Capability marker：

- `@pytest.mark.requires_capability("<name>")`：当前后端不支持时跳过；
- `@pytest.mark.sandbox_create_options(...)`：传入 `network`、`env_vars`、
  `lifecycle` 等 sandbox 创建参数；
- `@pytest.mark.sandbox_template_id("tpl-...")`：为单个用例或模块级用例集
  覆盖模板 ID；未设置时使用 `CUBE_TEMPLATE_ID` 或 `--cube-template-id`；
- `@pytest.mark.requires_cubeproxy`：依赖 CubeProxy/lifecycle-manager
  协调，未设置 `SDK_E2E_PLATFORM_LIFECYCLE=true` 时跳过；
- `@pytest.mark.volume`：Volume Plugin 用例，未设置
  `SDK_E2E_VOLUME_PLUGIN=true` 时跳过。
- `@pytest.mark.auth`：`CUBE_API_KEY` 简单密钥鉴权用例，未为 runner 设置
  `CUBE_API_KEY` 或后端不支持 `auth_simple_key`（仅 CubeSandbox）时跳过。

公共 capability 有 `lifecycle`、`commands`、`filesystem`、
`filesystem_extended`、`run_code`。可选 capability 包括 `code_interpreter`、
`pause_resume`、`set_timeout`、`rollback_clone`、`network_allow_deny`、
`network_public_access`、`network_mask_request_host`、`platform_lifecycle`、
`host_mount`、`volume_plugin` 和 `auth_simple_key`。
当前分支的 `platform_lifecycle` 与 `volume_plugin` 仅在 CubeSandbox
capability 集合中启用。
这不是 E2B 的固有能力限制，而是 E2B SDK 传递的 lifecycle 参数与 CubeAPI
接收字段尚未对齐，导致 E2B 生命周期参数暂未生效。相关兼容修复见
[PR #988](https://github.com/TencentCloud/CubeSandbox/pull/988)；修复合并并
完成版本验证后，应重新启用 E2B 平台生命周期 capability 和双 backend 用例。
`host_mount` 是 CubeSandbox 独有扩展；`cases/host-mount/` 通过
`@pytest.mark.requires_capability("host_mount")` 跳过不支持宿主目录挂载的后端（如 e2b）。
`volume_plugin` 仅用于 CubeSandbox Volume Plugin 用例。

## 清理

每个测试独立创建 sandbox，并在 teardown 中清理。SDK 清理失败时，框架会
回退到针对 `CUBE_API_URL` 的 `DELETE /sandboxes/{sandboxID}`。

调试失败实例时可以设置：

```bash
export SDK_E2E_KEEP_SANDBOX_ON_FAILURE=true
```

该开关仅保留通过 `sdk_sandbox` fixture 创建、且**失败**的测试的 sandbox；通过和
跳过的测试始终会被清理，直接创建 sandbox 的边界用例（使用各自的 helper）也始终清理。
