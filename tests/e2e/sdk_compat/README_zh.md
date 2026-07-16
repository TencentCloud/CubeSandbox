# SDK 兼容性 E2E 测试

本目录包含 Python SDK 兼容性在线端到端测试。相同的后端无关测试用例可
分别通过以下 SDK 执行：

- `cubesandbox`：`sdk/python` 中的 CubeSandbox Python SDK；
- `e2b`：`e2b-code-interpreter` 或 `e2b` Python SDK，连接兼容 CubeSandbox
  的后端。

测试套件默认不执行在线测试。未指定 `--run-e2e` 时，pytest 只进行安全的
收集。默认后端是 `cubesandbox`；使用
`SDK_E2E_BACKENDS=e2b,cubesandbox` 执行双后端兼容性测试。

相关文档：

- [English README](README.md)
- [English framework design](docs/framework-design.md)
- [中文框架设计](docs/framework-design.zh-CN.md)
- [English case authoring guide](docs/case-authoring.md)
- [中文用例编写指南](docs/zh/case-authoring.md)

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
- `SDK_E2E_COMMAND_TIMEOUT`：命令超时，默认 `30` 秒；
- `SDK_E2E_RUN_CODE_TIMEOUT`：代码执行超时，默认 `60` 秒；
- `SDK_E2E_NETWORK_PROBE_TIMEOUT`：network policy 用例中的 TCP socket
  探测超时，默认 `5` 秒；
- `SDK_E2E_TCP_TARGET_IP`：公网 TCP 探测地址，默认 `8.8.8.8`；
- `SDK_E2E_TCP_TARGET_PORT`：公网 TCP 探测端口，默认 `53`；
- `SDK_E2E_ALTERNATE_TCP_TARGET_IP`：备用公网 TCP 探测地址，默认
  `1.1.1.1`；
- `SDK_E2E_KEEP_SANDBOX_ON_FAILURE`：仅保留 setup/call 失败的 sandbox；
- `SDK_E2E_TRACE`：输出每次 SDK adapter 操作；
- `SDK_E2E_SKIP_INTERNET_TESTS`：当 runner 或环境没有稳定公网出站时，
  跳过 `requires_internet` 测试，默认 `false`；
- `SDK_E2E_REPORT_DIR`：JSONL 报告目录；
- `CUBE_PYTHON_SDK_PATH`：覆盖本地 CubeSandbox Python SDK 路径；
- `SDK_E2E_PLATFORM_LIFECYCLE`：启用平台生命周期测试；
- `SDK_E2E_PLATFORM_LIFECYCLE_IDLE_TIMEOUT`：平台空闲超时，默认 `30` 秒；
- `SDK_E2E_PLATFORM_LIFECYCLE_WAIT_MARGIN`：额外等待时间，默认 `20` 秒；
- `SDK_E2E_PLATFORM_LIFECYCLE_POLL_TIMEOUT`：轮询窗口，默认 `45` 秒；
- `CUBE_PROXY_ADMIN_PORT`：CubeProxy admin 端口，默认 `8082`。

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

JSONL 报告写入：

```text
SDK_E2E_REPORT_DIR/events.jsonl
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
  reports/        本地 JSONL 报告
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
- `cases/network/`：创建时的 allow/deny 和公网出站策略；
- `cases/concurrency/`：同时运行多个 sandbox 时的数据隔离。

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
- `@pytest.mark.requires_cubeproxy`：依赖 CubeProxy/lifecycle-manager
  协调，未设置 `SDK_E2E_PLATFORM_LIFECYCLE=true` 时跳过。

常用 capability 有 `lifecycle`、`commands`、`filesystem`、`run_code`。
可选共享 capability 包括 `pause_resume`、`network_allow_deny`、
`network_public_access`。
`platform_lifecycle` 仅由 CubeSandbox 的平台托管生命周期用例提供。

## 清理

每个测试独立创建 sandbox，并在 teardown 中清理。SDK 清理失败时，框架会
回退到针对 `CUBE_API_URL` 的 `DELETE /sandboxes/{sandboxID}`。

调试失败实例时可以设置：

```bash
export SDK_E2E_KEEP_SANDBOX_ON_FAILURE=true
```

通过和跳过的测试仍会清理 sandbox。
