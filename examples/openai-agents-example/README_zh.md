# OpenAI Agents SDK + CubeSandbox 示例

[English](README.md)

本目录包含两个示例脚本，演示如何通过 OpenAI Agents SDK 的 `E2BSandboxClient` 对接 [CubeSandbox](https://github.com/TencentCloud/CubeSandbox)。

| 脚本 | 场景 | 说明 |
|------|------|------|
| [`simple_demo.py`](simple_demo.py) | 快速上手 | 最简 Shell Agent + Pause/Resume 演示 |
| [`main.py`](main.py) | SWE-bench 调试 | 完整流式输出 + LLM 预检 + 全链路追踪 |

## 前置条件

- Python 3.10+
- CubeSandbox 平台已部署，CubeAPI 可访问
- 已创建沙箱模板（见下方「模板创建」）
- TokenHub 或其他 OpenAI 兼容 LLM 服务的 API Key

## 快速开始

### 1. 安装依赖

```bash
pip install -r requirements.txt
```

### 2. 配置环境变量

```bash
cp .env.example .env
```

编辑 `.env`：

| 变量 | 说明 |
|------|------|
| `TOKENHUB_API_KEY` | TokenHub API Key（自动映射为 `OPENAI_API_KEY`） |
| `OPENAI_BASE_URL` | LLM 地址（默认 `https://tokenhub.tencentmaas.com/v1`） |
| `E2B_API_URL` | CubeAPI 地址，如 `http://<cube-host>:3000` |
| `E2B_API_KEY` | CubeAPI 鉴权 Key |
| `CUBE_TEMPLATE_ID` | 沙箱模板 ID |
| `CUBE_SSL_CERT_FILE` | （可选）CubeSandbox CA 证书路径 |

### 3. 创建沙箱模板

**simple_demo.py** 可用任意 Linux 模板。**main.py** 需要预装 Django 源码的 SWE-bench 镜像：

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-image.tencentcloudcr.com/demo/django_1776_django-13447:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --cpu 4000 --memory 8192 \
  --probe 49983
```

命令输出中的模板 ID 填入 `.env` 的 `CUBE_TEMPLATE_ID`。

---

## simple_demo.py — 最简示例

最小化的 Shell Agent，展示 CubeSandbox 集成的核心步骤。

### 用法

```bash
# 基础 Agent 问答
python simple_demo.py
python simple_demo.py --question "What Linux distro is this?"

# Pause / Resume 演示（写入文件 → 暂停 → 恢复 → 验证文件）
python simple_demo.py --pause-resume

# SSL 调试模式
python simple_demo.py --no-ssl-patch        # 禁用所有 SSL 自定义
python simple_demo.py --llm-cube-ssl        # LLM 也用 cube 的证书
```

### 参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--model` | `openai/glm-5.1` | LLM 模型名 |
| `--question` | 查看 OS 版本 | 发送给 Agent 的问题 |
| `--template` | `CUBE_TEMPLATE_ID` | 沙箱模板 ID |
| `--timeout` | `300` | 沙箱超时（秒） |
| `--pause-resume` | — | 切换到 Pause/Resume 演示模式 |
| `--no-ssl-patch` | — | 禁用 SSL 自定义处理 |
| `--llm-cube-ssl` | — | LLM 客户端也使用 cube 证书 |

### 核心代码

```python
agent = SandboxAgent(
    name="Cube Demo Agent",
    model=make_model("openai/glm-5.1"),
    instructions="You are a helpful assistant running inside a cloud sandbox.",
    default_manifest=Manifest(),
    capabilities=[Shell()],
)

run_config = RunConfig(
    sandbox=SandboxRunConfig(
        client=E2BSandboxClient(),
        options=E2BSandboxClientOptions(
            sandbox_type=E2BSandboxType.E2B,
            template=os.environ["CUBE_TEMPLATE_ID"],
            timeout=300,
        ),
    ),
)

result = await Runner.run(agent, "What OS is running?", run_config=run_config)
```

### Pause/Resume 流程

```
[step 1] 创建沙箱
[step 2] 写入标记文件 pause-resume-test.txt
[step 3] 暂停沙箱（stop + shutdown, pause_on_exit=True）
[step 4] 恢复沙箱（client.resume）
[step 5] 读取文件，验证内容一致 → PASS / FAIL
[cleanup] 销毁沙箱
```

---

## main.py — SWE-bench Django 调试

完整的 SWE-bench Agent，在沙箱中自主分析 Django Bug 并提出修复方案。包含 LLM 预检、流式输出、全链路追踪。

### 用法

```bash
# 分析 Django Bug（django__django-13447）
python main.py

# 自定义问题
python main.py --question "What Python version is installed? Show the Django version too."

# 指定模型
python main.py --model openai/deepseek-v3.2

# 仅测试沙箱连通性（不调用 LLM）
python main.py --sandbox-only
python main.py --sandbox-only --timeout 60

# 限制工具调用轮数
python main.py --max-turns 20
```

### 参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--model` | `openai/glm-5.1` | LLM 模型名（TokenHub 用 `openai/` 前缀） |
| `--question` | 分析 Bug 并修复 | 发送给 Agent 的问题 |
| `--template` | `CUBE_TEMPLATE_ID` | SWE-bench Django 模板 ID |
| `--timeout` | `300` | 沙箱超时（秒） |
| `--max-turns` | `50` | 最大工具调用轮数 |
| `--sandbox-only` | — | 仅创建/销毁沙箱，验证连通性 |

### 内置功能

**LLM 预检**：正式运行前验证 LLM 连通性（纯文本 → tool-calling → streaming 三项测试）。

**流式输出**：实时打印 Agent 的每一步操作：

```
[preflight] 1/3 plain ok — glm-5.1 @ https://tokenhub.tencentmaas.com/v1/  856 ms
[preflight] 2/3 tool-call ok — 1204 ms: get_info({"cmd":"uname"})
[preflight] 3/3 streaming ok — 623 ms, 12 chunks
[status] creating sandbox & starting session ...
[agent] SWE-bench Agent running
[step 1] tool_call: exec_command({"cmd": "cat /testbed/django/contrib/admin/..."})
  → output: ...
[step 2] tool_call: exec_command({"cmd": "grep -n 'items_for_result' ..."})
  → output: ...
[answer] The bug is in the `items_for_result` function ...
[done] 8 tool calls, 42350 ms total
```

**全链路追踪**：自动记录 E2B 生命周期（create/start/exec/shutdown）和 LLM 调用（HTTP 请求/响应、首个 token 延迟）的耗时。

**防挂超时**：LLM 回答后 30 秒无新事件自动退出，避免 Runner 内部 finalization 导致无限挂起。

### SWE-bench 场景说明

默认任务是 `django__django-13447`：

> 当 `ModelAdmin` 设置 `list_display_links = None` 时，Django admin 仍然为第一个字段生成链接（应该显示为纯文本）。

Agent 会：
1. 在 `/testbed/django/contrib/admin/templatetags/admin_list.py` 中定位 `items_for_result` 函数
2. 分析 Bug 根因
3. 提出修复方案

沙箱模板镜像中 `/testbed` 包含完整的 Django 源码。

---

## CubeSandbox 适配

两个脚本都包含以下运行时补丁：

| 补丁 | 原因 |
|------|------|
| `default_username = "root"` + Filesystem 方法包装 | CubeSandbox envd 只服务 `root` 用户 |
| `Commands.run` 移除 `stdin` 参数 | 兼容老版本 envd |
| LLM 客户端使用系统 CA bundle | 避免 `SSL_CERT_FILE`（cube gRPC）污染 TokenHub HTTPS |
| 强制使用 `OpenAIChatCompletionsModel` | TokenHub 不支持 Responses API |

详细说明见 [集成指南](openai-agents-sandbox-cube-integration_zh.md)。

## 相关文档

- [OpenAI Agents SDK × CubeSandbox 集成指南](openai-agents-sandbox-cube-integration_zh.md)
- [OpenAI Sandbox Agents 概念解析](openai-agents_zh.md)
- [OpenAI Agents SDK GitHub](https://github.com/openai/openai-agents-python)
- [CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
