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

### 注意事项
- `timeout` 字段由 CubeAPI 透传至 CubeMaster 的 `CreateSandboxRequest.timeout`，**真实生效**，填 0 表示永不自动超时
- Mock handler 中 `createSandbox()` 会把新建的假沙箱追加到内存列表，支持后续列表页展示

---

## ④ 修复模板状态大小写不匹配

**日期**：2026-05-11
**状态**：✅ 完成

### 改动文件
- `web/src/pages/SandboxNew.tsx`
- `web/src/pages/Templates.tsx`
- `web/src/pages/Overview.tsx`

### 做了什么

后端（CubeMaster）返回的模板 status 为全大写（`READY` / `FAILED` / `BUILDING`），而前端三处均使用小写字符串字面量直接比较，导致：

- `SandboxNew` 页：所有模板均为 disabled，无法选择
- `Templates` 列表页：状态 Badge 全部显示为 err（红色）
- `Overview` 页：同上

**修复方式**：比较前统一调用 `.toLowerCase()`

```diff
- tpl.status === ready
+ tpl.status.toLowerCase() === ready
```

---

## ⑤ Vite 升级至 6.4.2

**日期**：2026-05-11
**状态**：✅ 完成

### 改动文件
- `web/package.json`
- `web/package-lock.json`

### 做了什么
将 Vite 从 `5.4.17` 升级到 `6.4.2`（大版本升级）。升级后页面功能正常，无兼容性问题。

