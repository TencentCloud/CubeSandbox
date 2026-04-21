# OpenAI Agents SDK + CubeSandbox: Code Interpreter 示例

[English](README.md)

在 [CubeSandbox](https://github.com/TencentCloud/CubeSandbox)（E2B 兼容 API）中运行数据分析 Agent，让 LLM 在云端 Python 沙箱里用 pandas / matplotlib 做真实计算和绘图。

## 两个 Demo 的区别

本目录有两份可直接运行的实现，功能等价，差异在 **Python 执行后端**：

| | [`code_interpreter_demo.py`](code_interpreter_demo.py) | [`code_interpreter_demo_ci.py`](code_interpreter_demo_ci.py) |
|---|---|---|
| 沙箱类型 | `E2BSandboxType.E2B` | `E2BSandboxType.CODE_INTERPRETER` |
| 底层 SDK | `e2b.AsyncSandbox` | `e2b_code_interpreter.AsyncSandbox` |
| 执行方式 | `session.write(script.py)` → `session.exec("python script.py")` | `sandbox.run_code(code)`（Jupyter kernel） |
| 会话状态 | 每次 exec 都是新进程 | **变量 / imports / DataFrame 跨 cell 保留** |
| 图像捕获 | agent 手动 `plt.savefig(...)` | 自动从 `Execution.results` 解 base64 |
| 错误信息 | stderr 全量回传 | 结构化 `Execution.error`（name / value / traceback） |
| 模板端口 | envd (49983) | envd (49983) + code-interpreter gateway (49999) |

**选择建议**：
- 模板只有普通 Python → 用 `code_interpreter_demo.py`
- 模板带 e2b code-interpreter 服务 → 用 `code_interpreter_demo_ci.py`（Jupyter kernel 体验，状态跨轮保留）

## 前置条件

- Python 3.10+
- CubeSandbox 平台已部署，CubeAPI 可访问
- 已创建带数据科学栈的沙箱模板（python3 + pandas + numpy + matplotlib）
- TokenHub 或其他 OpenAI 兼容 LLM 的 API Key

## 快速开始

### 1. 安装依赖

```bash
pip install -r requirements.txt
```

### 2. 配置环境变量

```bash
cp .env.example .env
```

| 变量 | 说明 |
|------|------|
| `TOKENHUB_API_KEY` | TokenHub API Key（自动映射为 `OPENAI_API_KEY`） |
| `OPENAI_BASE_URL` | LLM 地址（默认 `https://tokenhub.tencentmaas.com/v1`） |
| `E2B_API_URL` | CubeAPI 地址，如 `http://<cube-host>:3000` |
| `E2B_API_KEY` | CubeAPI 鉴权 Key |
| `CUBE_TEMPLATE_ID` | 沙箱模板 ID |
| `CUBE_SSL_CERT_FILE` | （可选）CubeSandbox CA 证书路径 |

### 3. 创建沙箱模板

```bash
cubemastercli template create-from-image \
  --image cube-sandbox-image.tencentcloudcr.com/demo/e2b-code-interpreter:v1.1-data \
  --writable-layer-size 1Gi \
  --template-id e2b-code-interpreter \
  --expose-port 49983 \
  --expose-port 49999 \
  --probe 49983
```

将输出的模板 ID 填入 `.env` 的 `CUBE_TEMPLATE_ID`。

> 注意：`code_interpreter_demo.py` 只需要暴露 49983 端口；`code_interpreter_demo_ci.py` 还需要 49999（Jupyter kernel gateway）。

### 4. 运行

```bash
# 通用 E2B 版（write+exec）
python code_interpreter_demo.py

# Jupyter kernel 版（run_code，状态跨轮保留）
python code_interpreter_demo_ci.py

# 自定义问题
python code_interpreter_demo.py --question "Plot units sold per product as a bar chart."

# 换模型
python code_interpreter_demo_ci.py --model openai/deepseek-v3.2

# 沙箱关停时暂停而非销毁（支持后续恢复）
python code_interpreter_demo.py --pause-on-exit
```

### 命令行参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--model` | `openai/glm-5.1` | LLM 模型名（TokenHub 用 `openai/` 前缀） |
| `--question` | 销售数据分析（见下方） | 发送给 Agent 的 prompt |
| `--template` | `CUBE_TEMPLATE_ID` 环境变量 | 沙箱模板 ID |
| `--timeout` | `600` | 沙箱生命周期（秒） |
| `--pause-on-exit` | `False` | 关停时暂停沙箱，保留状态 |

## 运行示例

```bash
python code_interpreter_demo_ci.py
```

```
Model:    openai/glm-5.1
Template: e2b-code-interpreter-2
CubeAPI:  http://localhost:3000
Question: Analyze sales.csv with Python: (1) compute revenue = units * unit_price,
          (2) plot monthly total revenue as a line chart, (3) report the top-3
          products by total revenue as a Markdown table, (4) finish with a
          2-sentence executive summary.

Creating sandbox & running agent ...
[python] code-interpreter ready: probe #3: HTTP_OK status=200 body[:300]='"OK"'
[python] bootstrapped kernel cwd -> /workspace
[python] running 121 bytes in kernel
[python] (14, 4)
[python] ['date', 'product', 'units', 'unit_price']
[python] date           object
[python] product        object
[python] units           int64
[python] unit_price    float64
[python] dtype: object
[python] (ok)
[python] running 567 bytes in kernel
[python] saved output/figure_01.png (32115 bytes)
[python] (ok)
[python] running 505 bytes in kernel
[python] | Product      |   Total Revenue |
[python] |:-------------|----------------:|
[python] | Gamma Gizmo  |          2178   |
[python] | Beta Gadget  |          2156   |
[python] | Alpha Widget |          1512.4 |
[python]
[python] Total revenue: $7,007.40
[python] Peak month: 2025-04 ($2,333.50)
[python] (ok)
================================================================
Here are the results:

**Monthly Revenue Line Chart** → `output/figure_01.png`

**Top-3 Products by Total Revenue**

| Product      | Total Revenue |
|:-------------|--------------:|
| Gamma Gizmo  |       2,178.00 |
| Beta Gadget  |       2,156.00 |
| Alpha Widget |       1,512.40 |

**Executive Summary**

Total revenue across the period was **$7,007.40**, with April 2025 as the
strongest month at **$2,333.50**. Gamma Gizmo narrowly leads product revenue
($2,178), followed closely by Beta Gadget ($2,156), while Alpha Widget
trails at $1,512.40.
================================================================
```

## 架构

```
SandboxAgent
├── WorkspaceShell            # session.exec("sh -lc ...") → ls/cat/pwd
└── PythonRunner              # 自定义 Capability (FunctionTool)
    ├── code_interpreter_demo.py:
    │     session.write(script.py)
    │     session.exec("python -I -B script.py")
    │     → 回传 exit_code / stdout / stderr / output/*
    └── code_interpreter_demo_ci.py:
          sandbox.run_code(code)
          → Execution.results (base64 图像自动解码)
          → Execution.logs (stdout/stderr)
          → Execution.error (结构化异常)
```

### Manifest 种子文件

Agent 启动时通过 `Manifest` 在沙箱工作区写入：

| 文件 | 内容 |
|------|------|
| `sales.csv` | 14 行销售数据（date, product, units, unit_price） |
| `README.md` | 任务说明，引导 agent 做数据分析 |

默认 prompt 要求 agent 基于 `sales.csv` 完成：计算收入 → 画月度趋势图 → Top-3 产品表格 → 总结。

## CubeSandbox 适配

| 补丁 | 原因 | 代码位置 |
|------|------|---------|
| `default_username = "root"` + Filesystem 方法包装 | envd 只服务 root 用户 | 两个文件顶部 |
| `Commands.run` 注入 `on_stdout/on_stderr` | 沙箱输出实时流式打印 | 两个文件顶部 |
| LLM 客户端使用系统 CA bundle | `SSL_CERT_FILE` 隔离 | `make_model()` |
| 强制 `OpenAIChatCompletionsModel` | TokenHub 不支持 Responses API | `make_model()` |

以下为 `code_interpreter_demo_ci.py` 特有的适配：

| 适配 | 说明 |
|------|------|
| `_wait_for_jupyter_ready()` | 沙箱内部探测 `127.0.0.1:49999/health`，确认 Jupyter 就绪 |
| `_find_code_interpreter_sandbox()` | BFS 穿透 SDK wrapper 找到底层 `AsyncSandbox` |
| `_run_code_annotated()` | 将 cube 502 错误从 "sandbox timeout" 改写为 "沙箱被驱逐" |
| kernel CWD 对齐 | `os.chdir` 到 envd 工作区根，让相对路径匹配 Manifest 文件 |

## 相关文档

- [OpenAI Agents SDK × CubeSandbox 集成指南](../openai-agents-example/openai-agents-sandbox-cube-integration_zh.md)
- [OpenAI Sandbox Agents 概念解析](../openai-agents-example/openai-agents_zh.md)
- [CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
