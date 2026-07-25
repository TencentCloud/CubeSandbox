# 
：本地镜像 & 远程镜像实战文档

本文基于实际操作经验，介绍两种制作自定义 Cube Sandbox 模板的完整流程：
- **方式一：本地构建镜像**（镜像只在当前机器，无需推送到远端）
- **方式二：远程镜像**（镜像已推送到 Registry，集群从远端拉取）

> 友情链接，建议同步阅读 [从 OCI 镜像制作模板](./template-from-image.md) 和 [自带镜像接入](./bring-your-own-image.md) 文档。

---

## 前置条件

- 已安装 `cubemastercli` 并加入 `$PATH`
- 已安装 Docker
- CubeMaster 服务正常运行（`cubemastercli tpl list` 能返回结果）
- 已找到 mkcert CA 证书路径（SDK 连接沙箱时需要）：

```bash
# 通常在这里
ls ~/.local/share/mkcert/rootCA.pem
```

---

## 方式一：本地构建镜像制作模板

适合在 **CubeMaster 所在机器上本地开发调试**，镜像无需推送到远端 Registry。

### Step 1：写 Dockerfile

所有 Cube 镜像必须包含 `envd`（负责沙箱内的通信协议），推荐基于官方 `cubesandbox-base` 构建，`envd` 已预装。

```dockerfile
FROM ghcr.io/tencentcloud/cubesandbox-base:latest

# 安装你需要的工具和依赖
RUN apt-get update && apt-get install -y --no-install-recommends \
    python3 python3-pip curl wget git vim jq \
    && rm -rf /var/lib/apt/lists/*

RUN pip install --no-cache-dir numpy pandas requests httpx
```

保存为 `/tmp/Dockerfile.cube-test`。



### Step 2：构建镜像

```bash
docker build -f /tmp/Dockerfile.cube-test -t my-sandbox:v1 .
```

### Step 3：验证 envd 正常

制作模板前，先本地跑一下验证 envd 能正常响应 `/health`，避免模板构建时探活失败：

```bash
cid=$(docker run -d my-sandbox:v1)
sleep 2
docker exec "$cid" curl -s -o /dev/null -w "envd /health => %{http_code}\n" http://127.0.0.1:49983/health
docker rm -f "$cid"
```

**期望输出：`envd /health => 204`**

如果不是 204，检查 Dockerfile 的 `ENTRYPOINT` 是否正确（见[常见问题](#常见问题)）。

### Step 4：制作模板

```bash
cubemastercli tpl create-from-image \
  --image my-sandbox:v1 \
  --writable-layer-size 2G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

命令立即返回 `job_id` 和 `template_id`：

```
job_id:      718b7ebd-5a2c-4f33-85d0-1c36f0d1b3ee
template_id: tpl-01adfa335c03460cb4a09225
status:      PENDING
phase:       PULLING
```

### Step 5：等待模板就绪

```bash
cubemastercli tpl watch --job-id <job_id>
```

等待输出 `status: READY` 即完成：

```
status:       READY
phase:        READY
progress:     100%
distribution: 1/1 ready, 0 failed
```

### Step 6：验证模板可用

两种 SDK 均可验证，任选其一。

#### 方式 A：e2b_code_interpreter（需要 SSL 证书）

```bash
export CUBE_TEMPLATE_ID=<template_id>
export E2B_API_URL=http://127.0.0.1:3000
export E2B_API_KEY=e2b_000000
export SSL_CERT_FILE=~/.local/share/mkcert/rootCA.pem

python3 - << 'EOF'
import os
from e2b_code_interpreter import Sandbox
with Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"]) as sb:
    r = sb.commands.run("python3 --version && echo hello-cube")
    print(r.stdout)
EOF
```

**期望输出：**
```
Python 3.x.x
hello-cube
```

#### 方式 B：cubesandbox SDK（无需 SSL 证书，推荐）

> 尚未安装 cubesandbox SDK或安装失败？参考（[常见问题](#常见问题) 的cubesandbox SDK 安装失败）。

```bash
export CUBE_API_URL=http://127.0.0.1:3000
export CUBE_TEMPLATE_ID=<template_id>
export CUBE_PROXY_NODE_IP=127.0.0.1   # 本机；远程访问填 CubeProxy 节点 IP

python3 - << 'EOF'
import os, time
from cubesandbox import Sandbox, Config
from cubesandbox._exceptions import ApiError

cfg = Config(
    api_url=os.environ["CUBE_API_URL"],
    template_id=os.environ["CUBE_TEMPLATE_ID"],
    proxy_node_ip=os.environ.get("CUBE_PROXY_NODE_IP", ""),
)

def run_with_retry(sb, code, max_retries=10, interval=1.0):
    for i in range(max_retries):
        try:
            return sb.run_code(code)
        except ApiError as e:
            if e.status_code == 502 and i < max_retries - 1:
                time.sleep(interval)
            else:
                raise

with Sandbox.create(config=cfg) as sb:
    r = run_with_retry(sb, 'import sys; print(sys.version); print("hello-cube")')
    for line in r.logs.stdout:
        print(line, end="")
EOF
```

**期望输出：**
```
Python 3.x.x
hello-cube
```

> ⚠️ `cubesandbox` SDK 的 `run_code` 依赖 Jupyter kernel（49999 端口）。
> 自定义模板需在 Dockerfile 中安装 `jupyter_kernel_gateway ipykernel`，
> 并在制作模板时同时暴露 49983 和 49999 两个端口。

---

## 方式二：远程镜像制作模板

适合**团队共享镜像**或**多节点集群**场景，镜像推送到 Registry 后，集群各节点都能拉取。

### Step 1：写 Dockerfile

与方式一相同，参考上方 Dockerfile 示例。

### Step 2：构建镜像（带 Registry 前缀）

```bash
docker build -f /tmp/Dockerfile.cube-test \
  -t ccr.ccs.tencentyun.com/<命名空间>/<镜像名>:v1 .
```

### Step 3：验证 envd 正常

同方式一 Step 3。

### Step 4：登录并推送镜像

```bash
# 登录 Registry（如需认证）
docker login ccr.ccs.tencentyun.com

# 推送
docker push ccr.ccs.tencentyun.com/<命名空间>/<镜像名>:v1
```

### Step 5：制作模板

```bash
cubemastercli tpl create-from-image \
  --image ccr.ccs.tencentyun.com/<命名空间>/<镜像名>:v1 \
  --writable-layer-size 2G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

私有仓库需要加认证参数：

```bash
cubemastercli tpl create-from-image \
  --image ccr.ccs.tencentyun.com/<命名空间>/<镜像名>:v1 \
  --writable-layer-size 2G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health \
  --registry-username <用户名> \
  --registry-password <密码>
```

### Step 6 & 7：等待就绪 + 验证

与方式一 Step 5、Step 6 完全相同。

---

## 两种方式对比

| | 本地镜像 | 远程镜像 |
|---|---|---|
| **是否需要推送** | ❌ 不需要 | ✅ 需要 push |
| **适用场景** | 单机开发调试 | 团队共享、多节点集群 |
| **镜像名格式** | `my-sandbox:v1` | `registry/ns/image:tag` |
| **私有仓库认证** | 不需要 | 可能需要 `--registry-username/password` |
| **速度** | 快（本地直接读） | 取决于网络和镜像大小 |

---

## 常见问题

### 💡 一、apt-get 报 `Temporary failure resolving`

**现象：** Docker build 时 apt 无法解析域名。

**原因：** Docker 容器内默认使用 `8.8.8.8` 作为 DNS，内网机器无法访问。`--dns` 参数在旧版 docker build（非 buildx）中不支持，写 `/etc/resolv.conf` 也会被 Docker 覆盖。

**解决：** 查出内网 apt 镜像站 IP，直接硬编码到 `sources.list`：

```bash
# 1. 查镜像站 IP（在宿主机上执行）
nslookup mirrors.tencent.com 9.218.233.130 | grep Address | tail -1
# => Address: 30.163.240.137

# 2. Dockerfile 里替换 apt 源
RUN sed -i 's|http://archive.ubuntu.com/ubuntu|http://30.163.240.137/ubuntu|g' /etc/apt/sources.list && \
    sed -i 's|http://security.ubuntu.com/ubuntu|http://30.163.240.137/ubuntu|g' /etc/apt/sources.list
```

pip 同理，用 IP 替换 pypi 镜像域名：
```dockerfile
RUN pip install --no-cache-dir \
    -i http://30.163.240.137/pypi/simple/ \
    --trusted-host 30.163.240.137 \
    numpy pandas
```

---

### 💡 二、envd /health 不返回 204

**现象：** Step 3 验证时 `curl` 返回非 204，或连接拒绝。

**原因：** 镜像的 `ENTRYPOINT` / `CMD` 把 `cube-entrypoint.sh` 覆盖掉了，导致 `envd` 没有启动。

**解决：** Dockerfile 里确保 `ENTRYPOINT` 使用 `cube-entrypoint.sh`：

```dockerfile
ENTRYPOINT ["/usr/local/bin/cube-entrypoint.sh"]
CMD ["your-app-command"]
```

或者在自定义 entrypoint 里手动拉起 envd：

```bash
/usr/bin/envd -port 49983 >/var/log/envd.log 2>&1 &
exec "$@"
```

---

### 💡 三、SDK 报 `SSL: CERTIFICATE_VERIFY_FAILED`

**现象：** Python SDK 调用沙箱时报证书错误。

**原因：** SDK 通过 HTTPS 访问沙箱域名（`*.cube.app`），但本机不信任 Cube 内置的 mkcert CA。

**解决：** 设置环境变量指向 CA 证书：

```bash
export SSL_CERT_FILE=~/.local/share/mkcert/rootCA.pem
```

或者在代码里临时禁用（仅测试环境）：

```python
import ssl, warnings
warnings.filterwarnings('ignore')
ssl._create_default_https_context = ssl._create_unverified_context
```

---

### 💡 四、模板一直卡在 `phase: PULLING`

**现象：** `tpl watch` 长时间停在 PULLING 阶段不动。

**原因：** CubeMaster 节点拉不到镜像（网络不通、Registry 认证失败等）。

**排查：**
```bash
# 查看任务详情，找 last_error 字段
cubemastercli tpl status --job-id <job_id> --json | jq '.last_error'

# 在 CubeMaster 所在节点手动 docker pull 验证
docker pull <镜像地址>
```

**本地镜像补充说明：** 如果用本地镜像（未推送到远端），CubeMaster 会直接从本地 Docker 读取，无需网络，不会卡在 PULLING。确认镜像名拼写正确即可：
```bash
docker images | grep <镜像名>
```

---

### 💡 五、模板 `status: FAILED`（BUILDING 阶段）

**现象：** 模板构建失败，卡在 BUILDING 阶段后变为 FAILED。

**排查：**
```bash
cubemastercli tpl status --job-id <job_id> --json | jq '.last_error'
```

**常见原因：**
- `--writable-layer-size` 设置过小，构建时磁盘写满
- 节点磁盘空间不足：`df -h` 检查

---

### 💡 六、`distribution: 0/N ready` 长时间不变

**现象：** 模板状态已 READY，但 distribution 显示 0 个节点就绪。

**原因：** artifact 正在分发到各节点，多节点集群时正常，稍等即可。

**排查长时间未恢复：**
```bash
# 检查目标节点 Cubelet 日志
journalctl -u cubelet -f
```
---
### 💡 七、模板制作卡住，沙箱一直处于 running 状态且无法删除

**现象：** 制作模板时（快照阶段）卡住不动，`tpl status` 显示任务长时间无进展；同时有沙箱残留，状态一直是 `running`，`DELETE /sandboxes/:id` 也无法删除。

**原因：** 磁盘空间不足，快照写入失败，导致沙箱进程卡死，无法正常退出，也无法响应删除请求。

**排查：**

```bash
# 检查宿主机磁盘使用情况
df -h

# 重点关注 Docker 数据目录和 Cube 数据目录
df -h /data/docker/lib/
df -h /var/lib/cube/ 2>/dev/null || df -h /data/cube/ 2>/dev/null

# 查看占用最大的目录
du -sh /data/docker/lib/overlay2/* 2>/dev/null | sort -rh | head -10
```

**解决：**

1. **释放磁盘空间**（清理无用镜像和容器）：

```bash
# 清理已退出的容器、无用镜像、build cache
docker system prune -f

# 如果空间还不够，清理无 tag 的镜像
docker image prune -f
```

2. **强制清理残留沙箱**（正常 API 删不掉时）：

```bash
# 查看残留沙箱进程
ps aux | grep -E 'firecracker|qemu|cubelet' | grep -v grep

# 强制 kill 对应进程（替换为实际 PID）
kill -9 <PID>

# 或通过 Cubelet 日志找到沙箱对应的进程组
journalctl -u cubelet -n 100 | grep <sandboxID>
```

3. **重新制作模板**：清理完磁盘后重新提交 `tpl create-from-image`，原失败的 job 无需处理。

**预防：** 制作模板前确认宿主机磁盘剩余空间 > 镜像大小 × 2（需要同时存放 rootfs 和快照文件）。

---

### 💡 八、`probe` 和 `probe-path` 参数说明

这两个参数告诉 CubeMaster **用哪个端口和路径来探测容器是否就绪**。探活返回 HTTP 2xx 后，模板才会进入 READY 状态。配错是模板构建失败最常见的原因之一。

**参数含义：**

| 参数 | 说明 |
|------|------|
| `--expose-port` | 声明容器对外暴露的端口 |
| `--probe` | 探活使用的端口，必须已在 `--expose-port` 中声明 |
| `--probe-path` | 探活的 HTTP GET 路径，该路径必须返回 2xx |

---

**不同镜像的正确配置：**

**官方 `sandbox-code` 镜像**（代码执行沙箱，envd 监听 49983，Jupyter kernel 监听 49999）：

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999 \
  --probe-path /
```

**官方 `sandbox-browser` 镜像**（浏览器沙箱，envd 监听 49983）：

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

**基于 `cubesandbox-base` 的自定义镜像**（envd 监听 49983）：

```bash
cubemastercli tpl create-from-image \
  --image my-registry/my-sandbox:v1 \
  --writable-layer-size 2G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

**自定义镜像 + 额外应用端口**（比如应用在 8080 提供服务）：

```bash
cubemastercli tpl create-from-image \
  --image my-registry/my-app:v1 \
  --writable-layer-size 2G \
  --expose-port 49983 \
  --expose-port 8080 \
  --probe 49983 \
  --probe-path /health
```



---

**常见配错场景：**

| 错误配置 | 现象 | 正确做法 |
|---------|------|---------|
| `--probe` 端口未在 `--expose-port` 中声明 | 参数校验报错 | 先 `--expose-port` 再 `--probe`，端口保持一致 |
| `--probe-path` 填了不存在的路径（如 `/healthz`，实际是 `/health`）| 探活超时，模板 FAILED | 本地先 `curl http://127.0.0.1:<port><path>` 确认返回 2xx |
| 用 `sandbox-code` 镜像只暴露了 49983，未暴露 49999 | 模板 READY，但 `run_code` 报连接失败 | `sandbox-code` 需同时暴露 49999 和 49983 |
| `--probe-path` 漏写开头的 `/`（写成 `health` 而非 `/health`）| 探活 404 | 路径必须以 `/` 开头 |

---

**本地验证 probe 配置（制作模板前先跑）：**

```bash
cid=$(docker run -d <你的镜像>)
sleep 2
docker exec "$cid" curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:<probe端口><probe-path>
# 期望: 200 或 204
docker rm -f "$cid"
```

---



### 💡 九、cubesandbox SDK 安装失败（历史问题，已修复）

**现象：** 在旧版仓库上 `pip install` 报错 `BackendUnavailable: Cannot import 'setuptools.backends.legacy'`。

**原因：** 早期 `sdk/python/pyproject.toml` 误填了不存在的 build-backend `setuptools.backends.legacy:build`（PEP 517 setuptools 官方 backend 只有 `setuptools.build_meta` 和 `setuptools.build_meta:__legacy__` 两个，`setuptools.backends.legacy` 从未存在过 —— 不是版本问题，是笔误）。

**解决：** 已在源仓库改为 `setuptools.build_meta`。如果你的本地副本仍是旧版，请 `git pull` 拿到最新 `dev-snapshot`，或手动改一下：

```bash
cd CubeSandbox/sdk/python
sed -i 's|setuptools.backends.legacy:build|setuptools.build_meta|g' pyproject.toml
pip install .
```

---

### 💡 十、如何向沙箱传递环境变量

Cube Sandbox 支持三种方式，适用于不同场景。

**方式 1：模板创建时传入（全局默认 env）**

通过 `--env` 烘焙进模板快照，所有从该模板创建的沙箱都会继承：

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --probe 49999 \
  --env MY_ENV=production \
  --env DB_HOST=10.0.0.1
```

> ⚠️ 写入模板的 env 对所有使用该模板的用户可见，**不要用于存放密钥等敏感信息**。

**方式 2：创建沙箱实例时传入（推荐）**

`Sandbox.create()` 的 `envs` 参数，动态注入，只作用于当前沙箱实例：

```python
from e2b_code_interpreter import Sandbox

with Sandbox.create(
    template="tpl-xxxx",
    envs={
        "API_KEY": "sk-xxx",
        "DEBUG": "true",
        "MY_CUSTOM_VAR": "hello"
    }
) as sandbox:
    result = sandbox.run_code("""
import os
print(os.environ.get('API_KEY'))
print(os.environ.get('DEBUG'))
print(os.environ.get('MY_CUSTOM_VAR'))
""")
    print(result)
```

**方式 3：沙箱内通过代码设置（运行时临时变量）**

```python
with Sandbox.create(template="tpl-xxxx") as sandbox:
    # 通过 shell 设置（只在该次命令子进程内有效）
    sandbox.commands.run("export FOO=bar && echo $FOO")

    # 通过 Python 设置（在同一个 kernel session 内持久）
    sandbox.run_code("""
import os
os.environ['RUNTIME_VAR'] = 'dynamic_value'
print(os.environ['RUNTIME_VAR'])
""")
```

**三种方式对比：**

| 方式 | 时机 | 作用范围 | 适用场景 |
|------|------|----------|---------|
| `--env`（模板创建时） | 构建模板时 | 所有沙箱实例 | 固定配置（运行环境、默认参数） |
| `envs={}`（SDK 创建时） | 创建沙箱时 | 单个沙箱实例 | **推荐** — 动态配置（密钥、用户参数） |
| 代码内 `os.environ` | 运行时 | 当前进程 | 临时变量 |

最常用的是**方式 2**，通过 `Sandbox.create(envs={...})` 传入，灵活且不污染模板。

---

## 快速参考

```bash
# 列出所有模板
cubemastercli tpl list
cubemastercli tpl list -o wide

# 查看模板详情（模板 ID 用位置参数传入，等价于 --template-id）
cubemastercli tpl info <template_id>

# 删除模板
cubemastercli tpl delete <template_id>

# 必要环境变量（使用 SDK 时）
export CUBE_TEMPLATE_ID=<template_id>
export E2B_API_URL=http://127.0.0.1:3000
export E2B_API_KEY=e2b_000000
export SSL_CERT_FILE=~/.local/share/mkcert/rootCA.pem
```
