---
title: OpenCode 集成指南
author: Sam-Hui-dot
date: 2026-07-05
tags:
  - integration
  - opencode
  - coding-agent
  - agent
lang: zh-CN
---

# OpenCode 集成指南

[English](../../../guide/integrations/opencode.md)

本指南介绍如何在 CubeSandbox MicroVM 中运行
[OpenCode](https://opencode.ai/) 这类开源终端编码 Agent。可运行示例位于
[`examples/opencode-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/opencode-integration)。

## 集成目标与版本

| 组件 | 版本 |
|---|---|
| OpenCode CLI | 通过 `OPENCODE_VERSION` 固定的 `opencode-ai` 1.17.13 |
| Node.js | Node.js 官方 22.23.1 归档，经过 SHA-256 校验 |
| CubeSandbox 基础镜像 | `ghcr.io/tencentcloud/cubesandbox-base:2026.16`，通过 digest 固定 |
| 宿主端 SDK | `e2b==2.7.0`、`cubesandbox==0.3.0` |
| CubeSandbox 平台 | pause/resume 需要 `>=0.3.0`；header 注入需要 CubeEgress |

OpenCode 官方的非交互模式是 `opencode run [message..]`。本示例使用：

```bash
opencode run --model <provider/model> --dir /workspace --auto --format json "<prompt>"
```

## 为什么放进 CubeSandbox

OpenCode 可以编辑文件、执行命令、安装依赖并访问 LLM API。放入 CubeSandbox 后，
每次编码任务都拥有独立 MicroVM 边界：

| 关注点 | CubeSandbox 模式 |
|---|---|
| 隔离性 | 每个任务或会话一个 MicroVM |
| 可复现 | 从固定模板镜像启动 |
| 快速复用 | pause/resume 保留 `/workspace` 和 OpenCode 状态 |
| 网络控制 | 默认拒绝出网，只放行 LLM host |
| 密钥处理 | CubeEgress 在链路上注入 API key |
| 可审查 | 宿主端脚本验证生成文件和测试结果 |

## 前置条件

- 已部署 CubeSandbox，CubeAPI 可通过 `http://<node>:3000` 访问。
- `cubemastercli` 已在 `$PATH` 中。
- Docker 和 Cube 节点可拉取的镜像仓库。
- 宿主端 Python 3.10+。
- 一个 OpenCode 支持的 LLM provider API key。

## 集成步骤

### 1. 构建模板镜像

```bash
cd examples/opencode-integration
docker build --platform linux/amd64 \
  -t <registry>/opencode-cube:latest .
docker push <registry>/opencode-cube:latest
```

镜像基于 CubeSandbox base，安装 Node.js、OpenCode、`git`、`ripgrep`、Python
以及少量排查工具。

### 2. 注册 Cube 模板

```bash
cubemastercli tpl create-from-image \
  --image <registry>/opencode-cube:latest \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

基础镜像入口会保持 envd 监听 `49983`，并提供标准 `/health` endpoint。

### 3. 配置宿主端脚本

```bash
cp .env.example .env
pip install -r requirements.txt
```

填写：

```bash
E2B_API_URL=http://<node>:3000
E2B_API_KEY=e2b_000000
CUBE_TEMPLATE_ID=<template-id>
OPENCODE_MODEL=openai/gpt-4.1-mini
OPENAI_API_KEY=<provider-key>
```

自定义端点可设置 `OPENCODE_BASE_URL`；如果未设置，脚本也接受 provider-specific
变量，例如 `OPENAI_BASE_URL`。脚本会在沙箱工作目录写入 `opencode.json`，
配置 `provider.<provider_prefix>.options.baseURL`。

如果使用本仓库的 `dev-env/` VM 验证，设置 `CUBE_DEV_SIDECAR=1`，让宿主端 SDK
通过 `examples/e2b-dev-sidecar` 转发 sandbox 流量。

`network_policy.py` 和 `checkpoint_fork_opencode.py` 使用原生 `cubesandbox` SDK。
用 `dev-env/` 跑任一脚本时，还需要设置：

```bash
CUBE_API_URL=http://127.0.0.1:13000
CUBE_PROXY_NODE_IP=127.0.0.1
CUBE_PROXY_PORT_HTTP=11080
```

### 4. 运行一次性编码任务

```bash
python3 run_opencode.py --dry-run
python3 run_opencode.py
```

脚本会放入一个小型 Python 项目，运行 OpenCode，并验证：

- `calculator.add` 已实现。
- `python3 -m unittest discover -v` 通过。
- `result.md` 包含 `OPENCODE_CUBE_OK`。

### 5. 演示会话保持

```bash
python3 resume_opencode.py
```

第一轮写入 `plan.md`；`sandbox.pause()` 会保存 VM 和根文件系统快照。脚本再连接
同一个 sandbox，验证 `/workspace` 和 OpenCode 状态目录仍存在，然后继续任务并
检查 `OPENCODE_RESUME_OK`。

### 6. 创建 checkpoint、fork 并继续任务

```bash
python3 checkpoint_fork_opencode.py
```

该流程在 source sandbox 中运行 OpenCode，在第一轮后创建显式 snapshot，使用
`template=snapshot_id` 启动新 sandbox，并在 fork 中继续同一个 OpenCode 会话。
它证明 workspace 和 OpenCode 状态已继承、source 与 fork 相互独立，最后删除临时
snapshot。两个 sandbox 都使用默认拒绝出网与同一条 LLM host 放行规则；真实 provider
凭证由 CubeEgress 在链路上注入。脚本会验证两次 OpenCode 命令环境中都只有占位 key，
且无关 host 始终被阻断。

### 7. 限制出网并注入凭证

```bash
python3 network_policy.py
```

严格模式使用原生 `cubesandbox` SDK：

- `allow_internet_access=False` 将出网设为默认拒绝。
- CubeEgress rule 只允许配置的 LLM host。
- `Inject` 在 HTTP header 中注入真实 provider 凭证。
- VM 全局环境中没有 provider key。
- OpenCode 命令环境中只能看到占位 key。
- 如果任一 secret 条件不满足，或无关 host 可以访问，脚本会直接失败。

当 LLM API host 与 provider 默认值不一致时（尤其使用 `OPENCODE_BASE_URL`
或 provider-specific `<PROVIDER>_BASE_URL` 时），请设置 `OPENCODE_LLM_HOST`：

```bash
OPENCODE_LLM_HOST=api.openai.com python3 network_policy.py
```

可用 `--skip-agent` 只验证策略，不消耗 LLM token：

```bash
python3 network_policy.py --skip-agent
```

## 关键代码片段

构造非交互 OpenCode 命令：

```python
opencode_command(
    prompt,
    workspace="/workspace",
    model="openai/gpt-4.1-mini",
    title="cube-opencode-demo",
)
```

受限出网规则：

```python
Rule(
    name="allow_openai_llm",
    match=Match(scheme="https", sni="api.openai.com", host="api.openai.com"),
    action=Action(
        allow=True,
        audit="metadata",
        inject=[Inject(
            header="Authorization",
            secret=secret,
            format="Bearer ${SECRET}",
        )],
    ),
)
```

## 注意事项

- live 运行仍需要真实 LLM provider key。
- `--auto` 会让 OpenCode 无需交互确认即可编辑和执行命令，应放在隔离沙箱内使用。
- `run_opencode.py` 的直接 env 注入适合本地验证；它只传递当前 provider 的 key，
  并会在失败输出中脱敏已知 secret。共享集群建议使用 `network_policy.py` 或
  `checkpoint_fork_opencode.py`。
- 自定义 provider 取决于 OpenCode provider 配置以及目标端点对所选模型的兼容性。
- 示例固定 Node.js 官方归档并在解压前校验 SHA-256。更新时应对照 Node.js
  `SHASUMS256.txt` 同时修改 `NODE_VERSION` 和 `NODE_SHA256`。

## 验证

在 `examples/opencode-integration` 下执行：

```bash
python3 -m py_compile env_utils.py _opencode_common.py egress_policy.py run_opencode.py resume_opencode.py checkpoint_fork_opencode.py network_policy.py
python3 -m unittest discover . -p 'test_*.py'
bash -n build-template.sh
docker build --platform linux/amd64 -t opencode-cube:verify .
docker run --rm opencode-cube:verify opencode --version
```

live CubeSandbox 验证：

```bash
python3 run_opencode.py
python3 resume_opencode.py
python3 checkpoint_fork_opencode.py
python3 network_policy.py --skip-agent
python3 network_policy.py
```

## 参考

- [OpenCode CLI 文档](https://opencode.ai/docs/cli/)
- [OpenCode provider 文档](https://opencode.ai/docs/providers/)
- [CubeSandbox OpenCode 示例](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/opencode-integration)
- [CubeSandbox Security Proxy](../../../guide/security-proxy.md)
