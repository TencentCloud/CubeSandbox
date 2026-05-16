---
title: LlamaIndex 集成指南
author: Johnny-zbb
date: 2026-05-16
tags:
  - integration
  - llamaindex
lang: zh-CN
---

# LlamaIndex 集成指南

## 集成对象与版本

[LlamaIndex](https://www.llamaindex.ai/) 是一个数据框架，用于基于自定义知识库构建 LLM 应用。本指南介绍如何将 LlamaIndex 与 Cube Sandbox 集成 — 以安全隔离的代码执行环境驱动 RAG（检索增强生成）工作流。

- **已验证 LlamaIndex 版本**：`>= 0.10.0`
- **已验证 Python 版本**：`3.9+`
- **集成类型**：代码执行工具（Tool）

## 前置条件

- Cube Sandbox 已部署（参考 [快速入门](https://github.com/TencentCloud/CubeSandbox)）
- Python `3.9+`，已装 `pip`
- 安装依赖：

```bash
pip install llama-index llama-index-agent-openai
```

- 配置环境变量：

```bash
export CUBE_API_URL=http://<your-cubeapi-host>:3000
export CUBE_TEMPLATE_ID=<your-template-id>
export CUBE_PROXY_NODE_IP=<your-cubeproxy-node-ip>   # 远程访问时需要
```

## 接入步骤

### 1. 安装依赖

```bash
pip install llama-index llama-index-agent-openai
```

### 2. 为 LlamaIndex 创建 Cube Sandbox 工具

核心思路：用 Cube Sandbox 替换 `SimpleDirectoryReader` 或本地 Python 执行，可以获得 **MicroVM 级隔离** — 不共享宿主机进程，Agent 之间互不干扰。

```python
# tools/cube_tool.py
import json
from llama_index.core.tools import FunctionTool
from cubesandbox import Sandbox, Config


def create_cube_tool(
    template_id: str,
    api_url: str = "http://127.0.0.1:3000",
    proxy_node_ip: str = "",
    timeout: int = 60,
) -> FunctionTool:
    """
    创建一个由 Cube Sandbox 驱动的 LlamaIndex FunctionTool。

    Args:
        template_id: Cube 模板 ID（如 python:3.12-slim）
        api_url: CubeAPI 地址
        proxy_node_ip: CubeProxy 节点 IP（远程访问时填写）
        timeout: 最大执行时间（秒）
    """

    def _run_code(code: str) -> str:
        cfg = Config(
            api_url=api_url,
            template_id=template_id,
            proxy_node_ip=proxy_node_ip,
        )
        with Sandbox.create(config=cfg) as sb:
            result = sb.run_code(code, timeout=timeout)
            if result.error:
                return f"Error: {result.error.name}: {result.error.value}"
            return result.text or ""

    return FunctionTool.from_defaults(
        fn=_run_code,
        name="cube_sandbox",
        description=(
            "在隔离的 Cube Sandbox MicroVM 中执行 Python 代码。"
            "适用于运行不受信任的代码、RAG 数据处理，或任何需要强隔离的操作。"
            "输入：待执行的单个 Python 代码字符串。"
            "输出：最后一个表达式的结果或标准输出/标准错误。"
        ),
    )
```

### 3. 与 LlamaIndex Agent 配合使用

```python
# main.py
from llama_index.core.agent import ReActAgent
from llama_index.llms.openai import OpenAI
from tools.cube_tool import create_cube_tool

llm = OpenAI(model="gpt-4o")

# 注册 Cube 工具作为代码执行后端
agent = ReActAgent.from_tools(
    tools=[create_cube_tool(
        template_id="python:3.12-slim",
        api_url="http://localhost:3000",
    )],
    llm=llm,
    verbose=True,
)

response = agent.chat(
    "下载维基百科上关于 RAG 的词条，"
    "用 BeautifulSoup 解析它，然后统计词频。"
)
print(response)
```

## 关键代码片段

### 原始做法（本地执行，无隔离）

```python
# 危险：代码直接在宿主机运行
exec("""
import requests
from bs4 import BeautifulSoup

url = "https://en.wikipedia.org/wiki/Retrieval-Augmented-Generation"
html = requests.get(url).text
soup = BeautifulSoup(html, "html.parser")
# ... 完全的宿主机访问权限，无任何沙箱保护
""")
```

### 改造后（Cube Sandbox，MicroVM 隔离）

```python
# 相同逻辑，但在隔离的 MicroVM 中执行
from cubesandbox import Sandbox, Config

cfg = Config(
    api_url="http://localhost:3000",
    template_id="python:3.12-slim",
)
with Sandbox.create(config=cfg) as sb:
    result = sb.run_code("""
import urllib.request
from html.parser import HTMLParser

class WordFreq(HTMLParser):
    def __init__(self):
        super().__init__()
        self.words = []
        self.skip = {'the','a','an','is','are','was','were','in','on','at','to','of','for'}
    def handle_data(self, data):
        for w in data.lower().split():
            w = w.strip('.,!?;:\"()[]{}')
            if w and w not in self.skip and len(w) > 3:
                self.words.append(w)

url = "https://en.wikipedia.org/wiki/Retrieval-Augmented-Generation"
req = urllib.request.Request(url, headers={'User-Agent': 'Mozilla/5.0'})
html = urllib.request.urlopen(req).read().decode()
p = WordFreq(); p.feed(html)
from collections import Counter
print(Counter(p.words).most_common(10))
    """, timeout=30)
    print(result.text)
```

### 改动对比

```diff
- exec("""...""")     # 直接在宿主机运行，无隔离
+ with Sandbox.create(config=cfg) as sb:
+     sb.run_code("""...")   # 在 MicroVM 中运行，完全隔离
```

**改造幅度极小**：只需把现有代码包裹在 `sb.run_code("""...")` 中，即可完成迁移。

## 进阶配置

### 网络隔离（处理不可信数据源）

LlamaIndex 常需从外部 URL 获取数据，Cube Sandbox 支持细粒度网络策略：

```python
# 完全禁止出站流量 — 避免数据泄露风险
with Sandbox.create(
    metadata={"network-policy": "deny-all"}
) as sb:
    # 只能读取通过 hostdir-mount 挂载的本地文件
    result = sb.run_code("open('/mnt/data/input.txt').read()")

# 仅允许特定 IP 范围
rules = json.dumps({"allow": ["151.101.0.0/16"]})  # GitHub IP 段
with Sandbox.create(
    metadata={"network-policy": "custom", "network-rules": rules}
) as sb:
    result = sb.run_code("""
import urllib.request
urllib.request.urlopen('https://api.github.com').read()
    """)
```

### 多轮 RAG 的持久化沙箱

在多次 `run_code` 调用间保持同一个沙箱，实现有状态的 RAG 流水线：

```python
sb = Sandbox.create(config=cfg)

# 在沙箱中构建索引
sb.run_code("""
from llama_index.core import SimpleDirectoryReader
documents = SimpleDirectoryReader('./data').load_data()
print(f"加载了 {len(documents)} 个文档")
""")

# 在同一沙箱中查询（变量持久化）
sb.run_code("""
from llama_index.core import VectorStoreIndex
index = VectorStoreIndex.from_documents(documents)
print("索引构建完成")
""")

sb.kill()  # 清理
```

### 暂停与恢复控制成本

```python
sb = Sandbox.create()

# 空闲时暂停 — 内存快照，不计费
sb.pause()  # 等待状态变为 paused

# 需要时恢复
sb2 = Sandbox.connect(sb.sandbox_id)
result = sb2.run_code("print('已恢复')")
```

## 注意事项

- **冷启动延迟**：Cube Sandbox 在首次 `Sandbox.create()` 时会启动一个 MicroVM，耗时约 1–2 秒。对延迟敏感的交互场景，建议通过 `pause()`/`connect()` 保持沙箱热状态，避免反复创建新实例。
- **目前仅支持 Python SDK**：Cube Sandbox Python SDK 是与 LlamaIndex 集成的推荐路径。其他语言 SDK（Go 等）暂未纳入本指南范围。
- **暂不支持内核上下文持久化**：不同 `Sandbox.create()` 调用之间变量不共享。如需有状态流水线，请使用同一沙箱实例；或通过 hostdir-mount 以文件形式共享状态。
- **网络策略仅支持 IP/CIDR**：自定义 `allow` 规则暂不支持域名。请提前规划外部 API 的 IP 范围。

## 参考资料

- [Cube Sandbox Python SDK](https://github.com/TencentCloud/CubeSandbox/tree/main/sdk/python)
- [LlamaIndex 官方文档](https://docs.llamaindex.ai/)
- [Cube Sandbox GitHub 仓库](https://github.com/TencentCloud/CubeSandbox)
- [Cube Sandbox 快速入门](https://github.com/TencentCloud/CubeSandbox/blob/main/README.md)
