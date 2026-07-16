# SDK 兼容性 E2E 用例编写指南

## 选择测试域

优先使用已有测试域：

```text
commands/      命令输出和退出行为
filesystem/    文件 API 和 shell 互操作
lifecycle/     创建、暂停、恢复、连接和销毁
network/       创建时的出站网络策略
run_code/      解释器输出和 kernel 状态
```

只有在行为具有独立 API、capability 边界或执行范围时，才新增测试域。

## 使用统一 adapter

共享用例不能直接导入具体 SDK：

```python
def test_command_output(sdk_sandbox, sdk_e2e_config):
    result = sdk_sandbox.run_command(
        "printf hello",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    assert result.stdout == "hello"
```

后端差异放到 `adapters/`，不支持的行为使用 `requires_capability` 表达。

## 配置和标记

模块通常使用：

```python
pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.p1,
]
```

通过 marker 传递创建参数：

```python
@pytest.mark.requires_capability(NETWORK_ALLOW_DENY)
@pytest.mark.requires_internet
@pytest.mark.sandbox_create_options(
    network={
        "allow_out": ["8.8.8.8/32"],
        "deny_out": ["0.0.0.0/0"],
    }
)
```

共享用例不要硬编码 template ID，应使用 `CUBE_TEMPLATE_ID` 或
`--cube-template-id`。

## 断言和状态验证

断言用户可观察的结果，并保留失败上下文：

```python
assert_command_ok(result)
assert result.stdout == "expected", (
    f"stdout={result.stdout!r} stderr={result.stderr!r}"
)
```

生命周期或 kernel 用例应在状态转换前写入状态，之后验证：

```python
seed = sdk_sandbox.run_code("value = 41")
assert_code_ok(seed)
sdk_sandbox.write_file("/tmp/checkpoint", "before")

# pause/resume 或 connect

result = resumed.run_code("value + 1")
assert_code_ok(result)
assert result.text == "42"
assert resumed.read_file("/tmp/checkpoint") == "before"
```

`state == "running"` 只是控制面结果，不能保证数据面已经 ready。应使用
`wait_until_running`，并在排查 readiness 竞态时单独记录第一次数据面操作。

## 网络用例

使用与目标行为匹配的协议：

- TCP：`socket.connect_ex`；
- UDP/DNS：发送 DNS query 并等待匹配响应；
- HTTP/HTTPS：断言状态码和响应输出；
- L7：验证 host、path、method、SNI、规则顺序和 header 注入。

严格域名白名单需要将 `allow_out` 与
`allow_internet_access=False` 或 `deny_out=["0.0.0.0/0"]` 一起配置。TCP
连接成功本身不能证明 HTTP 或 L7 策略正确。

## 生命周期和清理

平台生命周期用例通常使用 `slow` 和 `requires_cubeproxy`：

```bash
SDK_E2E_PLATFORM_LIFECYCLE=true \
pytest --run-e2e --sdk-e2e-trace cases/lifecycle/test_auto_lifecycle.py
```

优先使用生命周期 helper，而不是固定 sleep。由 fixture 负责清理 sandbox。
如果用例创建了恢复后的 adapter，应在 `finally` 中关闭：

```python
resumed = sdk_sandbox.resume_or_connect()
try:
    ...
finally:
    resumed.close()
```

## 评审清单

- 使用统一 adapter 和必要的 capability marker；
- 断言确定且包含失败上下文；
- 没有固定 sandbox ID 或跨测试依赖；
- setup、call、skip 和 cleanup 路径安全；
- 已按预期后端组合收集；
- 已运行 `pytest --collect-only -q` 和最小在线范围测试。
