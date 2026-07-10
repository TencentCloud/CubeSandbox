# Rust Playground + CubeSandbox

[English](README.md)

在 CubeSandbox MicroVM 内编译和运行 Rust 代码——可以用 `rustc` 编写一次性脚本，也可以使用 `cargo` 构建带外部依赖的完整项目，全部在一个隔离、可复现的环境中进行。

本示例包含：

- 一个 `Dockerfile`：在 CubeSandbox 基础镜像上叠加 Rust 工具链（rustup、rustc、cargo）。
- `hello_world.py`：最小示翻：写入 .rs 文件、用 rustc 编译、运行二进制。演示了 `get_info()` 沙箱自省和 `lifecycle` 自动暂停/恢复。
- `with_dependencies.py`：搭建一个带有外部 crate（serde_json、chrono）的 cargo 项目，构建并运行。演示了 `envs=` 在创建时注入环境变量，以及 `get_info()` 和 `lifecycle`。
- `snapshot_rollback.py`：展示 CubeCoW 快照、克隆和回滚在 Rust 迭代开发中的应用。演示了 `Sandbox.list_snapshots()`、`sb.clone(n=N)` 一键克隆和 `Sandbox.delete_snapshot()` 清理快照。

## 目录结构

```
rust-playground/
├── Dockerfile              # CubeSandbox 模板镜像（Rust 工具链）
├── .env.example            # 复制为 .env 并填写
├── .gitignore
├── requirements.txt        # 宿主端驱动依赖（e2b、cubesandbox、python-dotenv）
├── env_utils.py            # .env 加载辅助
├── hello_world.py          # 最小 rustc 编译并运行示翻
├── with_dependencies.py    # 带有外部 crate 的 Cargo 项目
├── snapshot_rollback.py    # 快照 / 克隆 / 回滚工作流
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

## 4. 运行示翻

### Hello world（rustc）

```bash
python hello_world.py
```

写入 Rust 源文件、用 `rustc` 编译、执行二进制。期望输出：

```
--- Running hello ---
stdout: Hello from CubeSandbox Rust playground!
Current time: <unix-timestamp>
```

### 带依赖的 Cargo 项目

```bash
python with_dependencies.py
```

在沙箱内搭建 Cargo 项目，从 crates.io 拉取 `serde_json` 和 `chrono`，用 `cargo build --release` 构建，然后运行二进制。首次构建会下载 crate，cargo 的 registry 缓存会保留在沙箱中。

### 快照、克隆与回滚

```bash
python snapshot_rollback.py
```

使用原生 `cubesandbox` SDK 演示 CubeSandbox 最具差异化的功能：

1. **快照** — 在开发过程中保存沙箱状态（检查点 A）。
2. **修改** — 修改 Rust 代码并重新构建（检查点 B）。
3. **回滚** — 在约 100ms 内将沙箱恢复到检查点 A。
4. **克隆** — 从检查点 A 分叉出一个新沙箱，原沙箱继续运行互不影响。

## 故障排除

| 现象 | 可能原因 | 解决方法 |
|---|---|---|
| `rustc: command not found` | 模板内未安装 Rust | 重新构建镜像，重新注册模板 |
| `cargo build` 超时 | 首次构建需下载大量 crate | 增加 `--exec-timeout` 或沙箱超时时间 |
| 就绪探测超时 | 镜像没有 envd | 确保使用 `FROM ghcr.io/tencentcloud/cubesandbox-base:...` |
| `pause()`/`connect()` 错误 | 平台版本过旧不支持快照 | 升级 CubeSandbox 平台 |
| cargo 权限被拒绝 | 以 root 运行而非 `user` | 确保 Dockerfile 中使用 `USER user` 安装 rustup |

## 参考

- 模板指南：[`docs/zh/guide/tutorials/template-from-image.md`](../../docs/zh/guide/tutorials/template-from-image.md)
- BYOI（envd）：[`docs/zh/guide/tutorials/bring-your-own-image.md`](../../docs/zh/guide/tutorials/bring-your-own-image.md)
- 快照 / 克隆 / 回滚：[`docs/zh/guide/snapshot-rollback-clone.md`](../../docs/zh/guide/snapshot-rollback-clone.md)
