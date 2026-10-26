# cube-sandbox 脚本与运维手册

本目录包含 cube-sandbox 示例项目的自动化脚本。所有脚本均支持 `--help` 查看用法。

## 脚本一览

| 脚本 | 用途 | 需要 cubecli | 需要 Docker |
|------|------|:---:|:---:|
| [setup-env.sh](#setup-envsh) | 一键初始化运行环境 | | |
| [inject-envd.sh](#inject-envdsh) | 将 envd 注入镜像并构建 | | ✅ |
| [run-swebench.sh](#run-swebenchsh) | 运行 SWE-bench 评测（单模型） | | |
| [run-all-tokenhub.sh](#run-all-tokenhubsh) | 并行运行所有 TokenHub 模型 | | |
| [check-instances.sh](#check-instancessh) | 查询/清理沙箱实例 | ✅ | |
| [run-concurrent.py](#run-concurrentpy) | 高并发运行 + 实时 TUI 仪表盘 | (可选) | |

---

## setup-env.sh

一键初始化 Python 环境、安装依赖、检查配置。

```bash
bash scripts/setup-env.sh
```

**执行内容：**
1. 检查 Python 版本（需 3.10+）
2. `pip install -r requirements.txt`
3. 验证 litellm / e2b-code-interpreter / mini-swe-agent 安装
4. 检查 `.env` 文件是否存在，不存在则从模板创建
5. 检查 SSL 证书配置

---

## inject-envd.sh

将 envd 二进制注入到 Docker 镜像中，使其兼容 cube-sandbox。

```bash
bash scripts/inject-envd.sh <base-image> [output-tag]
```

**参数：**

| 参数 | 必填 | 说明 |
|------|:---:|------|
| `base-image` | ✅ | 源镜像，如 `swebench/sweb.eval.x86_64.django_1776_django-13447:latest` |
| `output-tag` | | 输出镜像标签，默认为 `<base-image>-envd:latest` |

**示例：**

```bash
# 注入 SWE-bench 镜像
bash scripts/inject-envd.sh swebench/sweb.eval.x86_64.django_1776_django-13447:latest

# 自定义输出标签
bash scripts/inject-envd.sh ubuntu:22.04 my-ubuntu-envd:latest
```

**注入后需要注册为 cube-sandbox 模板，获得 `template_id` 填入 `.env`。**

---

## run-swebench.sh

使用指定 LLM 模型在 cube-sandbox 沙箱中运行 SWE-bench 评测。

```bash
bash scripts/run-swebench.sh --model <model> --instance <instance-id> [options]
```

**参数：**

| 参数 | 必填 | 默认值 | 说明 |
|------|:---:|--------|------|
| `--model` | ✅ | | LLM 模型名，如 `deepseek/deepseek-chat` |
| `--instance` | ✅ | | SWE-bench 实例 ID，如 `django__django-13447` |
| `--config` | | `configs/e2b-swebench.yaml` | YAML 配置文件 |
| `--subset` | | `lite` | 数据集子集：`lite` / `verified` / `full` |
| `--split` | | `test` | 数据集分割：`test` / `dev` |
| `--step-limit` | | 配置文件中的值 | 最大 Agent 步数，覆盖配置文件 |
| `--output` | | `results/` | 输出目录 |

**各模型运行示例：**

```bash
# DeepSeek Chat
bash scripts/run-swebench.sh \
  --model deepseek/deepseek-chat \
  --config configs/e2b-deepseek.yaml \
  --instance django__django-13447

# DeepSeek Reasoner
bash scripts/run-swebench.sh \
  --model deepseek/deepseek-reasoner \
  --config configs/e2b-deepseek-reasoner.yaml \
  --instance django__django-13447

# Gemini
bash scripts/run-swebench.sh \
  --model gemini/gemini-3-flash-preview \
  --config configs/e2b-swebench.yaml \
  --instance django__django-13447

# TokenHub 模型（GLM-5 / MiniMax）
export OPENAI_API_KEY=$TOKENHUB_API_KEY
bash scripts/run-swebench.sh \
  --model openai/glm-5 \
  --config configs/e2b-tokenhub.yaml \
  --instance django__django-13447

# Kimi K2.5
export OPENAI_API_KEY=$TOKENHUB_API_KEY
bash scripts/run-swebench.sh \
  --model openai/kimi-k2.5 \
  --config configs/e2b-kimi.yaml \
  --instance django__django-13447
```

**输出文件：**
- `results/<model>/<instance>/trajectory.json` — Agent 交互轨迹
- `results/<model>/<instance>/run.log` — 运行日志

---

## run-all-tokenhub.sh

并行运行所有 TokenHub 模型评测，自动选择对应配置。

```bash
bash scripts/run-all-tokenhub.sh [options]
```

**参数：**

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--instance` | `django__django-13447` | SWE-bench 实例 ID |
| `--subset` | `lite` | 数据集子集 |
| `--split` | `test` | 数据集分割 |
| `--step-limit` | 配置文件中的值 | 最大 Agent 步数，覆盖配置文件 |

**包含的模型：**

| 模型 | 配置文件 | 类型 |
|------|---------|------|
| `openai/glm-5` | `e2b-tokenhub.yaml` | 普通 |
| `openai/minimax-m2.7` | `e2b-tokenhub.yaml` | 普通 |
| `openai/deepseek-v3.2` | `e2b-tokenhub.yaml` | 普通 |
| `openai/kimi-k2.5` | `e2b-kimi.yaml` | Thinking（已禁用） |
| `openai/deepseek-r1-0528` | `e2b-kimi.yaml` | Thinking（已禁用） |
| `openai/hunyuan-2.0-thinking-20251109` | `e2b-kimi.yaml` | Thinking（已禁用） |

**示例：**

```bash
# 使用默认实例和配置
bash scripts/run-all-tokenhub.sh

# 指定实例
bash scripts/run-all-tokenhub.sh --instance django__django-13447

# 指定实例和步数限制
bash scripts/run-all-tokenhub.sh --instance django__django-13447 --step-limit 50
```

**输出：**
- 每个模型的日志：`results/_parallel_logs/<model>.log`
- 评测结果：`results/<model>/<instance>/`
- 运行结束后输出汇总（成功/失败数量）

---

## check-instances.sh

按模板 ID 查询正在运行的沙箱实例，支持批量清理。

```bash
bash scripts/check-instances.sh --template <template-id> [--kill]
```

**参数：**

| 参数 | 必填 | 说明 |
|------|:---:|------|
| `--template` | ✅ | 模板 ID，如 `tpl-c301a4f1b99d4a1f87deb7d4` |
| `--kill` | | 批量停止匹配的实例（默认仅列出） |

**示例：**

```bash
# 查看使用某模板的实例
bash scripts/check-instances.sh --template tpl-c301a4f1b99d4a1f87deb7d4

# 批量清理
bash scripts/check-instances.sh --template tpl-c301a4f1b99d4a1f87deb7d4 --kill
```

---

## run-concurrent.py

单机高并发任务运行器，提供实时 TUI 仪表盘，支持两种模式：

- **benchmark** – 压测模式：纯粹测试沙箱创建/销毁吞吐，无需 LLM
- **swebench** – 评测模式：高并发运行 SWE-bench RL 任务

### TUI 仪表盘

运行时实时展示：

| 面板 | 内容 |
|------|------|
| Header | 模式、并发数、模板 ID、总耗时 |
| Stats | 进度条、任务状态统计、创建耗时分布 (avg/p50/p95/max)、总 Cost |
| Tasks | 每个任务的状态、创建耗时、运行时间、Steps/Cost、Sandbox ID |
| cubecli ls | 实时 microVM 实例列表（自动刷新，需要 cubecli） |

运行结束后输出汇总报告，包含创建耗时直方图分布和失败任务详情。

### Benchmark 模式

测试沙箱创建/销毁性能，无需配置 LLM。

```bash
python scripts/run-concurrent.py benchmark [options]
```

**参数：**

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-w, --workers` | `4` | 并发数 |
| `-n, --count` | `10` | 创建沙箱数量 |
| `--template` | `.env` 中的值 | 模板 ID |
| `--run-cmd` | `echo ok` | 在沙箱中执行的验证命令 |
| `--keep` | | 不销毁沙箱（用于后续 cubecli 检查） |
| `--cubecli-interval` | `10` | cubecli ls 刷新间隔秒数，`0` 禁用 |

**示例：**

```bash
# 10 并发创建 20 个沙箱
python scripts/run-concurrent.py benchmark -w 10 -n 20

# 50 并发压测，保留沙箱不销毁
python scripts/run-concurrent.py benchmark -w 50 -n 100 --keep

# 指定模板 ID
python scripts/run-concurrent.py benchmark -w 8 -n 30 \
    --template tpl-c301a4f1b99d4a1f87deb7d4
```

### SWE-bench 模式

高并发运行 SWE-bench 评测任务，每个任务独立创建沙箱并执行 Agent。

```bash
python scripts/run-concurrent.py swebench [options]
```

**参数：**

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-w, --workers` | `4` | 并发数 |
| `-m, --model` | (必填) | LLM 模型名 |
| `-c, --config` | (必填) | YAML 配置文件 |
| `--subset` | `lite` | 数据集子集：`lite` / `verified` / `full` |
| `--split` | `test` | 数据集分割 |
| `--slice` | | 实例切片，如 `0:5`（前 5 个） |
| `--filter` | | 实例 ID 正则过滤 |
| `--instances` | | 指定实例 ID（逗号分隔） |
| `--repeat` | `1` | 每个实例重复运行次数（并发压测用） |
| `--template-map` | | 模板映射 JSON 文件 |
| `--step-limit` | 配置文件中的值 | 最大 Agent 步数 |
| `-o, --output` | `results/concurrent-<model>-<timestamp>` | 输出目录 |
| `--cubecli-interval` | `10` | cubecli ls 刷新间隔秒数，`0` 禁用 |

**示例：**

```bash
# DeepSeek 模型，4 并发跑前 10 个实例
python scripts/run-concurrent.py swebench -w 4 \
    -m deepseek/deepseek-chat \
    -c configs/e2b-deepseek.yaml \
    --slice 0:10

# 同一实例并发压测：10 并发跑 django__django-13447
python scripts/run-concurrent.py swebench -w 10 \
    -m deepseek/deepseek-chat \
    -c configs/e2b-deepseek.yaml \
    --instances django__django-13447 --repeat 10

# GLM-5，8 并发跑指定实例
export OPENAI_API_KEY=$TOKENHUB_API_KEY
python scripts/run-concurrent.py swebench -w 8 \
    -m openai/glm-5 \
    -c configs/e2b-tokenhub.yaml \
    --instances django__django-13447,django__django-13710

# 使用模板映射文件（多模板场景）
python scripts/run-concurrent.py swebench -w 6 \
    -m gemini/gemini-3-flash-preview \
    -c configs/e2b-swebench.yaml \
    --template-map configs/template-mapping.json \
    --filter 'django__.*' --slice 0:20
```

**模板映射 (`--template-map`)：**

当不同 SWE-bench 实例需要不同的 cube-sandbox 模板时，使用 JSON 文件映射：

```json
{
    "django__django-13447": "tpl-c301a4f1b99d4a1f87deb7d4",
    "astropy__astropy-12907": "tpl-another-template-id"
}
```

未列出的实例使用 `.env` 中的 `CUBE_TEMPLATE_ID` 作为默认值。参见 `configs/template-mapping.json`。

**输出文件：**
- `results/concurrent-<model>-<ts>/preds.json` — 所有实例的预测结果
- `results/concurrent-<model>-<ts>/<instance>/` — 各实例的 trajectory
- `results/concurrent-<model>-<ts>/<instance>_runN/` — repeat 模式下各轮次结果
- `results/concurrent-<model>-<ts>/runner.log` — 运行日志

---

## cubecli 常用命令

以下是 cube-sandbox 平台 CLI 工具 `cubecli` 的常用操作速查。

### 实例管理

```bash
# 列出所有运行中的实例
cubecli ls

# 查看实例详情（包括模板 ID、镜像信息等）
cubecli ctr info <cubebox_id>

# 销毁实例
cubecli unsafe destroy <sandbox_id>

# 按模板 ID 过滤实例
cubecli ctr info <cubebox_id> | grep <template_id>
```

### 模板管理

```bash
# 从镜像创建模板
cubemastercli tpl create-from-image \
  --image cube-sandbox-image.tencentcloudcr.com/demo/<image-name>:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --cpu 4000 --memory 8192 \
  --probe 49983

# 列出所有模板
cubecli tpl ls

# 查看模板详情
cubecli tpl info <template_id>
```

**创建模板参数说明：**

| 参数 | 说明 |
|------|------|
| `--image` | 已注入 envd 的镜像地址 |
| `--writable-layer-size` | 可写层大小，建议 `1G` |
| `--expose-port` | 暴露端口，envd 默认监听 `49983` |
| `--cpu` | CPU 配额（毫核），`4000` = 4 核 |
| `--memory` | 内存配额（MB），`8192` = 8GB |
| `--probe` | 健康检查端口，与 envd 端口一致 |

创建成功后会返回 `template_id`（如 `tpl-c301a4f1b99d4a1f87deb7d4`），填入 `.env` 的 `CUBE_TEMPLATE_ID`。

### 输出示例

```
$ cubecli ls
NS         CONTAINER       CUBEBOX         TYPE       STATUS    IMAGE                           CREATED
default    3a983cf8b3d9    3a983cf8b3d9    sandbox    Up        rfs-6dec1b0a8647284088b65794    2026-04-05 17:34:10
default    8e7dd9d7ce12    8e7dd9d7ce12    sandbox    Up        rfs-6dec1b0a8647284088b65794    2026-04-05 17:33:22
default    5bfabe35b991    5bfabe35b991    sandbox    Up        rfs-afaca17f5277573de0ef5d76    2026-04-05 15:38:40
```

---

## 故障排查

### SSL 证书错误

```
SSL: CERTIFICATE_VERIFY_FAILED
```

**原因**：`SSL_CERT_FILE` 被设置为 mkcert 的 `rootCA.pem`，覆盖了系统 CA。

**解决**：使用 `CUBE_SSL_CERT_FILE` 替代，见 [主 README](../README.md#ssl-证书配置自部署平台)。

### HuggingFace 数据集下载超时

```
ConnectionError: Connection timed out
```

**解决**：`.env` 中设置 `HF_ENDPOINT="https://hf-mirror.com"`。

### LLM API 连接失败

```
litellm.InternalServerError: Connection error
```

**排查**：
```bash
# 测试 API 连通性
curl -s -o /dev/null -w "%{http_code}" https://tokenhub.tencentmaas.com/v1/chat/completions
curl -s -o /dev/null -w "%{http_code}" https://api.deepseek.com/v1/chat/completions
```

### 模型端点不可用

```
litellm.APIError: endpoint is inactive
```

**原因**：TokenHub 上的模型端点被禁用或下线。登录 TokenHub 后台确认模型状态，或换用其他模型。

### 残留沙箱实例

评测异常退出后可能残留沙箱实例，使用 `check-instances.sh` 清理：

```bash
bash scripts/check-instances.sh --template <your-template-id> --kill
```
