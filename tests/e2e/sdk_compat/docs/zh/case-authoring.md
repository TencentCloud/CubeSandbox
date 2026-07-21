# SDK 兼容性 E2E 用例编写指南

## 1. 编写前先回答的问题

新增用例前先确认以下问题；这决定目录、marker、执行频率和断言方式：

1. 该行为是两个 SDK 都应支持的公共契约，还是某个 backend 的扩展？
2. 依赖控制面、数据面、外部网络还是平台生命周期组件？
3. 能否在一个独立 sandbox 中完成，还是确实需要 peer/control sandbox？
4. 失败后最有价值的诊断信息是什么？
5. 它属于 PR gate（`p0`）、每日回归（`p1`）还是更慢的周期性覆盖（`p2`/`p3`）？

共享用例默认应覆盖所有已选择的 backend。若行为不被某 backend 支持，应使用
capability marker 跳过，不能通过宽松断言或 backend 条件分支“兼容”。

## 2. 选择目录与测试域

优先复用现有测试域：

```text
commands/      命令 stdout/stderr、退出码、超时与环境
concurrency/   多 sandbox 隔离与并发行为
filesystem/    文件 API、路径、内容与 shell 互操作
lifecycle/     创建、connect、pause/resume、kill、平台托管生命周期
network/       创建时网络策略、公共网络访问与协议验证
run_code/      Code Interpreter 输出、错误与 kernel 状态
```

仅当新行为同时满足“独立 API 或独立 capability 边界”和“可形成可维护的执行
范围”时，才创建新目录。例如 snapshot、template、metadata 或 errors 可以
成为单独测试域；不要因单个用例名称不同就新增目录。

## 3. 模块骨架与 marker

每个文件应先声明共同 marker。常见模板：

```python
import pytest

from framework.capabilities import COMMANDS

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.p1,
    pytest.mark.commands,
    pytest.mark.requires_capability(COMMANDS),
]
```

### 3.1 优先级与环境 marker

- `smoke`：快速确认真实环境可创建并执行最小操作；
- `p0`：稳定、低成本、适合 PR gate；
- `p1`：每日双 SDK 回归；
- `p2`：更广的行为组合、第二实例或较慢外部依赖；
- `p3`：发布前长运行或破坏性场景；
- `slow`：超过常规 PR 时间预算；
- `requires_internet`：需要稳定公网的网络用例；
- `requires_cubeproxy`：依赖 CubeProxy 与 lifecycle-manager 的平台路径；
- `requires_code_interpreter`：依赖 stateful Code Interpreter/Jupyter。

环境 marker 不会替代 capability marker。例如网络用例通常同时需要
`requires_internet` 和 `requires_capability(NETWORK_ALLOW_DENY)`。

### 3.2 模板 ID 与创建参数

默认模板由环境变量 `CUBE_TEMPLATE_ID` 或命令行 `--cube-template-id` 提供，
适合整套用例共用一个 READY 模板。只有当某个用例或用例集依赖不同模板能力时，
才单独覆盖模板 ID。

单个用例覆盖模板：

```python
@pytest.mark.sandbox_template_id("tpl-code-interpreter-xxxxxxxx")
@pytest.mark.requires_code_interpreter
def test_kernel_state(sdk_sandbox, sdk_e2e_config):
    ...
```

整个文件共用专用模板时，把 marker 放进模块级 `pytestmark`：

```python
pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.run_code,
    pytest.mark.p1,
    pytest.mark.sandbox_template_id("tpl-code-interpreter-xxxxxxxx"),
    pytest.mark.requires_code_interpreter,
]
```

函数级 `sandbox_template_id` 会覆盖模块级设置；如果两者都没有，则使用全局
`CUBE_TEMPLATE_ID`。该 marker 只影响 `sdk_sandbox` 创建实例时使用的模板；
测试函数里拿到的 `sdk_e2e_config` 仍是 session 级全局配置。模板 ID 不要放进
`sandbox_create_options`，因为 template 是 fixture/adapters 的统一配置，不是
普通创建参数。

其他 sandbox 创建参数通过 `sandbox_create_options` 传入，而不是在用例中
重新调用具体 SDK：

```python
@pytest.mark.requires_capability(NETWORK_ALLOW_DENY)
@pytest.mark.requires_internet
@pytest.mark.sandbox_create_options(
    allow_internet_access=False,
    network={
        "allow_out": ["8.8.8.8"],
        "deny_out": ["0.0.0.0/0"],
    },
)
def test_allowlist(sdk_sandbox, sdk_e2e_config):
    ...
```

可传入 `timeout`、`metadata`、`env_vars`、`network` 和 `lifecycle` 等创建参数。
共享用例不得硬编码 CubeAPI 地址、sandbox ID 或 compute node；模板 ID 只能通过
全局配置或 `sandbox_template_id` marker 表达。

## 4. 使用统一 adapter 与断言

共享用例禁止直接导入 `cubesandbox.Sandbox`、`e2b.Sandbox` 或访问私有 SDK
字段。使用 `sdk_sandbox` 和归一化返回值：

```python
from framework.assertions import assert_command_ok

def test_command_output(sdk_sandbox, sdk_e2e_config):
    result = sdk_sandbox.run_command(
        "printf hello",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    assert result.stdout == "hello", (
        f"stdout={result.stdout!r} stderr={result.stderr!r}"
    )
```

断言原则：

- 先断言操作是否成功，再断言业务语义；
- 对 stdout、stderr、exit code、文件内容和结果文本做精确断言；
- 错误路径至少验证错误类型或稳定消息片段，不能只写 `pytest.raises(Exception)`；
- 不以“无异常”证明策略生效；
- 不断言 SDK 私有对象、实现相关 header 或不稳定时间戳；
- 失败信息中应保留 target、实际输出和 stderr。

优先使用 `framework.assertions` 中的 `assert_command_ok`、`assert_code_ok`，
避免在每个文件重复定义不一致的成功判定。

## 5. 从需求到可运行用例：完整示例

下面以“验证 sandbox 可以执行命令并正确返回 stdout”为例。假设需求已经
确认属于两个 backend 都应支持的公共契约，因此将文件放在
`cases/commands/test_run.py`；如果是新文件，先复制以下完整内容：

```python
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import pytest

from framework.assertions import assert_command_ok
from framework.capabilities import COMMANDS

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.commands,
    pytest.mark.p1,
    pytest.mark.requires_capability(COMMANDS),
]


def test_run_command_returns_stdout(sdk_sandbox, sdk_e2e_config):
    result = sdk_sandbox.run_command(
        "printf sdk-compat-command",
        timeout=sdk_e2e_config.command_timeout,
    )

    assert_command_ok(result)
    assert result.stdout == "sdk-compat-command", (
        f"stdout={result.stdout!r} stderr={result.stderr!r}"
    )
```

按以下顺序把需求转换为代码：

1. 选择测试域：命令行为放在 `cases/commands/`，不要放到生命周期文件；
2. 选择 capability：命令用 `COMMANDS`，并添加
   `pytest.mark.requires_capability(COMMANDS)`；
3. 添加公共 marker：`e2e`、`sdk_compat`、域 marker 和优先级 marker；
4. 使用 `sdk_sandbox` 操作实例，使用 `sdk_e2e_config` 提供 timeout；
5. 先用 `assert_command_ok` 断言调用成功，再断言 stdout 等业务结果；
6. 运行最小范围，先确认收集和单 backend，再扩展到双 backend：

```bash
cd CubeSandbox/tests/e2e/sdk_compat
pytest --collect-only -q cases/commands/test_run.py
pytest --run-e2e -vv \
  cases/commands/test_run.py::test_run_command_returns_stdout
SDK_E2E_BACKENDS=cubesandbox,e2b \
  pytest --run-e2e -vv \
  cases/commands/test_run.py::test_run_command_returns_stdout
```

如果用例需要创建参数，在函数上增加
`pytest.mark.sandbox_create_options(...)`；如果只支持某个 backend，先确认
是否应新增 capability，而不是在测试体中写 `if sdk_backend == ...`。如果需要
外部网络、CubeProxy 或 Code Interpreter，再分别增加
`requires_internet`、`requires_cubeproxy` 或
`requires_code_interpreter`，并在文档中说明环境前提。

## 6. 各测试域的最小范式

### 6.1 Commands

每个 command 用例至少明确预期的 stdout、stderr 和 exit code。超时场景应检查
timeout/deadline 语义；非零退出场景应确认 adapter 将其归一化为
`CommandResult`，而不是吞没异常。

### 6.2 Filesystem

文件用例需要区分文件 API 与 shell 数据面：

```python
sdk_sandbox.write_file("/tmp/marker.txt", "before")
result = sdk_sandbox.run_command(
    "cat /tmp/marker.txt",
    timeout=sdk_e2e_config.command_timeout,
)
assert_command_ok(result)
assert result.stdout == "before"
```

覆盖新文件语义时，分别考虑：嵌套路径、覆盖、空内容、多行、较大文本、缺失文件、
权限/用户、二进制内容以及目录操作。不要把 shell `cat` 成功当作文件 API 行为的
唯一证明。

### 6.3 Run code

`run_code` 用例应同时处理表达式文本、stdout、stderr 和结构化错误。验证 kernel
状态时需明确要求 `requires_code_interpreter`：

```python
from framework.assertions import assert_code_ok
from framework.capabilities import RUN_CODE

@pytest.mark.requires_capability(RUN_CODE)
@pytest.mark.requires_code_interpreter
def test_kernel_state(sdk_sandbox, sdk_e2e_config):
    assert_code_ok(sdk_sandbox.run_code("value = 41"))
    result = sdk_sandbox.run_code(
        "value + 1",
        timeout=sdk_e2e_config.run_code_timeout,
    )
    assert_code_ok(result)
    assert result.text == "42"
```

### 6.4 Lifecycle

生命周期测试必须把“控制面状态”和“数据面可用性”分开验证：

```python
from framework.lifecycle import wait_until_paused, wait_until_running

sdk_sandbox.write_file("/tmp/checkpoint", "before")
sdk_sandbox.pause(timeout=sdk_e2e_config.default_timeout)
assert wait_until_paused(sdk_sandbox) == "paused"

resumed = sdk_sandbox.resume_or_connect(timeout=sdk_e2e_config.default_timeout)
try:
    assert wait_until_running(resumed) == "running"
    assert resumed.read_file("/tmp/checkpoint") == "before"
finally:
    resumed.close()
```

平台生命周期用例应增加 `slow`、`requires_cubeproxy` 与
`requires_capability(PLATFORM_LIFECYCLE)`，并使用：

- `wait_for_platform_pause`；
- `wait_for_platform_destroy`；
- `wait_until_paused` / `wait_until_running`；
- 真实 command、file 或 code 操作确认恢复就绪。

固定 sleep 只能作为临时故障定位手段。对于等待窗口，应使用配置中的
`platform_lifecycle_*` 值和 helper。

### 6.5 Network

网络策略要用目标协议验证，而不是只验证 DNS 或 TCP：

- TCP：`socket.connect_ex`；
- UDP/DNS：发送 DNS 请求并验证匹配响应；
- HTTP/HTTPS：检查状态码、响应体、Host 与 TLS/SNI；
- L7：检查 host、path、method、规则优先级、audit 与 header 注入。

严格 allowlist 需要显式组合 `allow_out` 与
`allow_internet_access=False` 或 `deny_out=["0.0.0.0/0"]`。公网 target 必须
来自 `SDK_E2E_TCP_TARGET_*` 等配置，不得在用例中硬编码为唯一环境假设。

### 6.6 Concurrency 与多实例

需要第二个实例时，使用 `managed_control_sandbox`，使 peer 的清理语义与主
fixture 一致：

```python
from framework.lifecycle import managed_control_sandbox

def test_isolation(sdk_sandbox, sdk_backend, sdk_e2e_config):
    with managed_control_sandbox(sdk_backend, sdk_e2e_config) as peer:
        assert peer.sandbox_id != sdk_sandbox.sandbox_id
        # 为两个实例写同路径、不同内容，再分别读取。
```

真正并发的用例还应设置可控并发数、超时和资源上限，并确保异常路径回收所有
future/peer sandbox。

## 7. 清理、保留和额外 adapter

`sdk_sandbox` 默认负责主 sandbox 清理。不要在测试体中对它额外调用 `kill()`，
除非用例本身验证 kill 语义并接受 fixture 的幂等清理。

以下对象需要显式关闭或托管：

- `resume_or_connect()` 返回的新 adapter：在 `finally` 调用 `close()`；
- `connect_adapter()` 返回的 adapter：在 `finally` 调用 `close()`；
- 自建 peer/control sandbox：使用 `managed_control_sandbox`；
- 直接创建的临时资源：必须在 `finally` 中清理。

设置 `SDK_E2E_KEEP_SANDBOX_ON_FAILURE=true` 可保留 setup/call 失败的主
sandbox 以便诊断，但不能依赖它作为正常资源管理策略。

## 8. 可观测性与失败排查

框架自动记录 create、connect、command、file、code、lifecycle、cleanup 等
操作。建议本地最小复现时使用：

```bash
pytest --run-e2e --sdk-e2e-trace -vv \
  cases/lifecycle/test_auto_lifecycle.py::test_lifecycle_auto_resume_preserves_state
```

失败时依次检查：

1. pytest 的 setup/call/teardown phase；
2. terminal trace 的最后一次失败操作；
3. `SDK_E2E_REPORT_DIR/events.jsonl` 中同一 node ID 的事件；
4. sandbox `info().raw` 中的 state、endAt、metadata；
5. 平台生命周期场景下的 CubeProxy heartbeat 与 lifecycle-manager 日志。

禁止将真实 API key、JWT、私钥、密码或业务敏感输入写入 command、code、metadata
或自定义断言消息。框架会脱敏常见字段，但不能保证识别任意业务格式。

## 9. 常见反模式

| 反模式 | 风险 | 正确做法 |
| --- | --- | --- |
| 在共享用例直接调用具体 SDK | 双 backend 语义分叉 | 在 adapter 增加归一化方法 |
| `if sdk_backend == ...` | 用例失去可移植性 | 使用 capability 或拆分 backend 专属用例 |
| 固定 sandbox/template ID | 并发冲突、跨环境失效 | 使用 fixture 与 `CUBE_TEMPLATE_ID` |
| `time.sleep()` 等状态 | 慢且易抖动 | 使用 lifecycle wait helper |
| 捕获所有异常就通过 | 掩盖 SDK 或环境回归 | 匹配稳定异常类型/消息 |
| 公网依赖没有 marker | CI 环境失败难以解释 | `requires_internet` 与可配置 target |
| 自建第二实例没有 cleanup | 泄漏资源 | `managed_control_sandbox` |
| 只把 `running` 后的数据面失败当作正常延迟 | 掩盖 CLM/后端回归 | `running` 后直接执行数据面操作；失败时保留 trace 并报告为后端故障 |

## 10. 合入前检查

- 用例使用 adapter、框架模型和公共断言；
- marker、优先级和 capability 与真实前提一致；
- 创建参数仅通过 `sandbox_create_options` 传入；
- 断言包含稳定的业务语义和失败上下文；
- 主 sandbox、resumed adapter、peer/control sandbox 都有清理路径；
- 使用 `pytest --collect-only -q` 检查 backend 参数化；
- 运行最小在线范围；生命周期和网络用例额外带 trace；
- 若新增 capability、adapter 方法或测试域，同步更新框架设计、README 与
  [覆盖盘点](test-coverage.md)。
