# 接入 OpenClaw

[English](README.md)

部署 Cube Sandbox 并配置 OpenClaw skill，让 AI Agent 能够在隔离的 VM 环境中安全执行代码，无需任何额外搭建工作。

## 1. 背景

**Cube Sandbox** 是轻量级 MicroVM 平台，控制面和数据面完全兼容 [E2B SDK](https://e2b.dev)。`cube-sandbox` OpenClaw skill 封装了 E2B SDK，让任意 OpenClaw Agent 都能在一次性 KVM MicroVM 中执行任意代码和 Shell 命令。

```
OpenClaw Agent
    │  cube-sandbox skill
    ▼
E2B SDK（Python）
    │  REST API
    ▼
CubeAPI（端口 3000）
    │
    ▼
CubeMaster ──► Cubelet ──► KVM MicroVM
                               │
                           cube-agent（PID 1）
                               │
                           沙箱化代码
```

## 2. 核心能力

| 能力 | 说明 |
|------|------|
| **隔离执行** | 每个 Agent 任务运行在独立 MicroVM 中——独立内核、文件系统和网络 |
| **快速启动** | 从模板快照创建沙箱不到 50ms |
| **E2B 兼容** | 使用标准 E2B SDK，与所有 E2B 兼容工具链无缝集成 |
| **网络策略** | 支持出口 CIDR 白名单/黑名单及完全断网模式 |
| **暂停与恢复** | 快照运行中的沙箱，之后恢复（有状态复用） |
| **文件 I/O** | 通过 `sandbox.files` 读写沙箱内文件 |
| **宿主机挂载** | 通过 metadata 将宿主机目录挂载到沙箱 |

## 3. 前置条件

- 已部署的 Cube Sandbox 环境
- 已安装并运行的 OpenClaw Gateway
- Python 3.8+，已安装 `e2b-code-interpreter`

```bash
pip install e2b-code-interpreter
```

## 4. 配置步骤

### 第一步 — 部署 Cube Sandbox

参考以下部署文档，获取一个可用的 Cube Sandbox 实例：

- **[快速开始](https://cube-sandbox.pages.dev/zh/guide/quickstart)** — 最短路径：构建 → 部署 → 几分钟内启动第一个沙箱
- **[单机一键部署指南](https://cube-sandbox.pages.dev/zh/guide/one-click-deploy)** — 完整单节点部署，适合评估体验和开发测试
- **[多节点集群部署](https://cube-sandbox.pages.dev/zh/guide/multi-node-deploy)** — 扩展为多节点生产集群

### 第二步 — 为 CubeProxy 配置 HTTPS

CubeProxy 同时提供 **HTTPS（443 端口）和 HTTP（80 端口）** 两种访问方式：

- **E2B SDK（默认）**：使用 HTTPS。Cube 内置 DNS 服务并预装 `cube.app` 证书，开箱即用。
- **直接 HTTP 访问**：请求时 `Host` 头部须为 `<port>-<sandboxId>.<domain>` 格式，例如 `Host: 49999-abc123-cube.app`。

> `E2B_API_URL` 始终指向 **Cube API Server**（端口 3000），不经过 CubeProxy。

### 第三步 — 创建代码模板

```bash
cubemastercli tpl create-from-image \
  --image ccr.ccs.tencentyun.com/ags-image/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
```

记录输出的 `template_id`。

### 第四步 — 安装 OpenClaw Skill

`cube-sandbox` skill 位于仓库 `examples/openclaw-integration/skills/cube-sandbox/` 目录：

```bash
cp -r examples/openclaw-integration/skills/cube-sandbox/ ~/.openclaw/workspace/skills/
# 重启 OpenClaw Gateway
openclaw gateway restart
```

### 第五步 — 配置环境变量

在 Shell 或 OpenClaw 环境中设置以下变量：

```bash
export CUBE_TEMPLATE_ID=<template-id>       # 第三步获取的模板 ID
export E2B_API_URL=http://<节点IP>:3000     # Cube API Server 地址
export E2B_API_KEY=dummy                    # 任意非空字符串

# 使用 Cube 内置 mkcert 证书时才需要：
# export SSL_CERT_FILE=$(mkcert -CAROOT)/rootCA.pem
```

## 5. 使用方式

安装完成后，skill 会自动被以下语句触发：

- "在沙箱里跑这段代码"
- "安全执行 Python"
- "隔离环境运行"
- "cube sandbox"

### 示例

**运行 Python 代码：**
> "在沙箱里跑一段 Python，计算 1 到 100 的和"

**执行 Shell 命令：**
> "用沙箱执行 `uname -a` 并返回结果"

**网络隔离：**
> "在完全断网的沙箱中运行这段代码"

**文件操作：**
> "读取沙箱中 /etc/hosts 的内容"

## 6. Skill 参考

Skill 文件位于 `examples/openclaw-integration/skills/cube-sandbox/SKILL.md`，核心能力：

| 能力 | E2B API |
|------|---------|
| 执行 Python | `sandbox.run_code(code)` |
| Shell 命令 | `sandbox.commands.run(cmd)` |
| 读文件 | `sandbox.files.read(path)` |
| 写文件 | `sandbox.files.write(path, content)` |
| 暂停 | `sandbox.pause()` |
| 恢复 | `sandbox.connect()` |
| 断网 | `Sandbox.create(allow_internet_access=False)` |
| CIDR 白名单 | `Sandbox.create(network={"allow_out": [...]})` |
| CIDR 黑名单 | `Sandbox.create(network={"deny_out": [...]})` |
| 宿主机挂载 | `Sandbox.create(metadata={"host-mount": ...})` |

## 7. 常见问题

| 现象 | 可能原因 | 解决方法 |
|------|---------|---------|
| Skill 未触发 | Skill 未安装 | 确认 `~/.openclaw/workspace/skills/cube-sandbox/` 存在 |
| `SSL: CERTIFICATE_VERIFY_FAILED` | HTTPS 但未配置 CA 证书 | 设置 `SSL_CERT_FILE=$(mkcert -CAROOT)/rootCA.pem` |
| `Template not found` | `CUBE_TEMPLATE_ID` 错误 | 重新运行 `cubemastercli tpl list` |
| 域名解析失败 | DNS 未配置 | 参考 skill 中的 `/etc/hosts` 临时解决方案 |

## 8. 目录结构

```
openclaw-integration/
├── README.md                        # 英文文档
├── README_zh.md                     # 中文文档（本文件）
└── skills/
    └── cube-sandbox/
        ├── SKILL.md                 # Skill 定义和使用指南
        └── references/
            ├── api.md               # API 参考
            └── examples.md          # 更多示例
```
