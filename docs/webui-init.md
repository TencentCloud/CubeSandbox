# CubeSandbox WebUI 现状文档

> 文档版本：2026-05-12  
> 代码仓库：`https://git.woa.com/liciazhu/cubesandboxWebUI.git`  
> 分支：`feat/webui-dev`  
> 前端路径：`web/`

---

## 一、技术栈

| 类别 | 技术 | 版本 |
|------|------|------|
| 框架 | React | 18.x |
| 语言 | TypeScript | 5.x |
| 构建工具 | Vite | 5.x |
| 路由 | React Router DOM | v6 |
| 数据请求 | TanStack React Query | v5 |
| 状态管理 | Zustand | v5 |
| 样式 | TailwindCSS | v3 |
| 组件基础 | Radix UI | — |
| 图标 | Lucide React | — |
| 国际化 | i18next + react-i18next | — |
| Mock | MSW (Mock Service Worker) | v2 |
| API Schema | openapi-typescript（自动生成） | — |

### 设计系统

- **主题**：深色（Cube Midnight）/ 浅色（Paper），通过 CSS 变量切换，存 `localStorage`
- **品牌色**：`cube-cyan`、`cube-violet`、`cube-amber`、`cube-rose`、`cube-emerald`
- **字体**：Inter（正文）、JetBrains Mono（代码/ID）
- **圆角**：`--radius: 0.75rem`
- **动画**：`fade-in`（页面入场）、`pulse-soft`（实时指示器）

### 公共 UI 组件（`web/src/components/ui/`）

| 组件 | 说明 |
|------|------|
| `Badge` | 状态标签，支持 `ok/warn/err/info/mute/default` 六种 tone |
| `Button` | 按钮，支持 `default/outline/ghost/destructive` 变体 |
| `Card` + `CardHeader/Title/Description/Content` | 卡片容器 |
| `Input` | 输入框 |
| `Skeleton` | 加载骨架屏 |

---

## 二、项目结构

```
web/
├── src/
│   ├── main.tsx              # 入口，路由配置，QueryClient
│   ├── api/
│   │   ├── client.ts         # API 方法封装（sandboxApi / templateApi / clusterApi）
│   │   └── generated/
│   │       └── schema.ts     # openapi-typescript 自动生成的类型
│   ├── lib/
│   │   ├── api.ts            # fetch 封装，BASE=/cubeapi/v1，注入 X-API-Key
│   │   ├── utils.ts          # cn / formatBytes / formatRelative / short
│   │   └── mockFlag.ts       # Mock 开关（localStorage + URL 参数）
│   ├── components/
│   │   ├── AppShell.tsx      # 整体布局（Rail + TopBar + Outlet）
│   │   ├── Rail.tsx          # 左侧图标导航栏（68px）
│   │   ├── TopBar.tsx        # 顶部栏（搜索框 / 语言 / 主题 / Mock 开关）
│   │   ├── CommandPalette.tsx # ⌘K 命令面板（导航 + 创建沙箱快捷键）
│   │   ├── LanguageSwitcher.tsx
│   │   ├── ThemeToggle.tsx
│   │   ├── ThemeProvider.tsx
│   │   └── ui/               # 基础组件库
│   ├── pages/                # 页面组件（见第三节）
│   ├── store/
│   │   ├── ui.ts             # CommandPalette 开关状态（Zustand）
│   │   └── theme.ts          # 主题模式状态（Zustand）
│   ├── i18n/                 # i18next 配置
│   ├── locales/
│   │   ├── en/               # 英文翻译（14 个命名空间）
│   │   └── zh/               # 中文翻译（14 个命名空间）
│   ├── mocks/
│   │   ├── browser.ts        # MSW 启动
│   │   ├── handlers/index.ts # Mock 路由处理器
│   │   └── fixtures/index.ts # Mock 数据（沙箱/模板/节点/集群）
│   └── styles/
│       └── globals.css       # CSS 变量 + Tailwind 基础样式
├── vite.config.ts            # 开发代理：/cubeapi → localhost:3000
├── tailwind.config.js
├── tsconfig.json
└── package.json
```

---

## 三、页面详情

### 3.1 布局层（AppShell）

所有页面共享以下布局：

- **左侧导航栏 Rail**（固定 68px）：图标导航 + Hover tooltip
- **顶部栏 TopBar**（sticky）：面包屑 / 搜索框（⌘K 唤起 CommandPalette）/ 语言切换 / 主题切换 / Mock 开关 / 通知 / 头像
- **命令面板 CommandPalette**（⌘K / Ctrl+K）：快速跳转所有页面 + 创建沙箱

---

### 3.2 Overview 页面

**路由**：`/`  
**文件**：`src/pages/Overview.tsx`  
**状态**：✅ 已实现

#### 功能与接口

| 功能模块 | 接口 | 轮询 |
|---------|------|------|
| KPI：运行中沙箱数 | `GET /cubeapi/v1/v2/sandboxes` | 5s |
| KPI：可用模板数 | `GET /cubeapi/v1/templates` | 30s |
| KPI：CPU 利用率 + 进度条 | `GET /cubeapi/v1/cluster/overview` | 10s |
| KPI：内存利用率 + 进度条 | `GET /cubeapi/v1/cluster/overview` | 10s |
| KPI：健康节点数 / 总节点数 | `GET /cubeapi/v1/cluster/overview` | 10s |
| 最近沙箱列表（最多 6 条） | `GET /cubeapi/v1/v2/sandboxes` | 5s |
| 模板列表（最多 5 条） | `GET /cubeapi/v1/templates` | 30s |

#### UI 细节

- KPI 卡片：ok=emerald / warn=amber / err=rose / info=violet，有进度条（CPU/内存）
- 右上角「实时」指示器（pulse 动画）
- 最近沙箱：点击跳转 `/sandboxes/:id`
- 模板：点击跳转 `/templates/:id`（⚠️ 详情页未实现）

---

### 3.3 Sandboxes 列表页

**路由**：`/sandboxes`  
**文件**：`src/pages/Sandboxes.tsx`  
**状态**：✅ 基本实现，⚠️ 状态过滤未接参数

#### 功能与接口

| 功能 | 接口 | 状态 |
|------|------|------|
| 沙箱列表（ID/模板/CPU/内存/状态/时间） | `GET /cubeapi/v1/v2/sandboxes` | ✅ |
| 本地关键字搜索（sandboxID/templateID/alias/clientID） | 纯前端过滤 | ✅ |
| 状态筛选按钮（running/paused） | `GET /cubeapi/v1/v2/sandboxes?state=` | ⚠️ 按钮有但未传参 |
| 暂停沙箱 | `POST /cubeapi/v1/sandboxes/:id/pause` | ✅ |
| 恢复沙箱 | `POST /cubeapi/v1/sandboxes/:id/resume` | ✅ |
| 删除沙箱 | `DELETE /cubeapi/v1/sandboxes/:id` | ✅ |
| 点击 ID 跳转详情 | 路由跳转 `/sandboxes/:id` | ✅ |
| 创建沙箱按钮 | 路由跳转 `/sandboxes/new` | ❌ 目标页为占位符 |

#### UI 细节

- 表格列：state / sandboxID（mono） / template / cpu / memory / started / actions
- 轮询：5s 自动刷新
- 操作按钮互斥 disabled（同时只能执行一个操作）

---

### 3.4 SandboxDetail 详情页

**路由**：`/sandboxes/:sandboxID`  
**文件**：`src/pages/SandboxDetail.tsx`  
**状态**：✅ 基本实现，⚠️ 日志需验证

#### 功能与接口

| 功能 | 接口 | 状态 |
|------|------|------|
| 资源信息（vCPU/内存/clientID/alias/到期/域名） | `GET /cubeapi/v1/sandboxes/:id` | ✅ |
| 运行时信息（启动/到期/状态/envd 版本） | `GET /cubeapi/v1/sandboxes/:id` | ✅ |
| Metadata 键值展示 | `GET /cubeapi/v1/sandboxes/:id` | ✅ |
| 日志展示（pre 代码块） | `GET /cubeapi/v1/v2/sandboxes/:id/logs` | ⚠️ 有 UI，格式需验证 |
| 暂停沙箱 | `POST /cubeapi/v1/sandboxes/:id/pause` | ✅ |
| 恢复沙箱 | `POST /cubeapi/v1/sandboxes/:id/resume` | ✅ |
| 删除沙箱（成功后跳回列表） | `DELETE /cubeapi/v1/sandboxes/:id` | ✅ |
| 创建快照 | `POST /cubeapi/v1/sandboxes/:id/snapshots` | ❌ 未封装，无 UI |
| 连接终端 | `POST /cubeapi/v1/sandboxes/:id/connect` | ❌ 未封装，无 UI |

#### UI 细节

- 轮询：详情 5s、日志 10s
- 日志：`[level] timestamp message` 格式，fixed 高度 360px 可滚动
- 删除后自动导航回 `/sandboxes`

---

### 3.5 Sandboxes New 创建页

**路由**：`/sandboxes/new`  
**文件**：`src/pages/Placeholder.tsx`（占位）  
**状态**：❌ 未实现，当前显示 "Coming Soon"

所需接口：
- `GET /cubeapi/v1/templates`（获取模板列表供选择）
- `POST /cubeapi/v1/sandboxes`（提交创建）

---

### 3.6 Templates 列表页

**路由**：`/templates`  
**文件**：`src/pages/Templates.tsx`  
**状态**：✅ 已实现

#### 功能与接口

| 功能 | 接口 | 状态 |
|------|------|------|
| 模板卡片列表（ID/instanceType/version/status/创建时间/imageInfo） | `GET /cubeapi/v1/templates` | ✅ |
| 点击跳转详情 | 路由跳转 `/templates/:id` | ❌ 目标页为占位符 |
| 创建模板入口 | — | ❌ 无入口按钮 |

#### UI 细节

- 卡片式布局（grid 1/2/3 列响应式）
- 状态 Badge：ready=ok / building=warn / failed=err
- 无轮询（静态展示）

---

### 3.7 TemplateDetail 详情页

**路由**：`/templates/:templateID`  
**文件**：`src/pages/Placeholder.tsx`（占位）  
**状态**：❌ 未实现，当前显示 "Coming Soon"

所需接口：
- `GET /cubeapi/v1/templates/:id`（模板详情，含 replicas/createRequest）
- `POST /cubeapi/v1/templates/:id`（重建）
- `DELETE /cubeapi/v1/templates/:id`（删除）
- `POST /cubeapi/v1/templates/:id/builds/:buildID`（启动构建）
- `GET /cubeapi/v1/templates/:id/builds/:buildID/status`（构建状态）
- `GET /cubeapi/v1/templates/:id/builds/:buildID/logs`（构建日志）

---

### 3.8 Nodes 列表页

**路由**：`/nodes`  
**文件**：`src/pages/Nodes.tsx`  
**状态**：✅ 已实现

#### 功能与接口

| 功能 | 接口 | 状态 |
|------|------|------|
| 节点卡片列表（ID/hostname/状态/角色/地址） | `GET /cubeapi/v1/nodes` | ✅ |
| CPU 使用率进度条 | `GET /cubeapi/v1/nodes` | ✅ |
| 内存使用率进度条 | `GET /cubeapi/v1/nodes` | ✅ |
| Conditions 状态（最多 3 条） | `GET /cubeapi/v1/nodes` | ✅ |
| 点击节点跳转详情 | — | ❌ 无详情页 |

#### UI 细节

- 轮询：15s
- 进度条颜色：>85% rose / >65% amber / 其他 violet
- 卡片式布局（grid 1/2 列响应式）

---

### 3.9 NodeDetail 详情页

**路由**：`/nodes/:nodeID`  
**文件**：未创建  
**状态**：❌ 未实现（节点列表无跳转入口）

所需接口：
- `GET /cubeapi/v1/nodes/:nodeID`（节点详情，含 localTemplates 字段）

---

### 3.10 Keys 页面

**路由**：`/keys`  
**文件**：`src/pages/Keys.tsx`  
**状态**：✅ 已实现

#### 功能

| 功能 | 说明 |
|------|------|
| 输入并保存 API Key | 存入 `localStorage['cube.apiKey']` |
| 所有请求自动注入 `X-API-Key` 头 | `lib/api.ts` 封装层处理 |

#### 注意

- 无服务端存储，纯本地
- 后端 auth 中间件已预留（`auth_callback_url` 配置项），可后续对接 SSO

---

### 3.11 占位页面（Placeholder）

以下页面均使用 `src/pages/Placeholder.tsx` 组件渲染，显示 "Coming Soon"：

| 路由 | 页面 | 所需后端接口 |
|------|------|-------------|
| `/sandboxes/new` | 创建沙箱 | 现有接口即可 |
| `/templates/:id` | 模板详情 | 现有接口即可 |
| `/network` | 网络管理 | 需新增接口 |
| `/observability` | 可观测性 | 需新增接口 |
| `/settings` | 系统设置 | 需新增接口 |

---

## 四、API 层

### 4.1 请求封装（`lib/api.ts`）

```
BASE URL：/cubeapi/v1（开发环境由 Vite proxy 转发到 localhost:3000）
认证：X-API-Key 请求头（从 localStorage['cube.apiKey'] 读取）
错误处理：非 2xx 响应抛出 ApiError（含 status + body）
```

### 4.2 sandboxApi（`api/client.ts`）

| 方法 | 接口 | 有无 UI |
|------|------|---------|
| `list(params?)` | `GET /v2/sandboxes` | ✅ Overview、Sandboxes |
| `get(id)` | `GET /sandboxes/:id` | ✅ SandboxDetail |
| `kill(id)` | `DELETE /sandboxes/:id` | ✅ |
| `pause(id)` | `POST /sandboxes/:id/pause` | ✅ |
| `resume(id, body?)` | `POST /sandboxes/:id/resume` | ✅ |
| `setTimeout(id, sec)` | `POST /sandboxes/:id/timeout` | ❌ 封装了，无 UI |
| `logs(id, params?)` | `GET /v2/sandboxes/:id/logs` | ⚠️ 有 UI，需验证 |
| ~~create~~ | `POST /sandboxes` | ❌ 未封装 |
| ~~snapshots~~ | `POST /sandboxes/:id/snapshots` | ❌ 未封装 |
| ~~connect~~ | `POST /sandboxes/:id/connect` | ❌ 未封装 |
| ~~refresh~~ | `POST /sandboxes/:id/refreshes` | ❌ 未封装 |

### 4.3 templateApi（`api/client.ts`）

| 方法 | 接口 | 有无 UI |
|------|------|---------|
| `list()` | `GET /templates` | ✅ Overview、Templates |
| `get(id)` | `GET /templates/:id` | ❌ 封装了，页面未实现 |
| `remove(id)` | `DELETE /templates/:id` | ❌ 封装了，无 UI 入口 |
| ~~create~~ | `POST /templates` | ❌ 未封装 |
| ~~rebuild~~ | `POST /templates/:id` | ❌ 未封装 |
| ~~startBuild~~ | `POST /templates/:id/builds/:buildID` | ❌ 未封装 |
| ~~getBuildStatus~~ | `GET /templates/:id/builds/:buildID/status` | ❌ 未封装 |
| ~~getBuildLogs~~ | `GET /templates/:id/builds/:buildID/logs` | ❌ 未封装 |

### 4.4 clusterApi（`api/client.ts`）

| 方法 | 接口 | 有无 UI |
|------|------|---------|
| `overview()` | `GET /cluster/overview` | ✅ Overview |
| `nodes()` | `GET /nodes` | ✅ Nodes |
| `node(id)` | `GET /nodes/:id` | ❌ 封装了，无详情页 |

---


## 五、国际化

支持语言：**中文（zh）**、**英文（en）**，浏览器自动检测，手动切换后持久化。

命名空间（14 个）：

| 命名空间 | 用途 |
|---------|------|
| `common` | 通用词（Save/Cancel/Live/Preview 等）|
| `nav` | 左侧导航文字 |
| `topbar` | 顶部栏文字 |
| `command` | ⌘K 命令面板 |
| `overview` | Overview 页 |
| `sandboxes` | Sandboxes 列表页 |
| `sandboxDetail` | Sandbox 详情页 |
| `templates` | Templates 页 |
| `nodes` | Nodes 页 |
| `keys` | Keys 页 |
| `placeholder` | 占位页（Coming Soon 内容）|
| `theme` | 主题切换 |

---

## 六、已知问题与待验证项

| 问题 | 位置 | 说明 |
|------|------|------|
| 状态过滤未生效 | Sandboxes 页 | Filter 按钮有 UI，但未将 state 参数传入 `sandboxApi.list()` |
| 日志格式待验证 | SandboxDetail 页 | `logs.data?.logs` 结构是否与后端 `SandboxLogsV2Response` 对齐需调试 |
| 模板详情点击 404 | Templates 页 | 路由存在但渲染 Placeholder |
| 节点无法点击进详情 | Nodes 页 | 卡片无跳转链接，`clusterApi.node(id)` 已封装但无页面 |
| 创建沙箱入口无效 | Sandboxes 页 / ⌘K | 跳转到 `/sandboxes/new` 但渲染 Placeholder |



---

## 七、未来两周 Roadmap（2026-05-12 ~ 2026-05-26）

> **原则**
> - Week 1 聚焦「让现有功能真正可用」——全部基于已有后端接口
> - Week 2 聚焦「补齐核心管理功能」——前端新页面 + 少量后端接口扩展

---

### Week 1（05-12 ~ 05-19）：修复 + 补齐核心页面

#### Day 1-2：修复已知问题

| # | 任务 | 文件 | 接口 | 
|---|------|------|------|
| 1 | 修复沙箱状态过滤（Filter 按钮接入 `state` 参数） | `pages/Sandboxes.tsx` | `GET /v2/sandboxes?state=` | 
| 2 | 验证并修复沙箱日志格式（对齐 `SandboxLogsV2Response`） | `pages/SandboxDetail.tsx` | `GET /v2/sandboxes/:id/logs` | 


#### Day 2-3：沙箱创建页 `/sandboxes/new`

**目标**：用户能选模板、填配置、成功创建沙箱并跳转到详情页

| # | 任务 | 文件 | 接口 | 
|---|------|------|------|
| 4 | 新建 `SandboxNew.tsx` 页面，替换 Placeholder | `pages/SandboxNew.tsx` | — | 
| 4a | 模板选择器（下拉/卡片，展示 templateID + status） | 同上 | `GET /templates` | 
| 4b | 资源配置表单（CPU / 内存 / 超时 / alias / metadata KV 编辑器） | 同上 | — | 
| 4c | 提交创建，成功后跳转 `/sandboxes/:id` | 同上 | `POST /sandboxes` |
| 4d | 补充 Mock handler：`POST /sandboxes` | `mocks/handlers/index.ts` + `mocks/fixtures/index.ts` | — | 
| 4e | 注册路由（替换 Placeholder） | `main.tsx` | — | 

#### Day 4-5：模板详情页 `/templates/:templateID`

**目标**：展示模板完整信息、副本分布、构建历史，支持重建和删除

| # | 任务 | 文件 | 接口 | 
|---|------|------|------|
| 5 | 新建 `TemplateDetail.tsx`，替换 Placeholder | `pages/TemplateDetail.tsx` | — |
| 5a | 基本信息卡片（版本/状态/instanceType/imageInfo/创建时间） | 同上 | `GET /templates/:id` | 
| 5b | 副本分布列表（replicas：节点/ready 状态/本地版本） | 同上 | `GET /templates/:id` |
| 5c | 重建按钮（触发构建，返回 buildID） | 同上 | `POST /templates/:id` | 
| 5d | 构建进度追踪（轮询 status，展示 building→ready/failed） | 同上 | `GET /templates/:id/builds/:buildID/status` |
| 5e | 构建日志展示（可折叠 pre 块） | 同上 | `GET /templates/:id/builds/:buildID/logs` | 
| 5f | 删除模板（二次确认弹窗，成功后跳回列表） | 同上 | `DELETE /templates/:id` | 
| 5g | 补充 Mock handler：rebuild / buildStatus / buildLogs / delete | `mocks/handlers/index.ts` | — | 
| 5h | 注册路由（替换 Placeholder） | `main.tsx` | — |


---

### Week 2（05-16 ~ 05-22）：节点详情 + 模板创建 + 体验打磨

#### Day 1-2：节点详情页 `/nodes/:nodeID`

**目标**：展示单节点完整资源、Conditions、本地模板，从 Nodes 列表可跳转

| # | 任务 | 文件 | 接口 | 
|---|------|------|------|
| 6 | 新建 `NodeDetail.tsx` | `pages/NodeDetail.tsx` | — |
| 6a | 资源详情（CPU/内存进度条 + 数值、maxMvmSlots） | 同上 | `GET /nodes/:id` | 
| 6b | Conditions 完整列表（type/status/reason/message/时间） | 同上 | `GET /nodes/:id` | 
| 6c | 本地模板列表（localTemplates，点击跳转模板详情） | 同上 | `GET /nodes/:id` | 
| 6d | 节点上运行的沙箱列表（按 nodeID 过滤） | 同上 | `GET /v2/sandboxes`（前端过滤） |
| 6e | Nodes 列表卡片添加跳转链接 | `pages/Nodes.tsx` | — | 
| 6f | 注册路由 `/nodes/:nodeID` | `main.tsx` | — |

#### Day 2-3：模板创建页 `/templates/new`

**目标**：用户能填写配置创建新模板并追踪构建进度

| # | 任务 | 文件 | 接口 | 工时 |
|---|------|------|------|
| 7 | 新建 `TemplateNew.tsx` | `pages/TemplateNew.tsx` | — | 
| 7a | 创建表单（templateID / instanceType / image）| 同上 | — | 
| 7b | 提交创建，获取 buildID | 同上 | `POST /templates` |
| 7c | 构建进度轮询展示（复用 TemplateDetail 构建组件） | 同上 | `GET /templates/:id/builds/:buildID/status` + `logs` |
| 7d | Templates 列表页添加「New Template」入口按钮 | `pages/Templates.tsx` | — |
| 7e | ⌘K 命令面板添加「Create Template」入口 | `components/CommandPalette.tsx` | — | 
| 7f | 补充 Mock handler：`POST /templates` | `mocks/handlers/index.ts` | — | 
| 7g | 注册路由 `/templates/new` | `main.tsx` | — | 

#### Day 4：沙箱快照功能

| # | 任务 | 文件 | 接口 |
|---|------|------|------|
| 8a | `sandboxApi` 补充 `snapshot(id, name?)` 方法 | `api/client.ts` | `POST /sandboxes/:id/snapshots` | 
| 8b | SandboxDetail 页添加「创建快照」按钮 + 输入快照名弹窗 | `pages/SandboxDetail.tsx` | 同上 |
| 8c | 补充 Mock handler | `mocks/handlers/index.ts` | — |

#### Day 5：全局体验打磨

| # | 任务 | 文件 | 说明 | 工时 |
|---|------|------|------|------|
| 9a | 统一错误处理：接口失败 Toast 通知 | `lib/api.ts` + 新增 `components/Toaster.tsx` | 用 Radix Toast | 2h |
| 9b | 删除操作统一二次确认弹窗组件 | 新增 `components/ConfirmDialog.tsx` | 复用于沙箱/模板删除 | 1h |
| 9c | Overview 页沙箱趋势：展示 running/paused 数量分布 | `pages/Overview.tsx` | 纯前端计算，无需新接口 | 1h |
| 9d | Sandboxes 列表支持 metadata 过滤（搜索框支持 `key=value` 格式） | `pages/Sandboxes.tsx` | `GET /v2/sandboxes?metadata=` | 1h |



---

### 两周完成后的状态总览

| 页面 | Week 前 | Week 1 后 | Week 2 后 |
|------|---------|-----------|----------|
| Overview | ✅ 已实现 | ✅ + 数量分布 | ✅ 完善 |
| Sandboxes 列表 | ⚠️ 过滤未接 | ✅ 过滤/日志修复 | ✅ + metadata 过滤 |
| Sandbox 详情 | ⚠️ 日志有问题 | ✅ 日志| ✅ + 快照 |
| Sandbox 创建 | ❌ 占位符 | ✅ 完整实现 | ✅ |
| Templates 列表 | ✅ 已实现 | ✅ | ✅ + New Template 入口 |
| Template 详情 | ❌ 占位符 | ✅ 完整实现 | ✅ |
| Template 创建 | ❌ 无入口 | ❌ | ✅ 完整实现 |
| Nodes 列表 | ✅ 已实现 | ✅ | ✅ + 跳转链接 |
| Node 详情 | ❌ 无页面 | ❌ | ✅ 完整实现 |
| Network | ❌ 占位符 | ❌ | ❌（需新后端接口）|
| Observability | ❌ 占位符 | ❌ | ❌（需新后端接口）|
| Settings | ❌ 占位符 | ❌ | ❌（需新后端接口）|
| Keys | ✅ 已实现 | ✅ | ✅ |

---


---

## 八、开发时间线（2026-05-12 ~ 2026-05-26）

> 📅 

```
日期        Mon 5/12  Tue 5/13  Wed 5/14  Thu 5/15  Fri 5/16
            ─────────────────────────────────────────────────
Bug 修复
  状态过滤    ██
  日志格式    ██

沙箱创建页               ████████
  模板选择器              ████
  表单 + 提交                  ████
  Mock handler                 ██

模板详情页                          ████████████
  基础信息卡                          ████
  副本分布                                ████
  重建 + 构建进度                             ████
  构建日志 + 删除                                  ██

日期        Mon 5/19  Tue 5/20  Wed 5/21  Thu 5/22  Fri 5/23
            ─────────────────────────────────────────────────
节点详情页   ████████
  资源 + Cond ████
  本地模板         ████
  Nodes 跳转       ██

模板创建页            ████████
  创建表单               ████
  构建进度轮询                ████
  ⌘K 入口 + Mock               ██

沙箱快照                              ████
  API + UI                              ████

全局体验                                    ████████
  Toast 错误处理                              ████
  ConfirmDialog                                   ██
  Overview 趋势                                   ██
  Sandboxes metadata 过滤                              ████
```

### 关键里程碑

| 日期 | 里程碑 |
|------|--------|
| **05-14（周三）** | 沙箱创建页完成，用户能端到端创建沙箱 |
| **05-16（周五）** | 模板详情页完整落地 |
| **05-20（周二）** | 节点详情页上线，集群视角补齐 |
| **05-22（周四）** | 模板创建流程完整，核心管理功能闭环 |
| **05-26（周一）** | 全局体验打磨完成 |

### 优先级说明

- **P0**：Bug 修复、沙箱创建、模板详情
- **P1**：节点详情、模板创建、Toast 错误处理
- **P2**：沙箱快照、ConfirmDialog 统一、metadata 过滤、Overview 趋势

