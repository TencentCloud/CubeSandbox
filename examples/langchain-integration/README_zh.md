# LangChain + CubeSandbox 集成示例

[English](README.md)

运行一个 LangChain Agent，其代码执行工具在
[CubeSandbox](https://github.com/TencentCloud/CubeSandbox) 的 MicroVM 内执行。这些示例使用官方
**`cubesandbox` Python SDK**（`from cubesandbox import Sandbox`）来创建 MicroVM 并执行代码——
无需裸 HTTP，沙箱由 `with` 上下文管理器在结束时自动销毁。

本目录提供同一 Agent 的**两个大版本变体**。请按你的 LangChain / Python 版本选择：

| 变体 | LangChain | Python | Agent API | 路径 |
|---|---|---|---|---|
| **0.x** | `0.3.x`（传统） | 3.9+ | `AgentExecutor` + `create_react_agent` | [`0.x/`](0.x) |
| **1.x** | `1.x`（现代） | 3.10+ | `langchain.agents.create_agent` + `@tool` | [`1.x/`](1.x) |

两个变体共用同一个代码执行工具（`run_python`，基于 `cubesandbox` SDK）和同一个沙箱模板。
唯一区别在于 LangChain 的 Agent 构建 API 以及 Python 版本要求。

## 我该用哪个？

- 使用 **Python 小于 3.10**（≥ 3.9），或已有 LangChain 0.3.x 代码 → 用 **`0.x/`**。
- 使用 **Python 3.10+** 且从零开始，或已在 LangChain 1.x 上 → 用 **`1.x/`**。

## 共享资源

以下文件位于顶层，两个变体共用：

| 文件 | 用途 |
|---|---|
| `Dockerfile` | 数据科学模板镜像（基于 `cubesandbox-base`） |
| `.env.example` | 环境变量模板（在 `0.x/` 或 `1.x/` 内复制为 `.env`） |
| `sales.csv` | 预置进模板镜像的种子数据集 |

## 快速开始（任一变体）

```bash
# 构建并注册共享模板镜像（顶层）
docker build --platform linux/amd64 -t <your-registry>/langchain-cube:latest .
docker push <your-registry>/langchain-cube:latest
cubemastercli tpl create-from-image \
  --image <your-registry>/langchain-cube:latest \
  --writable-layer-size 2G --expose-port 49983 --probe 49983 --probe-path /health

# 然后选择一个变体，例如 1.x：
cd 1.x
cp ../.env.example .env        # 填写对应的值
pip install -r requirements.txt
python langchain_agent_demo.py
```

详见各变体的 `README.md`。
