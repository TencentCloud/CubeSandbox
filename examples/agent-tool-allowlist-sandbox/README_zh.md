# Agent 工具白名单沙箱

[English](README.md)

演示在 `Sandbox.create` **之前**对 Agent 工具做 argv 白名单：
白名单内命令在 Cube Sandbox MicroVM 中执行；非白名单在宿主机失败（拒绝路径不建沙箱）。

门控在宿主机（`assert_allowlisted`），不能替代出口 CIDR 或 guest 内核强制。

**适用：** 调用方只允许转发固定工具集（`echo` / `ls` / `python3` 等），并尽早拒绝 shell / 网络类工具。

**不适用：** 完整 Agent 框架、任意解释器，或替代 [`network-policy`](../network-policy)。

## 1. 前置条件

- 已部署的 Cube Sandbox（[开发环境](../../docs/zh/guide/dev-environment.md)）
- Python 3.8+
- 仅 Path B 需要 Docker

```bash
pip install -r requirements.txt
```

## 2. Quick Start

### Step 1 — 创建模板（推荐）

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
```

记下 `template_id`。海外可用
`cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest`。

| 项 | 建议 |
|----|------|
| 可写层 | `1G` |
| 端口 | `49999`、`49983`；`--probe 49999` |

### Step 2 — 配置环境

```bash
cp .env.example .env
# 填写 E2B_API_URL（如 http://127.0.0.1:13000）与 CUBE_TEMPLATE_ID
```

### Step 3 — 运行

```bash
python verify_local.py          # 单测 + 拒绝；不需沙箱
python run_allowlisted.py       # 放行（需 *.cube.app DNS 或在开发虚机内）
python run_denied.py            # 宿主机拒绝；不建沙箱
```

放行（宿主机门控通过后，`Sandbox.create` 带
`allow_internet_access=False`——[`network-policy`](../network-policy) Mode 1
断网；argv 放行 ≠ 网络通）：

```text
egress: allow_internet_access=False (airgap; argv gate != network)
agent-tool-allowlist-ok
artifact: artifact-ok
```

拒绝：

```text
denied_as_expected: command not on tool allowlist: 'bash' ...
```

拒绝路径零 `Sandbox.create` 的证据：见 `test_allowlist.py` 中
`test_deny_path_never_calls_sandbox_create`（`verify_local.py` 会跑到）。

宿主机无 `*.cube.app` DNS 时，用 `python run_allowlisted_sidecar.py` 替代
`run_allowlisted.py`（代理变量见 `.env.example`；另见
[`e2b-dev-sidecar`](../e2b-dev-sidecar)）。

## 3. Path B（可选）— 构建本示例镜像

```bash
docker build -t agent-tool-allowlist-sandbox:latest .
# 可选: --build-arg SANDBOX_CODE_IMAGE=.../sandbox-code:latest

cubemastercli tpl create-from-image \
  --image agent-tool-allowlist-sandbox:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
```

若 CubeMaster 看不到本地 tag，先打标签推到可拉取的仓库。

## 4. 默认白名单

`allowlist.py`（`DEFAULT_ALLOWED_BINARIES`）：`echo`、`uname`、`pwd`、`ls`、
`cat`、`head`、`wc`、`sha256sum`、`python3`。路径形式二进制以及 `bash` / `curl`
等会被拒绝。

## 5. 限制

- 仅宿主机门控——跳过 `assert_allowlisted` 仍可向 API 发任意命令。
- 不能替代出口 CIDR（`network-policy`）或 guest seccomp / AppArmor。放行路径叠加
  Mode 1 断网，用以演示两层正交。
- Path B 写入的 `/etc/cube-sandbox/tool-allowlist.txt` 仅为信息标记；本演示以宿主机门控为准。

## 6. 目录结构

```text
agent-tool-allowlist-sandbox/
├── README.md
├── README_zh.md
├── Dockerfile
├── allowlist.py
├── allowlist_sync.py
├── verify_local.py
├── run_allowlisted.py
├── run_allowlisted_sidecar.py
├── run_denied.py
├── test_allowlist.py
├── env_utils.py
├── requirements.txt
└── .env.example
```
