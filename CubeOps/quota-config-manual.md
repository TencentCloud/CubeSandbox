# Quota 配置手册

本文说明如何为计算节点配置 quota。quota 分两大类：

| 类别 | 字段 | 配置方式 |
|---|---|---|
| **超卖配置** | `mcpu_limit` / `mem_limit` / `mvm_limit` / `creation_concurrent_num` | Web 或 CLI |
| **Paused 释放比例** | `paused_resource_release_ratio`（下文简称 ratio） | 仅 CLI（节点级 / 集群级） |

两类相互独立：超卖配置只有节点级，ratio 有节点级与集群级两级，节点级优先级高于集群级。

---

## 一、超卖配置（前四个字段）

### 1.1 字段说明

| 字段 | 类型 | 语义 | 守卫上限 |
|---|---|---|---|
| `mcpu_limit` | 整数（milli-core） | CPU 配额，1 核 = 1000 | ≤ 物理核数 × 1000 × 20 |
| `mem_limit` | 字符串（二进制后缀） | 内存配额，如 `256Gi` / `262144Mi` | ≤ 物理内存 × 3 |
| `mvm_limit` | 整数 | 单节点最大 MVM 数 | 无（节点资源自然约束） |
| `creation_concurrent_num` | 整数 | 创建并发上限 | 无（节点资源自然约束） |

> 0 / 空串 = 不设置，节点回落到 cubelet 按宿主机推导的默认值。
> `mem_limit` 只接受 `Ki`/`Mi`/`Gi`/`Ti` 二进制后缀（大小写敏感），裸数字与十进制后缀会被拒绝。

### 1.2 Web 配置

进入节点详情页 → 「配额设置」区块，四个输入框分别对应上述字段。空字段回显该节点心跳上报的实际值作为占位提示，保存前弹确认框展示变更字段的新旧值。

Web 只提交前四个字段，不涉及 ratio（ratio 走 CLI，见第二节）。

### 1.3 CLI 配置

```bash
cubeopscli -a <cubeops-ip> -p 3010 node quota set <node-id> \
  --mcpu 80000 \
  --mem 256Gi \
  --mvm 100 \
  --create-concurrent 20 \
  --operator <你的名字>
```

- `set` 为**增量 overlay**：只覆盖显式传入的字段，未传入字段保持原值；每次 set 以读到的 revision 做 CAS，并发写冲突返回 409。
- 恢复默认：`--mcpu 0 --mem '' --mvm 0 --create-concurrent 0`。

查询与审计：

```bash
cubeopscli -a <cubeops-ip> -p 3010 node quota get <node-id>      # spec vs actual vs drift
cubeopscli -a <cubeops-ip> -p 3010 node quota history <node-id>  # 变更审计
```

`get` 输出示例：

```
NODE_ID    10.0.1.15
ACTUAL     mcpu=80000    mem=256Mi    maxMvm=100    createConcurrent=20    pausedRatio=0.6
DRIFT      none
```

### 1.4 API

CLI 走内部无鉴权路由：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/internal/v1/nodes/:nodeID/config/quota` | 读取期望/实际/漂移 |
| PUT | `/internal/v1/nodes/:nodeID/config/quota` | 写入期望配额 |
| GET | `/internal/v1/nodes/:nodeID/config/quota/history` | 审计 |

请求体（PUT，部分字段可省略，缺省保持原值）：

```json
{
  "mcpu_limit": 80000,
  "mem_limit": "256Gi",
  "mvm_limit": 100,
  "creation_concurrent_num": 20,
  "expected_revision": 0
}
```

---

## 二、Paused 释放比例（ratio）

`paused_resource_release_ratio` 是调度策略参数：暂停沙箱的 CPU/内存配额中，释放回调度器的比例，取值 `[0,1]`。

- `0` = 不释放（暂停保留全额，恢复有保证）
- `1` = 全部释放（密度最高，恢复尽力而为）

### 2.1 两级配置与优先级

ratio 分节点级和集群级：

```
生效值 = 节点显式值（优先级高）
       > 集群默认值
       > 0
```

| 配置项 | 作用范围 | 命令 |
|---|---|---|
| 节点级 | 单节点覆盖，独立于集群 | `node quota set --paused-ratio` |
| 集群级 | 所有「未显式设置」的节点跟随 | `cluster quota-defaults set` |

**节点级 > 集群级**：节点设了显式 ratio 后，集群默认变更不会覆盖它；只有清除节点显式值（`--paused-ratio-inherit`）才会回落跟随集群。

### 2.2 集群级 CLI

```bash
# 查询
cubeopscli -a <cubeops-ip> -p 3010 cluster quota-defaults get

# 设置（扇出到所有继承节点）
cubeopscli -a <cubeops-ip> -p 3010 cluster quota-defaults set 0.6 --operator <名字>

# 清除（继承节点回落为 0）
cubeopscli -a <cubeops-ip> -p 3010 cluster quota-defaults set clear --operator <名字>

# 查看集群默认变更的审计
cubeopscli -a <cubeops-ip> -p 3010 node quota history '*'
```

`set` 输出含传播统计 `bumped`/`pushed`/`push_failed`，失败的节点由 300s pull reconcile 兜底收敛。

### 2.3 节点级 CLI

```bash
# 显式覆盖（优先级高于集群默认）
cubeopscli -a <cubeops-ip> -p 3010 node quota set <node-id> \
  --paused-ratio 0.2 --operator <名字>

# 清除覆盖，回落跟随集群默认
cubeopscli -a <cubeops-ip> -p 3010 node quota set <node-id> \
  --paused-ratio-inherit --operator <名字>
```

`--paused-ratio` 与 `--paused-ratio-inherit` 互斥，不能同时传。

查看节点当前生效 ratio：

```bash
cubeopscli -a <cubeops-ip> -p 3010 node quota get <node-id>
```

### 2.4 API

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/internal/v1/cluster/quota-defaults` | 读集群默认（内部路由，无鉴权） |
| PUT | `/internal/v1/cluster/quota-defaults` | 写集群默认并扇出（内部路由，无鉴权） |

节点级 ratio 复用超卖配置的 `/internal/v1/nodes/:nodeID/config/quota`（`paused_resource_release_ratio` 字段，`null` = 继承）。

---

## 三、生效链路与排障

配置经以下链路生效：`set` 写库 → 同步 push 到节点 ops-agent → 改写 cubelet dynamicconf → cubelet 热加载 → 心跳上报实际值 → 控制面 drift 对账归零。

- **`get` 显示 `DRIFT detected`**：期望与心跳实际不一致。刚 set 后需等一个心跳周期（默认 ~1s，视图刷新 ~3s）归零；持续不一致说明 push 未达节点，检查 `set` 输出中的 `applied` / `skip_reason`。
- **`set` 输出 `not applied yet`**：spec 已落库但节点未确认写盘，由 pull reconcile（300s 周期）自动重试，无需人工干预。
- **守卫拒绝（400）**：超卖值超过上限（见 1.1 表）或 ratio 超出 `[0,1]`，按报错信息调整。
- **手改节点 conf.yaml**：会被覆盖纠正（改前自动备份，轮转保留 5 份），请通过 CLI/Web 而非直接改文件。具体是否被覆盖、覆盖哪些字段，取决于该节点的「纳管状态」，见 3.1。

### 3.1 手改文件的生效边界（纳管状态）

节点本地文件 `/usr/local/services/cubetoolbox/Cubelet/dynamicconf/conf.yaml` 里的 `host.quota` 字段，权威性来自 CubeOps 数据库。是否覆盖你的手改值，取决于该节点当前处于哪种纳管状态：

| 纳管状态 | 触发条件 | 手改 conf.yaml 的效果 |
|---|---|---|
| **全纳管（node-managed）** | 通过接口 `node quota set` 设置过该节点（哪怕只改了一个字段） | 5 个字段（`mcpu_limit`/`mem_limit`/`mvm_limit`/`creation_concurrent_num`/`paused_resource_release_ratio`）**全部被数据库值覆盖** |
| **仅纳管 ratio（cluster-managed）** | 只设置过集群默认 ratio，未单独设置该节点 | 仅 `paused_resource_release_ratio` 被覆盖回集群默认；其余 4 个字段手改**仍生效** |
| **不纳管（unmanaged）** | 两者都没做过 | 手改值**全部生效，不被覆盖** |

四点说明：

1. **纳管是节点级，不是字段级**：只要用接口 `node quota set` 写过该节点（即使只传了一个 `mcpu_limit`），整个节点就进入「全纳管」，5 个字段全部以数据库为准——包括你**没在接口里设置过的字段**，手改同样会被覆盖。
2. **「不纳管」是升级兼容的保护**：老版本没有 quota 管理，用户只能手改 conf.yaml；升级后只要不通过接口设置，这些手改值会原样保留，不会被新功能清空或重置。
3. **「纳管」是隐式可切换的**：即使你从未单独设置过某节点，只要集群管理员执行了 `cluster quota-defaults set`，该节点就会进入「仅纳管 ratio」状态，其 ratio 字段开始跟随集群默认、手改会被覆盖。
4. **一旦进入纳管，请改用 CLI/Web**：纳管状态下手改会被 300s pull reconcile 自动纠正（写前备份、轮转保留 5 份）。
