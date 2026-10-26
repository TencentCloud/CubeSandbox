# LangChain 1.x + CubeSandbox 集成示例

[English](README.md)

一个 [LangChain](https://github.com/langchain-ai/langchain) **1.x** Agent，其代码执行工具
运行在 [CubeSandbox](https://github.com/TencentCloud/CubeSandbox) 的 MicroVM 内。本变体使用
**现代 `langchain.agents.create_agent` API** 配合 `@tool` 装饰器，需要 **Python 3.10+**。

通过官方 **`cubesandbox` Python SDK**（`from cubesandbox import Sandbox`）创建 MicroVM 并执行代码——
无需裸 HTTP，也无需 LangChain 0.x 的 `AgentExecutor` / `PromptTemplate` 样板代码。
沙箱由 `with` 上下文管理器在结束时自动销毁。

> 仍在使用 LangChain 0.3.x / Python 3.9？请看 [`../0.x`](../0.x)。

## 你需要准备

- 已部署 CubeSandbox，CubeAPI 可从 `http://<node>:3000` 访问。
- 一个预装了 Python + pandas/numpy/matplotlib/scikit-learn 的沙箱模板（用顶层
  `../Dockerfile` 构建）。
- **Python 3.10+**（由 `langchain-openai` 1.x 要求）。
- 一个 OpenAI 兼容的 LLM 端点（示例使用 TokenHub）。

## 配置

```bash
# 1. 构建并推送数据科学模板镜像（在父目录执行）
cd ..
docker build --platform linux/amd64 \
  -t <your-registry>/langchain-cube:latest .
docker push <your-registry>/langchain-cube:latest

# 2. 注册为 Cube 模板
cubemastercli tpl create-from-image \
  --image <your-registry>/langchain-cube:latest \
  --writable-layer-size 2G \
  --expose-port 49983 --probe 49983 --probe-path /health
# 记下得到的 template_id

# 3. 配置主机驱动（在本 1.x/ 目录执行）
cd 1.x
cp ../.env.example .env        # 填写 CUBE_API_URL、CUBE_API_KEY、CUBE_TEMPLATE_ID、CUBE_PROXY_NODE_IP、LLM 密钥
pip install -r requirements.txt
```

## 运行

```bash
python langchain_agent_demo.py
# 自定义任务：
python langchain_agent_demo.py "在数据集上训练线性模型并报告 RMSE。"
```

> 本示例使用官方 `cubesandbox` SDK 在 MicroVM 内执行代码。
> 代理通过 `.env` 中的 `CUBE_PROXY_NODE_IP`（及 `CUBE_PROXY_PORT_HTTP`）配置。

模板镜像已在 `/workspace` 下预置了 `sales.csv` 种子数据，Agent 可直接加载分析。
Agent 通过基于 `cubesandbox` 的 `run_python` 工具计算月营收——全部在 MicroVM 内执行。

## 预期效果

使用默认提示词时，主机终端会打印 Agent 的**最终回答**——各月营收数字
（2780.0 / 3375.5 / 3872.0）。（1.x 使用 `create_agent`，默认**不**显示详细过程，
因此不会打印逐步推理日志。）

日志与清理行为：

- 仅 Agent 的最终回答会打印到 **stdout**。
- 销毁是尽力而为：`kill()` 失败时，SDK 的上下文管理器会静默吞掉错误，
  不会向 **stderr** 打印任何警告。

## 文件

| 文件 | 用途 |
|---|---|
| `langchain_agent_demo.py` | 可运行的 1.x Agent + `run_python` Cube 工具 |
| `requirements.txt` | 主机驱动依赖（`langchain>=1.3.14,<2.0`、`langchain-openai>=1.0,<2.0`、`cubesandbox`） |
| `../Dockerfile` | 共享的数据科学模板镜像（基于 `cubesandbox-base`） |
| `../.env.example` | 共享的环境变量模板 |
