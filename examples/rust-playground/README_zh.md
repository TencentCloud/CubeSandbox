# Rust Playground + CubeSandbox

[English](README.md)

在 CubeSandbox MicroVM 内编译和运行 Rust 代码——本示例通过 Rust 工作负载展示 CubeSandbox
**即时、并发、安全、轻量** 的核心价值。

## 展示的 CubeSandbox 功能

| 示例 | CubeSandbox 概念 |
|------|-----------------|
| `hello_world.py` | **即时** + **并发** — 并行创建 3 个沙箱并计时 <br> **生命周期** — 通过 `lifecycle` 自动暂停/恢复 <br> **自省** — `get_info()` 查询沙箱状态 |
| `with_dependencies.py` | **安全** — 通过 `allow_internet_access` 实现网络隔离 <br> **并发构建** — 在线 vs 离线沙箱对比 <br> **环境变量注入** — 创建时通过 `envs=` 参数设置 |
| `snapshot_rollback.py` | **CubeCoW 快照** — 检查点独立于源沙箱存在 <br> **即时回滚** — ~100ms 恢复 <br> **克隆** — `sb.clone(n=N)` 一键分叉 <br> **快照管理** — `list_snapshots()` + `delete_snapshot()` |

## 目录结构

```
rust-playground/
├── Dockerfile              # CubeSandbox 模板镜像（Rust 工具链）
├── .env.example            # 复制为 .env 并填写
├── .gitignore
├── requirements.txt        # 宿主端驱动依赖（e2b、cubesandbox、python-dotenv）
├── env_utils.py            # .env 加载辅助
├── hello_world.py          # 即时 + 并发：3 个沙箱并行编译
├── with_dependencies.py    # 安全：通过 allow_internet_access 网络隔离
├── snapshot_rollback.py    # CubeCoW 快照独立于沙箱 + 克隆 + 回滚
├── tests/
│   ├── mock_sdk.py         # 离线验证用的 Mock SDK
│   └── run_verification.py # 运行全部 3 个 demo（无需集群）
├── README.md               # 英文文档
└── README_zh.md            # 中文文档（本文件）
```

## 前置条件

- 已部署 CubeSandbox，CubeAPI 可访问（`http://<node>:3000`）。
- `cubemastercli` 已在 `$PATH` 且已连通集群。
- 构建机装有 Docker，且 registry 能被 Cube 集群拉取。
- Python 3.10+（宿主端驱动脚本）。

## 1. 构建模板镜像

```bash
docker build --platform linux/amd64 \
  -t <your-registry>/rust-playground:latest \
  examples/rust-playground

docker push <your-registry>/rust-playground:latest
```

镜像通过 rustup 安装 Rust 工具链（stable），以及 `gcc`、`git`、`make`、`libssl-dev` 等构建依赖。可通过 `--build-arg RUST_TOOLCHAIN=nightly` 切换工具链版本。

## 2. 注册为 Cube 模板

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/rust-playground:latest \
  --writable-layer-size 2G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health

cubemastercli tpl watch --job-id <job_id>
```

记录作业达到 `READY` 状态后的 `template_id`。

## 3. 配置宿主端驱动

```bash
cd examples/rust-playground
cp .env.example .env
# 填写 E2B_API_URL、CUBE_TEMPLATE_ID
pip install -r requirements.txt
```

| 变量 | 流经路径 | 说明 |
|---|---|---|
| `E2B_API_URL` | 本地进程 | CubeAPI 地址（`http://<node>:3000`） |
| `E2B_API_KEY` | 本地进程 | 本地开发可用任意非空值 |
| `CUBE_TEMPLATE_ID` | `Sandbox.create(template=...)` | 来自步骤 2 |

## 4. 运行示例

### 即时 + 并发（hello_world.py）

```bash
python hello_world.py
```

并行创建 3 个沙箱，在每个沙箱中编译并运行 Rust 程序，报告创建和编译耗时：

```
  [sb-0] created in 0.87s  id=sb-xxx  state=running
  [sb-1] created in 0.92s  id=sb-yyy  state=running
  [sb-2] created in 1.01s  id=sb-zzz  state=running
Total: 3 sandboxes in 3.21s  (1.07s avg per sandbox)
```

### 网络隔离（with_dependencies.py）

```bash
python with_dependencies.py
```

对比两个沙箱——一个有互联网访问，一个没有：

```
  sb-1 (online)    : PASS — cargo 成功从 crates.io 拉取
  sb-2 (offline)   : FAIL — cargo 被网络隔离阻止
```

展示了 `allow_internet_access=False` 这一关键安全功能。

### 快照独立于沙箱（snapshot_rollback.py）

```bash
python snapshot_rollback.py
```

展示 CubeSandbox 最具差异化的功能：

1. **快照** — 在开发过程中保存沙箱状态。
2. **快照独立于沙箱** — 杀死源沙箱后仍可从快照克隆出新沙箱。
3. **回滚** — 在 ~100ms 内将沙箱恢复到检查点 A。
4. **克隆(n)** — `sb.clone(n=3)` 一键创建 3 个分叉。

## 离线验证（无需集群）

```bash
python3 -m venv /tmp/rust-verify-venv
/tmp/rust-verify-venv/bin/pip install python-dotenv pexpect Pillow
/tmp/rust-verify-venv/bin/python tests/run_verification.py
ls verification-logs/
```

## 故障排除

| 现象 | 可能原因 | 解决方法 |
|---|---|---|
| `rustc: command not found` | 模板内未安装 Rust | 重新构建镜像，重新注册模板 |
| `cargo build` 超时 | 首次构建需下载大量 crate | 增加 `--exec-timeout` 或沙箱超时时间 |
| 离线沙箱仍在拉取 crate | 未设置 `allow_internet_access` | 确保传给 `Sandbox.create()` 的参数包含 `allow_internet_access=False` |
| `pause()`/`connect()` 错误 | 平台版本过旧不支持快照 | 升级 CubeSandbox 平台 |
| cargo 权限被拒绝 | 以 root 运行而非 `user` | 确保 Dockerfile 中使用 `USER user` 安装 rustup |

## 参考

- 模板指南：[`docs/zh/guide/tutorials/template-from-image.md`](../../docs/zh/guide/tutorials/template-from-image.md)
- BYOI（envd）：[`docs/zh/guide/tutorials/bring-your-own-image.md`](../../docs/zh/guide/tutorials/bring-your-own-image.md)
- 快照 / 克隆 / 回滚：[`docs/zh/guide/snapshot-rollback-clone.md`](../../docs/zh/guide/snapshot-rollback-clone.md)
- 生命周期：[`docs/zh/guide/lifecycle.md`](../../docs/zh/guide/lifecycle.md)
- 网络策略：[`docs/zh/guide/network-policy.md`](../../docs/zh/guide/network-policy.md)
