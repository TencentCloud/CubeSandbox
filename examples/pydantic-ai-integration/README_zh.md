# Pydantic AI + CubeSandbox 集成示例

[English](README.md)

一个 [Pydantic AI](https://ai.pydantic.dev) Agent，它的 `run_python` 函数工具在
[CubeSandbox](https://github.com/TencentCloud/CubeSandbox) MicroVM 中执行代码。
由模型决定何时运行代码；代码在隔离的 MicroVM 中执行，其**真实的** stdout / stderr /
退出码再回传给模型。

示例使用官方 **`cubesandbox` Python SDK**（`from cubesandbox import Sandbox`），
无需手写 HTTP。**每次 Agent 运行只创建一个 MicroVM，并在所有工具调用间复用**；
`with` 上下文管理器会在运行结束时自动销毁它。

```text
用户提问
  -> Pydantic AI Agent (agent.run_sync)
  -> 模型调用 run_python 工具
  -> 工具在 CubeSandbox MicroVM 中写入脚本并执行
  -> stdout / stderr / 退出码回传给模型
  -> 模型继续推理
  -> 最终答案
```

## 前置条件

- 一个 CubeAPI 可达的 CubeSandbox 部署（例如 `http://<node>:3000`）。
- 一个具备 Python 的沙箱模板。默认示例只用 Python 标准库，因此官方 **`sandbox-code`**
  模板即可满足，无需自定义镜像。
- 运行 Agent 的机器需 **Python 3.10+**（`pydantic-ai` 的要求）。
- 一个 OpenAI 兼容的 LLM 端点及其 API Key。

> 运行 Agent 的机器和运行 CubeSandbox 的机器不必是同一台。Agent 通过网络访问
> CubeAPI/CubeProxy，因此可以在笔记本上驱动一台远程 Linux Cube 主机。

## 安装

```bash
# 1. 注册官方 code sandbox 模板（在 Cube 主机上执行一次）。
cubemastercli tpl create-from-image \
  --image cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
# 中国大陆请使用 cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest
# 复制输出中的 template_id。

# 2. 配置并安装依赖（在运行 Agent 的机器上）。
cp .env.example .env        # 填写 CUBE_TEMPLATE_ID 以及 LLM key/端点
pip install -r requirements.txt
```

`.env` 中的关键变量：

| 变量 | 必填 | 用途 |
|---|---|---|
| `CUBE_TEMPLATE_ID` | 是 | 步骤 1 得到的沙箱模板 ID |
| `CUBE_API_URL` | 否 | CubeAPI 地址；默认 `http://127.0.0.1:3000` |
| `CUBE_API_KEY` | 否 | 仅在开启鉴权的 CubeAPI 上需要 |
| `CUBE_PROXY_NODE_IP` / `CUBE_PROXY_PORT_HTTP` | 否 | 未配置 DNS 时直连 CubeProxy |
| `OPENAI_API_KEY` | 是 | OpenAI 兼容端点的 API Key |
| `OPENAI_BASE_URL` | 否 | 端点地址；默认 `https://tokenhub.tencentmaas.com/v1` |
| `MODEL_NAME` | 否 | 模型名；默认 `deepseek-v3`（也接受 `CHAT_MODEL`） |

## 运行

```bash
python pydantic_ai_agent_demo.py
# 或传入自定义任务（同样仅依赖标准库）：
python pydantic_ai_agent_demo.py "计算前 15 个质数及它们的和。"
```

## 预期表现

默认任务让 Agent 生成并校验斐波那契数列。一次成功的运行会：

1. 打印 `Sandbox <id> created. Running agent...`。
2. 模型至少调用一次 `run_python`；代码片段在 **MicroVM 内**执行（而非本地）。
3. 打印 `=== Final answer ===` 段落，其中的数字与被执行代码打印的结果一致
   （例如第 20 个斐波那契数为 `6765`，且全部递推校验通过）。

最终答案的具体措辞因模型而异，因此请验证**行为**而非某个固定句子：

- `run_python` 确实被调用（先出现 sandbox id 那一行，再开始干活）。
- 报告的数字来自真实执行——它们前后自洽且正确（F(20) = 6765）。
- Agent 以最终答案收尾，而不是陷入循环。

## 沙箱的使用方式

- **每次运行一个 MicroVM，并在工具调用间复用。** 沙箱在 `main()` 中创建一次，
  通过带类型的 `deps` 传给 Agent；每次 `run_python` 调用都复用它，因此没有
  逐次调用的 MicroVM 启动开销，且同一次运行内文件得以保留。
- **工作目录。** 官方 `sandbox-code` 镜像不带 `/workspace`，因此 `main()` 会在沙箱
  启动后先执行一次 `mkdir -p /workspace`。
- **每次调用使用唯一脚本名。** 每次调用写入 `/workspace/agent_step_<n>.py`
  （编号来自宿主侧计数器），因此重复调用不会互相覆盖。
- **失败以工具输出形式返回。** `CubeSandboxError` 或传输超时会以文本形式返回给
  模型，便于它重试，而不是中断整个运行；stderr 会被分隔展示，非零退出码会被报告。
- **默认网络隔离。** 由于任务无需联网，示例以 `allow_internet_access=False`
  创建沙箱。如果你的任务需要联网，请在脚本中改为 `True`。

## 文件说明

| 文件 | 用途 |
|---|---|
| `pydantic_ai_agent_demo.py` | 可运行的 Agent 与 `run_python` Cube 工具 |
| `requirements.txt` | 宿主依赖（`pydantic-ai`、`cubesandbox`、`python-dotenv`） |
| `.env.example` | 环境变量模板 |
