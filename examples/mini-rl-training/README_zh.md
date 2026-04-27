# cube-sandbox RL SWE-bench Example

使用 cube-sandbox（E2B 兼容）+ mini-swe-agent 在隔离沙箱中自动化解决 SWE-bench 编程任务。

> 详细需求文档见 [docs/PRD.md](docs/PRD.md)

## 前置条件

- Python 3.10+
- Docker（用于构建 envd 注入镜像）
- cube-sandbox 平台访问权限（API URL + API Key）
- 至少一个 LLM API Key（Gemini / TokenHub / OpenAI 等）

## 快速开始

### Step 1: 安装依赖

```bash
pip install -r requirements.txt
```

### Step 2: 安装 mini-swe-agent E2B 补丁

mini-swe-agent 默认不支持 E2B 环境，需要安装补丁：

```bash
bash mini-swe-agent-patch/install.sh
```

补丁会将 3 个文件覆盖到 mini-swe-agent 的 site-packages 中，添加 E2B 环境支持。详见 [mini-swe-agent-patch/README.md](mini-swe-agent-patch/README.md)。

### Step 3: 配置环境变量

```bash
cp .env.example .env
```

编辑 `.env`，填入真实值：

```bash
# HuggingFace 镜像（国内网络使用 hf-mirror.com 避免超时）
HF_ENDPOINT="https://hf-mirror.com"

# cube-sandbox 平台连接信息
E2B_API_URL="http://<your-cube-sandbox-ip>:3000"
E2B_API_KEY="<your-api-key>"

# SWE-bench 镜像模板 ID（Step 4 获取）
CUBE_TEMPLATE_ID="<your-template-id>"

# SSL 证书（自部署平台需要，仅作用于 E2B SDK 连接）
CUBE_SSL_CERT_FILE="/etc/pki/tls/cert.pem"

# LLM API Key（选择一个或多个）
GEMINI_API_KEY="<your-gemini-key>"
TOKENHUB_API_KEY="<your-tokenhub-key>"
OPENAI_API_KEY="<your-openai-key>"
```

### Step 4: 准备 SWE-bench 镜像模板

我们提供了预构建的公开镜像（已注入 envd），可直接使用：

```
cube-sandbox-image.tencentcloudcr.com/demo/django_1776_django-13447:latest
```

> 该镜像公网可拉取，无需自行构建。

使用 `cubemastercli` 从镜像创建沙箱模板：

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-image.tencentcloudcr.com/demo/django_1776_django-13447:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --cpu 4000 \
  --memory 8192 \
  --probe 49983
```

| 参数 | 说明 |
|------|------|
| `--image` | 已注入 envd 的 SWE-bench 镜像地址 |
| `--writable-layer-size` | 可写层大小（沙箱内文件修改空间） |
| `--expose-port` | envd gRPC 端口（固定 49983） |
| `--cpu` | CPU 配额（毫核，4000 = 4 核） |
| `--memory` | 内存配额（MB，8192 = 8G） |
| `--probe` | 健康检查端口（与 expose-port 一致） |

命令输出的 `template_id` 填入 `.env` 的 `CUBE_TEMPLATE_ID`。

<details>
<summary>自定义镜像：从原始 SWE-bench 镜像注入 envd</summary>

如果需要使用其他 SWE-bench 题目的镜像，可通过脚本注入 envd：

```bash
bash scripts/inject-envd.sh swebench/sweb.eval.x86_64.django_1776_django-13447:latest
```

脚本会构建一个带 envd 的新镜像，将其推送到镜像仓库后，再用上述 `cubemastercli` 命令创建模板。

</details>

### Step 5: 运行评测

使用 Gemini 3 Flash 解决 django__django-13447：

```bash
bash scripts/run-swebench.sh \
  --model gemini/gemini-3-flash-preview \
  --instance django__django-13447 \
  --config configs/e2b-swebench.yaml
```

运行结果（trajectory + patch）保存在 `results/` 目录。

## 多模型切换

### Gemini 模型（直连）

```bash
# 设置 GEMINI_API_KEY
bash scripts/run-swebench.sh \
  --model gemini/gemini-3-pro-preview \
  --config configs/e2b-swebench.yaml \
  --instance django__django-13447
```

### TokenHub 模型

```bash
# 所有 TokenHub 模型都需要设置 OPENAI_API_KEY
export OPENAI_API_KEY=$TOKENHUB_API_KEY

# GLM-5
bash scripts/run-swebench.sh \
  --model openai/glm-5 \
  --config configs/e2b-tokenhub.yaml \
  --instance django__django-13447

# MiniMax M2.7
bash scripts/run-swebench.sh \
  --model openai/minimax-m2.7 \
  --config configs/e2b-tokenhub.yaml \
  --instance django__django-13447

# DeepSeek V3.2（通过 TokenHub）
bash scripts/run-swebench.sh \
  --model openai/deepseek-v3.2 \
  --config configs/e2b-tokenhub.yaml \
  --instance django__django-13447
```

### TokenHub Thinking 模型（需禁用 thinking）

```bash
export OPENAI_API_KEY=$TOKENHUB_API_KEY

# Kimi K2.5
bash scripts/run-swebench.sh \
  --model openai/kimi-k2.5 \
  --config configs/e2b-kimi.yaml \
  --instance django__django-13447

# DeepSeek R1-0528
bash scripts/run-swebench.sh \
  --model openai/deepseek-r1-0528 \
  --config configs/e2b-kimi.yaml \
  --instance django__django-13447

# 混元 2.0 Thinking
bash scripts/run-swebench.sh \
  --model openai/hunyuan-2.0-thinking-20251109 \
  --config configs/e2b-kimi.yaml \
  --instance django__django-13447
```

### DeepSeek 直连

```bash
# 设置 DEEPSEEK_API_KEY

# DeepSeek Chat
bash scripts/run-swebench.sh \
  --model deepseek/deepseek-chat \
  --config configs/e2b-deepseek.yaml \
  --instance django__django-13447

# DeepSeek Reasoner（需禁用 thinking）
bash scripts/run-swebench.sh \
  --model deepseek/deepseek-reasoner \
  --config configs/e2b-deepseek-reasoner.yaml \
  --instance django__django-13447
```

## 并发评测

使用 `scripts/run-concurrent.py` 可多模型、多实例并发评测，支持沙箱预创建、TUI 实时监控。

### 单模型并发

```bash
# GLM-5-Turbo 并发 10 个沙箱解同一题
export OPENAI_API_KEY=$TOKENHUB_API_KEY

python scripts/run-concurrent.py swebench \
  -m openai/glm-5-turbo \
  --instances django__django-13447 \
  --repeat 10 \
  --pre-create --pre-create-workers 10 \
  -w 10
```

### 多模型并发

```bash
# 5 个模型各并发 2 次 = 10 个任务同时运行
python scripts/run-concurrent.py swebench \
  -m openai/glm-5,openai/glm-5-turbo,openai/kimi-k2.5,openai/deepseek-v3.2,openai/minimax-m2.7 \
  --instances django__django-13447 \
  --repeat 2 \
  --pre-create --pre-create-workers 10 \
  -w 10
```

### 全部 TokenHub 模型并发

```bash
# tokenhub 关键字展开全部 7 个模型，每模型 20 次 = 140 任务
python scripts/run-concurrent.py swebench \
  -m tokenhub \
  --instances django__django-13447 \
  --repeat 20 \
  --pre-create --pre-create-workers 50 \
  -w 140 \
  --max-rows 0
```

### 纯沙箱性能压测

```bash
# 跳过 LLM 调用，只压测沙箱创建/销毁
python scripts/run-concurrent.py swebench \
  -m tokenhub \
  --instances django__django-13447 \
  --repeat 15 \
  --pre-create --pre-create-workers 50 \
  -w 105 \
  --sandbox-only --max-rows 0
```

### 常用参数

| 参数 | 说明 |
|------|------|
| `-m` | 模型名（逗号分隔多个，或 `tokenhub` 表示全部 TokenHub 模型） |
| `--repeat N` | 每个模型重复 N 次 |
| `--pre-create` | 多进程预创建沙箱，任务启动时直连 |
| `--pre-create-workers N` | 预创建并发数 |
| `-w N` | 任务执行并发数 |
| `--step-limit N` | 限制 Agent 最大步数 |
| `--sandbox-only` | 跳过 LLM，纯沙箱压测 |
| `--max-rows N` | TUI 显示行数（`0` 显示全部） |

> 详细参数说明见 [scripts/README.md](scripts/README.md)

## SSL 证书配置（自部署平台）

如果 cube-sandbox 使用 mkcert 证书，需要安装 root CA：

```bash
# 从 cube-sandbox 节点获取 root CA
ssh <cube-node> 'cat /root/.local/share/mkcert/rootCA.pem' \
  > /etc/pki/ca-trust/source/anchors/cube-rootCA.pem

# 安装到系统信任链
sudo update-ca-trust

# 在 .env 中设置（注意使用 CUBE_SSL_CERT_FILE 而非 SSL_CERT_FILE）
CUBE_SSL_CERT_FILE="/etc/pki/tls/cert.pem"
```

> **注意**：使用 `CUBE_SSL_CERT_FILE` 而非 `SSL_CERT_FILE`，避免全局覆盖 Python 的 CA 证书包导致访问 HuggingFace 等公网站点时 SSL 验证失败。脚本会在连接 cube-sandbox 时自动将其设置为 `SSL_CERT_FILE`。

## 目录结构

```
cube-sandbox-rl-example/
├── README.md                        # 本文件
├── docs/
│   └── PRD.md                       # 产品需求文档（含 RL 愿景）
├── mini-swe-agent-patch/            # mini-swe-agent E2B 改造代码
│   ├── README.md                    # 补丁说明
│   ├── install.sh                   # 一键安装脚本
│   ├── environments/
│   │   ├── __init__.py              # 注册 e2b 环境类型
│   │   └── extra/
│   │       └── e2b.py               # E2BEnvironment 实现
│   └── run/benchmarks/
│       └── swebench.py              # SWE-bench 入口（添加 e2b 支持）
├── configs/
│   ├── e2b-swebench.yaml            # 基础配置（Gemini 等直连模型）
│   ├── e2b-tokenhub.yaml            # TokenHub 模型配置
│   ├── e2b-kimi.yaml                # Kimi K2.5 专用配置
│   ├── e2b-deepseek.yaml            # DeepSeek Chat 配置
│   └── e2b-deepseek-reasoner.yaml   # DeepSeek Reasoner 配置
├── scripts/
│   ├── setup-env.sh                 # 一键环境准备
│   ├── inject-envd.sh               # envd 注入 SWE-bench 镜像
│   ├── run-swebench.sh              # 运行单次 SWE-bench 评测
│   ├── run-concurrent.py            # 并发评测（多模型/多实例/预创建）
│   ├── run-all-tokenhub.sh          # 并行跑全部 TokenHub 模型
│   └── check-instances.sh           # 检查/清理沙箱实例
├── envd-inject/
│   └── Dockerfile                   # envd 注入 Dockerfile
├── .env.example                     # 环境变量模板
└── requirements.txt                 # Python 依赖
```

## 已验证的模型表现

| 模型 | 步骤 | 费用 | 耗时 | 结果 |
|------|------|------|------|------|
| DeepSeek Chat | 35 | $0.02 | 253s | 成功 |
| DeepSeek Reasoner | 75 | $0.05 | 389s | 成功 |
| DeepSeek V3.2 | - | - | - | 待测 |
| DeepSeek R1-0528 | - | - | - | 待测 |
| Kimi K2.5 | 42 | $0.93 | 179s | 成功 |
| Gemini 3 Pro | 22 | $0.26 | 224s | 成功 |
| Gemini 3 Flash | 46 | $0.19 | 278s | 成功 |
| MiniMax M2.7 | 56 | $1.62 | 363s | 成功 |
| GLM-5 | 41 | $0.75 | 400s | 成功 |
| 混元 2.0 Thinking | - | - | - | 待测 |

> 测试题目：django__django-13447，环境：cube-sandbox E2B 沙箱
