# SDK 兼容性 E2E 测试框架设计

## 1. 目标与边界

本框架验证 **同一个 CubeSandbox 兼容后端** 在 CubeSandbox Python SDK 与
E2B Python SDK 下的可观察行为是否一致。它不是 SDK 单元测试，也不是服务端
压测框架；测试会根据场景访问真实 CubeAPI、CubeProxy 和 sandbox 数据面。

设计目标：

- 让业务语义测试不依赖某个具体 SDK；
- 把 SDK 参数、返回值和异常差异限制在 adapter；
- 默认安全收集，不因误执行创建线上实例；
- 为失败保留足够的、已脱敏的诊断数据；
- 对平台托管生命周期提供独立的慢测试路径；
- 确保每个测试拥有并清理自己的 sandbox。

非目标：

- 不在共享用例中覆盖 SDK 私有实现细节；
- 正常状态判断不以固定 sleep 模拟平台状态转换；只有为了等待 idle timeout
  触发而使用的受控等待可以例外；
- 不将测试运行器本身作为 CubeProxy、Redis 或 lifecycle-manager 的健康检查替代品。

## 2. 目录与职责

```text
tests/e2e/sdk_compat/
├── adapters/       SDK 归一化、REST 诊断客户端、trace 装饰器
├── framework/      配置、preflight、能力声明、等待、清理、报告、断言
├── cases/          后端无关的行为用例，按 capability domain 划分
├── docs/           框架设计、用例编写和覆盖盘点文档
├── env.example     本地在线执行的环境变量示例
├── conftest.py     pytest 入口、fixture 与 hook
└── pytest.ini      marker 注册与 pytest 默认配置
```

`cases/` 只表达产品行为；`adapters/` 只处理 SDK 差异；`framework/` 只提供
跨用例的测试基础设施。出现 SDK 特有分支时，优先判断它是否应下沉到 adapter
或以 capability 显式表达，而不是在测试函数中写 `if backend == ...`。

## 3. 总体执行流

```text
pytest collection
  -> 未传 --run-e2e：在线用例跳过
  -> 传 --run-e2e
       -> session preflight
       -> 参数化 backend（cubesandbox / e2b）
       -> sdk_sandbox fixture
          -> marker 与 capability 判定
          -> 合并创建参数
          -> create_adapter
             -> SDK adapter
             -> TracingSandboxAdapter
          -> 测试函数
          -> safe_kill / REST DELETE 回退
       -> pytest hooks 写入 JSONL test_result
```

用例面对的公共接口由 `SandboxAdapter` 定义：

- 查询：`sandbox_id`、`info`；
- 命令与文件：`run_command`、`write_file`、`read_file`；
- 代码：`run_code`；
- 生命周期：`pause`、`resume_or_connect`、`kill`、`close`；
- 公网访问：`get_host`、`traffic_access_token`。

返回值使用 `CommandResult`、`CodeResult`、`SandboxInfo` 归一化。用例应对归一化
结果断言，不能解析 SDK 原始对象。

## 4. Adapter 层

### 4.1 责任

`CubeSandboxAdapter` 和 `E2BAdapter` 各自负责：

1. 将框架创建参数转换为 SDK 参数；
2. 将 SDK 返回值映射成框架模型；
3. 对 SDK 版本差异做受控兼容，例如仅在签名支持时传递某参数；
4. 将各 SDK 的非零命令退出、生命周期 API 和 list 行为归一化；
5. 在创建半途中失败时尽力释放已经创建的原始对象。

`adapters/__init__.py` 是 adapter 工厂和 backend 注册表。增加 backend 时必须：

- 实现 `SandboxAdapter` 的全部抽象方法；
- 声明真实 capabilities；
- 接入 create/connect/list 的 factory；
- 为不支持的功能使用 `UnsupportedCapability`，不伪造成功；
- 确认 trace wrapper 能覆盖新接口。

### 4.2 Trace 装饰器

`TracingSandboxAdapter` 不改变业务行为，只在每次 adapter 调用前后记录：

- 操作名、backend、sandbox ID；
- 已脱敏的输入和输出摘要；
- 成功或异常；
- 调用耗时。

文件内容只记录长度；环境变量容器、API key、token、Bearer、密码及私钥类字段
都会被隐藏。trace 是诊断数据，不可视为完整审计日志。

## 5. 配置与 preflight

`SdkE2EConfig.from_env()` 集中解析环境变量和 pytest CLI 覆盖项。配置分为：

- **目标环境**：`CUBE_API_URL`、`CUBE_TEMPLATE_ID`、认证与 CA；
- **执行范围**：`SDK_E2E_BACKENDS`、`--run-e2e`；
- **超时**：create、command、run_code、API、网络探测与生命周期等待；
- **诊断与保留**：trace、报告目录、失败实例保留；
- **平台生命周期**：CubeProxy 节点、admin 端口和 idle/poll 参数。

启用 `--run-e2e` 后，session 级 preflight 会聚合错误而不是在第一个测试才失败：

1. 检查模板 ID；
2. 检查已选择 backend 的 Python 依赖与 E2B 认证；
3. 请求 CubeAPI `/health`，要求状态为 `ok` 或 `healthy`；
4. 查询模板，要求状态为 `ready`、`active` 或 `available`；
5. 启用平台生命周期时，尽力检查 CubeProxy admin heartbeat。

CubeProxy admin 地址不可达时，生命周期 probe 会记录告警而非阻断整个 session；
具体用例仍会在规定窗口内观察不到平台动作时给出可诊断的 skip。对于必须验证
平台生命周期的 CI job，建议将该告警配置为 job failure。

## 6. Fixture、marker 与 capability

`sdk_sandbox` 是核心 function-scope fixture。其生命周期如下：

1. 根据 `requires_capability`、`requires_code_interpreter`、
   `requires_internet` 和 `requires_cubeproxy` 决定执行或跳过；
2. 读取并合并 `sandbox_create_options`；
3. 对 `requires_cubeproxy` 用例注入平台生命周期 idle timeout；
4. 生成带 node ID、backend、run ID 的 metadata；
5. 创建 adapter，并将其绑定到当前 `TraceCollector`；
6. `yield` 给测试函数；
7. 测试结束后按失败保留策略清理；
8. 重置 context-local trace。

主要 fixture：

- `sdk_e2e_config`：session scope，解析环境变量和 pytest CLI 配置；
- `sdk_e2e_reporter`：session scope，写入 JSONL 事件；
- `sdk_e2e_preflight`：session scope，在在线执行前检查环境；
- `sdk_e2e_trace`：function scope，收集当前测试的操作 trace；
- `sdk_backend`：function scope，提供当前参数化 backend；
- `sdk_sandbox`：function scope，创建、提供并清理主 sandbox。

优先级 marker 表示建议的执行层级，而不是功能能力：

- `smoke`：最小在线环境验证；
- `p0`：稳定 PR gate；
- `p1`：每日兼容性回归；
- `p2`：更广或更慢的周期性回归；
- `p3`：发布资格验证；
- `slow`：超出普通 PR 时间预算。

capability 只表达 backend 是否能正确承诺一种行为。目前公共能力有
`lifecycle`、`commands`、`filesystem`、`run_code`；可选能力包括
`code_interpreter`、`pause_resume`、`network_allow_deny`、
`network_public_access`。当前分支的 `platform_lifecycle` 只在
CubeSandbox capability 集合中启用，并不表示 E2B 后端天然不支持平台生命周期。
现状是 E2B SDK 传递的 lifecycle 参数与 CubeAPI 接收字段尚未对齐，导致 E2B
生命周期参数未生效；相关字段兼容修复见
[PR #988](https://github.com/TencentCloud/CubeSandbox/pull/988)。该 PR 合并并
完成版本验证后，应将 E2B capability、创建参数映射和平台生命周期用例重新纳入
双 backend 验证。

## 7. 生命周期状态与就绪性

按照生命周期契约，`state == "running"` 应表示实例已经可用，CubeShim、envd、
kernel 和 Code Interpreter 等数据面链路也应正常。之前出现的
`EnvelopeFlags invalid value 123` 并不是正常的“running 但仍在恢复”状态，而是
cube-lifecycle-manager（CLM）状态与数据流恢复不一致导致的数据面故障，属于
后端 bug。测试中额外的数据面探测用于暴露和定位这类回归，不能把该故障解释为
正常的异步恢复行为。因此：

1. 需要持久化验证时，先写入文件或 kernel 变量；
2. 使用 `wait_until_paused`、`wait_until_running`、
   `wait_for_platform_pause`、`wait_for_platform_destroy` 等 helper；
3. `wait_until_running` 返回后应直接执行实际的 command、file 或 `run_code`；
   若失败，应记录为生命周期/后端回归，而不是继续延长等待来掩盖问题；
4. 断言恢复前后业务状态，而不只断言状态字符串。

平台 auto-pause / auto-kill 的协作链路为：

```text
创建 lifecycle sandbox
  -> CubeMaster 发布 lifecycle metadata
  -> Redis 存储状态与协调信息
  -> cube-lifecycle-manager 检查 idle timeout
  -> CubeProxy / CubeMaster 触发 pause 或 kill
  -> CubeAPI 反映 paused / terminal 状态
```

已暂停实例是平台 sweeper 的终态保护对象；测试必须区分“平台未执行预期动作”
与“主动 pause 后应维持 paused”的语义。

## 8. 清理、保留与资源归属

每个 `sdk_sandbox` 由当前测试独占。默认 teardown 使用 `safe_kill`：

1. 查询实例状态；
2. 对 paused 实例先尝试 `resume_or_connect`；
3. 使用 SDK kill；
4. SDK 失败时回退 `DELETE /sandboxes/{sandboxID}`；
5. 关闭恢复后的 adapter 和原 adapter。

当前 paused 实例需要先 `resume_or_connect` 再执行 kill。TODO：当服务端支持
直接删除 paused 实例后，优化 `safe_kill`，优先直接删除 paused sandbox，并
增加删除结果和实例列表的确认，避免不必要的 resume。

清理错误记录在报告中，不应覆盖测试主体的失败。设置
`SDK_E2E_KEEP_SANDBOX_ON_FAILURE=true` 时，只保留 setup/call 失败的实例；
passed、skipped 和 teardown-only 失败不能被该开关意外保留。
该策略只作用于 fixture 管理的主 sandbox；测试额外创建的 peer/control
sandbox 仍必须由 `managed_control_sandbox` 或 `finally` 清理。保留实例用于
诊断而不是常规资源管理，排查结束后应手动终止。

测试自己创建的额外 control/peer sandbox 必须使用
`managed_control_sandbox`，或在 `finally` 中调用同样的清理路径。

## 9. 报告与诊断

`JsonlReporter` 以 session 为边界持有 `events.jsonl` 文件句柄，并写入：

- `preflight_passed` / `preflight_failed`；
- `sandbox_created`；
- `sandbox_cleanup` / `sandbox_kept`；
- `test_result`。

失败 test result 会携带 traceback、sandbox 信息和最近的 trace 快照。报告层
再次调用脱敏函数，确保即使新的调用方绕开 trace，也不会把原始敏感字段写入
JSONL。HTML 与 JUnit XML 仍由 pytest 插件输出，适合人工阅读和 CI 平台消费。

生成外部报告：

```bash
pytest --run-e2e --junit-xml=reports/sdk-dual/junit.xml
pytest --run-e2e --html=reports/sdk-dual/report.html --self-contained-html
```

实时排查使用：

```bash
pytest --run-e2e --sdk-e2e-trace \
  cases/lifecycle/test_auto_lifecycle.py -vv
```

trace 模式可能在 terminal 显示非敏感命令或代码输出；不要在命令、代码或
metadata 中主动传入不应暴露给测试日志的业务密钥。

## 10. 设计约束与演进方向

- 用例必须保持 backend-neutral；新增 SDK 特性前先定义是否属于公共能力。
- 网络和生命周期依赖外部环境，必须用 marker、可配置 target 和明确的 skip
  条件描述前提。
- 测试框架优先提供确定性等待、统一清理和诊断能力，再增加慢测试覆盖。
- 新增 adapter 方法时，同时更新 base interface、trace wrapper、模型、能力、
  authoring guide 和覆盖盘点。

当前覆盖范围与推荐优化项见
[测试覆盖盘点与优化建议](test-coverage.md)。
