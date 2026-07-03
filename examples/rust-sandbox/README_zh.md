# Rust 沙箱

[English](README.md)

在 Cube Sandbox 中编译和运行 Rust 代码——从一行代码片段到完整的 Cargo 项目，全部通过 E2B Python SDK 完成。

## 1. 背景

**Cube Sandbox** 是轻量级 MicroVM 平台，控制面和数据面完全兼容 [E2B SDK](https://e2b.dev)。本示例在官方 `cubesandbox-base` 镜像之上添加了完整的 Rust 工具链：

- **rustc** 和 **cargo** — Rust 编译器和包管理器，沙箱内所有用户均可直接使用
- **预热好的 crate 缓存** — 常用 crate（`serde`、`serde_json`、`axum`、`tokio`）已在构建镜像时下载并缓存，新沙箱中的首次 `cargo build` 仅需编译你的代码，无需等待下载依赖

所有交互通过 E2B SDK 进行——用 `sandbox.files.write()` 写入 Rust 源文件，用 `sandbox.commands.run("rustc ...")` 编译，再读取运行结果。沙箱是完整的 KVM MicroVM，拥有独立内核、文件系统和网络栈。`with` 块退出时，沙箱自动销毁。

```text
  用户脚本 (E2B SDK)
       │
       │  sandbox.commands.run("rustc ...")
       │  sandbox.commands.run("cargo build --release")
       │  sandbox.files.write(...) / sandbox.files.read(...)
       ▼
  ┌─────────────────────────────┐
  │        KVM MicroVM          │
  │                             │
  │  envd (:49983)              │
  │    │                        │
  │    ▼                        │
  │  rustc   cargo   rustup     │
  │  ~/.cargo/registry/ (已缓存) │
  │  target/                    │
  └─────────────────────────────┘
```

## 2. 前置条件

- 已部署的 Cube Sandbox 环境
- Python 3.8+
- Docker

```bash
pip install -r requirements.txt
```

示例脚本会使用 `python-dotenv` 自动加载脚本所在目录的 `.env` 文件；如果文件不存在，则继续使用当前进程的环境变量。

## 3. 快速开始

### 第一步 — 构建 Docker 镜像

```bash
docker build -t rust-sandbox:latest examples/rust-sandbox/

# 本地冒烟测试
docker run --rm -d -p 49983:49983 --name test-rust rust-sandbox:latest
curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:49983/health  # → 204
docker exec test-rust rustc --version
docker exec test-rust cargo --version
docker rm -f test-rust
```

将镜像推送到 Cube 集群可访问的仓库：

```bash
docker tag rust-sandbox:latest <your-registry>/rust-sandbox:latest
docker push <your-registry>/rust-sandbox:latest
```

### 第二步 — 创建 Rust 模板

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/rust-sandbox:latest \
  --writable-layer-size 2G \
  --expose-port 49983 \
  --expose-port 8080 \
  --probe 49983 \
  --probe-path /health
```

> Rust 的 `target/` 目录体积较大。建议可写层至少设为 2G，依赖较多的项目建议 4G 或更大。

记录输出的 `template_id`。

### 第三步 — 配置环境变量

```bash
cp .env.example .env
# 编辑 .env，填写 E2B_API_URL 和 CUBE_TEMPLATE_ID
```

之后直接运行任意示例脚本即可，无需手动 `export`。

或直接导出：

```bash
export E2B_API_KEY=e2b_000000
export E2B_API_URL=http://<节点IP>:3000
export CUBE_TEMPLATE_ID=<template-id>

# 仅在使用 Cube 内置 mkcert 证书时需要：
# export SSL_CERT_FILE=/root/.local/share/mkcert/rootCA.pem
```

### 第四步 — 运行示例

```bash
# 编译并运行 Rust 示例代码片段
python rust_compile_run.py
```

预期输出：

```
Hello from Rust inside CubeSandbox!
Fibonacci(10) = 55
```

## 4. 所有脚本

| 脚本 | 展示内容 |
| --- | --- |
| `rust_compile_run.py` | `sandbox.files.write()` + `sandbox.commands.run("rustc ...")` — 写入 Rust 源码、编译、运行 |
| `rust_cargo_project.py` | `cargo new` → `cargo build --release` → `cargo run` — 沙箱内完整的 Cargo 工作流 |
| `rust_snapshot_cache.py` | `sandbox.pause()` / `sandbox.connect()` — 冷构建后快照整个 VM，恢复后增量编译只需几秒 |
| `rust_web_service.py` | `sandbox.get_host(8080)` — 构建 axum HTTP 服务，启动后通过 CubeProxy 从外部访问 |
| `rust_secure_eval.py` | `allow_internet_access=False` — 在完全断网的沙箱中编译和运行不受信任的 Rust 代码 |
| `test_rust_sandbox.py` | 针对 Docker 镜像的本地冒烟测试，无需 Cube 集群 |

### rust_compile_run.py — 编译运行

```python
with Sandbox.create(template=template_id) as sandbox:
    sandbox.files.write("/tmp/main.rs", code)
    sandbox.commands.run("rustc -o /tmp/main /tmp/main.rs")
    result = sandbox.commands.run("/tmp/main")
    print(result.stdout)
```

### rust_cargo_project.py — Cargo 项目

```python
with Sandbox.create(template=template_id) as sandbox:
    sandbox.commands.run("cd /home/user && cargo new hello-cube")
    sandbox.files.write("/home/user/hello-cube/src/main.rs", source)
    sandbox.commands.run("cd /home/user/hello-cube && cargo build --release")
    result = sandbox.commands.run("/home/user/hello-cube/target/release/hello-cube 10 42 99")
    print(result.stdout)
```

### rust_snapshot_cache.py — 快照与恢复

创建带依赖的 Cargo 项目，完成冷构建（约 30–60 秒），暂停沙箱释放资源，之后恢复。 `target/` 目录和 crate 缓存在暂停/恢复周期中完整保留，下次构建仅重新编译变更的文件：

```python
with Sandbox.create(template=template_id) as sandbox:
    # 冷构建 — 下载依赖、全量编译
    sandbox.commands.run("cd /home/user/snapshot-demo && cargo build --release")

    sandbox.pause()       # 保存 VM 快照，释放计算资源
    sandbox.connect()     # 从快照恢复，继续执行

    # 热构建 — 仅重新编译变更文件（2–5 秒，而非 30–60 秒）
    sandbox.commands.run("cd /home/user/snapshot-demo && cargo build --release")
```

### rust_web_service.py — 对外暴露 HTTP 服务

```python
with Sandbox.create(template=template_id) as sandbox:
    sandbox.commands.run("cd /opt/rust-demo && cargo build --release")
    sandbox.commands.run(
        "cd /opt/rust-demo && nohup ./target/release/rust-demo > /tmp/server.log 2>&1 &"
    )

    url = f"https://{sandbox.get_host(8080)}/"
    resp = requests.get(url, verify=False)
    print(resp.json())   # {"status":"ok","runtime":"rust",...}
```

### rust_secure_eval.py — 断网环境中的代码评估

```python
with Sandbox.create(
    template=template_id,
    allow_internet_access=False,  # 完全断网
) as sandbox:
    # 将 Cargo.toml 和 main.rs 写入工作区
    sandbox.files.write("/tmp/secure-eval/Cargo.toml", cargo_toml)
    sandbox.files.write("/tmp/secure-eval/src/main.rs", user_code)
    sandbox.commands.run("cd /tmp/secure-eval && cargo build --release")

    result = sandbox.commands.run(
        "timeout 10 sh -c 'ulimit -v 524288 && /tmp/secure-eval/target/release/secure-eval'"
    )
```

## 5. 常见问题

| 现象 | 可能原因 | 解决方法 |
| --- | --- | --- |
| `SSL: CERTIFICATE_VERIFY_FAILED` | HTTPS 缺少 CA 证书 | 设置 `SSL_CERT_FILE=/root/.local/share/mkcert/rootCA.pem` |
| `cargo: command not found` | 沙箱内 PATH 未包含 cargo | 检查 Dockerfile 是否将二进制文件复制到了 `/usr/local/bin/`，如缺失请重建镜像 |
| `cargo build` 卡在 "Updating crates.io index" | 沙箱无网络访问 | 在 Docker 镜像中预填充所需 crate，或设置 `allow_internet_access=True` |
| `Template not found` | 模板 ID 错误 | 重新运行 `cubemastercli tpl list` |
| `Connection refused` | CubeAPI 不可达 | 检查 `E2B_API_URL` 和 3000 端口 |
| "No space left on device" | 可写层空间不足 | 增大 `--writable-layer-size`（至少 2G） |
| `error: linker 'cc' not found` | 缺少编译工具链 | Dockerfile 已包含 `build-essential`，如被移除请重新安装 |
| `sandbox.pause()` 耗时过长 | 可写层数据量过大 | 暂停前清理不再需要的 `target/` 目录 |

## 6. 目录结构

```
rust-sandbox/
├── Dockerfile                    # 基于 cubesandbox-base 的 Rust 工具链镜像
├── demo_project/                 # 预置 axum 演示项目（用于预热 crate 缓存）
│   ├── Cargo.toml
│   └── src/
│       └── main.rs
├── README.md                     # 英文文档
├── README_zh.md                  # 中文文档（本文件）
├── requirements.txt              # Python 依赖
├── .env.example                  # 环境变量模板
├── rust_compile_run.py           # 写入 Rust 源码 → rustc 编译 → 运行
├── rust_cargo_project.py         # cargo new → build → run
├── rust_snapshot_cache.py        # 冷构建 → 暂停 → 恢复 → 热构建
├── rust_web_service.py           # 构建并运行 axum HTTP 服务
├── rust_secure_eval.py           # 断网环境下运行不受信任的代码
└── test_rust_sandbox.py          # 本地 Docker 冒烟测试
```
