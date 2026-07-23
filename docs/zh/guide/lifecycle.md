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

状态转换主要由两个参数驱动：

- **`timeout`**（可选）：沙箱**空闲**多久后触发超时，单位为**秒**（e2b 的 `timeoutMs` 是毫秒）。不传时由服务端决定；SDK 不再自带 300 秒之类的默认值。
- **`on_timeout`**：超时后怎么办——`"kill"`（默认，销毁）或 `"pause"`（暂停，可之后恢复）。

`timeout` 取值（语义对齐 e2b）：

| 取值 | 行为 |
|------|------|
| 不传 | 使用服务端配置的默认空闲超时；服务端未配置或设为 ≤ 0 时，**永不超时** |
| `NEVER_TIMEOUT`（`-1`） | **永不超时**——不会因空闲被自动回收 |
| `0` | **立刻超时**——空闲后首次扫描即回收 |
| 正整数 `N` | 空闲 **N 秒**后触发超时 |

Go：`cubesandbox.NeverTimeout`；Python：`from cubesandbox import NEVER_TIMEOUT`。

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
| `timeout`               | 可选，空闲超时（秒），见上文取值说明 |
| `lifecycle`             | 生命周期策略，详见下文 "[平台自动暂停 / 自动恢复](#平台自动暂停-自动恢复)" |
| `metadata`              | 任意键值对，写入沙箱元数据，可在列表 / 详情接口中读出                      |
| `env_vars`              | 注入沙箱进程的环境变量                                                     |
| `allow_internet_access` | 是否允许出公网；`network` 提供更细粒度的出站策略                           |

> Cube 不像托管 e2b 那样有严格的 24h/1h 单次运行上限。省略 `timeout` 时，实际空闲 TTL 由集群运维在服务端配置（见下文[设计与运维要点](#设计与运维要点)）。

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

`endAt` 表示按当前 `timeout` 估算的下一次超时时间。每次接收到新请求或调用 `set_timeout`（若有），`endAt` 会被刷新。对于**永不超时**的沙箱没有截止时间，因此响应中会**省略** `endAt`，而不是把它渲染成等于 `startedAt`。

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

### 删除暂停状态的沙箱

`kill()` 和 `DELETE /sandboxes/{sandboxID}` 均可用于删除处于 `running` 或 `paused` 状态的沙箱。

删除 `paused` 沙箱时，CubeSandbox 会先在内部恢复其运行时状态，再执行正常销毁流程。因此，删除 `paused` 沙箱通常比删除 `running` 沙箱耗时更长。接口仍保持同步语义：只有沙箱及相关资源清理完成后，才会返回 `204 No Content`。

内部恢复仅用于完成删除，不会被视为一次独立的恢复操作：

* 不会触发 `sandbox.resumed` 生命周期事件；
* 不会重置空闲超时；
* 不会改变 DELETE 接口的同步语义。

为给销毁流程预留足够时间，内部恢复最多执行五秒。如果当前请求剩余时间不足以完成恢复和销毁，CubeSandbox 会在开始销毁前返回可重试错误。

删除暂停状态的沙箱时，通常会遇到以下几类情况：

* 删除成功时返回 **`204 No Content`**，表示沙箱及相关资源已经完成清理。

* 如果恢复未通过资源准入检查，会返回 **`409 Conflict`**。例如节点容量不足，或沙箱缺少恢复所需的资源元数据。此时沙箱仍保持 `paused` 状态。客户端应根据响应中的诊断信息处理：资源释放后可以重试；如果缺少资源元数据，则需要修复或重新创建沙箱。

* 如果沙箱正在进入暂停状态，或其他生命周期操作尚未完成，会返回 **`503 Service Unavailable`**，并携带 `Retry-After: 2`。客户端应等待至少两秒后重试。

* 如果删除前的恢复或状态准备未能在时间预算内完成，或者剩余时间不足以启动销毁流程，会返回 **`503 Service Unavailable`**，并携带 `Retry-After: 5`。客户端应先查询沙箱状态，再等待至少五秒后重试。

`Retry-After` 的单位为秒，仅用于提示客户端等待后重试。它不表示 CubeSandbox 会在后台继续删除，也不会启动后台重试任务。

`404 Not Found`、`408 Request Timeout` 以及 `running` 沙箱的删除行为保持不变。

如果恢复过程返回错误，但运行时状态确认沙箱已经处于 `running`，CubeSandbox 仍会继续执行销毁。若后续销毁失败，则按照现有销毁错误语义返回；不会重新暂停沙箱，也不会创建后台清理任务。

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

未配置 `auto_pause` / 不传 `lifecycle` 的沙箱默认行为是 `on_timeout="kill"`：空闲超过 `timeout` 秒后，平台会主动销毁该沙箱。这与 e2b `lifecycle.on_timeout="kill"` 语义一致。若不希望被自动回收，可传 `timeout=NEVER_TIMEOUT`、省略 `timeout`（且服务端未设正数默认）、把 `timeout` 设得足够大，或通过定期活动刷新空闲计时。

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

### 集群默认空闲超时（`default_timeout_insec`）

客户端不传 `timeout` 时，由 CubeMaster 读取 `CubeMaster/conf.yaml` 中的 `cubelet_conf.default_timeout_insec`（one-click 安装路径：`/usr/local/services/cubetoolbox/CubeMaster/conf.yaml`）。

| 配置值 | 客户端省略 `timeout` 时的效果 |
|--------|------------------------------|
| 未配置或 `<= 0` | **不设集群级空闲 TTL** —— 沙箱不会因空闲被自动回收 |
| 正整数 `N` | 默认空闲 **N 秒**后触发超时 |

仓库默认**不配置集群级空闲超时**（`default_timeout_insec: -1`）。若希望集群自动回收未显式传 `timeout` 的沙箱，可改为正数（例如 `300`）。修改后需重启 `cube-sandbox-cubemaster.service`。

同一段里的 `create_timeout_insec` 与空闲 TTL 无关，仅限制创建/调度 RPC 的截止时间。更多 CubeMaster 配置项见[服务管理 — CubeMaster 配置项](service-management.md#cubemaster-settings)。

- **暂停的状态保真度**：CPU 寄存器、进程内存、TCP 连接（无外部对端）、文件系统改动都会随快照保留；面向外部的连接（如 sandbox 主动建立的 outbound socket）会在暂停时断开，恢复后由应用层自行重连。
- **集群一致性**：自动暂停由部署在 control 节点上的 `cube-lifecycle-manager` 服务统一协调；它消费 CubeMaster 通过 Redis stream 发布的生命周期事件，通过 Redis 注册表实时发现所有在线的 CubeProxy 副本并广播状态。多副本环境下用 Redis SETNX 互斥锁确保同一沙箱不会被并发暂停或恢复。
- **失败回退**：自动恢复 RPC 失败时，CubeProxy 直接对客户端返回 503 + `Retry-After`，不会让用户卡在长超时上；当沙箱已经被销毁（`killing` / `killed`），则返回 410 Gone 让客户端立即停止重试。
- **故障排查**：控制节点上执行 `docker logs cube-lifecycle-manager` 查看运行日志，关键事件包括 `create event applied`、`auto-paused sandbox`、`auto-resumed sandbox`、`timeout-killed sandbox`。每个 CubeProxy 副本额外提供 `GET http://<node-ip>:8082/admin/healthz`，其中 `heartbeat_last_pushed_ms` 表示该副本最近一次向 manager 上报心跳的时间戳。

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
