# SDK 兼容性 E2E 测试覆盖盘点与优化建议

## 1. 阅读方式与统计口径

本文盘点 `tests/e2e/sdk_compat/cases/` 下的在线兼容性用例。每个共享用例会按
`SDK_E2E_BACKENDS` 参数化；因此“用例函数数”与 pytest 最终 node 数不同。
例如同时选择 `e2b,cubesandbox` 时，支持两个 backend 的一个函数通常产生两个
node。

用以下命令获取当前环境的准确集合：

```bash
cd tests/e2e/sdk_compat
pytest --collect-only -q
SDK_E2E_BACKENDS=e2b,cubesandbox pytest --collect-only -q
```

在线执行前必须加 `--run-e2e`。平台生命周期用例还需要：

```bash
SDK_E2E_PLATFORM_LIFECYCLE=true \
pytest --run-e2e -m "lifecycle and slow"
```

## 2. 当前覆盖清单

### 2.1 Lifecycle

| 文件 | 主要行为 | 能力/前提 | 风险与执行建议 |
| --- | --- | --- | --- |
| `cases/lifecycle/test_create.py` | 创建后的 `info`、Linux command smoke | `lifecycle` | P0/PR gate 候选 |
| `cases/lifecycle/test_connect.py` | connect 既有实例、ID 与文件/命令可用性 | `lifecycle` | P1 |
| `cases/lifecycle/test_create_options.py` | metadata、env vars、timeout 和创建参数后的 command | `lifecycle` | P1 |
| `cases/lifecycle/test_pause_resume.py` | SDK pause、connect resume、文件/env/kernel 状态保留 | `pause_resume`，部分需 Code Interpreter | P1 |
| `cases/lifecycle/test_kill.py` | kill 后不可连接、列表移除、重复 kill 终态语义 | `lifecycle` | P1 |
| `cases/lifecycle/test_auto_lifecycle.py` | auto-pause、手动/自动恢复、重入、auto-kill、主动 pause 与 timeout 的交互 | `platform_lifecycle`、CubeProxy、lifecycle-manager；部分需 Code Interpreter | P1 + `slow`，每日运行 |

当前清单中的 `platform_lifecycle` 前提只代表本分支的执行配置：
它依赖 CubeProxy 与 lifecycle-manager 协调，但 E2B 暂未启用是因为其 SDK
生命周期参数与 CubeAPI 字段尚未对齐，并非 E2B 后端的设计性限制。PR #988
修复该创建参数兼容问题；合并并完成部署验证后，应补充 E2B capability 并将
本组用例纳入双 backend 覆盖。

生命周期覆盖的强项是同时验证控制面 state、文件、kernel 状态和 command 数据面。
需要注意主动 pause 后的 auto-kill 语义当前作为回归行为记录：实例在 timeout 后
保持 `paused`。当服务端支持 timeout 回收主动 paused 实例后，应将该测试更新为
预期 terminal 状态。清理侧的 `safe_kill` 目前需要先恢复 paused 实例；TODO 是
待服务端支持直接删除 paused 实例后改为直接删除，并确认实例已从列表消失。

### 2.2 Commands

`cases/commands/test_run.py` 覆盖：

- stdout、stderr、非零 exit code；
- 环境变量；
- 特殊字符和多行输出；
- 缺失命令的 `127`；
- command timeout 的错误语义。

这些用例验证 adapter 对命令返回结果的归一化，是最适合作为 P0/P1 基础回归的
数据面测试。

### 2.3 Filesystem

`cases/filesystem/test_read_write.py` 覆盖：

- 文件 write/read round-trip；
- 覆盖写、多行文本和较深路径；
- 较大文本；
- 文件 API 与 shell 双向互操作；
- 读取不存在文件的错误语义。

当前覆盖以文本文件为主，尚未覆盖目录、权限、二进制、原子覆盖与并发访问。

### 2.4 Run code

`cases/run_code/test_python.py` 覆盖：

- 表达式结果文本；
- stdout 与 stderr 捕获；
- Python 错误和语法错误；
- stateful kernel 变量保留。

这些场景要求 Code Interpreter 能力。它们验证的是框架归一化后的 `CodeResult`，
而非单个 SDK 的内部响应格式。

### 2.5 Network

`cases/network/test_policy.py` 覆盖创建时网络策略：

- `allow_out` 穿透 `deny_out=0.0.0.0/0`；
- deny-all 阻断公共 TCP；
- `allow_internet_access=False` 阻断公共 TCP；
- 禁用公共网络后指定 allowlist 仍可访问；
- 指定 deny target 时其他公共 target 仍可访问；
- 无公网时 sandbox 内部 command 仍可运行；
- 限制公网 URL 访问时，缺失/错误 token 返回 403，`e2b-traffic-access-token`
  与 `cube-traffic-access-token` 携带正确 token 时均可访问。

当前以可配置的公共 TCP endpoint 验证 L3/L4 出站策略。用例带
`requires_internet`，运行器没有稳定公网时应使用
`SDK_E2E_SKIP_INTERNET_TESTS=true` 跳过。

### 2.6 Concurrency

`cases/concurrency/test_isolation.py` 目前覆盖两个 sandbox 同路径不同内容的文件
隔离，第二个实例通过 `managed_control_sandbox` 创建和清理。

它证明了基础实例隔离，但不等于并发压力、资源竞争或多 worker 安全性验证。

## 3. 覆盖边界

当前 suite 主要验证 Python SDK 的同步常用路径和 CubeSandbox/E2B 兼容表面：

- 已验证：创建、查询、命令、文本文件、Python code、pause/resume、kill、
  部分自动生命周期、基本出站策略、两实例文件隔离；
- 未验证：异步 SDK、流式输出、template/snapshot/metadata 完整 API、目录与
  二进制文件、完整 HTTP/HTTPS/L7/UDP-DNS 网络语义、取消与资源限制、真实并发
  负载、多节点故障恢复。

这不是缺陷清单本身；是否增加用例取决于 API 契约成熟度、环境可重复性和 CI
预算。下方优先级按“影响兼容性正确性与回归发现能力”排序。

## 4. 推荐补充用例

### P0：阻止兼容性假阳性

| 主题 | 建议用例 | 预期收益 | 建议位置 |
| --- | --- | --- | --- |
| Adapter contract | 为每个 adapter 建立 hermetic contract 测试：参数映射、结果归一化、异常映射、unsupported capability | 避免只有在线环境才能发现 adapter 回归 | `tests/e2e/sdk_compat/tests/` 或 adapter 邻近单测目录 |
| 清理语义 | running、paused、resume 失败、REST fallback、重复 teardown 的单测 | 防止在线 suite 泄漏实例 | `framework/cleanup` 单测 |
| Preflight | 缺失 SDK/key、health 异常、模板不存在/非 READY、生命周期 heartbeat 过期 | 将配置错误提前到 session 开始 | `framework/preflight` 单测 |
| 关键 lifecycle 终态 | auto-kill 后 connect、command、list、REST 查询的一致终态断言 | 防止“请求失败但实例仍泄漏” | `cases/lifecycle/` |
| 报告数据契约 | JSONL schema、trace 截断、敏感字段脱敏、文件长度摘要 | 保证失败诊断可被 CI 消费且不会泄露 secret | `framework/trace`、`framework/reporting` 单测 |

### P1：每日兼容性与真实环境覆盖

| 主题 | 建议用例 | 预期收益 | 建议位置 |
| --- | --- | --- | --- |
| 生命周期竞争 | pause 与 timeout 同时发生、resume 请求重入、kill 与 resume 竞争 | 定位状态机与分布式锁问题 | `cases/lifecycle/` |
| 生命周期时间语义 | timeout 刷新、手动 pause 后 timeout、manual connect 后新 idle 周期、endAt 漂移边界 | 验证 lifecycle contract 而非单次状态 | `cases/lifecycle/` |
| 文件系统 | 目录创建/列举、权限与非 root user、二进制 round-trip、空文件、删除/rename | 扩大 SDK 文件 API 兼容表面 | `cases/filesystem/` |
| 命令 | 工作目录、用户切换、信号/取消、输出截断、大输出与 timeout 后资源释放 | 捕获 process/stream 清理问题 | `cases/commands/` |
| Run code | 多单元异常后 kernel 是否可继续、超时、资源超限、富结果与图表、语言参数 | 验证解释器韧性 | `cases/run_code/` |
| Network DNS/UDP | 域名 allow/deny、DNS 解析失败、UDP DNS 请求响应 | 不能用 TCP 结果代替 DNS/UDP 策略 | `cases/network/` |
| Network HTTP/HTTPS | HTTP host/path/method、HTTPS SNI 与证书、代理行为 | 验证真实应用流量 | `cases/network/` |
| 双实例 | 同时 create/run/kill，多实例文件/环境/网络隔离 | 发现 ID、session 或网络规则串扰 | `cases/concurrency/` |

### P2：平台、性能与发布资格

| 主题 | 建议用例 | 预期收益 | 建议位置 |
| --- | --- | --- | --- |
| L7 策略 | rule 优先级、header 注入、audit、deny/allow 覆盖 | 覆盖当前 L3/L4 之外的策略平面 | `cases/network/` |
| 多节点 | sandbox 被调度到不同 compute node 时的 create/connect/pause/resume | 发现模板与路由不一致 | `cases/lifecycle/` 或新 `scheduling/` |
| 规模 | 固定小并发（例如 5/10）create-command-kill，统计失败和清理残留 | 及早发现资源竞争且控制 CI 成本 | `cases/concurrency/` |
| 故障注入 | CubeProxy 暂时不可达、CubeAPI 5xx、网络 target 波动 | 验证错误分类和报告质量 | 专用 chaos 环境 |
| 长运行 | 多次 auto-pause/resume 循环、磁盘写入后的状态保留、持续 idle timer 刷新 | 发布前发现资源泄漏和状态漂移 | `p3` lifecycle suite |

## 5. 框架优化建议

### 5.1 P0：为框架本身建立离线测试层

现状：`framework/` 和 `adapters/` 的正确性大多依赖在线 E2E 间接覆盖。
建议：增加纯 Python contract/unit tests，并将其纳入默认 PR gate。
收益：adapter 参数、等待、清理、preflight、trace 脱敏等回归无需真实 cluster
即可定位；在线 P0 可更小、更稳定。

### 5.2 P0：定义并版本化报告 schema

现状：JSONL 是诊断输出，但事件字段主要由代码约定。
建议：在 `framework/reporting.py` 旁定义 event schema/version，给每类事件写
schema 单测，并在报告中写入 schema version。
收益：CI、HTML 转换器和后续分析工具可以稳定消费报告，字段变更可控。

### 5.3 P1：将外部依赖探测升级为“可执行前提”

现状：网络 target、CubeProxy admin、模板与 Code Interpreter 的可用性部分依赖
运行时失败或 skip。
建议：为 `requires_internet`、Code Interpreter 与网络 target 增加可选的
session preflight；失败时记录明确原因、target 和建议，而不是由业务断言报错。
收益：减少“环境不通”伪装成“策略错误”的噪声。

### 5.4 P1：完善等待与重试诊断

现状：平台等待已使用退避轮询，但失败结果只保留最后状态。
建议：等待 helper 记录有界状态时间线（状态、时间、异常、list 结果），并把它
作为 assertion failure 和 JSONL 字段的一部分。
收益：快速判断是“未触发”“停在过渡态”“API 不可达”还是“状态与列表不一致”。

### 5.5 P1：提供标准化 peer sandbox fixture

现状：第二实例通过 `managed_control_sandbox` 手工创建，适合少数用例。
建议：在确认并发场景增长后提供 `sdk_peer_sandbox` 或工厂 fixture，支持 N 个
命名 peer、统一 metadata 和 cleanup。
收益：减少多实例用例重复代码，避免异常路径资源泄漏。

### 5.6 P2：CI 执行矩阵显式化

现状：README 描述 smoke/P0/P1/P2 范围，但未在本目录定义可审计的测试矩阵。
建议：以 CI workflow 或版本化配置明确以下 job：离线 framework 单测、
CubeSandbox P0、双 SDK P1、网络专用、平台生命周期每日、P3 长运行。
收益：测试成本、环境要求、owner 和失败升级路径透明，避免 marker 失去执行约束。

### 5.7 P2：并行执行准入

现状：pytest-xdist 已安装，但平台生命周期、共享公网 target 和环境容量对并发
敏感。
建议：在 CI 明确哪些 marker 可 `-n auto`，哪些必须串行；为并发 job 限制
sandbox 数量并在报告中记录 worker ID。
收益：缩短常规回归时间，同时避免 lifecycle/network 的偶发竞争。

## 6. 推荐实施顺序

1. 先补 trace/reporting、preflight、cleanup、adapter 的离线单测；
2. 再扩展 lifecycle 终态和时间语义、网络 DNS/HTTP/HTTPS；
3. 建立标准 peer fixture 后增加小并发隔离；
4. 最后进入多节点、故障注入和 P3 长运行覆盖。

每新增一项，应在 [用例编写指南](case-authoring.md) 更新范式，并在本文
“当前覆盖清单”中更新文件、前提和执行等级。
