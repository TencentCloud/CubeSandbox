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

未配置 `auto_pause` / 不传 `lifecycle` 的沙箱默认行为是 `on_timeout="kill"`：空闲超过 `timeout` 秒后，平台会主动销毁该沙箱。这与 e2b `lifecycle.on_timeout="kill"` 语义一致。如果完全不希望被自动回收，请在创建时把 `timeout` 设得足够大、或主动在客户端发心跳调用刷新 idle 计时。

### 端到端示例

平台提供两个**互为镜像**的端到端演示，对应 `on_timeout` 的两种取值：

- [`examples/code-sandbox-quickstart/auto-resume.py`](https://github.com/tencentcloud/CubeSandbox/blob/master/examples/code-sandbox-quickstart/auto-resume.py) —— `on_timeout="pause"` + `auto_resume=True`。创建沙箱、空闲触发**自动暂停**、再发请求触发**自动恢复**，最终对比"内核内存 + 文件系统"两层状态，验证全状态保留。
- [`examples/code-sandbox-quickstart/auto-kill.py`](https://github.com/tencentcloud/CubeSandbox/blob/master/examples/code-sandbox-quickstart/auto-kill.py) —— `on_timeout="kill"`（默认行为）。创建沙箱、空闲触发**自动销毁**、验证后续请求以 410 Gone 快速失败、`Sandbox.list()` 不再返回该沙箱，并通过创建一个对照沙箱排除集群整体故障。

```bash
export CUBE_TEMPLATE_ID=<your-template>

# 自动暂停 + 自动恢复
python examples/code-sandbox-quickstart/auto-resume.py

# 自动销毁（不可恢复）
python examples/code-sandbox-quickstart/auto-kill.py
```

## 设计与运维要点

- **暂停的状态保真度**：CPU 寄存器、进程内存、TCP 连接（无外部对端）、文件系统改动都会随快照保留；面向外部的连接（如 sandbox 主动建立的 outbound socket）会在暂停时断开，恢复后由应用层自行重连。
- **集群一致性**：自动暂停由部署在 CubeProxy 容器内的 `cube-proxy-sidecar` 协调；它消费 CubeMaster 通过 Redis stream 发布的生命周期事件，对所有 CubeProxy 实例广播状态。多副本环境下用 Redis SETNX 互斥锁确保同一沙箱不会被并发暂停或恢复。
- **失败回退**：自动恢复 RPC 失败时，CubeProxy 直接对客户端返回 503 + `Retry-After`，不会让用户卡在长超时上；当沙箱已经被销毁（`killing` / `killed`），则返回 410 Gone 让客户端立即停止重试。
- **故障排查**：`/data/log/cube-proxy/sidecar.log` 是 sidecar 的运行日志，关键事件包括 `create event applied`、`auto-paused sandbox`、`auto-resumed sandbox`、`timeout-killed sandbox`。

### 暂停资源释放与节点调度配额

沙箱暂停后，其 CPU 和内存在物理上已被回收——但在默认情况下，节点资源计账仍然将暂停中（`paused`/`pausing`）的沙箱视为"已占用"调度配额。这意味着：即使大量闲置沙箱被暂停，宿主机上仍然没有"空位"来创建新沙箱。

为了解决这个问题，Cube 提供了一个**节点级调节旋钮** `host.quota.paused_resource_release_ratio`（在 `Cubelet/config/config.toml` 中配置），值域 `[0, 1]`，默认 `0`：

| 值 | 行为 | 适用场景 |
|---|---|---|
| `0.0` | 暂停沙箱保留完整配额（与旧版本行为一致）。恢复始终有保障，不会因资源不足被拒绝。 | 对可用性要求极高、不希望恢复失败的场景 |
| `1.0` | 暂停沙箱的 CPU/内存配额**全部释放**给调度器。恢复变为尽力而为——节点资源不足时恢复会被拒绝。 | 追求最大化部署密度、允许恢复偶尔失败的场景 |
| `0 < r < 1` | 释放 `r` 比例，保留 `(1-r)` 作为余量。保留的配额仍会计入调度器的 CPU/内存使用量，因此**暂停密集的节点会被自然降权**，调度器不会在已有大量暂停沙箱的节点上继续堆积新沙箱。 | 需要在可用性和高利用率之间做折中的场景 |

**配置示例**：

```toml
# Cubelet/config/config.toml
[host.quota]
paused_resource_release_ratio = 0.5   # 释放一半，保留一半
```

**恢复准入检查**：

当 `ratio > 0` 时，恢复操作会触发**本地实时准入检查**——如果节点当前无法容纳该沙箱释放出去的资源量，恢复会被拒绝：

```
resume rejected by paused_resource_release_ratio policy: need 1024MB > quota 512MB
```

拒绝信息通过以下链路透传给客户端：`Cubelet (130409 Conflict)` → `CubeAPI (HTTP 409)` → `WebUI（显示容量诊断）`。409 是可重试的状态码——当其他沙箱被销毁或暂停、节点资源释放后，恢复可以重新尝试。

**注意事项**：

- 磁盘和 MvmNum **不受 ratio 影响**——暂停快照始终占用存储空间，沙箱对象始终存在。
- `ratio=0` 是零值安全的默认值：如果从未配置过此项，行为与旧版本完全一致，升级不会产生意外。
- 此项为**节点级配置**，不同节点可以设置不同的比值，灵活应对异构硬件或分池部署的需求。
- 当节点上一大批沙箱同时被唤醒、单节点无法承载时，控制面会返回 409 并给出具体配额数字。后续版本将支持**跨节点恢复**，让沙箱可以在集群内自由漂移，最大化整集群利用率。

## 下一步

- [模板概览](./templates.md) —— 沙箱基于模板启动，模板的构建过程也会影响首次冷启动开销。
- [快速开始](./quickstart.md) —— 完整跑通"创建沙箱 → 执行代码 → 销毁"的最短路径。
- 上游参考：[e2b · Sandbox lifecycle](https://e2b.dev/docs/sandbox)、[e2b · Auto-resume](https://e2b.dev/docs/sandbox/auto-resume)。
