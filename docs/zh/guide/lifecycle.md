# 沙箱生命周期

沙箱（Sandbox）是 Cube-Sandbox 的核心运行单元。本页介绍沙箱从创建到销毁的**完整生命周期**，以及如何让平台自动管理生命周期、降低成本。

> 本页 SDK 形态与 [e2b](https://e2b.dev/docs/sandbox) 保持一致，便于已有 e2b 用户直接迁移。

## 状态模型

一个沙箱在它的生命周期里会处于以下几种状态之一：

| 状态        | 含义                                                                 |
|-------------|----------------------------------------------------------------------|
| `running`   | 正在运行，CPU/内存被实际占用，可以接收请求与执行代码                 |
| `pausing`   | 平台正在暂停沙箱（保存 VM 快照中），瞬时态                           |
| `paused`    | 沙箱已暂停，VM 内存已落盘为快照，**不消耗** CPU 与内存，状态完整保留 |
| `resuming`  | 平台正在从快照恢复沙箱，瞬时态                                       |
| `terminated`| 沙箱被显式销毁（`kill`）或因 `on_timeout="kill"` 超时被回收，无法恢复 |

状态转换主要由两个变量驱动：

- **`timeout`**：空闲多久后触发"超时"（默认在 SDK Config 里给一个固定值，比如 300 秒）。
- **`on_timeout`**：超时之后做什么 —— `"kill"`（默认，直接销毁）或 `"pause"`（暂停以备恢复）。

```
                       ┌──────────────────────────────────────┐
                       │                                      │
   create()       ┌────▼────┐   timeout & on_timeout=pause   ┌─────────┐
  ───────────────►│ running │ ──────────────────────────────►│ paused  │
                  │         │◄──────── connect() 或          │         │
                  └─┬─────┬─┘     auto_resume 触发的请求     └────┬────┘
                    │     │                                       │
        kill()      │     │ timeout & on_timeout=kill             │ kill()
        ────────────┘     └─────────────────┐                     │
                                            ▼                     ▼
                                      ┌────────────┐
                                      │ terminated │
                                      └────────────┘
```

## 创建沙箱

```python
from cubesandbox import Sandbox

# 创建沙箱，空闲 60 秒后自动销毁（默认 on_timeout="kill"）
sandbox = Sandbox.create(
    template="<your-template-id>",
    timeout=60,                # 单位：秒
)

print(sandbox.sandbox_id)
```

`Sandbox.create()` 关键参数：

| 参数                    | 说明                                                                       |
|-------------------------|----------------------------------------------------------------------------|
| `template`              | 模板 ID，沙箱基于它启动；缺省读环境变量 `CUBE_TEMPLATE_ID`                  |
| `timeout`               | 空闲超时，**秒**（注意：e2b 的 `timeoutMs` 是毫秒，Cube 是秒）              |
| `lifecycle`             | 生命周期策略，详见下文 "[平台自动暂停 / 自动恢复](#平台自动暂停-自动恢复)" |
| `metadata`              | 任意键值对，写入沙箱元数据，可在列表 / 详情接口中读出                      |
| `env_vars`              | 注入沙箱进程的环境变量                                                     |
| `allow_internet_access` | 是否允许出公网；`network` 提供更细粒度的出站策略                           |

> Cube 的最大单次运行时长不像托管 e2b 那样有严格的 24h/1h 平台上限——但 idle `timeout` 仍然是必需的，它防止意外遗漏的沙箱长期占用资源。

## 查询沙箱信息

```python
info = sandbox.get_info()
print(info)
# {
#   "sandboxID": "iiny0783cype8gmoawzmx-ce30bc46",
#   "templateID": "rki5dems9wqfm4r03t7g",
#   "state": "running",
#   "startedAt": "2026-06-17T12:34:56Z",
#   "endAt":     "2026-06-17T12:39:56Z",
#   "metadata":  {...}
# }
```

`endAt` 表示按当前 `timeout` 估算的下一次超时时间。每次接收到新请求或调用 `set_timeout`（若有），`endAt` 会被刷新。

## 列出运行中的沙箱

```python
for sb in Sandbox.list():
    print(sb["sandboxID"], sb["state"])
```

## 显式销毁

```python
sandbox.kill()
```

`kill()` 是不可逆的：与暂停不同，被 kill 的沙箱**不能**恢复。即便 `lifecycle.on_timeout="pause"`，调用 `kill()` 仍然立即终止并丢弃快照。

## 显式暂停 / 恢复

```python
sandbox.pause()                       # 主动保存快照，释放 CPU/内存
# ... 一段时间过去 ...
sandbox.connect()                     # 从快照恢复
sandbox.run_code("print('back!')")    # 像没暂停过一样继续用
```

可参考示例：[`examples/code-sandbox-quickstart/pause.py`](https://github.com/tencentcloud/CubeSandbox/blob/master/examples/code-sandbox-quickstart/pause.py)。

## 平台自动暂停 / 自动恢复

很多 Agent 工作负载并不持续繁忙：用户敲一段代码 → 模型推理 → 沙箱执行 → 等待下一轮交互。在等待期间让沙箱**自动暂停**，下次请求来时再**自动恢复**，可以显著降低资源占用。

Cube 提供与 e2b [`lifecycle`](https://e2b.dev/docs/sandbox/auto-resume) 完全一致的配置形态：

```python
sandbox = Sandbox.create(
    template="<your-template-id>",
    timeout=300,                      # 5 分钟空闲后触发 on_timeout
    lifecycle={
        "on_timeout": "pause",        # 空闲超时后 → 暂停（而不是销毁）
        "auto_resume": True,          # 暂停后下一次请求 → 透明恢复
    },
)
```

### 行为说明

- **`on_timeout="pause"`**：沙箱空闲 `timeout` 秒后，平台调度暂停流程，`state` 变为 `paused`，VM 内存被冷藏到快照存储。
- **`auto_resume=True`**：当再有任何请求路由到这个 `paused` 沙箱（HTTP 请求、`run_code`、文件读写等），平台自动唤醒它，调用方**无需**显式 `connect()`；典型恢复时间在亚秒级到秒级。
- 如果 `auto_resume=False`（或省略），沙箱暂停后必须显式 `Sandbox.connect(sandbox_id=...)` 才能再用 —— 适合"等用户决定"的场景。

### 自动恢复后的 timeout 重置

每次自动恢复成功后，沙箱获得一个**全新的 `timeout` 计时窗口**（与 e2b 同样语义），所以"恢复 → 短暂使用 → 再次空闲超时 → 再次暂停"的循环可以无缝持续。

### 何时算"活跃"

下列动作都会重置 idle 计时：

- 通过 SDK 调用：`sandbox.run_code(...)`、`sandbox.commands.run(...)`、`sandbox.files.read(...)` / `write(...)`。
- 通过 HTTP 直连沙箱内的服务（例如 `getHost()` 返回的 URL）。

未配置 `auto_pause` / 不传 `lifecycle` 的沙箱完全保留旧行为：空闲超时直接销毁。

### 端到端示例

[`examples/code-sandbox-quickstart/auto-resume.py`](https://github.com/tencentcloud/CubeSandbox/blob/master/examples/code-sandbox-quickstart/auto-resume.py) 是一个 TUI 演示：创建带 `lifecycle.on_timeout=pause` 的沙箱、空闲触发自动暂停、再发请求触发自动恢复，最终对比"内核内存 + 文件系统"两层状态，验证全状态保留。

```bash
export CUBE_TEMPLATE_ID=<your-template>
python examples/code-sandbox-quickstart/auto-resume.py
```

## 设计与运维要点

- **暂停的状态保真度**：CPU 寄存器、进程内存、TCP 连接（无外部对端）、文件系统改动都会随快照保留；面向外部的连接（如 sandbox 主动建立的 outbound socket）会在暂停时断开，恢复后由应用层自行重连。
- **集群一致性**：自动暂停由部署在 CubeProxy 容器内的 `cube-proxy-sidecar` 协调；它消费 CubeMaster 通过 Redis stream 发布的生命周期事件，对所有 CubeProxy 实例广播状态。多副本环境下用 Redis SETNX 互斥锁确保同一沙箱不会被并发暂停或恢复。
- **失败回退**：自动恢复 RPC 失败时，CubeProxy 直接对客户端返回 503 + `Retry-After`，不会让用户卡在长超时上。
- **故障排查**：`/data/log/cube-proxy/sidecar.log` 是 sidecar 的运行日志，关键事件包括 `create event applied`、`auto-paused sandbox`、`auto-resumed sandbox`。

## 下一步

- [模板概览](./templates.md) —— 沙箱基于模板启动，模板的构建过程也会影响首次冷启动开销。
- [快速开始](./quickstart.md) —— 完整跑通"创建沙箱 → 执行代码 → 销毁"的最短路径。
- 上游参考：[e2b · Sandbox lifecycle](https://e2b.dev/docs/sandbox)、[e2b · Auto-resume](https://e2b.dev/docs/sandbox/auto-resume)。
