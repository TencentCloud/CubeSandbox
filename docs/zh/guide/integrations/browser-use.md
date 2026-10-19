---
title: browser-use 集成指南
author: tunsuy
date: 2026-10-19
tags:
  - integration
  - browser-use
  - agent
  - browser-automation
lang: zh-CN
---

# browser-use 集成指南

[English](../../../guide/integrations/browser-use.md)

在宿主机上运行 [browser-use](https://github.com/browser-use/browser-use) Agent，而它驱动的浏览器
——一个真实的 Chromium——运行在 [CubeSandbox](https://github.com/TencentCloud/CubeSandbox) 的
MicroVM 里。Agent 主循环与 LLM 凭证都留在本地；Agent 访问的每一个网页、执行的每一段脚本、下载的
每一个文件都被限制在一次性的 KVM 虚拟机内。该集成复用了仓库
[`examples/browser-sandbox`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/browser-sandbox)
已经验证过的 CDP over CubeProxy 模式，无需 fork 或修改 browser-use。

```
宿主机                                    Cube Sandbox（MicroVM）
┌──────────────────────────┐             ┌────────────────────────────┐
│ browser-use Agent        │   CDP over  │ Chromium                   │
│  - LLM 调用（密钥在本地）│  WebSocket  │  --remote-debugging-port   │
│  - DOM/视觉处理          │────────────►│  :9000                     │
└──────────────────────────┘  CubeProxy  └────────────────────────────┘
```

## 集成对象与版本

| 组件 | 本文参考的版本 |
| --- | --- |
| browser-use | `>= 0.3`（`Agent` + `BrowserSession` + `browser_use.ChatOpenAI`）；同时给出旧版 `0.1.x`（`Browser` + `BrowserConfig`）的写法 |
| Python | 3.11+（browser-use 的要求） |
| cubesandbox SDK | `>= 0.6.0` |
| 浏览器模板镜像 | `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest`（Chromium 在 `9000` 端口提供 CDP） |
| CubeSandbox 平台 | `>= 0.3.0` |

browser-use 迭代很快，各大版本之间的连接 API 有差异；请固定你验证过的版本，升级前先阅读
[注意事项](#注意事项)。

## 前置条件

- 已运行的 [CubeSandbox 部署](/zh/guide/quickstart)，CubeAPI 可访问，通常为
  `http://<cube-host>:3000`。
- `cubemastercli` 在 `$PATH` 中且已连接集群。
- 运行 browser-use Agent 的机器上有 Python 3.11+。**本机不需要安装 Chromium** —— 浏览器运行在
  沙箱里。
- 沙箱域名（默认 `*.cube.app`）必须能在**运行 browser-use 的机器**上解析到 CubeProxy。CDP 连接由
  browser-use 自己发起，而不是经过 `cubesandbox` SDK，因此 `CUBE_PROXY_NODE_IP` 对它不生效——请按
  [HTTPS 与域名配置](/zh/guide/https-and-domain)配置泛域名 DNS（或 dnsmasq）。
- 一个 OpenAI 兼容的 LLM 端点和 API Key。browser-use 会把页面截图发给模型，请选择支持
  **视觉输入**的强模型。

## 为什么把浏览器放进沙箱

Web Agent 天然要消费不可信内容：任意页面、任意 JavaScript、任意文件。把浏览器跑在宿主机上，等于
把 Agent 的爆炸半径和你的机器混在一起。放进 CubeSandbox 之后：

| 关注点 | CubeSandbox 提供什么 |
| --- | --- |
| **不可信网页内容** | 页面、脚本、下载都在带独立 guest 内核的一次性 KVM MicroVM 中执行，碰不到宿主机 |
| **提示词注入** | 即使恶意页面诱导 Agent 执行危险点击或下载，破坏也被限制在 VM 内并随 VM 一起销毁 |
| **每个任务全新状态** | 每次运行一个新 MicroVM：任务之间没有 Cookie、缓存或会话泄漏；冷启动低于 60ms |
| **密钥安全** | LLM 密钥留在宿主机 Agent 进程里，浏览网页的 VM 永远看不到它 |
| **长任务** | `sandbox.pause()` 可对 VM（含浏览器状态）做快照，之后恢复 |
| **出口管控** | CubeEgress 可以精确限定浏览器允许访问的域名，并留下审计日志 |

## 接入步骤

### 1. 创建浏览器模板

官方浏览器镜像已内置以远程调试模式启动的 Chromium（端口 `9000`），CDP 端点挂在 `/cdp` 路径前缀下
（就绪探针访问 `/cdp/json/version`）：

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest \
  --alias browser-use \
  --writable-layer-size 1G \
  --expose-port 9000 \
  --probe 9000 \
  --probe-path /cdp/json/version \
  --allow-internet-access
```

说明：

- 中国大陆环境请改用
  `cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest`。
- `--allow-internet-access` 让 VM 内的浏览器能访问公网。在加固集群上应去掉该参数，改为定义放行
  规则（见[网络策略](/zh/guide/network-policy)与[安全代理](/zh/guide/security-proxy)）。如果节点
  需要显式指定解析器，追加 `--dns <dns1> --dns <dns2>`。
- `create-from-image` 默认阻塞并跟踪构建任务；也可加 `--detach` 提交后用
  `cubemastercli tpl watch --job-id <job_id>` 轮询。任务到达 `READY` 后记下 `template_id`
  （加了 `--alias browser-use` 之后，`Sandbox.create` 也可以直接传别名）。
- 页面较重时建议提高资源配额——如果 Agent 要访问富媒体站点，可调大 `--cpu` / `--memory`
  （默认 2 核、2000 MB）。

任何以 `--remote-debugging-port` 启动 Chromium 的自定义镜像同样可用；参见
[自定义镜像](../tutorials/bring-your-own-image.md)与
[从镜像创建模板](../tutorials/template-from-image.md)。

### 2. 安装宿主机依赖

```bash
python3 -m venv .venv
source .venv/bin/activate
pip install "browser-use>=0.3" "cubesandbox>=0.6.0" python-dotenv
```

### 3. 配置环境变量

```bash
cat > .env <<'EOF'
CUBE_API_URL=http://<cube-host>:3000
CUBE_TEMPLATE_ID=browser-use
OPENAI_API_KEY=<your-llm-key>
OPENAI_BASE_URL=https://tokenhub.tencentmaas.com/v1
CHAT_MODEL=deepseek-v3
EOF
```

| 变量 | 作用对象 | 说明 |
| --- | --- | --- |
| `CUBE_API_URL` | `cubesandbox` SDK | CubeAPI 地址（`http://<node>:3000`） |
| `CUBE_API_KEY` | `cubesandbox` SDK | （可选）仅在 CubeAPI 开启鉴权时设置 |
| `CUBE_TEMPLATE_ID` | `Sandbox.create(template=...)` | 第 1 步得到的模板 ID 或别名 |
| `OPENAI_API_KEY` / `TOKENHUB_API_KEY` | LLM 客户端 | OpenAI 兼容密钥；只留在宿主机 |
| `OPENAI_BASE_URL` | LLM 客户端 | 例如 `https://tokenhub.tencentmaas.com/v1` |
| `CHAT_MODEL` | LLM 客户端 | 使用支持视觉的模型，例如 `deepseek-v3` |
| `SSL_CERT_FILE` | Python TLS | CubeProxy 使用自签名/mkcert 证书时的 CA bundle |
| `NODE_EXTRA_CA_CERTS` | Playwright 驱动 | 仅旧版 browser-use `0.1.x` 路径需要 |

### 4. 把 browser-use 接到沙箱浏览器

每次 Agent 运行创建一个 MicroVM，把它的 CDP 端点交给 `BrowserSession`：

```python
# browser_use_cube_demo.py
import asyncio
import os

from browser_use import Agent, BrowserSession, ChatOpenAI
from dotenv import load_dotenv

from cubesandbox import Sandbox

load_dotenv()


async def main() -> None:
    # with 语句退出时会调用 sandbox.kill()，不会泄漏 VM。
    with Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"], timeout=900) as sandbox:
        # VM 内的 Chromium 在 9000 端口提供 CDP；CubeProxy 将其发布为
        # https://9000-<sandbox-id>.cube.app，端点位于 /cdp 路径下。
        cdp_url = f"https://{sandbox.get_host(9000)}/cdp"

        agent = Agent(
            task=(
                "Go to https://news.ycombinator.com and return the title of "
                "the current top post."
            ),
            llm=ChatOpenAI(
                model=os.getenv("CHAT_MODEL", "deepseek-v3"),
                base_url=os.getenv("OPENAI_BASE_URL",
                                   "https://tokenhub.tencentmaas.com/v1"),
                api_key=os.getenv("OPENAI_API_KEY") or os.getenv("TOKENHUB_API_KEY"),
            ),
            browser_session=BrowserSession(cdp_url=cdp_url),
        )
        history = await agent.run()
        print("Final result:", history.final_result())


asyncio.run(main())
```

### 5. 运行

```bash
python browser_use_cube_demo.py
```

browser-use 驱动 VM 内 Chromium 时会逐步打印日志（`📍 Step N: ...` 以及将要执行的动作），结束后
脚本打印 Agent 的最终答案——按默认任务即 Hacker News 当前头条的标题（具体值显然取决于线上站点）。
`with` 块退出时沙箱被 kill，浏览器状态随之销毁。

## 关键代码片段

### 旧版 browser-use 0.1.x（基于 Playwright）

旧 API 的连接方式与仓库自带的
[`examples/browser-sandbox/browser.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/browser-sandbox/browser.py)
完全一致——`BrowserConfig(cdp_url=...)` 会被原样传给 Playwright 的 `connect_over_cdp`，URL 末尾
同样带 `?`：

```python
from browser_use import Agent, Browser, BrowserConfig
from langchain_openai import ChatOpenAI

browser = Browser(config=BrowserConfig(
    cdp_url=f"https://{sandbox.get_host(9000)}/cdp?",
))
agent = Agent(
    task="...",
    llm=ChatOpenAI(model="deepseek-v3",
                   base_url="https://tokenhub.tencentmaas.com/v1",
                   api_key=os.environ["OPENAI_API_KEY"]),
    browser=browser,
)
history = await agent.run()
```

### 一个任务一个沙箱（并行 Agent）

由于冷启动低于 60ms，最自然的模式是每个任务开一个全新 MicroVM——多个 Agent 并行运行且互不共享
浏览器状态：

```python
async def run_task(task: str) -> str:
    with Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"], timeout=900) as sandbox:
        agent = Agent(
            task=task,
            llm=llm,
            browser_session=BrowserSession(
                cdp_url=f"https://{sandbox.get_host(9000)}/cdp"),
        )
        history = await agent.run()
        return history.final_result() or ""

results = await asyncio.gather(*(run_task(t) for t in tasks))
```

### 把文件从 VM 里取出来

浏览器下载的内容都留在沙箱内。需要产物时在销毁前取回（或者干脆随 VM 一起销毁——这往往正是目的）。
`files.read` 只支持文本，二进制产物请通过命令配合 `base64` 获取：

```python
import base64

result = sandbox.commands.run("base64 -w0 /tmp/screenshot.png")
data = base64.b64decode(result.stdout)          # 原始 PNG 字节
```

## 注意事项

- **CDP 端点发现方式随 browser-use 版本变化。** `BrowserSession(cdp_url=...)` 需要浏览器的 CDP
  端点；新版本使用纯 Python 的 CDP 客户端而不是 Playwright。如果你的版本无法从 `https://…/cdp`
  解析出 WebSocket 端点，可请求 `https://<host>/cdp/json/version`（与就绪探针相同的 URL），用返回
  的 `webSocketDebuggerUrl` 字段构造 WebSocket URL——把其中的回环地址替换为代理主机——再作为
  `cdp_url` 传入。具体 URL 形态请以
  [`examples/browser-sandbox`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/browser-sandbox)
  为准，它固定了该镜像期望的写法。
- **CDP 这一段需要自行解决 DNS。** `CUBE_PROXY_NODE_IP` 只改写 `cubesandbox` SDK 内部请求的主机
  名。CDP WebSocket 由 browser-use 自己发起，因此宿主机必须能解析 `*.cube.app`（配置泛域名 DNS 或
  dnsmasq 指向 CubeProxy）。
- **自签名代理证书。** 将 CA bundle 导出为 `SSL_CERT_FILE`（新版纯 Python CDP 客户端）或
  `NODE_EXTRA_CA_CERTS`（0.1.x Playwright 路径），WebSocket 握手才能通过 TLS 校验。
- **必须使用视觉模型。** browser-use 每一步都依赖截图理解页面；纯文本模型无论沙箱与否都会失败或
  表现很差。
- **出口网络。** 不加 `--allow-internet-access`（或显式的 CubeEgress 放行规则）时，VM 内的浏览器
  无法访问公网站点；在默认拒绝策略下，每个目标域名都必须加入白名单。
- **两层超时。** 沙箱 `timeout` 回收整个 VM；browser-use 的 `agent.run(max_steps=...)` 限制 Agent
  循环步数。长浏览任务可能触发其中任何一个。
- **资源规格。** Chromium 很吃内存：访问富媒体页面时请调大模板的 `--memory`（与 `--cpu`），否则
  标签页可能在运行中被 guest 的 OOM killer 杀掉。

## 故障排查

| 现象 | 可能原因 | 处理 |
| --- | --- | --- |
| `9000-<id>.cube.app` 报 `Name or service not known` | 宿主机无法解析沙箱域名 | 配置 `*.cube.app` 泛域名 DNS 指向 CubeProxy；`CUBE_PROXY_NODE_IP` 对这条连接无效 |
| `SSL: CERTIFICATE_VERIFY_FAILED` | 代理使用自签名/mkcert 证书 | 通过 `SSL_CERT_FILE` 导出 CA（0.1.x 用 `NODE_EXTRA_CA_CERTS`） |
| WebSocket 握手失败 / 404 | `cdp_url` 形态与 browser-use 版本不匹配 | `curl -k https://<host>/cdp/json/version` 对比你传入的 URL；见注意事项 |
| 创建后立刻连 CDP 超时 | Chromium 尚未就绪 | 就绪探针会保证 `READY` 之后才可用；先重试一次再排查 |
| 页面一直加载不出来、Agent 卡在导航 | VM 没有出口网络 | 重建模板时加 `--allow-internet-access`（必要时加 `--dns`），或添加 CubeEgress 放行规则 |
| `403 Forbidden - CubeEgress` | 默认拒绝出口且目标站点没有规则 | 在网络策略中放行目标域名 |
| Agent 乱点 / 找不到元素 | 模型不支持视觉或能力不足 | 将 `CHAT_MODEL` 换成支持视觉的强模型 |
| 沙箱运行中途被 kill | 沙箱 `timeout` 太短 | 调大 `Sandbox.create(timeout=...)` |

## 参考资料

- 可运行的 CDP 基线示例：[`examples/browser-sandbox`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/browser-sandbox)
- browser-use 上游项目：<https://github.com/browser-use/browser-use>
- browser-use 文档：<https://docs.browser-use.com>
- GUI 桌面替代方案（沙箱内完整 Ubuntu 桌面）：[Ubuntu 桌面沙箱指南](./ubuntu-desktop.md)
- 自定义模板镜像：[`docs/guide/tutorials/bring-your-own-image.md`](../tutorials/bring-your-own-image.md)
- 从镜像创建模板：[`docs/guide/tutorials/template-from-image.md`](../tutorials/template-from-image.md)
- 出口白名单与审计：[`docs/guide/network-policy.md`](../network-policy.md)
- 凭证保管 + 出口控制：[`docs/guide/security-proxy.md`](../security-proxy.md)
