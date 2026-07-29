---
title: OpenCode 集成指南
author: blues-kun
date: 2026-07-29
tags:
  - integration
  - opencode
  - coding-agent
  - agent
lang: zh-CN
---

# OpenCode 集成指南

[English](../../../guide/integrations/opencode.md)

本指南把 OpenCode 终端编码 Agent 运行在 CubeSandbox MicroVM 中，覆盖可复现模板、
无交互执行、敏感配置、默认拒绝出口，以及工作区与 OpenCode 会话的暂停恢复。配套代码位于
[`examples/opencode-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/opencode-integration)。

## 集成目标与版本

| 组件 | 验证版本 |
|---|---|
| OpenCode | `1.18.9` |
| CubeSandbox 基础镜像 | `ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| CubeSandbox 平台 | 暂停恢复 `>= 0.3.0`；CubeEgress 保险库 `>= 0.4.0` |
| 主机 SDK | `e2b 2.35.0`、`cubesandbox 0.6.0` |
| 示例模型 | 腾讯云 TokenHub Hy3，OpenAI-compatible Chat Completions |

OpenCode 2 当前仍是 beta，配置格式已经变化。本示例固定稳定版 OpenCode 1：自定义模型位于
单数 `provider` 下，并使用 `npm` 与 `options.baseURL`。

## 为什么把编码 Agent 放进 CubeSandbox

OpenCode 能读取和编辑文件、执行 shell 及调用嵌套工具。CubeSandbox 把这些动作限制在独立的
KVM MicroVM 中：

| 要求 | CubeSandbox 控制 |
|---|---|
| 主机隔离 | 独立 Guest Kernel 与可写层 |
| 可复现 | 固定 Agent 二进制与版本化模板 |
| 长任务 | `pause()` 快照内存与根文件系统，`connect()` 恢复 |
| 凭据最小暴露 | 单进程环境注入或 CubeEgress 凭据保险库 |
| 网络治理 | 默认拒绝出口、主机白名单与审计 |

OpenCode 权限配置只是操作便利层，不是安全边界。shell 命令存在多种等价写法，真正的隔离仍由
MicroVM 与出口策略承担。

## 前置条件

- CubeSandbox 已部署，CubeAPI 可通过 `http://<node>:3000` 访问；
- `cubemastercli` 已连接集群；
- Docker 可向所有 Cube 节点可访问的仓库推送镜像；
- 主机具有 Python 3.10+；
- 具备 OpenAI-compatible 模型 Key 与 Base URL。示例默认 TokenHub Hy3。

## 架构

```text
主机驱动
  |  create(template)
  v
CubeSandbox MicroVM
  |-- /workspace                      项目与产物
  |-- /root/.local/share/opencode     会话与本地状态
  |-- opencode run --format json      无交互事件流
  |
  +--> CubeEgress --> 唯一放行的模型主机
          |
          +-- 注入 Authorization
          +-- 审计请求元数据
```

## 集成步骤

### 1. 构建模板

Dockerfile 继承官方 Cube 基础镜像，保留 `49983` 上的 envd，并复用基础镜像已有的
`bash`、CA 证书与 `curl`，只安装新增编码工具。OpenCode x86-64 发布包与 SHA256
均固定；非 `amd64` 构建会提前失败，而不会静默安装不兼容二进制。`.dockerignore`
使用白名单，只发送 Dockerfile 与 V1 配置，防止 `.env`、虚拟环境、测试和缓存进入构建上下文：

```bash
IMAGE=<your-registry>/opencode-cube:1.18.9 PUSH=1 \
  ./examples/opencode-integration/build-template.sh
```

镜像关闭自动更新、会话分享、models.dev 更新、外部插件和 LSP 下载，消除隐式运行时域名，
使默认拒绝出口可以只放行模型 API。

### 2. 注册 Cube 模板

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/opencode-cube:1.18.9 \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health

cubemastercli tpl watch --job-id <job_id>
```

需要在会话内安装大型编译器或依赖缓存时，把可写层提高到 `8G+`。任务到达 `READY` 后记录
`template_id`。

### 3. 配置 OpenCode

源码命名为 `opencode.v1.json`，显式绑定 OpenCode 1.x；Dockerfile 再把它复制到标准运行路径
`/root/.config/opencode/opencode.json`：

```json
{
  "model": "tokenhub/hy3",
  "autoupdate": false,
  "share": "disabled",
  "provider": {
    "tokenhub": {
      "npm": "@ai-sdk/openai-compatible",
      "options": {
        "baseURL": "{env:HY3_BASE_URL}",
        "apiKey": "{env:HY3_API_KEY}"
      },
      "models": {
        "hy3": {
          "name": "Hy3"
        }
      }
    }
  }
}
```

镜像只保存环境变量引用；主机 `.env` 被 Git 忽略：

```dotenv
E2B_API_URL=http://<cube-host>:3000
E2B_API_KEY=e2b_000000
CUBE_TEMPLATE_ID=<template-id>
HY3_API_KEY=<your-tokenhub-key>
HY3_BASE_URL=https://tokenhub.tencentmaas.com/v1
HY3_MODEL=hy3
```

### 4. 运行无交互修复任务

```bash
cd examples/opencode-integration
python -m venv .venv
.venv/bin/pip install -r requirements.txt
.venv/bin/python run_opencode.py
```

驱动写入一个已知缺陷和两个 unittest。Hy3 驱动 OpenCode 先运行失败测试、读取文件、只修改
实现并复测。随后主机独立断言：

- `tests/test_stats.py` 未被修改；
- 目标 unittest 通过；
- `git diff --check` 通过；
- `result.md` 存在且非空。

实际命令：

```bash
opencode run \
  --pure \
  --auto \
  --format json \
  --model tokenhub/hy3 \
  "<task>"
```

`--pure` 禁止外部插件。`--auto` 在一次性 MicroVM 中免去交互审批，但配置仍拒绝常见 push、
提权、外部目录与联网工具，并拒绝直接调用 `rm`、`/bin/rm`、`/usr/bin/rm` 与
`command rm`。包装器和等价工具仍可能绕过命令模式，因此这些规则不能替代沙箱隔离。

精简 JSONL 渲染器默认展示模型文本、工具与错误；传入 `--verbose-events` 可识别被省略的其他
事件类型，`--raw` 则原样保留全部事件。
若 SDK 接受但不触发回调参数，兼容层会在命令结束后把已收集的 stdout/stderr 交给同一处理器
补放；输出与会话捕获保持正确，但显示会退化为非实时。

## API Key 注入

### 直接模式

`run_opencode.py` 只在本次 exec 信封中注入 Key：

```python
result = sandbox.commands.run(
    command,
    envs={
        "HY3_API_KEY": tokenhub_key,
        "HY3_BASE_URL": "https://tokenhub.tencentmaas.com/v1",
        "HY3_MODEL": "hy3",
    },
)
```

这避免把凭据写入镜像。但 OpenCode 和子进程仍能读取环境变量，开放出口也允许外传，因此只适合
本地评估，不适合多租户生产。驱动会在创建沙箱前把该警告输出到 stderr。

### CubeEgress 保险库模式

`network_policy.py` 把真实 Key 留在主机，在网络出口注入：

```python
rules = [
    Rule(
        name="allow_tokenhub_hy3",
        match=Match(
            scheme="https",
            sni="tokenhub.tencentmaas.com",
            host="tokenhub.tencentmaas.com",
        ),
        action=Action(
            allow=True,
            audit="metadata",
            inject=[
                Inject(
                    header="Authorization",
                    secret=tokenhub_key,
                    format="Bearer ${SECRET}",
                )
            ],
        ),
    )
]

sandbox = Sandbox.create(
    template=template_id,
    allow_internet_access=False,
    network={"rules": rules},
)
```

脚本先验证沙箱全局环境中没有 `HY3_API_KEY`，再仅向 OpenCode 子进程传入
`HY3_API_KEY=cube-egress-managed-placeholder`。OpenCode 生成占位 Authorization，
CubeEgress 仅对命中主机的请求替换为真实凭据；其他目的地被拒绝并记录。

独立 OpenCode 运行时必须信任 CubeEgress 拦截 CA。示例同时设置 `SSL_CERT_FILE` 与
`NODE_EXTRA_CA_CERTS`，路径不同可覆盖 `OPENCODE_CA_BUNDLE`。

## 会话保持与状态持久化

```bash
.venv/bin/python resume_opencode.py
```

脚本按以下顺序工作：

1. 第一轮 JSONL 分块到达时同步捕获真实 `sessionID`，仅以 `result.stdout` 作兼容兜底；
2. Agent 进程退出后执行 `sandbox.pause()`；
3. 用 `Sandbox.connect(sandbox_id=...)` 恢复；
4. 验证 `/workspace/plan.md` 与 OpenCode 状态目录；
5. 使用 `opencode run --session <id>` 完成第二轮；
6. 在 `finally` 中销毁沙箱。

不要使用 `with Sandbox.create(...)` 包裹暂停恢复流程，退出上下文会直接 kill 沙箱。

该驱动使用进程级直接注入，因此与修复示例一样输出开放出口警告。它证明的是持久化机制；生产使用
前仍须与保险库出口控制组合。

## 典型场景与最佳实践

- **隔离仓库修复**：把仓库放入 `/workspace`，只导出经人工审核的补丁和测试日志；
- **并行候选方案**：快照准备好的工作区，再克隆多个沙箱比较不同修复；
- **长时重构**：里程碑间暂停并恢复真实 Agent 会话，而不是靠自然语言重建上下文；
- **不可信生成代码**：始终在 MicroVM 内执行，设置 CPU/超时并收集确定性测试产物；
- **依赖预装**：默认拒绝出口时不应临时访问包仓库，应在派生模板中烘焙常用工具链；
- **验收优先**：以测试与产物契约为准，不以 Agent 的最终自述作为成功依据。

## 常见问题

| 现象 | 可能原因 | 处理 |
|---|---|---|
| `opencode: command not found` | 模板早于本集成 | 重新构建并注册 |
| `Model not found: tokenhub/hy3` | V1 混入了 V2 配置 | 固定 `1.18.9` 与单数 `provider` |
| `Unsupported TARGETARCH=...` | x64 摘要用于非 amd64 构建 | 使用文档规定的 `linux/amd64` |
| 模型返回 `401` | 未注入 Key 或保险库规则错误 | 检查主机 `.env`/Authorization 注入 |
| 模型返回 `404` | `/v1` 缺失或重复 | Base URL 只以一个 `/v1` 结尾 |
| `403 Forbidden - CubeEgress` | 主机名未命中规则 | 从实际 `HY3_BASE_URL` 派生 SNI/Host |
| 保险库模式 TLS 错误 | 运行时不信任拦截 CA | 设置 `OPENCODE_CA_BUNDLE` |
| 出现额外联网请求 | 未禁用更新/模型表/插件 | 保留镜像环境变量与 `--pure` |
| 第一轮无 `sessionID` | 输出非 JSON 或请求中断 | 使用 `--format json` 并等待完成 |
| 无法恢复 | 平台不支持暂停恢复 | 升级 CubeSandbox 与 SDK |
| 模板停在 `PULLING` | 节点无法拉镜像 | 使用可达仓库并配置鉴权 |
| Agent 超时 | 模型或工具循环超限 | 先检查 JSONL，再谨慎提高超时 |

## 验证状态

2026-07-29 已用固定的 OpenCode `1.18.9` 和真实 Hy3 完成文本请求及一次原生 `read`
工具调用。离线测试覆盖 URL 校验、两阶段凭据边界、危险命令规则、模板依赖与架构约束、
最小构建上下文、OpenCode V1 配置绑定、运行时安全警告、命令引用、旧版 SDK 参数回退、
多 chunk 且结果 stdout 为空时的会话 ID 捕获、不触发回调的 SDK 补放，以及详细 JSONL
诊断。CubeEgress 与暂停恢复全链仍需在可访问的 CubeSandbox 集群中运行并保留证据后再合并。

## 参考

- 可运行示例：[`examples/opencode-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/opencode-integration)
- [自定义镜像](../tutorials/bring-your-own-image.md)
- [从镜像创建模板](../tutorials/template-from-image.md)
- [快照、克隆与回滚](../snapshot-rollback-clone.md)
- [安全代理](../security-proxy.md)
- [OpenCode 稳定版 Providers](https://opencode.ai/docs/providers/)
