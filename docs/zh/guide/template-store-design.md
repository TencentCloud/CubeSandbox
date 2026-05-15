# 模板市场（Template Store）设计方案

## 背景

当前用户制作模板需要手动执行 `cubemastercli tpl create-from-image`，需要了解镜像地址、probe 参数等细节，门槛较高。模板市场提供类似 App Store 的体验，让用户**一键从预置镜像创建可用模板**。

---

## 功能模块

### 1. 市场页面

卡片展示可用镜像，支持分类筛选和搜索：

- 分类：全部 / AI·LLM / 代码执行 / 浏览器 / 数据科学
- 卡片信息：镜像名、描述、预装内容标签、镜像大小、来源（官方/社区）、已安装状态
- 操作按钮：一键安装 / 已安装（展示已有模板）/ 有新版本（可更新）

### 2. 一键安装流程

```
用户点击安装
    ↓
弹出配置面板（可选）
  · 模板名称（默认用镜像名）
  · writable-layer-size（默认 2G）
  · 资源规格 CPU/内存（默认 2C2G）
    ↓
后台调用 tpl create-from-image（probe 参数从市场配置自动填入）
    ↓
实时展示进度（PULLING → BUILDING → READY）
    ↓
完成后可直接「创建沙箱」
```

### 3. 已安装状态联动

安装完成后：
- 市场页标记"已安装"
- 新建沙箱页的模板下拉列表中直接出现该模板

---

## 镜像配置结构

镜像列表由静态 JSON 驱动，前期打包进前端，无需后端：

```json
{
  "templates": [
    {
      "id": "sandbox-code",
      "name": "代码执行沙箱",
      "description": "官方代码执行环境，预装 Python3 + Jupyter Kernel",
      "image_cn": "cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest",
      "image_intl": "cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest",
      "digest": "sha256:a7b8654aac5b90e241b98e195ae1d8c85d59fe1fb8c282bcccf1071f877db20f",
      "tags": ["Python", "Jupyter", "官方"],
      "category": "code",
      "size_mb": 290,
      "expose_ports": [49983, 49999],
      "probe_port": 49999,
      "probe_path": "/",
      "writable_layer_size": "1G",
      "official": true
    },
    {
      "id": "sandbox-browser",
      "name": "浏览器沙箱",
      "description": "预装 Chromium，支持网页自动化和截图",
      "image_cn": "cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest",
      "image_intl": "cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest",
      "digest": "sha256:1786786af8510c34eda64ebec5b0a61a98583cb311c3045c0222910ec0680d60",
      "tags": ["浏览器", "Chromium", "官方"],
      "category": "browser",
      "size_mb": 380,
      "expose_ports": [49983],
      "probe_port": 49983,
      "probe_path": "/health",
      "writable_layer_size": "1G",
      "official": true
    },
    {
      "id": "openclaw-aio",
      "name": "OpenClaw All-in-One",
      "description": "集成 OpenClaw AI 助手的完整沙箱环境",
      "image_cn": "...",
      "image_intl": "...",
      "digest": "sha256:...",
      "tags": ["AI", "OpenClaw", "全功能"],
      "category": "ai",
      "size_mb": 2100,
      "expose_ports": [49983, 49999, 3000],
      "probe_port": 49983,
      "probe_path": "/health",
      "writable_layer_size": "4G",
      "official": true
    }
  ]
}
```

> 镜像地址分 `image_cn`（内网/国内）和 `image_intl`（境外）两个字段，由前端根据部署环境选择。

---

## 已安装状态判断逻辑

同一个镜像可能被用不同参数安装多次，不能用 `template_id` 判断，用 **image digest** 匹配：

### 匹配优先级

```
1. digest 完全匹配  →  已安装（精确）
2. 镜像名匹配但无 digest  →  已安装（模糊，提示可能版本不同）
3. 无匹配  →  未安装
```

### 多个匹配时的展示

同一镜像安装了多个模板（参数不同），卡片展开列出所有已安装版本：

```
✅ 已安装 2 个
  tpl-1529...  2C2G  1G层   2026-05-13  [创建沙箱]
  tpl-0e44...  4C4G  2G层   2026-05-12  [创建沙箱]
                              [用新参数再安装一个]
```

### 镜像版本更新

市场配置中 `digest` 字段在发布时固定，安装时使用 `image@digest` 精确拉取。当市场配置更新了 digest，对比本地已安装模板的 digest，不同则提示"有新版本可更新"。

---

## 实现路径

### Phase 1 · MVP（约 2 周）

- [ ] 市场页面 UI + 卡片展示（静态 JSON 驱动）
- [ ] 一键安装：调用现有 `tpl create-from-image` API，probe 参数自动填入
- [ ] 安装进度实时展示（复用 tpl watch WebSocket）

### Phase 2（约 1 个月）

- [ ] 已安装状态检测（digest 匹配逻辑）
- [ ] 新建沙箱页模板下拉关联市场镜像
- [ ] 支持自定义安装参数（规格、层大小）
- [ ] 版本更新提示

### Phase 3（后续迭代）

- [ ] 社区镜像提交 + 审核上架
- [ ] 镜像版本管理（多版本共存）
- [ ] 使用统计、热门排序
- [ ] 管理后台（替代静态 JSON）

---

## 关键决策记录

| 问题 | 决策 |
|------|------|
| 镜像来源 | 统一从公开地址拉取，分 CN / Intl 两个地址 |
| 镜像列表维护 | 前期静态 JSON 打包进前端，后期做管理后台 |
| 已安装判断 | 用 digest 匹配，不用 template_id |
| 同镜像多次安装 | 全部展示，用户自选；支持用新参数再装一个 |
| latest tag 滚动 | 配置中固定 digest，安装时用 image@digest 精确拉取 |
