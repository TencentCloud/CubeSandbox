# cube-sandbox RL SWE-bench Example — PRD

> 版本：v1.0
> 日期：2026-04-05

## 1. 项目背景

**cube-sandbox** 是一个轻量级虚拟化平台，管理流和数据流完全兼容 [E2B SDK](https://e2b.dev)。平台通过 in-sandbox agent（**envd**）提供 gRPC 接口，支持命令执行、文件读写和沙箱生命周期管理。

本项目以 **SWE-bench**（软件工程基准测试）为应用场景，演示如何：

1. 使用 cube-sandbox 创建隔离的代码执行环境
2. 通过 E2B SDK 驱动 LLM Agent 在沙箱内交互式解题
3. 自动化评测 Agent 的解题能力
4. 为后续 RL（强化学习）训练建立基础设施

## 2. 目标

### 2.1 短期目标（本 Example）

- 提供一个**可运行的端到端示例**，从镜像准备到评测完成
- 支持**多 LLM 模型**切换（Gemini、GLM、MiniMax、Kimi 等）
- 输出结构化的评测结果（patch、步骤数、费用、耗时）

### 2.2 长期愿景（RL 训练）

将评测流水线扩展为完整的 RL 训练循环：

```
┌─────────────────────────────────────────────────────────────────┐
│                      RL Training Loop                           │
│                                                                 │
│  ┌──────────┐    ┌───────────┐    ┌──────────┐    ┌──────────┐ │
│  │  Policy   │    │   Action  │    │  Env     │    │  Reward  │ │
│  │  (LLM)   │───►│ bash 命令  │───►│ E2B 沙箱 │───►│ 测试通过率│ │
│  │          │◄───│           │◄───│ testbed  │◄───│          │ │
│  └──────────┘    └───────────┘    └──────────┘    └──────────┘ │
│       ▲                                                │       │
│       └────────────── Policy Update ◄──────────────────┘       │
└─────────────────────────────────────────────────────────────────┘
```

| RL 概念 | 对应实现 |
|---------|---------|
| **Environment** | cube-sandbox 沙箱（通过 E2B SDK 管理） |
| **State** | testbed 中的代码仓库状态 + 命令历史 |
| **Action** | Agent 生成的 bash 命令 |
| **Reward** | SWE-bench 测试通过率（0 或 1） |
| **Policy** | LLM（可通过 RL 微调优化） |

## 3. 系统架构

```
┌─────────────────┐          ┌──────────── cube-sandbox ────────────┐
│                 │          │                                       │
│  mini-swe-agent │  E2B SDK │  ┌──────────────┐                    │
│  (LLM Agent)    │─────────►│  │    envd       │──► bash 命令执行   │
│                 │  HTTPS/  │  │  :49983       │──► 文件读写        │
│  ┌───────────┐  │  gRPC    │  └──────────────┘                    │
│  │  LiteLLM  │  │          │                                       │
│  │  ┌──────┐ │  │          │  ┌──────────────────────────────┐    │
│  │  │Gemini│ │  │          │  │  /testbed                     │    │
│  │  │GLM-5 │ │  │          │  │  (SWE-bench repo + testcase) │    │
│  │  │Kimi  │ │  │          │  └──────────────────────────────┘    │
│  │  │...   │ │  │          │                                       │
│  │  └──────┘ │  │          └───────────────────────────────────────┘
│  └───────────┘  │
└─────────────────┘
```

### 关键组件

| 组件 | 说明 |
|------|------|
| **mini-swe-agent** | 开源 LLM Agent 框架，支持多轮 tool call 交互 |
| **LiteLLM** | 统一 LLM API 接口，支持 OpenAI、Gemini、TokenHub 等 |
| **E2B SDK** | Python 客户端，通过 `Sandbox` 类管理沙箱生命周期 |
| **envd** | cube-sandbox 的 in-sandbox agent，静态链接二进制，监听 :49983 |
| **SWE-bench** | 软件工程基准测试集，包含真实 GitHub issue + 测试用例 |

### E2B SDK 接口

| 接口 | 功能 |
|------|------|
| `Sandbox(template=...)` | 从模板创建沙箱 |
| `sbx.commands.run(cmd, user=...)` | 执行 bash 命令 |
| `sbx.files.read(path)` | 读取文件内容 |
| `sbx.files.write(path, content)` | 写入文件 |
| `sbx.files.list(dir)` | 列出目录 |
| `sbx.kill()` | 销毁沙箱 |

## 4. mini-swe-agent 改造

mini-swe-agent 原生支持 Docker、Singularity 等执行环境，但不支持 E2B。为接入 cube-sandbox，需要对 mini-swe-agent 做以下改造：

### 4.1 修改的文件

| 文件 | 改动 |
|------|------|
| `environments/extra/e2b.py` | **新增** — E2B 环境类，封装 E2B SDK 的沙箱操作 |
| `environments/__init__.py` | 注册 `"e2b"` 到环境类型映射表 `_ENVIRONMENT_MAPPING` |
| `run/benchmarks/swebench.py` | 在 `get_sb_environment` 中将 `"e2b"` 加入支持 `image` 参数的类型列表 |

### 4.2 E2BEnvironment 类实现

核心实现约 130 行 Python（`environments/extra/e2b.py`），关键设计：

**配置类 `E2BEnvironmentConfig`**：

```python
class E2BEnvironmentConfig(BaseModel):
    template_id: str = ""     # E2B 模板 ID（或从 CUBE_TEMPLATE_ID 环境变量读取）
    image: str = ""           # SWE-bench 兼容字段，实际不使用
    cwd: str = "/testbed"     # 工作目录
    timeout: int = 60         # 单条命令超时（秒）
    user: str = "root"        # 执行用户
    sandbox_timeout: int = 1800  # 沙箱生命周期（秒）
```

**核心方法**：

| 方法 | 功能 |
|------|------|
| `__init__` | 通过 `Sandbox.create(template=template_id)` 创建 E2B 沙箱 |
| `execute(action)` | 拼接 `cd {cwd} && {command}`，通过 `sbx.commands.run()` 执行，合并 stdout/stderr |
| `_check_finished(output)` | 检测输出首行是否为 `COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT`，若是则抛出 `Submitted` 异常触发提交 |
| `cleanup()` | 调用 `sbx.kill()` 销毁沙箱，释放资源 |

**命令执行流程**：

```
Agent 发出 action {"command": "grep -r 'def _build_app_dict' ."}
    │
    ▼
E2BEnvironment.execute()
    │  拼接: cd /testbed && grep -r 'def _build_app_dict' .
    ▼
sandbox.commands.run(full_cmd, user="root", timeout=60)
    │  通过 HTTPS/gRPC 发送到 envd
    ▼
envd 在沙箱内执行 bash 命令
    │
    ▼
返回 {output, returncode, exception_info}
    │
    ▼
_check_finished() 判断是否提交 patch
```

### 4.3 环境注册

在 `environments/__init__.py` 的 `_ENVIRONMENT_MAPPING` 中添加一行：

```python
_ENVIRONMENT_MAPPING = {
    "docker": "...",
    "singularity": "...",
    # ...
    "e2b": "minisweagent.environments.extra.e2b.E2BEnvironment",  # 新增
}
```

### 4.4 SWE-bench 入口适配

在 `run/benchmarks/swebench.py` 的 `get_sb_environment()` 中，将 `"e2b"` 加入支持 `image` 参数的环境列表：

```python
if env_config["environment_class"] in ["docker", "swerex_modal", "e2b"]:
    env_config["image"] = image_name
```

这样 SWE-bench 运行器会自动将题目对应的 Docker 镜像名传给 E2B 环境配置（E2B 环境类内部忽略 `image` 字段，使用 `template_id`）。

## 5. 核心流程

### 5.1 镜像准备

```
SWE-bench 原始镜像 ──► envd 注入 ──► 注册为 cube-sandbox 模板
```

1. 从 `cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest` 提取 envd 二进制（境外访问请使用 `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest`）
2. 将 envd 注入 SWE-bench 镜像（Dockerfile 覆盖 ENTRYPOINT）
3. 将注入后的镜像注册为 cube-sandbox 模板，获得 `template_id`

### 5.2 评测流程

```
加载 SWE-bench 题目
       │
       ▼
创建 E2B 沙箱（template_id）
       │
       ▼
┌──────────────────────┐
│  Agent 交互循环       │
│  ① LLM 分析问题      │
│  ② 生成 bash 命令     │
│  ③ E2B 执行命令       │
│  ④ 返回结果给 LLM     │
│  ⑤ 重复直到提交 patch │
└──────────────────────┘
       │
       ▼
提取 patch + 评估结果
       │
       ▼
销毁沙箱
```

### 5.3 RL 训练扩展（愿景）

在评测流程基础上增加：

1. **Batch 采样**：并行创建多个沙箱，同一题目多次尝试
2. **Reward 计算**：`reward = 1 if tests_pass else 0`，可扩展为部分分
3. **Trajectory 收集**：记录 (state, action, reward) 序列
4. **Policy Update**：使用 GRPO/PPO 等算法更新 LLM 权重

cube-sandbox 的优势在于：
- 沙箱创建/销毁开销低（秒级），适合大规模并行采样
- E2B SDK 兼容性确保与主流工具链无缝集成
- envd 无依赖注入，支持任意 Linux 镜像

## 6. 已验证结果

以 `django__django-13447` 题目在 E2B 沙箱环境下的多模型评测：

| 模型 | 步骤 | 费用 | 耗时 | 结果 |
|------|------|------|------|------|
| Kimi K2.5 | 42 | $0.93 | 179 秒 | 成功 |
| Gemini 3 Pro | 22 | $0.26 | 224 秒 | 成功 |
| Gemini 3 Flash | 46 | $0.19 | 278 秒 | 成功 |
| MiniMax M2.7 | 56 | $1.62 | 363 秒 | 成功 |
| GLM-5 | 41 | $0.75 | 400 秒 | 成功 |

### Docker vs E2B 对比（Gemini 3 Flash, django__django-13447）

| 指标 | Docker 直连 | E2B 沙箱 | 差异 |
|------|------------|---------|------|
| 耗时 | 208 秒 | 278 秒 | +34% |
| 步骤 | 49 步 | 46 步 | -6% |
| 费用 | $0.16 | $0.19 | +19% |

E2B 额外耗时主要来自沙箱创建网络开销和 HTTPS/gRPC 传输延迟，整体性能损耗在可接受范围内。

## 7. 技术约束与注意事项

### 7.1 SSL 证书

自部署 cube-sandbox 使用 mkcert 证书，客户端需要：
1. 安装 cube-sandbox 节点的 root CA 到系统信任链
2. 设置 `SSL_CERT_FILE` 环境变量指向系统证书包（Python httpx/certifi 需要）

### 7.2 envd 注入

- envd 是静态链接的 x86_64 二进制（~15MB），无外部依赖
- 注入后必须设置为 `ENTRYPOINT ["/usr/bin/envd"]`
- 默认监听端口 49983

### 7.3 Kimi K2.5 thinking 模式

Kimi K2.5 默认开启 thinking 模式，与 LiteLLM 的 tool call 消息格式不兼容。需通过 `extra_body: {thinking: {type: disabled}}` 禁用，否则多轮 tool call 会报 `reasoning_content is missing` 错误。

### 7.4 SWE-bench 镜像用户

SWE-bench 镜像以 root 运行，E2B SDK 默认使用 `user` 用户。必须在配置中指定 `user: "root"`。

## 8. 非目标（不在本 Example 范围内）

- RL 训练代码实现（GRPO/PPO 等）
- 模型微调基础设施
- SWE-bench 全集评测（仅演示单题）
- 生产环境部署方案
