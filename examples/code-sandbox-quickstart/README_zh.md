# 代码沙箱快速入门

[English](README.md)

Cube Sandbox 最基础的使用方式：创建沙箱、在其中运行 Python 代码、执行 Shell 命令——全部通过本地的 E2B Python SDK 完成。

## 1. 背景

**Cube Sandbox** 是轻量级 MicroVM 平台，控制面和数据面完全兼容 [E2B SDK](https://e2b.dev)。其设计分为两个平面：

- **控制面 (Control Plane)**：负责沙箱生命周期管理。每次 `Sandbox.create()` 调用都会在 50ms 内从模板快照启动一个新的 KVM MicroVM。本示例使用的官方 `sandbox-code` 镜像中，创建请求到达 Cubelet 后，由 VM 内的 `cube-agent` (PID 1) 拉起 `envd`。
- **数据面 (Data Plane)**：负责通过沙箱内的代码解释器执行代码，同时将 `commands.run`、`files.read/write` 等请求经由 CubeProxy 直接路由至沙箱内的 `envd`。沙箱完全隔离——拥有独立内核、文件系统和网络。`with` 块退出时，沙箱自动销毁。

```text
                             用户脚本 (E2B SDK)
                                      │
                                      ▼
        ┌─────────────────────────────┴─────────────────────────────┐
        │                                                           │
 【1. 管理流程 Control Plane】                            【2. 调用流程 Data Plane】
  (如 Sandbox.create / delete)             (如 run_code, commands.run, files.read/write)
        │                                                           │
        ▼  REST API (端口 3000)                                     ▼  WSS / HTTP
     CubeAPI                                                    CubeProxy
        │                                                           │
        ▼                                                           │
    CubeMaster                                                      │
        │                                                           │
        │                  ┌────────────────────────────────────┐   │
        ▼                  │            KVM MicroVM             │   │
     Cubelet ──────────────┼──► cube-agent ──► envd  ◄──────────┼───┘
                           │     (PID 1)         │              │
                           │                     ▼              │
                           │          Code Interpreter / envd   │
                           └────────────────────────────────────┘
```

## 2. 前置条件

- 已部署的 Cube Sandbox 环境
- Python 3.9+（`cubesandbox` 与 `e2b-code-interpreter` 依赖）

```bash
pip install -r requirements.txt
```

示例脚本会使用 `python-dotenv` 尝试自动加载当前目录或脚本所在目录中的 `.env`；
如果文件不存在，则继续使用当前进程环境变量，不会因为缺少 `.env` 直接报错。

## 3. 快速开始

### 第一步 — 创建代码模板

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
```

> **镜像仓库说明：** 国内优先使用 `cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest`；境外访问推荐使用 `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest`。

记录输出的 `template_id`。

### 第二步 — 配置环境变量

```bash
cp .env.example .env
# 编辑 .env，填写 E2B_API_URL 和 CUBE_TEMPLATE_ID
```

之后直接运行任意示例脚本即可，无需手动 `export`。

**集群外本地开发：** 若无法解析 `*.cube.app`，在 `.env` 中设置
`CUBE_REMOTE_PROXY_BASE=https://<节点IP>:443`（CubeProxy 常用 443/8080/9090）。
`load_local_dotenv()` 仅加载 `.env`；需要数据面的 E2B 脚本会调用
`ensure_dev_sidecar()` 启动同级
[`examples/e2b-dev-sidecar/`](../e2b-dev-sidecar/) 并为 **E2B SDK**
（`e2b_code_interpreter`）打补丁，经本地 sidecar 转发数据面到 CubeProxy。
需完整克隆本仓库；sidecar 启动失败时脚本会 warn 并继续。仅控制面的脚本
（如 `create.py`）不会启动 sidecar。同目录下使用 `cubesandbox` SDK 的脚本
（如 `auto-kill.py`）**不受** sidecar 补丁影响，集群外仍需 `*.cube.app` DNS
或其它路由。`apply_create_time_envs()` 仅在 dev sidecar 生效时用 HTTP；
否则 `/init` 默认走 HTTPS（仅在有意使用明文 HTTP 的部署中才用
`CUBE_ENVD_INIT_SCHEME` 覆盖）。单次重试超时可调 `CUBE_ENVD_INIT_ATTEMPT_TIMEOUT_S`
（默认每次 `5` 秒）。

或直接导出：

```bash
export E2B_API_KEY=e2b_000000
export E2B_API_URL=http://<节点IP>:3000
export CUBE_TEMPLATE_ID=<template-id>

# 使用 Cube 内置 mkcert 证书时才需要：
# export SSL_CERT_FILE=/root/.local/share/mkcert/rootCA.pem
```

### 第三步 — 在沙箱中运行 Python 代码

```bash
python exec_code.py
```

预期输出：

```
hello cube

Execution(Results: [], Logs: Logs(stdout: ['hello cube\n'], stderr: []), Error: None)
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
| `create_with_envs.py` | `Sandbox.create(envs=...)` — 创建时注入环境变量 |
| `read.py` | `sandbox.files.read()` — 读取沙箱文件系统中的文件 |
| `pause.py` | `sandbox.pause()` / `sandbox.connect()` — 快照与恢复 |
| `auto-resume.py` | `lifecycle={"on_timeout": "pause", "auto_resume": True}` — 平台在空闲超时后自动暂停沙箱，下一次请求自动恢复 |
| `auto-kill.py` | `lifecycle={"on_timeout": "kill"}` — 平台在空闲超时后直接销毁沙箱（默认行为，销毁不可逆，沙箱无法恢复） |
| `network_no_internet.py` | `allow_internet_access=False` — 完全断网沙箱 |
| `network_allowlist.py` | `allow_out` — 白名单 CIDR，拦截其余所有出口 |
| `network_denylist.py` | `deny_out` — 黑名单 CIDR，其余放行 |
| `restrict_public_access.py` | `network={"allow_public_traffic": False}` — 公网 URL 必须携带 per-sandbox token 才可访问 |
| `sandbox-in-sandbox.py` | 控制面 — 在沙箱 A 内使用 `cubesandbox` SDK `Sandbox.connect()` 连接沙箱 B 并执行命令 |
| `sandbox-curl-sandbox.py` | 数据面 — 在沙箱 B 内启动 HTTP 服务，从沙箱 A 通过 CubeProxy Host 路由 `curl` 访问 |
| `sandbox-port-mapping.py` | 母机端口映射 — 沙箱 A 直接 `curl` B 所在节点的 `hostIP:hostPort`（模板 `--expose-port` 触发的 CubeVS DNAT） |

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

### 创建时注入环境变量

可以在创建沙箱时传入环境变量，后续在该沙箱中的命令执行也可以读取到这些变量：

```python
python create_with_envs.py
```

预期输出:

```text
session is user-session-test
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

### auto-resume.py — 自动暂停与自动恢复

与 `pause.py` 类似，但暂停/恢复完全交给平台自动管理。`lifecycle` 参数与 e2b SDK 对齐
（参考 [e2b 文档](https://e2b.dev/docs/sandbox/auto-resume)）：`on_timeout="pause"`
表示空闲超时后由 sidecar 触发暂停；`auto_resume=True` 让下一次请求命中
暂停沙箱时自动恢复：

```python
sandbox = Sandbox.create(
    template=template_id,
    timeout=30,             # auto-pause sidecar 用作空闲阈值
    lifecycle={"on_timeout": "pause", "auto_resume": True},
)
sandbox.run_code("print('first call')")
time.sleep(45)              # 超过空闲阈值，sidecar 暂停沙箱
sandbox.run_code("print('back from a transparent resume')")
sandbox.kill()
```

### auto-kill.py — 空闲超时后自动销毁

`auto-resume.py` 的孪生销毁版本。`on_timeout="kill"`（不传 `lifecycle`
时的默认值）告诉平台：沙箱空闲超过 `timeout` 后直接拆除 VM，不
保留快照，下一次请求会以 **410 Gone** 快速失败：

```python
sandbox = Sandbox.create(
    template=template_id,
    timeout=30,             # 扫描器使用的空闲阈值
    lifecycle={"on_timeout": "kill"},
)
sandbox.run_code("print('first call')")
time.sleep(50)              # 超过空闲阈值，扫描器销毁沙箱
try:
    sandbox.run_code("print('should never run')")
except Exception as exc:
    print(f"sandbox is gone: {exc!r}")  # 销毁不可逆
```

TUI 版本额外交叉校验 `Sandbox.list()` 不再返回该沙箱，并创建一个对照
沙箱来排除集群整体故障造成的假阳性。

### 网络策略

```bash
# 完全断网
python network_no_internet.py

# 白名单：只允许指定 CIDR
python network_allowlist.py

# 黑名单：屏蔽指定 CIDR，其余放行
python network_denylist.py
```

### restrict_public_access.py — 限制公网 URL 访问

默认情况下沙箱的公网 URL 任何知道地址的人都可访问。对敏感场景，可以在创建沙箱时
传入 `network={"allow_public_traffic": False}`：CubeMaster 会为该沙箱签发一个
`traffic_access_token`，CubeProxy 随后会拒绝所有未携带该 token 的请求
（参考 [e2b 文档](https://e2b.dev/docs/network/restrict-public-access)）。
请求方可以使用以下任一 header：

- `e2b-traffic-access-token`（与 E2B 完全兼容）
- `cube-traffic-access-token`（CubeSandbox 原生别名）

```python
sandbox = Sandbox.create(
    template=template_id,
    network={"allow_public_traffic": False},
)
url = f"http://{sandbox.get_host(80)}/"

# 不带 token → 403
requests.get(url)

# 带 token → 200
requests.get(url, headers={"e2b-traffic-access-token": sandbox.traffic_access_token})
```

### 沙箱间互访

下面三个脚本演示沙箱如何访问另一个沙箱：创建沙箱 A 和 B，然后分别通过
**控制面**（SDK）、**CubeProxy**（Host 头路由）和**母机端口映射**（eBPF DNAT）
三种方式在 A 内访问 B。三种方式的原理、如何选择、前提与限制（含出口策略、
`allow_public_traffic` token、端点生命周期）见
[沙箱间互访指南](../../docs/zh/guide/sandbox-to-sandbox.md)。

```text
sandbox-in-sandbox.py（控制面 / SDK）

  本地脚本                    沙箱 A                       沙箱 B
      │                     ┌────────────────┐          ┌──────────────┐
      ├─ Sandbox.create ───►│ pip install    │          │              │
      ├─ Sandbox.create ────┼───────────────►│          │              │
      │                     │ cubesandbox    │ connect  │              │
      │                     │ Sandbox.connect├─────────►│ commands.run │
      │                     └────────────────┘          └──────────────┘
      │                          │ CubeAPI :3000（create/connect）
      │                          │ CubeProxy :80（commands.run → envd）

sandbox-curl-sandbox.py（数据面 / HTTP）

  沙箱 A ── curl ──► CubeProxy :80 ── Host: <port>-<sandboxID-B>.cube.app ──► B 内服务
```

脚本相关的环境变量：

- `CUBE_INNER_API_URL` / `CUBE_PROXY_NODE_IP`：沙箱内访问 CubeAPI / CubeProxy
  的地址。默认自动把 `.env` 里的 loopback 地址改写为节点出网 IP
  （`127.0.0.1` 在沙箱内指向沙箱自身）。
- `CUBE_PIP_INDEX_URL`：`sandbox-in-sandbox.py` 在 A 内安装 SDK 时使用的 pip
  镜像源（沙箱无公网时）。注意 pip 在沙箱内执行，guest DNS 需能解析 pip 源
  域名 —— guest DNS 来自模板 `--dns` 或 cubelet `default_dns_servers`，
  实验室无法访问公共 DNS 时请重建模板并加 `--dns`。
- `CUBE_SERVICE_PORT`：`sandbox-port-mapping.py` 的服务端口（默认 8080）。
  两个 curl 类脚本的服务端口都需在模板的 `--expose-port` 中声明
  （`sandbox-curl-sandbox.py` 固定用 8080）。
- B 以 `allow_public_traffic=False` 创建也可以 —— curl 示例会自动携带 B 的
  `traffic_access_token`。

```bash
# A 内安装 cubesandbox SDK，connect 到 B 并执行命令
python sandbox-in-sandbox.py

# B 在 :8080 提供 HTTP 服务，A 经 CubeProxy curl 访问
python sandbox-curl-sandbox.py

# B 在 expose-port 上提供服务，A 直接 curl 节点 hostIP:hostPort 映射
python sandbox-port-mapping.py
```

`sandbox-in-sandbox.py` 预期输出：

```text
[outer] sandbox A: 6d8a025c...  sandbox B: 1b26d1bb...
[inner] connecting to sandbox B: 1b26d1bb...
hello-from-B
```

`sandbox-curl-sandbox.py` 预期输出：

```text
[outer] service host: 8080-23af3202....cube.app
[inner:A] curl -sS --fail --resolve 8080-23af3202....cube.app:80:<node-ip> http://...
hello-from-B-service
[outer] OK: sandbox A reached the service in sandbox B
```

`sandbox-port-mapping.py` 预期输出：

```text
[outer] B exposed endpoint (host port mapping): 9.135.152.214:20006
[inner:A] curl -sS --fail --max-time 10 http://9.135.152.214:20006/
hello-from-B-portmapping
[outer] OK: sandbox A reached B via the node host-port mapping
```

## 5. 常见问题

| 现象 | 可能原因 | 解决方法 |
|------|---------|---------|
| `SSL: CERTIFICATE_VERIFY_FAILED` | HTTPS 但未配置 CA 证书 | 设置 `SSL_CERT_FILE=/root/.local/share/mkcert/rootCA.pem` |
| `Template not found` | 模板 ID 错误 | 重新运行 `cubemastercli tpl list` |
| `Connection refused` | CubeAPI 不可达 | 检查 `E2B_API_URL` 及端口 3000 |
| `Sandbox timeout` | 沙箱超过 TTL | 增大 `Sandbox.create()` 中的 `timeout` |
| `create_with_envs.py` 打印 `session is ` 但值为空 | cubebox/VNC 模板 create-time env 未落到 shell | 示例会 best-effort 调 `apply_create_time_envs()`（失败仅 warn）；仍为空则用 `commands.run(..., envs={...})` |
| 设置 `CUBE_REMOTE_PROXY_BASE` 后 sidecar 未生效 | 只拷贝了 quickstart 目录或 sidecar 启动失败 | 使用完整仓库；检查 warn 信息。控制面脚本仍可运行，数据面需 sidecar 或 DNS |

## 6. 目录结构

```
code-sandbox-quickstart/
├── README.md                  # 英文文档
├── README_zh.md               # 中文文档（本文件）
├── exec_code.py               # 在沙箱中运行 Python 代码
├── cmd.py                     # 执行 Shell 命令
├── create.py                  # 创建沙箱并查看元数据
├── create_with_envs.py        # 创建时注入环境变量
├── env_utils.py               # 共享的 .env 加载辅助脚本
├── read.py                    # 读取沙箱文件系统中的文件
├── pause.py                   # 暂停与恢复沙箱
├── auto-resume.py             # 自动暂停 / 自动恢复（基于空闲超时）
├── auto-kill.py               # 空闲超时后自动销毁（不可恢复）
├── network_no_internet.py     # 完全断网沙箱
├── network_allowlist.py       # 出口 CIDR 白名单
├── network_denylist.py        # 出口 CIDR 黑名单
├── restrict_public_access.py  # 公网 URL 鉴权 token
├── sandbox-in-sandbox.py      # 沙箱 A 内通过 SDK 在沙箱 B 中执行命令
├── sandbox-curl-sandbox.py    # 沙箱 A 内 curl 访问沙箱 B 中的 HTTP 服务
├── sandbox-port-mapping.py    # 沙箱 A 经母机 hostIP:hostPort DNAT 访问 B 的服务
├── requirements.txt           # Python 依赖
└── .env.example               # 环境变量模板
```
