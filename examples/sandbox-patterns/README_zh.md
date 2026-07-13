# CubeSandbox 场景化示例

[English](README.md)

四个场景化示例，展示 **CubeSandbox 平台模式**——有状态工作区、出口网络策略执行、
检查点驱动开发、多沙箱协作。

模板以 **Rust 工具链**为工作负载载体，但展示的模式本身与语言无关。

---

## 这些 Demo 模拟了什么？

### 1. `parallel_workspaces.py` — 有状态工作区生命周期

**模拟场景**：AI Agent 同时管理多个有状态工作区（例如同时分析多个代码仓库）。

执行过程：
- 并行创建 3 个沙箱工作区。
- 每个工作区编译并运行工作负载，报告创建时间和状态。
- 生命周期 `on_timeout: pause` + `auto_resume: True` 确保工作区在空闲时存活。
- `get_info()` 提供实时状态自省。

### 2. `network_isolation.py` — 出口网络策略执行

**模拟场景**：两个安全策略不同的 Agent — 一个可以从互联网下载依赖，另一个必须完全离线运行。

执行过程：
- 两个沙箱用相同的工作负载并行创建。
- sb-1 设置 `allow_internet_access=True` — 成功拉取依赖。
- sb-2 设置 `allow_internet_access=False` — 被出口策略阻止。
- 工作负载完全一致，只有按沙箱设置的策略不同。

### 3. `snapshot_driven_dev.py` — 检查点驱动的迭代开发

**模拟场景**：Agent 对代码库进行迭代调试 — 修改代码、遇到问题、回滚到已知的正确状态。

执行过程：
- 阶段 1：创建沙箱，搭建项目，编译。
- 阶段 2：创建 CubeCoW 快照（检查点 A）。
- 阶段 2b：杀死源沙箱 — 快照仍然独立存在。
- 阶段 3：从检查点 A 分叉出一个新沙箱（恢复工作区状态）。
- 阶段 4：在分叉中修改代码，然后回滚到检查点 A（亚秒级）。
- 阶段 5：通过 `Sandbox.create(template=snap_id)` 从快照分叉出 3 个沙箱。

### 4. `multi_container.py` — 多沙箱协作

**模拟场景**：CI/CD 流水线 — 构建服务（有网络）编译产物，测试服务（离线）运行产物——通过角色分离实现纵深防御。

执行过程：
- 构建者沙箱（允许联网）下载依赖并编译 release 二进制文件。
- 宿主 SDK 从构建者沙箱读取二进制文件。
- 运行者沙箱（离线）接收二进制文件并执行。
- 运行者无需联网即可成功运行。

## 展示的 CubeSandbox 能力

| 示例 | 场景 | CubeSandbox 能力 |
|------|------|-----------------|
| `parallel_workspaces.py` | 有状态工作区生命周期管理 | **生命周期** — 通过 `lifecycle` 自动暂停/恢复 <br> **自省** — `get_info()` 查询沙箱状态 <br> **并发工作区** — 多沙箱并行 |
| `network_isolation.py` | 出口网络策略执行 | **安全** — 按沙箱设置 `allow_internet_access` <br> **环境变量注入** — 创建时通过 `envs=` 参数 <br> **策略对比** — 在线 vs 离线沙箱对比 |
| `snapshot_driven_dev.py` | 检查点驱动的迭代开发 | **CubeCoW 快照** — 检查点独立于源沙箱存在 <br> **即时回滚** — 从检查点恢复 <br> **分叉** — `Sandbox.create(template=snap_id)` 批量分叉 <br> **快照管理** — `list_snapshots()` + `delete_snapshot()` |
| `multi_container.py` | 多沙箱协作 | **基于角色的隔离** — 构建者（在线）vs 运行者（离线） <br> **跨沙箱产物传输** — 通过宿主 SDK |

---

## 目录结构

```
sandbox-patterns/
├── Dockerfile                   # 模板镜像 — cubesandbox-base + Rust 工具链
├── .env.example                 # 复制为 .env 并填写
├── .gitignore
├── requirements.txt             # 宿主端驱动依赖（e2b、python-dotenv）
├── env_utils.py                 # .env 加载辅助
├── parallel_workspaces.py       # 场景：有状态工作区生命周期
├── network_isolation.py         # 场景：出口网络策略执行
├── snapshot_driven_dev.py       # 场景：检查点驱动开发
├── multi_container.py           # 场景：多沙箱协作
├── tests/
│   ├── mock_sdk.py              # 离线验证用的 Mock SDK
│   └── run_verification.py      # 运行全部 demo（无需集群）
├── README.md                    # 英文文档
├── README_zh.md                 # 中文文档（本文件）
└── REAL_ENV_VERIFICATION.md     # 真实环境验证指南
```

## 快速开始

### 前置条件

- 已部署 CubeSandbox，CubeAPI 可访问（`http://<node>:3000`）。
- `cubemastercli` 已在 `$PATH` 且已连通集群。
- 构建机装有 Docker，且 registry 能被 Cube 集群拉取。
- Python 3.10+（宿主端驱动脚本）。

### 1. 构建模板镜像

```bash
docker build --platform linux/amd64 \
  -t <your-registry>/sandbox-patterns:latest \
  examples/sandbox-patterns

docker push <your-registry>/sandbox-patterns:latest
```

镜像通过 rustup 安装 Rust 工具链（stable），以及 `gcc`、`git`、`make`、`libssl-dev` 等构建依赖。
构建过程中会预编译一个虚拟 Cargo 项目，缓存 crates.io 索引，减少沙箱内首次编译的等待时间。

### 2. 注册为 Cube 模板

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/sandbox-patterns:latest \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health

cubemastercli tpl watch --job-id <job_id>
```

记录作业达到 `READY` 状态后的 `template_id`。

### 3. 配置宿主端驱动

```bash
cd examples/sandbox-patterns
cp .env.example .env
# 填写 E2B_API_URL、CUBE_TEMPLATE_ID
pip install -r requirements.txt
```

| 变量 | 流经路径 | 说明 |
|---|---|---|
| `E2B_API_URL` | 本地进程 | CubeAPI 地址（`http://<node>:3000`） |
| `E2B_API_KEY` | 本地进程 | 本地开发可用任意非空值 |
| `CUBE_TEMPLATE_ID` | `Sandbox.create(template=...)` | 来自步骤 2 |

### 4. 运行示例

```bash
# 有状态工作区生命周期
python parallel_workspaces.py

# 出口网络策略执行
python network_isolation.py

# 检查点驱动开发
python snapshot_driven_dev.py

# 多沙箱协作
python multi_container.py
```

## 离线验证（无需集群）

```bash
python3 -m venv /tmp/rust-verify-venv
/tmp/rust-verify-venv/bin/pip install python-dotenv pexpect Pillow
/tmp/rust-verify-venv/bin/python tests/run_verification.py
ls verification-logs/
```

## 真实环境验证

参考 [REAL_ENV_VERIFICATION.md](REAL_ENV_VERIFICATION.md) 逐步指南，
在真实 CubeSandbox 集群上验证所有 demo。

## 已知限制

| 限制 | 详情 | 解决方法 |
|------|------|----------|
| **Dockerfile ENV PATH 不继承** | Dockerfile 中设置的 `ENV PATH` 不会被 CubeSandbox MicroVM 运行时继承。`/home/user/.cargo/bin/cargo` 存在但 `$PATH` 中没有。 | Demo 脚本在运行 cargo/rustc 命令时自动前置 `CARGO_HOME` / `RUSTUP_HOME` 环境变量。 |
| **快照克隆后 `$HOME` 上下文丢失** | 从快照创建沙箱时，`rustup home` 指向 `/root/.rustup` 而非 `/home/user/.rustup`，cargo 报 "no active toolchain" 错误。 | Demo 脚本在工具链命令前设置 `CARGO_HOME=/home/user/.cargo RUSTUP_HOME=/home/user/.rustup HOME=/home/user`。 |
| **`files.read()` 默认文本模式** | 不使用 `format="bytes"` 时，`files.read()` 会将二进制内容按 UTF-8 解码，破坏 ELF 等非文本文件。 | 多沙箱协作 demo 使用 `format="bytes"` 保持二进制完整性。 |
| **首次 `cargo build` 较慢** | 即使有预编译缓存，沙箱内首次真实编译因依赖解析和编译仍需 ~20-30s。 | Rust 编译属预期行为。快照 demo 可预编译后创建快照，从快照分叉复用已有编译产物。 |
| **可写层大小** | Rust 编译会产生很大的 `target/` 目录（1-2 GB），默认可写层可能不够。 | 注册模板时使用 `--writable-layer-size 4G`（构建说明中已展示）。 |

## 故障排除

| 现象 | 可能原因 | 解决方法 |
|---|---|---|
| `rustc: command not found` | 模板内未安装 Rust | 重新构建镜像，重新注册模板 |
| `cargo build` 超时 | 首次构建需下载大量 crate | 增加 `--exec-timeout` 或沙箱超时时间 |
| 离线沙箱仍在拉取 crate | 未设置 `allow_internet_access` | 确保传给 `Sandbox.create()` 的参数包含 `allow_internet_access=False` |
| `pause()`/`connect()` 错误 | 平台版本过旧不支持快照 | 升级 CubeSandbox 平台 |
| cargo 权限被拒绝 | 以 root 运行而非 `user` | 确保 Dockerfile 中使用 `USER user` 安装 rustup |

## 关于 CubeSandbox

CubeSandbox 是一个为 **AI Agent 设计**的**即时、并发、安全、轻量**的沙箱服务。
它基于 **RustVMM + KVM** 构建，能在 **60ms** 内创建一个硬件隔离的 MicroVM，
每个实例内存开销不到 **5MB**，单节点可运行数千个沙箱。

### 关键技术支撑

1. **RustVMM + KVM MicroVM** — 用 Rust 编写的最小化 VMM，配合 KVM，毫秒级启动精简 Guest 内核。无共享内核意味着硬件级隔离，且没有全虚拟机的开销。
2. **内存去重与密度优化** — 沙箱内存在页面级别去重，将每个实例的内存开销从 GB 级降至 MB 级，单节点可承载数千个并发沙箱。
3. **CubeCoW 写时复制快照引擎** — 快照只记录自上一个检查点以来的差异，创建、恢复和克隆都在 sub-100ms（CubeSandbox 文档标称性能）。快照是完全独立的对象——杀死源沙箱不会影响它。
4. **按沙箱隔离的网络命名空间** — 每个 MicroVM 拥有独立的网络栈。`allow_internet_access` 在沙箱级别控制出口，而非全局。

### 解决的痛点

| 痛点 | 传统方案 | CubeSandbox |
|---|---|---|
| **冷启动慢** | Docker ~1s，VM ~30s | **<60ms** — 毫秒级沙箱创建 |
| **隔离性弱** | Docker 共享宿主机内核，逃逸漏洞常见 | **硬件级隔离** — 每个沙箱通过 KVM 拥有独立 Guest OS 内核 |
| **资源开销大** | Docker ~100MB，VM ~GB | **<5MB 每个沙箱** — 单节点数千个实例 |
| **状态管理麻烦** | 不支持快照，或快照耗时数秒 | **CubeCoW sub-100ms 快照/回滚** — 快照独立存在，不受源沙箱生命周期影响 |
| **网络策略粒度粗** | 全局设置，难以按实例控制 | **按沙箱设置 `allow_internet_access`** — 精确到每个实例的出口控制 |

## 参考

- 模板指南：[`docs/zh/guide/tutorials/template-from-image.md`](../../docs/zh/guide/tutorials/template-from-image.md)
- BYOI（envd）：[`docs/zh/guide/tutorials/bring-your-own-image.md`](../../docs/zh/guide/tutorials/bring-your-own-image.md)
- 快照 / 克隆 / 回滚：[`docs/zh/guide/snapshot-rollback-clone.md`](../../docs/zh/guide/snapshot-rollback-clone.md)
- 生命周期：[`docs/zh/guide/lifecycle.md`](../../docs/zh/guide/lifecycle.md)
- 网络策略：[`docs/zh/guide/network-policy.md`](../../docs/zh/guide/network-policy.md)
