# LangGraph + CubeSandbox 集成示例

[English](README.md)

在 [CubeSandbox](https://github.com/TencentCloud/CubeSandbox) MicroVM 中，通过官方 **`cubesandbox`**
Python SDK 运行一个 [LangGraph](https://github.com/langchain-ai/langgraph) Agent —— 由节点和条件边
构成的显式 `StateGraph`，其代码执行工具在沙箱内运行。这些是可运行的
[LangGraph 集成指南](../../docs/zh/guide/integrations/langgraph.md) 对应脚本。

| 脚本 | 用途 |
|---|---|
| `langgraph_agent_demo.py` | 在单个 MicroVM 上运行「生成 → 执行 → 审查 → 重试」循环 |
| `langgraph_checkpoint_demo.py` | 结合 LangGraph checkpointer，在 `pause()` / `connect()` 之间恢复 |

## 前置条件

- Python 3.10+
- 已部署 CubeSandbox，并构建注册了一个含 pandas / numpy / matplotlib / scikit-learn 的
  模板镜像（见 `Dockerfile`）。

## 快速开始

```bash
# 构建并注册共享模板镜像
docker build --platform linux/amd64 -t <your-registry>/langgraph-cube:latest .
docker push <your-registry>/langgraph-cube:latest
cubemastercli tpl create-from-image \
  --image <your-registry>/langgraph-cube:latest \
  --writable-layer-size 2G --expose-port 49983 --probe 49983 --probe-path /health

# 配置环境变量并运行
cp .env.example .env        # 填入实际值
pip install -r requirements.txt
python langgraph_agent_demo.py
python langgraph_checkpoint_demo.py
```

## 环境变量

| 变量 | 必填 | 说明 |
|---|---|---|
| `CUBE_API_URL` | — | CubeAPI 基础地址（默认 `http://127.0.0.1:3000`） |
| `CUBE_TEMPLATE_ID` | ✓ | 演示使用的沙箱模板 id |
| `CUBE_PROXY_NODE_IP` | — | 直连 CubeProxy 的 IP（未设置时 SDK 回退到 wildcard-DNS 主机） |
| `CUBE_API_KEY` | — | 仅当 CubeAPI 后端启用鉴权时需要 |
| `OPENAI_API_KEY` | ✓ | LLM API key（或 `TOKENHUB_API_KEY`） |
| `OPENAI_BASE_URL` | — | OpenAI 兼容端点 |
| `CHAT_MODEL` | — | 模型名（默认 `deepseek-v3`） |
