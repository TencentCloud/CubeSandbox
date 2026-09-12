---
title: Scheduler Profile 配置示例
description: 可复制的 CubeMaster 运行时 scheduler.profile / scheduler.profiles YAML 示例，含内置预设与 binpack_score。运行时覆盖层不等于离线模拟器 strategy profile。
status: working_guide
source: CubeMaster/pkg/base/config/config.go
updated: 2026-09-10
---

# Scheduler Profile 配置示例

本文提供 CubeMaster **运行时** Profile 覆盖层与 `binpack_score` 的可复制 YAML。
内容覆盖本运行时 Profiles + binpack 变更所交付的能力。HTTP 插件评分器
（`external_http_score`）属于 #1700 跟踪的相关开放工作，**未**在此注册或允许。
#1699 / #1700 仍为相关开放工作，**不会**随本变更集合并。

## 范围

本文说明如何配置 `scheduler.profile` 与 `scheduler.profiles`，使配置
`preHandle` 应用 Filter/Score 选择器列表，并将 Profile 的
`resource_weights` 合并到基础 `scheduler.score.resource_weights` 映射上。

范围内：

- 用户自定义运行时 Profile 覆盖；
- 内置预设 `balanced_spread`、`template_locality_first`、
  `binpack_utilization`（空 `scheduler.profile` 仍保留默认）；
- 按名称启用 `binpack_score`（直接或经 Profile）；
- 将 `plugin_conf` 保留在 `scheduler.score.plugin_conf`。

范围外：

- 将运行时预设视为与离线模拟器 `weightsForProfile` / `schedulerbench`
  公式等价；
- 在 `scheduler.profiles.<name>.score` 下放置 `plugin_conf`；
- 在 `scheduler.profile` 为空时改变生产 Filter/Score 默认值；
- 将 `external_http_score` 作为本 PR 的一部分交付或文档化
  （见 #1700；相关开放工作，未合并）；
- 对真实多机性能作任何宣称。

## 运行时 Profile 契约

源码：`CubeMaster/pkg/base/config/config.go`
（`SchedulerConf`、`SchedulerProfileConf`、`SchedulerProfileScoreConf`、
`applySchedulerProfile`、`builtinSchedulerProfiles`、
`validateSchedulerProfileSelectors`）。

| YAML 路径 | 作用 |
|---|---|
| `scheduler.profile` | 当前生效覆盖层名称。空字符串（默认）表示不展开。内置名无需用户 map key 即可应用。 |
| `scheduler.profiles` | 用户自定义命名覆盖层。与内置同名的用户 key **完全覆盖**该内置。 |
| `scheduler.profiles.<name>.filter.enable_filters` | 当 Profile 提供非 nil 列表时，**替换** `scheduler.filter.enable_filters`。丢掉基础列表中已有名称时配置加载失败，除非该 Profile 设置 `allow_dropped_filters: true`。 |
| `scheduler.profiles.<name>.allow_dropped_filters` | **用户** Profile 的显式 opt-in：允许 `enable_filters` 替换丢掉基础准入过滤器（如 `disk`、`thirtparty`）。默认 `false`。内置预设已允许丢掉（场景化短列表）。同名条目若只写该标志（无 filter/score）会回退到内置并带上该标志。 |
| `scheduler.profiles.<name>.score.enable_scorers` | 当 Profile 提供非 nil 列表时，**替换** `scheduler.score.enable_scorers`。 |
| `scheduler.profiles.<name>.score.resource_weights` | 合并覆盖到 `scheduler.score.resource_weights`；Profile 同名键胜出，无关基础键保留。这些是因子权重，不是插件权重。 |
| `scheduler.score.plugin_conf.*` | 各评分器参数。**不是** Profile 覆盖字段。 |

运行时 Profile 是**选择器覆盖层**，不改变 `Select()` 阶段顺序。内置预设是
面向场景的现有 filter/score 组合（外加薄的 `binpack_score`）。它们**不是**
离线模拟器模型，与模拟器 strategy profile 仅共享名称字符串。

`SchedulerProfileScoreConf` 有意省略 `plugin_conf`。当设置了
`scheduler.profile` 时，最终生效 `enable_scorers` 中每个已注册评分器都必须有
对应的 `scheduler.score.plugin_conf` 块；配置缺失，或因子型评分器没有任何正的
`resource_weights` 项，会在调度器构建前失败。内置预设还会拒绝其必需评分器被
显式禁用（`disable: true` / `weight: 0`）。用户 Profile 可以保留启用名同时故意
禁用。空 `scheduler.profile` 保持升级前加载路径：无效的因子/插件组合仍可能加载并
在运行时成为空操作。

三个内置仅在对应 `plugin_conf` 条目为 `nil` 时注入自包含默认值：
`balanced_spread` 对应 `real_time_weighted_average`，
`template_locality_first` 对应 `image_score`，
`binpack_utilization` 对应 `binpack_score`。运维已提供的
`plugin_conf` / `enable_weight_factors` 块不会被内置默认覆盖。

### 优先级

1. **用户 `scheduler.profiles.<name>`** 与内置同名时，完全替换该内置覆盖层。
2. **显式 `scheduler.score.plugin_conf.*`** 优先于内置注入默认值（仅当指针为
   `nil` 时才注入）。
3. **Profile `resource_weights` 同名覆盖**：先复制基础 map，再写入 Profile 键
   （冲突键以 Profile 为准；无关基础键保留）。

因子型 `real_time_weighted_average`、`multi_factor_weighted_average` 与
`image_score` 仅在其某个 `enable_weight_factors` 对应正的 `resource_weights`
值时才会被构造。纯插件型 `binpack_score` 不以该 map 作为构造门禁，也不需要伪造
`resource_weights` 块。遗留的 `affinity_score` 保持空 Profile 兼容：无 Profile 且
`resource_weights: null`（省略）时不构造；有 Profile 或非 nil `resource_weights`
map 时正常构造。`plugin_conf.binpack_score.weight < 0` 一律在配置加载阶段拒绝；
`weight: 0` 禁用 Select；省略整个块则保留运行时默认。

未知的 `scheduler.profile` 名称（既非用户定义也非内置），以及所选覆盖层中未知的
filter/score 名称，会在调度器运行前于 `preHandleScheduler` 中失败关闭。

允许的 filter 名称（须与 `CubeMaster/pkg/selector/filter/init.go` 一致）：

- `cpu`
- `mem`
- `template_locality`
- `realtime_create_num`
- `disk`
- `thirtparty`

允许的 score 名称（须与 `CubeMaster/pkg/selector/score/init.go` 一致）：

- `real_time_weighted_average`
- `multi_factor_weighted_average`
- `affinity_score`
- `image_score`
- `binpack_score`

### 运维注意事项

**Filter 列表替换（准入风险）。** 当 Profile（内置或用户）提供非 nil 的
`filter.enable_filters` 列表时，该列表会**整体替换**
`scheduler.filter.enable_filters`，**不会**与基础列表合并。对**用户** Profile，
丢掉基础列表中已有名称时**配置加载失败**，除非设置 `allow_dropped_filters: true`。
内置预设已带该 opt-in，因此现成配置（`cpu` / `mem` / `template_locality` /
`realtime_create_num`）可直接按名选用 `balanced_spread` /
`template_locality_first` / `binpack_utilization`。它们仍会换成短列表——若你此前
依赖 `disk` / `thirtparty`，请审查生效的 `enable_filters`。同名
`profiles.<builtin>` 若只写 `allow_dropped_filters`（无 filter/score）会回退到
内置，而不是应用空覆盖。

**`weight: 0` 禁用评分器。** 对四个遗留 Score 插件
（`real_time_weighted_average`、`multi_factor_weighted_average`、
`affinity_score`、`image_score`），插件级 `weight: 0`（或 `disable: true`）会禁用该
评分器并跳过其 `Select`。由于这些 `weight` 是 YAML `float64`，在已存在的
`plugin_conf.<scorer>` 块中省略 `weight` 键也会解码为 `0` 并禁用——要保持活跃请显式
写正的 `weight`。**`binpack_score` 不同：** 其 `weight` 是 `*float64`，在已有
`plugin_conf.binpack_score` 块中省略 `weight` 会保留运行时默认 `1`（启用）；只有显式
写 `0`（或 `disable: true`）才禁用。**负的**插件 `weight` 会对所有已注册评分器
（含 `binpack_score`）在配置加载阶段拒绝；升级前能带着负权重启动的配置，升级后会在
`config.Init` 失败。在 `enable_scorers` 中列出因子型 / affinity 评分器但省略整个
`plugin_conf.<scorer>` 块时，配置加载也会失败（空 Profile 同样适用）。`binpack_score`
可省略整个块并保留运行时默认；非空 Profile 下内置在指针仍为 `nil` 时可能注入默认。

**`binpack_score` 占用权重。** `cpu_weight` / `mem_weight` / `mvm_weight` 取值
`<= 0` 时，运行时回退为默认 `1`。**不能**通过把某维因子权重设为 `0` 来排除该维。
只有插件级 `weight: 0`（或 `disable: true`）才会禁用该评分器。负的 binpack
子权重与负的插件 `weight` 在配置加载时失败。不要把 `binpack_score` 与
剩余容量 / spread 评分器放进同一 `enable_scorers`：占用率极性与
`real_time_weighted_average` / `multi_factor_weighted_average` 相反，加权后会抵消。
内置 `binpack_utilization` 只启用 `binpack_score`。

**非空 Profile 下因子型评分器失败关闭。** 当 `scheduler.profile` 非空时，最终
`enable_scorers` 中的每个因子型评分器（`real_time_weighted_average`、
`multi_factor_weighted_average`、`image_score`）都要求非空的
`enable_weight_factors`，且这些因子中至少有一个正的 `resource_weights` 项。空或省略的
因子列表，或全部为零/缺失的因子权重，会在调度器运行前于 `preHandleScheduler`
中失败关闭。空 Profile 下，已有但无效的因子列表会在选择器构造时以 Error 日志
跳过，而不是让 Init 失败。

**MVM 占用容量。** `binpack_score`（以及共享同一 helper 的其他评分器）使用
`localcache.MaxMvmLimit(n)` 计算 MVM 占用——该 helper 是权威的按节点容量回退
（实例类型 / `node_max_mvm_num` 路径）。**不要**假设单独使用原始
`node.MaxMvmLimit` 就是分母。

**重启 vs 热更新。** Profile 展开发生在配置 `Init` / `preHandle` 中
（经 `preHandleScheduler` 调用 `applySchedulerProfile`）。CubeMaster **确实**通过
文件监视器热加载 `conf.yaml`：变更时 `listener.OnEvent` 会再次执行 `preHandle`，
成功则更新内存中的 `Config`；`preHandle` / `validate` 失败时通过 `CubeLog.Fatalf`
写 FATAL 日志（**不会**调用 `os.Exit`）并保留旧 Config，错误的覆盖不会生效。
选择任意 Profile 也会让有效 `enable_filters` / `enable_scorers` 中的未知名在该热加载
边界 fail-closed。但是，调度器的 Filter/Score 插件切片只在
`scheduler.InitScheduler` 中构建一次（`filter.NewSelector` / `score.NewSelector`），
**不会**在配置热加载时重建。因此，更改 `scheduler.profile`、Profile 的
`enable_filters` / `enable_scorers`，或以其他方式切换已注册选择器集合，都需要
**重启 CubeMaster** 才能在调度管线上生效。已构造评分器上实时读取的插件参数
（例如 `weight` / `disable`）可能随热加载的 Config 更新而无需重启，但选择器集合
变更不会。

## 内置 Profile 示例

保持 `scheduler.profile` 为空即可保留当前 Filter/Score 配置。设置内置名
**不**要求在 `scheduler.profiles` 下有对应 key。

`balanced_spread`（高并发短生命周期 sandbox）在缺失时提供默认
`real_time_weighted_average` 块：

```yaml
scheduler:
  profile: balanced_spread
```

`template_locality_first`（重复同模板创建）同样提供安全的 `image_score` 默认：

```yaml
scheduler:
  profile: template_locality_first
```

`binpack_utilization`（混合规格 / 长生命周期）在省略插件块时注入插件权重 1 以及
相等的 CPU/内存/MVM 占用权重：

```yaml
scheduler:
  profile: binpack_utilization
```

这些覆盖层是面向场景的选择器组合。它们**不是**模拟器 `weightsForProfile`，也
**不做**任何真实性能宣称。请记住上文的 filter 替换警告：内置会用更短列表替换
`enable_filters`，可能丢掉基础配置中的 `disk` / `thirtparty` 等准入过滤器。

## 最小 Profile 示例

保持 `scheduler.profile` 为空即可保留当前 Filter/Score 配置。一旦设置名称，除非是
内置名，否则必须存在于 `scheduler.profiles`。下面的名称（`locality_combo`）是运维自选
key，不是 CubeMaster 内置。

```yaml
scheduler:
  profile: locality_combo
  profiles:
    locality_combo:
      filter:
        enable_filters:
          - cpu
          - mem
          - template_locality
      score:
        enable_scorers:
          - image_score
          - affinity_score
        resource_weights:
          image_id: 1
          template_id: 2
  score:
    plugin_conf:
      image_score:
        weight: 1
        enable_weight_factors:
          - image_id
          - template_id
      affinity_score:
        weight: 1
```

该覆盖在 `preHandle` 时复制的内容：

- `scheduler.filter.enable_filters` 变为 `cpu`、`mem`、`template_locality`；
- `scheduler.score.enable_scorers` 变为 `image_score`、`affinity_score`；
- Profile 因子权重合并覆盖现有 `scheduler.score.resource_weights`；插件权重仍在
  `plugin_conf` 下。

省略的覆盖段保持不动。仅 filter 的 Profile 不会清空现有 `enable_scorers`；仅 score 的
Profile 不会清空现有 `enable_filters`。当 Profile **确实**提供 `enable_filters` 时，该列表
会替换基础列表（见运维注意事项）。

## BinpackScore 示例

Profile（或直接的 `enable_scorers`）可以启用 `binpack_score`。插件权重与占用因子权重
仍放在 `scheduler.score.plugin_conf.binpack_score`。**不要**把 `plugin_conf` 放在
`scheduler.profiles.<name>.score` 下。

```yaml
scheduler:
  profile: binpack_combo
  profiles:
    binpack_combo:
      filter:
        enable_filters:
          - cpu
          - mem
      score:
        enable_scorers:
          - binpack_score
  score:
    plugin_conf:
      binpack_score:
        weight: 1
        cpu_weight: 1
        mem_weight: 1
        mvm_weight: 1
        disable: false
```

若非空 Profile 下最终 `enable_scorers` 列出了 `binpack_score` 但省略了
`plugin_conf.binpack_score`，内置 `binpack_utilization` 会注入默认值；用户 Profile
也可省略该块并保留相同的运行时默认（`BinpackPluginWeight(nil)` → weight 1）。
显式 `weight: 0` 禁用 Select。负权重一律在配置加载时拒绝。
`cpu_weight` / `mem_weight` / `mvm_weight` 取值 `<= 0` 回退为 `1`（不能靠 `0` 排除某维）。
MVM 占用使用 `localcache.MaxMvmLimit`，而非单独的原始 `node.MaxMvmLimit`。

无效写法（不会覆盖 `plugin_conf`；Go 类型无此字段）：

```yaml
# Do not do this. scheduler.profiles.<name>.score has no plugin_conf.
scheduler:
  profiles:
    binpack_combo:
      score:
        enable_scorers:
          - binpack_score
        plugin_conf:          # not a SchedulerProfileScoreConf field
          binpack_score:
            weight: 1
```

## 相关开放工作

`external_http_score`（HTTP 插件评分器）在 #1700 单独跟踪，不属于本运行时 Profiles +
binpack 变更集合。#1699 / #1700 为相关开放工作，**未**在此合并。不要在本分支的
`enable_scorers` 中列出 `external_http_score`。选中 Profile 时，最终生效的
`enable_filters` / `enable_scorers` 中的未知名称会在配置加载阶段失败关闭；空
Profile 下，base `enable_scorers` 中的未知名仍在 `NewSelector` 时告警并跳过
（升级兼容）。

## 运行时 Profile vs 模拟器 Profile

运行时 Profile 与模拟器 strategy profile 都叫 “profile”，但**不等价**。

| | 运行时 Profile | 模拟器 strategy profile |
|---|---|---|
| 位置 | CubeMaster 配置：`scheduler.profile` / `scheduler.profiles` | 离线模拟器 / `schedulerbench` 的 `weightsForProfile` |
| 是什么 | 对现有 filter/score 选择器名与因子 `resource_weights` 的用户覆盖层 | 离线放置模型内部的评分权重预设 |
| 内置名 | `balanced_spread`、`template_locality_first`、`binpack_utilization`（选择器覆盖；用户同名 key 覆盖）。运维也可自选其他 map key。 | 仅模拟器侧的权重，可能复用相同字符串 |
| `plugin_conf` | 不可覆盖。参数仍在 `scheduler.score.plugin_conf` | 不是 CubeMaster 调度 YAML |

**运行时内置预设名**（选择器覆盖，不是模拟器权重）：

- `balanced_spread`
- `template_locality_first`
- `binpack_utilization`

把上述字符串之一写入 `scheduler.profile` 会加载 CubeMaster 内置覆盖，除非你还在
`scheduler.profiles` 下定义了同名用户 key（用户胜出）。离线模拟器负载不能证明真实多机部署、
CubeAPI/Cubelet 创建路径、真实创建延迟或生产性能。

## 应做 / 不应做

**应做**

- 用户自定义额外 profile map key（`locality_combo`、`binpack_combo` 或任意运维自选名）。
- 仅使用上文列出的已注册选择器名。
- 将 `plugin_conf` 保留在 `scheduler.score.plugin_conf`。
- 将评分器/插件权重放在 `plugin_conf.<scorer>.weight`；不要用评分器名作为
  `resource_weights` 键。
- 若希望现有 Filter/Score 配置不变，保持 `scheduler.profile` 为空。
- 将运行时内置预设与模拟器 `weightsForProfile` 视为两条共享名称但**公式不等价**的路径。
- 选定 Profile 后审计生效的 `enable_filters`，确认所需准入过滤器（`disk`、
  `thirtparty` 等）未被替换丢掉。
- 对每个打算保持活跃的 `plugin_conf.<scorer>` 显式设置正的 `weight`；在更改 Profile /
  选择器列表后重启 CubeMaster。

**不应做**

- 声称运行时预设使用与离线模拟器相同的评分公式。
- 在 `scheduler.profiles.<name>.score` 下放置 `plugin_conf`。
- 发明上文允许集合之外的 filter 或 score 名称。
- 将运行时 `scheduler.profiles` 等同于模拟器 `weightsForProfile`。
- 把 Profile 覆盖当作对 `PreFilter -> Filter -> Score -> PostScore` 的改动。
- 声称 #1699 / #1700 已通过本 PR 合并（它们仍是相关开放工作）。
- 假设 `cpu_weight: 0` / `mem_weight: 0` / `mvm_weight: 0` 能排除 binpack 某维
  （它们会回退为 `1`）。
- 期望在不重启 CubeMaster 的情况下，Profile / `enable_filters` / `enable_scorers`
  切换会重建活跃选择器集合。

## 验证

以下检查对照当前源码确认 YAML 契约，不是真实集群证明。

在仓库根目录：

```bash
rg -n "scheduler.profile|scheduler.profiles|binpack_score" CubeMaster/pkg/base/config/config.go
rg -n "binpack_score" CubeMaster/pkg/selector/score/init.go
```

在 `CubeMaster` 下：

```bash
go test ./pkg/base/config ./pkg/selector/score ./pkg/scheduler -count=1
```

相关测试：

- `TestPreHandleScheduler_NoProfileLeavesSchedulerUnchanged`
- `TestPreHandleScheduler_ProfileAppliesFilterAndScore`
- `TestPreHandleScheduler_UnknownProfileReturnsError`
- `TestPreHandleScheduler_BuiltinProfilesApplyWithoutUserMap`
- `TestPreHandleScheduler_UserProfileOverridesBuiltin`
- `TestInit_BinpackScoreWeightSemantics`
- `TestRunScoreFilterBinpackScorePrefersFullerNode`
- `TestRunScoreFilterBuiltinProfileOverlayChangesPlacementOrder`
