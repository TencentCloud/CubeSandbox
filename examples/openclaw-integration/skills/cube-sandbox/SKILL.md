---
name: cube-sandbox
description: >
  Cube Sandbox 安全沙箱执行技能。当需要安全执行 Python 代码、Shell 命令，或需要隔离环境运行不受信任的代码时使用。
  适用场景：
  (1) 用户要求"在沙箱中执行代码"、"跑一段 Python"、"安全执行"、"隔离环境运行"；
  (2) 需要执行可能有副作用的代码（文件操作、网络请求、安装包等），希望通过沙箱隔离风险；
  (3) 需要读写沙箱内文件、挂载宿主机目录到沙箱；
  (4) 需要控制沙箱网络策略（完全断网、白名单、黑名单）；
  (5) 需要暂停/恢复沙箱（保留内存快照以实现快速复用）。
  Cube Sandbox 兼容 E2B SDK，通过环境变量 E2B_API_URL 指向 Cube 部署地址即可使用。
---

# Cube Sandbox Skill

Cube Sandbox 是 Tencent 自研的安全沙箱基础设施，兼容 E2B SDK，为 AI Agent 提供隔离的代码执行环境。

## 环境配置

### 必需环境变量

运行前必须设置以下环境变量（可写入 `.env` 或 `~/.bashrc`）：

```bash
export CUBE_TEMPLATE_ID=<模板ID>       # 沙箱镜像模板，必填
export E2B_API_URL=http://<host>:3000  # Cube API 地址（用于创建沙箱），必填
export E2B_API_KEY=dummy               # SDK 非空校验用，填任意字符串
```

### SSL 证书配置（按需）

`SSL_CERT_FILE` 仅在使用 Cube 内置 `cube.app` 测试证书时需要配置。如果你使用自定义受信任域名，或通过 HTTP 访问沙箱，可跳过此配置。

**使用 cube.app 测试证书时：**

```bash
# 如果部署机器使用 mkcert，证书通常在以下位置
export SSL_CERT_FILE=/root/.local/share/mkcert/rootCA.pem

# 或者指定自定义证书路径
export SSL_CERT_FILE=/path/to/your/rootCA.pem
```

**仅测试时可禁用证书校验（不推荐生产环境）：**

在 Python 代码中添加：
```python
import ssl
import warnings
warnings.filterwarnings('ignore')
ssl._create_default_https_context = ssl._create_unverified_context
```

### 安装 Python 和 SDK

**1. 安装 Python 3（如未安装）**

```bash
# Ubuntu / Debian
sudo apt-get update && sudo apt-get install -y python3 python3-pip python3-venv

# macOS（使用 Homebrew）
brew install python3
```

**2. 安装 e2b-code-interpreter**

直接安装：
```bash
pip3 install e2b-code-interpreter
```

如果系统提示必须使用 venv（常见于 Ubuntu 22.04+、macOS Homebrew 环境）：
```bash
python3 -m venv .venv
source .venv/bin/activate   # Windows: .venv\Scripts\activate
pip install e2b-code-interpreter
```

> 使用 venv 后，后续所有 `python` / `pip` 命令都需在激活 venv 的终端中执行，或用 `.venv/bin/python` 直接调用。

### 完整配置示例

```bash
# 必需配置
export CUBE_TEMPLATE_ID="tpl-4cc15c28f7a04115a295c159"
export E2B_API_URL="http://127.0.0.1:3000"
export E2B_API_KEY="dummy"

# 使用 cube.app 测试证书时才需要配置（自定义受信任域名或 HTTP 访问可不填）
export SSL_CERT_FILE="/Users/username/Downloads/rootCA.pem"
# 或
# export SSL_CERT_FILE=/root/.local/share/mkcert/rootCA.pem
```

### 重要说明

1. **网络访问方式**：CubeProxy 同时提供 HTTPS（宿主机 443）和 HTTP（宿主机 80）两种访问方式。
   - **E2B SDK**：SDK 默认通过 HTTPS 访问沙箱 domain（格式如 `49999-{sandboxID}.cube.app`）。Cube 已内置 DNS 服务并预装了 `cube.app` 测试证书，开箱即可使用 HTTPS，无需额外配置证书。
   - **直接 HTTP 访问（不使用 SDK）**：可通过 HTTP 直接请求沙箱服务，无需证书。请求时 `Host` 头部须符合格式：`<sandbox-service-port>-<sandboxId>-<domain>`，例如 `Host: 49999-abc123def456-cube.app`。
2. **创建沙箱**：使用 HTTP 访问 `E2B_API_URL` 指定的 Cube API Server 地址（默认端口 `3000`），此流量**不经过** CubeProxy。
3. **SSL_CERT_FILE**：仅在使用 Cube 内置 `cube.app` 测试证书时需要配置，指向对应 CA 根证书。使用自定义受信任域名或通过 HTTP 访问沙箱时可不配置。
4. **域名解析**：SDK 需要能解析沙箱返回的 domain。Cube 已内置 DNS 服务，默认 domain 为 `cube.app`，也可通过 `CUBE_API_SANDBOX_DOMAIN` 自定义。

## 核心用法

### 执行 Python 代码

```python
import os
import ssl
import warnings

# 方式一：配置证书路径（推荐）
os.environ['SSL_CERT_FILE'] = '/path/to/rootCA.pem'

# 方式二：禁用证书校验（仅测试）
# warnings.filterwarnings('ignore')
# ssl._create_default_https_context = ssl._create_unverified_context

from e2b_code_interpreter import Sandbox

with Sandbox.create(template=os.environ['CUBE_TEMPLATE_ID']) as sb:
    result = sb.run_code("print('hello cube')")
    print(result)
```

### 执行 Shell 命令

```python
import os
import ssl
import warnings

# 配置 SSL（选择一种方式）
os.environ['SSL_CERT_FILE'] = '/path/to/rootCA.pem'
# 或禁用校验（仅测试）
# warnings.filterwarnings('ignore')
# ssl._create_default_https_context = ssl._create_unverified_context

from e2b_code_interpreter import Sandbox

with Sandbox.create(template=os.environ['CUBE_TEMPLATE_ID']) as sb:
    r = sb.commands.run("echo hello")
    print(r.stdout)
```

### 读写沙箱文件

```python
with Sandbox.create(template=template_id) as sb:
    content = sb.files.read("/etc/hosts")
    sb.files.write("/tmp/out.txt", "hello")
```

### 挂载宿主机目录

```python
import json
with Sandbox.create(template=template_id, metadata={
    "host-mount": json.dumps([
        {"hostPath": "/tmp/data", "mountPath": "/mnt/data", "readOnly": False}
    ])
}) as sb:
    ...
```

### 网络策略

```python
# 完全断网
Sandbox.create(template=template_id, allow_internet_access=False)

# 白名单（只允许指定 CIDR）
Sandbox.create(template=template_id, allow_internet_access=False,
               network={"allow_out": ["10.0.0.0/8"]})

# 黑名单（屏蔽指定 CIDR，其余放行）
Sandbox.create(template=template_id,
               network={"deny_out": ["192.168.1.0/24"]})
```

### 暂停与恢复

```python
with Sandbox.create(template=template_id) as sb:
    sb.pause()          # 保存内存快照，释放计算资源
    sb.connect()        # 恢复快照，继续执行
    print(sb.get_info())
```

## 使用流程

1. **配置环境变量**（必填）：
   - `CUBE_TEMPLATE_ID`：沙箱模板 ID
   - `E2B_API_URL`：HTTP API 地址（如 `http://127.0.0.1:3000`）
   - `E2B_API_KEY`：任意字符串（SDK 校验用）

2. **配置 SSL 证书（按需）**：
   - 使用 Cube 内置 `cube.app` 测试证书时：设置 `SSL_CERT_FILE` 指向 CA 根证书路径
   - 使用自定义受信任域名或通过 HTTP 访问沙箱时：无需配置
   - 测试时也可在代码中禁用证书校验（不推荐生产环境）

3. **创建沙箱**：
   - SDK 使用 HTTP 访问 `E2B_API_URL` 创建沙箱
   - 沙箱创建成功后返回 domain 信息

4. **执行代码**：
   - SDK 自动使用 HTTPS 访问沙箱 domain（如 `49938-{sandboxID}.cube.app`）
   - 确保网络能够解析沙箱的 domain

5. **清理资源**：
   - 使用 `with Sandbox.create(...) as sb:` 确保沙箱用完自动销毁
   - 或手动调用 `sandbox.kill()`

6. **处理结果**：
   - 捕获 `result.stdout`、`result.stderr`、`result.error` 处理执行结果
   - 需要复用沙箱状态时，使用 `pause()` + `connect()` 而非重新创建

## 常见问题

### 1. 域名解析失败

**错误**：`[Errno 8] nodename nor servname provided, or not known`

**原因**：SDK 无法解析沙箱返回的 domain（如 `49999-{sandboxID}.cube.app`）

**首选方案**：确认 Cube 内置 DNS 服务是否正常运行，或联系管理员检查 DNS 配置。

**备用方案：手动写入 /etc/hosts**

当 DNS 服务不可用时，可由 AI Agent 在创建沙箱后将所需域名临时写入 `/etc/hosts`，沙箱销毁后再清除。

原理：
- `create_sandbox` 返回的 sandbox 信息中包含 `sandbox_id` 和 `domain`（如 `cube.app`）
- CubeProxy 的域名格式为 `<port>-<sandboxId>-<domain>`，对应 CubeProxy 所在宿主机 IP
- 需要访问哪些端口（如 `49999`、`49983`），就为每个端口各写一条 hosts 记录

操作示例（假设 CubeProxy 宿主机 IP 为 `127.0.0.1`，sandbox_id 为 `abc123`，domain 为 `cube.app`）：

```python
import subprocess
import os

PROXY_IP = "127.0.0.1"          # CubeProxy 所在宿主机 IP
PORTS = [49999, 49983]              # 需要访问的沙箱服务端口

def add_hosts(sandbox_id: str, domain: str, ports: list[int], ip: str):
    """创建沙箱后写入 /etc/hosts"""
    entries = []
    for port in ports:
        hostname = f"{port}-{sandbox_id}-{domain}"
        entries.append(f"{ip}  {hostname}  # cube-sandbox-{sandbox_id}")
    lines = "\n".join(entries) + "\n"
    # 需要 root 权限
    subprocess.run(["sudo", "tee", "-a", "/etc/hosts"],
                   input=lines.encode(), check=True)
    print(f"Added hosts entries:\n{lines}")

def remove_hosts(sandbox_id: str):
    """沙箱销毁后清除 /etc/hosts 中对应记录"""
    subprocess.run(
        ["sudo", "sed", "-i", f"/# cube-sandbox-{sandbox_id}/d", "/etc/hosts"],
        check=True
    )
    print(f"Removed hosts entries for sandbox {sandbox_id}")

# 使用示例
from e2b_code_interpreter import Sandbox

sb = Sandbox.create(template=os.environ['CUBE_TEMPLATE_ID'])
try:
    add_hosts(sb.sandbox_id, "cube.app", PORTS, PROXY_IP)
    result = sb.run_code("print('hello')")
    print(result)
finally:
    sb.kill()
    remove_hosts(sb.sandbox_id)
```

> ⚠️ 写入 `/etc/hosts` 需要 sudo 权限。沙箱异常退出时注意在 `finally` 块中确保清理，避免残留脏记录。

### 2. SSL 证书错误

**错误**：SSL 证书校验失败

**解决方案**：
- 方式一：设置 `SSL_CERT_FILE` 环境变量
- 方式二：在代码中禁用证书校验（仅测试环境）

### 3. 502 Bad Gateway

**错误**：API 返回 502

**原因**：Cube Sandbox 服务端不可用

**解决方案**：
- 检查服务状态和日志
- 联系服务管理员

## 详细参考

- API 接口列表：见 `references/api.md`
- 完整示例代码：见 `references/examples.md`
- 接入 OpenClaw 配置指南：见 `references/openclaw-integration.md`
