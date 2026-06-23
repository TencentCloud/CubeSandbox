# CrewAI + Cube Sandbox

本示例通过 Cube Sandbox 的 E2B 兼容 API，为 CrewAI Agent 提供 Python 执行能力。Agent 生成的代码运行在隔离的 MicroVM 中，而不是本地 CrewAI 进程中。

## 文件说明

- `smoke_test.py` 在不调用 LLM 的情况下验证 Cube 连接。
- `main.py` 通过 CrewAI Agent 执行一个确定性数据分析任务。
- `.env.example` 说明 Cube 与 LLM 的必需配置。

## 环境准备

1. 部署 Cube Sandbox，并创建一个代码解释器模板。生成的模板 ID 必须使用 `tpl-...` 格式。
2. 创建 Python 环境并安装依赖：

   ```bash
   python -m venv .venv
   source .venv/bin/activate
   pip install -r requirements.txt
   ```

3. 配置环境：

   ```bash
   cp .env.example .env
   ```

   将 `E2B_API_URL` 设置为通常使用 `http://<host>:3000` 的 Cube API Server 地址，不要使用 CubeProxy 地址。

   `http://` 只适合在可信机器上进行本地开发。生产环境应为 CubeAPI 配置 TLS 并使用 `https://`，或将 CubeAPI 绑定到 loopback 并使用 `http://127.0.0.1:3000`，避免 `E2B_API_KEY` 以明文形式在网络中传输。

## 验证 Cube

在调用 CrewAI 的 LLM 前先运行 smoke test：

```bash
python smoke_test.py
```

结果中应包含：

```text
{"runtime": "cube", "sum": 45}
```

如果结果中没有出现预期的 Cube 输出，smoke test 会以非零状态退出。

## 运行 Crew

```bash
python main.py
```

Agent 必须调用 `E2BPythonTool`，在 Cube 中模拟掷骰子，并将结果与 `5/36` 比较。

只有在本地调试时才建议设置 `CREWAI_VERBOSE=true`。CrewAI verbose 日志可能包含模型服务配置，分享日志前请先检查是否含有凭证信息。

## 安全默认值

示例使用 `persistent=False`，因此每次工具调用都会获得一个新的 MicroVM，并在使用后销毁。处理不可信输入时应保留该默认值。如果必须跨调用保留状态，请设置 `persistent=True`、使用较短的 `sandbox_timeout`，并在退出时调用 `tool.close()`。

网络隔离和宿主机挂载配置请参考完整的 [CrewAI 集成指南](../../docs/zh/guide/integrations/crewai.md)。
