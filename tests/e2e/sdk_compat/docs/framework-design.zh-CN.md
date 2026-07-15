# SDK 兼容性 E2E 测试框架设计

## 目标

测试套件通过 CubeSandbox 和 E2B Python SDK 执行同一组后端无关的 pytest
用例。套件默认不执行在线测试，每个用例独立创建 sandbox，并输出脱敏的
SDK 操作 trace 以便定位失败。

## 架构

```text
pytest 用例
    -> SandboxAdapter
       -> CubeSandboxAdapter -> CubeSandbox SDK
       -> E2BAdapter         -> E2B SDK
       -> TracingSandboxAdapter -> TraceCollector
                                  -> terminal / JSONL
```

用例只使用统一 adapter 方法：`info`、`run_command`、`run_code`、
`write_file`、`read_file`、`pause`、`resume_or_connect`、`kill` 和
`close`。SDK 差异应封装在 `adapters/` 中。

## Fixture 生命周期

`sdk_sandbox` 的流程是：

1. 加载 pytest、环境变量和 `.env` 配置；
2. 指定 `--run-e2e` 时执行 session preflight；
3. 检查 capability 和平台 marker；
4. 合并 `sandbox_create_options`；
5. 创建独立 sandbox 并绑定 `TraceCollector`；
6. 将 adapter 交给测试；
7. `SDK_E2E_KEEP_SANDBOX_ON_FAILURE=true` 时只保留 setup/call 失败实例；
8. 其他情况执行尽力清理。

测试不能依赖固定 sandbox ID，也不能依赖其他测试创建的实例。

## Capability 和 Marker

使用 `requires_capability` 表达后端支持边界：

```python
@pytest.mark.requires_capability(NETWORK_ALLOW_DENY)
```

当前 capability 包括 `lifecycle`、`commands`、`filesystem`、`run_code`、
`pause_resume`、`network_allow_deny`、`network_public_access` 和
`platform_lifecycle`。

使用 `smoke`、`p0`、`p1`、`p2`、`p3` 和 `slow` 表达执行优先级。只有依赖
CubeProxy/lifecycle-manager 协调的用例才使用 `requires_cubeproxy`。

## Preflight 和报告

Preflight 检查 CubeAPI 健康状态和模板 readiness。平台生命周期模式下，
还可以检查 CubeProxy admin heartbeat。

`TraceCollector` 记录时间戳、操作、脱敏后的输入输出、耗时和成功状态。
敏感信息会被脱敏，大对象会被截断，文件内容只记录长度。
JSONL 事件写入 `SDK_E2E_REPORT_DIR/events.jsonl`。

实时查看操作：

```bash
pytest --run-e2e --sdk-e2e-trace cases/lifecycle/test_pause_resume.py
```

## 生命周期 readiness

控制面 `state == "running"` 不代表 CubeShim、envd 或代码解释器已经 ready。
生命周期用例应先写入 kernel/文件状态，等待 `paused`，执行目标恢复路径，
等待 `running`，再执行并验证数据面操作。

优先使用 `wait_for_platform_pause`、`wait_for_platform_destroy` 和
`wait_until_running`，不要用固定 sleep 代替状态判断。临时 sleep 可以用于
定位竞态，但合入前必须说明原因。
