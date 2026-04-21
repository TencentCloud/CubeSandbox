# 代码沙箱快速入门

[English](README.md)

Cube Sandbox 最基础的使用方式：创建沙箱、在其中运行 Python 代码、执行 Shell 命令——全部通过本地的 E2B Python SDK 完成。

## 1. 背景

**Cube Sandbox** 是轻量级 MicroVM 平台，控制面和数据面完全兼容 [E2B SDK](https://e2b.dev)。每次 `Sandbox.create()` 调用都会在 50ms 内从模板快照启动一个新的 KVM MicroVM。沙箱完全隔离——独立内核、文件系统和网络。`with` 块退出时沙箱自动销毁。

```
你的脚本  (E2B SDK)
    │  REST API
    ▼
CubeAPI  (端口 3000)
    │
    ▼
CubeMaster ──► Cubelet ──► KVM MicroVM
                               │
                           cube-agent (PID 1)
                               │
                           Python / Shell 进程
```

## 2. 前置条件

- 已部署的 Cube Sandbox 环境
- Python 3.8+

```bash
pip install -r requirements.txt
```

示例脚本会使用 `python-dotenv` 尝试自动加载当前目录或脚本所在目录中的 `.env`；
如果文件不存在，则继续使用当前进程环境变量，不会因为缺少 `.env` 直接报错。

## 3. 快速开始

### 第一步 — 创建代码模板

```bash
cubemastercli tpl create-from-image \
  --image ccr.ccs.tencentyun.com/ags-image/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
```

记录输出的 `template_id`。

### 第二步 — 配置环境变量

```bash
cp .env.example .env
# 编辑 .env，填写 E2B_API_URL 和 CUBE_TEMPLATE_ID
```

之后直接运行任意示例脚本即可，无需手动 `export`。

或直接导出：

```bash
export E2B_API_KEY=dummy
export E2B_API_URL=http://<节点IP>:3000
export CUBE_TEMPLATE_ID=<template-id>

# 使用 Cube 内置 mkcert 证书时才需要：
# export SSL_CERT_FILE=$(mkcert -CAROOT)/rootCA.pem
```

### 第三步 — 在沙箱中运行 Python 代码

```bash
python exec_code.py
```

预期输出：

```
Python 3.x.x (...)
hello cube
sum(1..100) = 5050
```

### 第四步 — 执行 Shell 命令

```bash
python cmd.py
```

预期输出：

```
hello cube
```

## 4. 所有示例

| 脚本 | 演示内容 |
|------|---------|
| `exec_code.py` | `sandbox.run_code()` — 在沙箱中执行 Python 代码 |
| `cmd.py` | `sandbox.commands.run()` — 执行 Shell 命令 |
| `create.py` | `sandbox.get_info()` — 获取沙箱元数据 |
| `read.py` | `sandbox.files.read()` — 读取沙箱文件系统中的文件 |
| `pause.py` | `sandbox.pause()` / `sandbox.connect()` — 快照与恢复 |
| `network_no_internet.py` | `allow_internet_access=False` — 完全断网沙箱 |
| `network_allowlist.py` | `allow_out` — 白名单 CIDR，拦截其余所有出口 |
| `network_denylist.py` | `deny_out` — 黑名单 CIDR，其余放行 |

### exec_code.py — 运行 Python 代码

```python
with Sandbox.create(template=template_id) as sandbox:
    sandbox.run_code(python_code, on_stdout=lambda line: print(line))
```

### cmd.py — Shell 命令

```python
with Sandbox.create(template=template_id) as sandbox:
    result = sandbox.commands.run("echo hello cube")
    print(result.stdout)
```

### pause.py — 暂停与恢复

将运行中的沙箱快照以释放计算资源，之后恢复：

```python
with Sandbox.create(template=template_id) as sandbox:
    sandbox.pause()       # 保存内存快照，释放 VM
    time.sleep(3)
    sandbox.connect()     # 恢复快照，继续执行
    print(sandbox.get_info())
```

### 网络策略

```bash
# 完全断网
python network_no_internet.py

# 白名单：只允许指定 CIDR
python network_allowlist.py

# 黑名单：屏蔽指定 CIDR，其余放行
python network_denylist.py
```

## 5. 常见问题

| 现象 | 可能原因 | 解决方法 |
|------|---------|---------|
| `SSL: CERTIFICATE_VERIFY_FAILED` | HTTPS 但未配置 CA 证书 | 设置 `SSL_CERT_FILE=$(mkcert -CAROOT)/rootCA.pem` |
| `Template not found` | 模板 ID 错误 | 重新运行 `cubemastercli tpl list` |
| `Connection refused` | CubeAPI 不可达 | 检查 `E2B_API_URL` 及端口 3000 |
| `Sandbox timeout` | 沙箱超过 TTL | 增大 `Sandbox.create()` 中的 `timeout` |

## 6. 目录结构

```
code-sandbox-quickstart/
├── README.md                  # 英文文档
├── README_zh.md               # 中文文档（本文件）
├── exec_code.py               # 在沙箱中运行 Python 代码
├── cmd.py                     # 执行 Shell 命令
├── create.py                  # 创建沙箱并查看元数据
├── read.py                    # 读取沙箱文件系统中的文件
├── pause.py                   # 暂停与恢复沙箱
├── network_no_internet.py     # 完全断网沙箱
├── network_allowlist.py       # 出口 CIDR 白名单
├── network_denylist.py        # 出口 CIDR 黑名单
├── requirements.txt           # Python 依赖
└── .env.example               # 环境变量模板
```
