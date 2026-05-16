# LlamaIndex + CubeSandbox Integration Example

## 功能

本示例演示如何将 Cube Sandbox 作为安全的代码执行后端，与 LlamaIndex Agent 集成，用于 RAG（检索增强生成）工作流。

## 特性

- **MicroVM 级隔离**：代码在完全隔离的虚拟机中执行
- **网络安全策略**：可配置网络访问控制，防止数据泄露
- **RAG 数据处理**：安全地处理外部数据源
- **无状态/有状态**：支持单次执行和多轮对话

## 安装

```bash
# 安装依赖
pip install -r requirements.txt
```

## 配置

1. 复制环境变量文件：
   ```bash
   cp .env.example .env
   ```

2. 编辑 `.env` 文件，填入以下配置：
   ```env
   # Cube Sandbox API 地址
   E2B_API_URL=http://127.0.0.1:3000

   # Cube Sandbox 模板 ID（创建模板后获取）
   CUBE_TEMPLATE_ID=<your-template-id>

   # 用于 Agent 的 LLM API Key
   OPENAI_API_KEY=sk-...
   ```

## 使用

### 基本用法

```python
from llamaindex_integration import create_cube_tool, create_rag_agent

# 创建 Cube Sandbox 工具
tool = create_cube_tool(
    template_id="tpl-xxxx",
    api_url="http://127.0.0.1:3000",
)

# 创建带代码执行能力的 Agent
agent = create_rag_agent(
    template_id="tpl-xxxx",
    api_url="http://127.0.0.1:3000",
)

# 使用 Agent 处理任务
response = agent.chat("下载网页并计算词频")
```

### 网络隔离示例

```python
from cubesandbox import Sandbox, Config

cfg = Config(api_url="http://127.0.0.1:3000", template_id="tpl-xxxx")

# 完全禁用网络访问
with Sandbox.create(config=cfg, allow_internet_access=False) as sb:
    result = sb.run_code("print('Hello from isolated sandbox!')")

# IP 白名单
with Sandbox.create(
    config=cfg,
    allow_internet_access=False,
    network={"allow_out": ["151.101.0.0/16"]},
) as sb:
    result = sb.run_code("import urllib.request; ...")
```

## 运行示例

```bash
# 运行完整演示
python llamaindex_integration.py
```

## 环境设置

如需在本地运行 Cube Sandbox，请参考以下步骤：

### 方式一：开发环境（推荐）

```bash
# 克隆仓库
git clone https://github.com/TencentCloud/CubeSandbox.git

# 进入开发环境目录
cd CubeSandbox/dev-env

# 准备虚拟机镜像（仅首次）
./prepare_image.sh

# 启动虚拟机
./run_vm.sh

# 新开终端，登录虚拟机
cd CubeSandbox/dev-env && ./login.sh

# 在虚拟机内安装 Cube Sandbox
curl -sL https://cnb.cool/CubeSandbox/CubeSandbox/-/git/raw/master/deploy/one-click/online-install.sh | MIRROR=cn bash

# 创建代码解释器模板
cubemastercli tpl create-from-image \
  --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999

# 监控模板构建进度
cubemastercli tpl watch --job-id <job_id>

# 记录输出的 template_id
```

### 方式二：直接安装（Linux 服务器）

```bash
# 在 Linux 服务器上执行
curl -sL https://cnb.cool/CubeSandbox/CubeSandbox/-/git/raw/master/deploy/one-click/online-install.sh | MIRROR=cn bash
```

## 架构图

```
                             用户脚本 (LlamaIndex + SDK)
                                      |
                                      ▼
        ┌─────────────────────────────┴─────────────────────────────┐
        │                                                           │
 【1. 管理流程 Control Plane】                          【2. 调用流程 Data Plane】
  (如 Sandbox.create / delete)                      (如 run_code, commands.run)
        │                                                           │
        ▼  REST API (端口 3000)                                     ▼  WSS / HTTP
     CubeAPI                                                     CubeProxy
        │                                                           │
        ▼                                                           │
    CubeMaster                                                      │
        │                                                           │
        ▼                  ┌────────────────────────────────────┐   │
     Cubelet ──────────────┼──► cube-agent ──► envd  ◄──────────┼───┘
                           │     (PID 1)         │              │
                           │                     ▼              │
                           │                Python / Shell      │
                           └────────────────────────────────────┘
```

## 参考链接

- [Cube Sandbox GitHub](https://github.com/TencentCloud/CubeSandbox)
- [LlamaIndex 文档](https://docs.llamaindex.ai/)
- [Python SDK](https://github.com/TencentCloud/CubeSandbox/tree/main/sdk/python)
