# 浏览器沙箱（Playwright）

[English](README.md)

在 Cube Sandbox 中运行无头 Chromium 浏览器，通过 Chrome DevTools Protocol (CDP) 使用 [Playwright](https://playwright.dev/) 远程控制。

## 1. 背景

**Cube Sandbox** 是轻量级 MicroVM 平台，控制面和数据面完全兼容 [E2B SDK](https://e2b.dev)。浏览器沙箱镜像在启动时会以 **远程调试模式** 在端口 `9000` 启动 Chromium。CubeProxy 将 CDP WebSocket 端点通过标准 `<port>-<sandbox_id>.<domain>` URL 方案路由出来，Playwright 无需任何自定义网络即可从任意机器接入。

```
你的脚本
    │  Playwright CDP (WebSocket)
    ▼
CubeProxy ── https://<sandbox_id>-9000.<domain>/cdp?
    │
    ▼
沙箱 VM（Chromium，端口 9000）
```

## 2. 使用场景

- 在隔离的一次性环境中进行网页爬取
- 对任意网站进行自动化 UI 测试
- 截图 / PDF 生成服务
- LLM Agent 浏览任务（每次 Agent 运行获得全新的浏览器 VM）

## 3. 架构

```
┌──────────────────────┐         ┌─────── Cube Sandbox ──────────────┐
│                      │         │                                    │
│  你的脚本             │  CDP WS │  ┌───────────────────────────┐    │
│  (Playwright Python) │────────►│  │  Chromium --remote-debug  │    │
│                      │  HTTPS  │  │  端口 9000                │    │
│                      │         │  └───────────────────────────┘    │
└──────────────────────┘         │                                    │
                                 │  CubeProxy（TLS 终结）              │
                                 └────────────────────────────────────┘
```

| 组件 | 说明 |
|------|------|
| **Cube Sandbox** | 从浏览器模板启动的 KVM MicroVM |
| **Chromium** | 预装，启动时带 `--remote-debugging-port=9000` |
| **CubeProxy** | 将 `<port>-<sandbox_id>.<domain>` 路由到对应 VM 端口 |
| **Playwright** | 通过 CDP 接入，宿主机无需安装浏览器 |

## 4. 前置条件

- 已部署的 Cube Sandbox 环境
- Python 3.8+

```bash
pip install -r requirements.txt
playwright install chromium
```

## 5. 快速开始

### 第一步 — 创建浏览器模板

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest \
  --writable-layer-size 1G \
  --expose-port 9000 \
  --probe 9000 \
  --probe-path /cdp/json/version
```

> **镜像仓库说明：** 国内优先使用 `cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest`；境外访问推荐使用 `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest`。

记录输出的 `template_id`。

### 第二步 — 配置环境变量

```bash
cp .env.example .env
# 编辑 .env，填写 E2B_API_URL 和 CUBE_TEMPLATE_ID
```

或直接导出：

```bash
export E2B_API_KEY=dummy
export E2B_API_URL=http://<节点IP>:3000
export CUBE_TEMPLATE_ID=<template-id>

# 使用 Cube 内置 mkcert 证书时才需要配置：
# export NODE_EXTRA_CA_CERTS=/root/.local/share/mkcert/rootCA.pem
```

### 第三步 — 运行示例

```bash
python browser.py
```

预期输出：

```
SandboxInfo(sandbox_id='...', template_id='...', ...)
腾讯网
```

## 6. 脚本工作原理

```python
sandbox = Sandbox.create(template=template_id)
cdp_url = f"https://{sandbox.get_host(9000)}/cdp?"

with sync_playwright() as playwright:
    browser = playwright.chromium.connect_over_cdp(cdp_url)
    ...
```

| 步骤 | 代码 | 说明 |
|------|------|------|
| 1 | `Sandbox.create(template=...)` | 从浏览器模板启动新的 MicroVM |
| 2 | `sandbox.get_host(9000)` | 解析该沙箱端口 9000 的 CubeProxy URL |
| 3 | `connect_over_cdp(cdp_url)` | Playwright 接入已运行的 Chromium 进程 |
| 4 | `page.goto(...)` | 完整 Playwright API：导航、点击、截图、抓取等 |

## 7. 进阶用法

```python
# 截图
page.screenshot(path="screenshot.png")

# 执行 JavaScript
title = page.evaluate("document.title")

# 等待元素
page.wait_for_selector("#main-content")

# 填表并提交
page.fill('input[name="q"]', "cube sandbox")
page.press('input[name="q"]', "Enter")
page.wait_for_load_state("networkidle")
```

完整 API 参考 [Playwright Python 文档](https://playwright.dev/python/docs/api/class-page)。

## 8. 常见问题

| 现象 | 可能原因 | 解决方法 |
|------|---------|---------|
| `Error: connect ECONNREFUSED` | CubeAPI 不可达 | 检查 `E2B_API_URL` 及端口 3000 是否开放 |
| `SSL: CERTIFICATE_VERIFY_FAILED` | HTTPS 但未配置 CA 证书 | 设置 `NODE_EXTRA_CA_CERTS=/root/.local/share/mkcert/rootCA.pem` |
| `Timeout waiting for CDP` | Chromium 尚未就绪 | 浏览器镜像启动时会拉起 Chromium，稍后重试或增大超时 |
| `Template not found` | 模板 ID 错误 | 重新运行 `cubemastercli tpl list` 确认 ID |

## 9. 目录结构

```
browser-sandbox/
├── README.md           # 英文文档
├── README_zh.md        # 中文文档（本文件）
├── browser.py          # 示例脚本
├── requirements.txt    # Python 依赖
└── .env.example        # 环境变量模板
```
