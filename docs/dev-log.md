# WebUI 开发日志

> 记录每个功能的开发信息：改了什么文件、为什么这么做、注意事项。

---

## ① 沙箱状态过滤

**日期**：2026-05-09
**状态**：✅ 完成

### 改动文件
- `web/src/pages/Sandboxes.tsx`
- `web/src/locales/zh/sandboxes.json`
- `web/src/locales/en/sandboxes.json`

### 做了什么
将原来不可交互的 `Filter` 按钮，改为三个切换 Tab：**全部 / 运行中 / 已暂停**。

- 切换 Tab 时把 `state` 参数传给 `sandboxApi.list({ state })`，由后端过滤
- Running / Paused Tab 上显示当前数量角标
- React Query 的 `queryKey` 加入 `stateFilter`，切换 Tab 时独立缓存

### 为什么只有三个状态
CubeMaster 底层定义了 6 种状态（Unknown / Running / Stopped / Pausing / Paused / Error），但 CubeAPI 对外只暴露了 `running` 和 `paused`：
- `stopped` 的沙箱已被删除，不会出现在列表
- `pausing` 是毫秒级过渡态，不需要单独过滤
- `unknown` 是异常态，暂不暴露

如后续 CubeAPI 扩展状态枚举，在此文件 `STATE_TABS` 数组里追加即可。

### 相关接口
```
GET /cubeapi/v1/v2/sandboxes?state=running
GET /cubeapi/v1/v2/sandboxes?state=paused
GET /cubeapi/v1/v2/sandboxes         （全部，不传 state）
```

---

<!-- 下一个功能从这里追加 -->

## ② 沙箱详情页日志优化

**日期**：2026-05-09
**状态**：✅ 完成

### 改动文件

**前端**

**后端 CubeMaster（开发目录）**
- （新增）
- （新增路由常量 + switch case）
- （新增路由注册）

**后端 CubeAPI（开发目录）**
- （更新 SandboxLogsRequest / SandboxLogsResponse / SandboxLogLine 结构体）
- （更新 build_logs_request + to_log_entry）

---

### 做了什么

#### 前端
日志面板重写：每行带颜色区分（debug 灰 / info 青 / warn 黄 / error 红），时间格式化为本地时间精确到毫秒，新日志自动滚动到底部，空状态友好提示，右上角手动刷新按钮


#### 后端 CubeMaster（新增接口）
新增 ，读取 ，按  字段过滤出指定沙箱的日志行，支持 （Unix 毫秒时间戳分页）和 （默认 200，最大 2000）。

**为什么用 CubeShim 日志**：这是目前唯一按沙箱 ID 聚合的结构化日志，记录了沙箱从创建到销毁的完整生命周期操作事件（create / pause / resume / kill 等），对运维排障有价值。用户程序的 stdout/stderr 目前底层未收集，是后续待实现的功能。

#### 后端 CubeAPI（对接新接口）
将原来因 CubeMaster 未实现而返回占位消息的逻辑，更新为真正调用新接口。同步更新了结构体字段名（ → ， → ， → ）以对齐 CubeMaster 返回格式。

---

### 接口链路


### 注意事项
- CubeMaster 开发版二进制在 ，启动需要 
- CubeAPI 开发版二进制在 
- 日志随机器运行持续追加，不会随沙箱销毁消失（存在磁盘 ）
- 日志文件路径硬编码在  的  常量中，如部署路径变化需同步修改





---

## ③ 沙箱创建页 `/sandboxes/new`

**日期**：2026-05-11
**状态**：✅ 完成

### 改动文件

**前端**
- `web/src/pages/SandboxNew.tsx`（新增）
- `web/src/api/client.ts`（`sandboxApi` 新增 `create()` 方法）
- `web/src/main.tsx`（路由 `/sandboxes/new` 指向 `SandboxNewPage`，移除未用的 `Plus` 图标 import）
- `web/src/i18n/resources.ts`（注册 `sandboxNew` namespace）
- `web/src/i18n/index.ts`（`ns` 数组追加 `sandboxNew`）
- `web/src/locales/en/sandboxNew.json`（新增）
- `web/src/locales/zh/sandboxNew.json`（新增）
- `web/src/mocks/handlers/index.ts`（新增 `POST /sandboxes` mock handler）
- `web/src/mocks/fixtures/index.ts`（新增 `createSandbox()` fixture）

### 做了什么

新建 `SandboxNew.tsx`，替换原来的 Placeholder 占位页，包含三个区域：

1. **模板选择器**：卡片网格布局，展示 templateID / instanceType / version / status Badge，仅 `ready` 状态可点击
2. **资源配置表单**：超时时间（秒）、别名（可选）、autoPause 开关
3. **Metadata 编辑器**：支持动态增删键值对

提交时调用 `POST /cubeapi/v1/sandboxes`，成功后跳转至 `/sandboxes/:id`。

### 相关接口
```
GET  /cubeapi/v1/templates        （获取模板列表）
POST /cubeapi/v1/sandboxes        （创建沙箱）
```

---



---

## ⑥ 修复沙箱列表启动时间显示错误

**日期**：2026-05-11
**状态**：✅ 完成

### 改动文件
- `CubeAPI/src/cubemaster/mod.rs`
- `CubeAPI/src/services/sandboxes.rs`

### 问题描述
沙箱列表页所有沙箱的「启动时间」均显示「1秒钟前」，实际上各沙箱应有不同的创建时间。

### 根因分析
CubeMaster `/cube/sandbox/list` 接口返回的 `SandboxBriefData` 中，`started_at` 字段为空（`None`）。CubeAPI 在 `from_cubemaster_info` 中处理时：

```rust
started_at: s.started_at.unwrap_or(now),  // started_at 为 None，fallback 成当前时间
```

导致每次调用 list 接口，所有沙箱的 `startedAt` 都被设置为「当前时间」。

### 修复方案
CubeMaster list 接口的 `SandboxBriefData` 中实际有 `create_at` 字段（Unix 纳秒，来自 Cubelet container），但 CubeAPI 侧未读取。

**修改 `cubemaster/mod.rs`**：
- `SandboxInfo` 结构体新增 `create_at: i64` 字段，接收 CubeMaster 返回的 Unix 纳秒时间戳
- 将 `datetime_from_unix_nanos` 函数改为 `pub(crate)` 供其他模块使用

**修改 `services/sandboxes.rs`**：
- `from_cubemaster_info` 中改为三级 fallback：
  1. `started_at`（显式字段，优先）
  2. `datetime_from_unix_nanos(create_at)`（Cubelet 容器创建时间）
  3. `now`（兜底，理论上不会走到这里）

```rust
let started_at = s.started_at
    .or_else(|| datetime_from_unix_nanos(s.create_at))
    .unwrap_or(now);
```

### 验证
修复后与 `cubemastercli list` 输出对比，时间完全一致（UTC 与北京时间差 8 小时）：

| sandbox_id | cubemastercli create_at | API startedAt |
|---|---|---|
| 8143aD3d | 2026-05-08 15:25:25 | 2026-05-08T07:25:25Z ✅ |
| dA68B3eE | 2026-05-08 15:27:27 | 2026-05-08T07:27:27Z ✅ |
| 6D6a013A | 2026-05-08 15:30:55 | 2026-05-08T07:30:54Z ✅ |
| 5AC42Cd3 | 2026-05-11 14:39:00 | 2026-05-11T06:39:00Z ✅ |

