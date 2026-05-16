---
title: LlamaIndex Integration Guide
author: Johnny-zbb
date: 2026-05-16
tags:
  - integration
  - llamaindex
lang: en-US
---

# LlamaIndex Integration Guide

## Integration Target and Version

[LlamaIndex](https://www.llamaindex.ai/) is a data framework for building LLM applications with custom knowledge bases. This guide covers integrating LlamaIndex with Cube Sandbox as a secure, isolated code execution environment for RAG (Retrieval-Augmented Generation) workflows.

- **Tested LlamaIndex versions**: `>= 0.10.0`
- **Tested Python versions**: `3.9+`
- **Integration type**: Code execution tool (Tool)

## Prerequisites

- Cube Sandbox deployment (see [Quick Start](https://github.com/TencentCloud/CubeSandbox))
- Python `3.9+` with `pip`
- LlamaIndex installed: `pip install llama-index`
- A Cube Sandbox template. If you don't have one:
  1. Build from base image with `envd` support, e.g.:

     ```dockerfile
     FROM cubesandbox-base:latest
     # Or: FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16
     ```

  2. Create a template via CLI:
     ```bash
     cubemastercli tpl create-from-image --image <your-image-tag>
     # Output includes: template_id="tpl-xxxxxxxxxxxx"
     ```
  3. Use the returned `template_id` (format: `tpl-<hex>`) in your code.

- Environment variables configured:

```bash
export CUBE_API_URL=http://<your-cubeapi-host>:3000
export CUBE_TEMPLATE_ID=<your-template-id>   # e.g. tpl-748094d2f2374b0a8a37e6ec
export CUBE_PROXY_NODE_IP=<your-cubeproxy-node-ip>   # for remote access
```

## Integration Steps

### 1. Install dependencies

```bash
pip install llama-index llama-index-agent-openai
```

### 2. Create a Cube Sandbox tool for LlamaIndex

The key insight: replacing `SimpleDirectoryReader` or local Python execution with Cube Sandbox gives you **MicroVM-level isolation** — no shared host process, no interference between agents.

```python
# tools/cube_tool.py
from llama_index.core.tools import FunctionTool
from cubesandbox import Sandbox, Config


def create_cube_tool(
    template_id: str,
    api_url: str = "http://127.0.0.1:3000",
    proxy_node_ip: str = "",
    timeout: int = 60,
) -> FunctionTool:
    """
    Create a LlamaIndex FunctionTool backed by a Cube Sandbox.

    Args:
        template_id: Cube template ID (e.g. tpl-748094d2f2374b0a8a37e6ec)
        api_url: CubeAPI address
        proxy_node_ip: CubeProxy node IP for remote access
        timeout: max execution time in seconds
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
            "Executes Python code inside an isolated Cube Sandbox MicroVM. "
            "Use this for untrusted code, RAG data processing, or any operation "
            "that needs strong isolation. "
            "Input: a single Python code string to execute. "
            "Output: the result of the last expression or stdout/stderr."
        ),
    )
```

### 3. Use with a LlamaIndex agent

```python
# main.py
from llama_index.core.agent import ReActAgent
from llama_index.llms.openai import OpenAI
from tools.cube_tool import create_cube_tool

llm = OpenAI(model="gpt-4o")

# Register Cube tool as a code execution backend
agent = ReActAgent.from_tools(
    tools=[create_cube_tool(
        template_id="<your-template-id>",  # e.g. tpl-748094d2f2374b0a8a37e6ec
        api_url="http://localhost:3000",
    )],
    llm=llm,
    verbose=True,
)

response = agent.chat(
    "Download the Wikipedia page about RAG, "
    "parse it with BeautifulSoup, and compute word frequency stats."
)
print(response)
```

## Key Code Snippets

### Before (local execution, no isolation)

```python
# Dangerous: code runs directly on the host
exec("""
import requests
from bs4 import BeautifulSoup

url = "https://en.wikipedia.org/wiki/Retrieval-Augmented-Generation"
html = requests.get(url).text
soup = BeautifulSoup(html, "html.parser")
# ... full host access, no sandboxing
""")
```

### After (Cube Sandbox, MicroVM isolation)

```python
# Same logic, but inside an isolated MicroVM
from cubesandbox import Sandbox, Config

cfg = Config(
    api_url="http://localhost:3000",
    template_id="<your-template-id>",  # e.g. tpl-748094d2f2374b0a8a37e6ec
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

### Diff summary

```diff
- exec("""...""")     # runs on host, no isolation
+ with Sandbox.create(config=cfg) as sb:
+     sb.run_code("""...")   # runs in MicroVM, fully isolated
```

**The refactor is minimal**: wrap your existing code in `sb.run_code("""...")`, and you're done.

## Going Further

### Network isolation for untrusted data sources

LlamaIndex often fetches data from external URLs. Cube Sandbox supports network policies via the SDK's native `network` parameter:

```python
from cubesandbox import Sandbox, Config

cfg = Config(
    api_url="http://localhost:3000",
    template_id="<your-template-id>",
)

# Deny all outbound — no data exfiltration risk
with Sandbox.create(config=cfg, allow_internet_access=False) as sb:
    # can still read local files mounted via hostdir-mount
    result = sb.run_code("open('/mnt/data/input.txt').read()")

# Allow specific IP ranges only (CIDR notation)
with Sandbox.create(
    config=cfg,
    allow_internet_access=False,
    network={"allow_out": ["151.101.0.0/16"]},  # GitHub IP range
) as sb:
    result = sb.run_code("""
import urllib.request
urllib.request.urlopen('https://api.github.com').read()
    """)
```

### Persistent sandbox for multi-turn RAG

Keep a sandbox alive across multiple `run_code` calls for stateful RAG pipelines:

```python
sb = Sandbox.create(config=cfg)

# Build index in sandbox
sb.run_code("""
from llama_index.core import SimpleDirectoryReader
documents = SimpleDirectoryReader('./data').load_data()
print(f"Loaded {len(documents)} documents")
""")

# Query in same sandbox (variables persist)
sb.run_code("""
from llama_index.core import VectorStoreIndex
index = VectorStoreIndex.from_documents(documents)
print("Index built")
""")

sb.kill()  # clean up
```

### Pause & resume for cost control

```python
sb = Sandbox.create()

# Pause when idle — memory snapshot, no billing
sb.pause()  # waits for state=paused

# Resume when needed
sb2 = Sandbox.connect(sb.sandbox_id)
result = sb2.run_code("print('resumed')")
```

## Caveats

- **Cold start latency**: Cube Sandbox spawns a MicroVM on first `Sandbox.create()`, which takes ~1–2s. For latency-sensitive interactive use, consider keeping a sandbox warm via `pause()`/`connect()` instead of repeatedly creating new ones.
- **Python SDK only for now**: The Cube Sandbox Python SDK is the recommended integration path for LlamaIndex. Other SDKs (Go, etc.) are not yet covered in this guide.
- **No kernel context persistence yet**: Variables do not persist across separate `Sandbox.create()` calls. Use a single sandbox instance for stateful pipelines, or mount a host directory for file-based state sharing.
- **Network policy domain limitation**: Custom `allow` rules only support IP ranges/CIDRs, not domain names. Plan accordingly for external API access.

## References

- [Cube Sandbox Python SDK](https://github.com/TencentCloud/CubeSandbox/tree/main/sdk/python)
- [LlamaIndex Documentation](https://docs.llamaindex.ai/)
- [Cube Sandbox GitHub](https://github.com/TencentCloud/CubeSandbox)
- [Cube Sandbox Quick Start Guide](https://github.com/TencentCloud/CubeSandbox/blob/main/README.md)
