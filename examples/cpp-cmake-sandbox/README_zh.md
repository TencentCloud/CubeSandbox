# C/C++ CMake 沙箱

[English](README.md)

一个开箱即用的 Cube Sandbox **C/C++ 开发模板**：在隔离的 MicroVM 中用
**CMake + Ninja** 构建一个最小 C++17 项目，并借助**快照持久化并恢复 `ccache`
编译缓存**，实现秒级增量构建。

本模板与 Node / Python / Go / Rust / Java 模板相互独立，专注 C/C++ 工具链。

## 1. 内容一览

| 文件 | 作用 |
|------|------|
| `Dockerfile` | 基于 `cubesandbox-base` 的 C/C++ 开发镜像（gcc/g++/clang、CMake、Ninja、ccache、gdb） |
| `project/` | 最小 C++17 项目：`greeter` 静态库 + `app` 可执行文件 + 一个 CTest 用例，**零第三方依赖** |
| `01_build_in_sandbox.py` | E2B SDK：推送项目 → CMake+Ninja 构建 → 运行 `./app` |
| `02_run_ctest.py` | E2B SDK：构建并运行 CTest 测试 |
| `03_ccache_snapshot.py` | 原生 SDK：冷构建 → 快照（含 ccache）→ 克隆 → 热增量构建，输出耗时与加速倍数 |
| `04_ccache_rollback.py` | 原生 SDK：热缓存快照 → 破坏工作区 → `rollback()` → 重建命中缓存 |

> **刻意使用两套 SDK。** `01`/`02` 使用 E2B 兼容 SDK（`e2b_code_interpreter`），
> 与[代码沙箱快速入门](../code-sandbox-quickstart)保持一致；`03`/`04` 使用原生
> `cubesandbox` SDK，因为其快照 / 回滚 API 更直接。见[环境变量](#4-环境变量)。

## 2. 适用场景

- C/C++ CI：在干净隔离的环境中编译并测试项目。
- 增量构建加速：预热 `ccache` 后打快照，克隆或恢复出来的沙箱直接命中缓存，
  无需从零重编。
- 长时构建的断点续跑：通过快照 + 回滚实现 checkpoint / resume。

**资源建议：** `--writable-layer-size 4G`（C/C++ 产物 + ccache 比脚本语言模板更大），
ccache 上限设为 2G。

**已知限制：** 仅支持 linux/amd64；未接入第三方包管理器（vcpkg / Conan）——
示例项目刻意做到零依赖。

## 3. 快速开始

### 步骤 1 — 构建镜像并注册模板

```bash
# 本地构建（在仓库根目录执行）
docker build -t cubesandbox-cpp-cmake:latest examples/cpp-cmake-sandbox

# 可选的本地自检：envd 就绪探针应返回 204
docker run --rm -d -p 49983:49983 --name cube-cpp cubesandbox-cpp-cmake:latest
curl -s -o /dev/null -w "envd /health => %{http_code}\n" http://127.0.0.1:49983/health
docker rm -f cube-cpp

# 推送到你的镜像仓库，然后注册为 Cube 模板
cubemastercli tpl create-from-image \
    --image <your-registry>/cubesandbox-cpp-cmake:latest \
    --writable-layer-size 4G \
    --expose-port 49983 \
    --probe       49983 \
    --probe-path  /health
```

记下成功后打印的 `template_id`。

### 步骤 2 — 安装依赖并配置环境

```bash
pip install -r requirements.txt

cp .env.example .env
# 编辑 .env，填入下方变量
```

### 步骤 3 — 运行示例

```bash
# E2B SDK：构建 + 运行
python 01_build_in_sandbox.py

# E2B SDK：构建 + ctest
python 02_run_ctest.py

# 原生 SDK：通过快照 + 克隆持久化 ccache（核心亮点）
python 03_ccache_snapshot.py

# 原生 SDK：原地回滚到热缓存快照
python 04_ccache_rollback.py
```

预期亮点：

- `01` 打印 `Hello, CubeSandbox!`
- `02` 打印 `100% tests passed`
- `03` 打印 `first build` 与 `rebuild after snapshot` 的耗时对比，以及一行
  `speedup: Nx`
- `04` 回滚后 `ccache` 统计显示缓存命中数 > 0

## 4. 环境变量

| 变量 | 使用脚本 | 含义 |
|------|----------|------|
| `E2B_API_URL` | `01`、`02` | Cube API 地址，如 `http://<node-ip>:3000` |
| `E2B_API_KEY` | `01`、`02` | 任意非空值即可满足 E2B SDK 校验 |
| `CUBE_API_URL` | `03`、`04` | Cube API 地址（由原生 SDK 的 `Config` 读取） |
| `CUBE_TEMPLATE_ID` | 全部 | `create-from-image` 得到的模板 ID |
| `SSL_CERT_FILE` | 可选 | CubeAPI 走 HTTPS 时的根证书路径 |

## 5. 本地试跑项目（可选）

`project/` 是标准 CMake 项目，推送进沙箱前可在任意带 C++ 工具链的机器上先验证：

```bash
cd project
cmake -G Ninja -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build
./build/app                 # -> Hello, CubeSandbox!
ctest --test-dir build --output-on-failure
```

## 6. 故障排查

| 现象 | 可能原因 | 解决方法 |
|------|----------|----------|
| `SSL: CERTIFICATE_VERIFY_FAILED` | HTTPS 缺少 CA 证书 | 设置 `SSL_CERT_FILE=/root/.local/share/mkcert/rootCA.pem` |
| `Template not found` | 模板 ID 错误 | 重新执行 `cubemastercli tpl list` |
| `Connection refused` | 无法连到 CubeAPI | 检查 `E2B_API_URL` / `CUBE_API_URL` 及 3000 端口 |
| 构建超时 | 默认命令超时太短 | 脚本已传 `timeout=300`，需要时再调大 |
| `speedup` 接近 `1x` | 缓存未复用 | 确认 Dockerfile 中的 `CCACHE_DIR=/workspace/.ccache` 已包含在快照内 |

## 7. 目录结构

```
cpp-cmake-sandbox/
├── README.md                 # 英文文档
├── README_zh.md              # 中文文档（本文件）
├── Dockerfile                # 基于 cubesandbox-base 的 C/C++ 开发镜像
├── requirements.txt          # Python 依赖（两套 SDK）
├── .env.example              # 环境变量模板
├── env_utils.py              # 共享 .env 加载器（E2B 脚本）
├── env.py                    # TEMPLATE_ID 辅助（原生脚本）
├── seed.py                   # 将 project/ 推入沙箱
├── project/                  # 最小 C++17 CMake 项目
│   ├── CMakeLists.txt
│   ├── include/greeter.hpp
│   ├── src/greeter.cpp
│   ├── src/main.cpp
│   └── tests/test_greeter.cpp
├── 01_build_in_sandbox.py    # E2B：构建 + 运行
├── 02_run_ctest.py           # E2B：构建 + ctest
├── 03_ccache_snapshot.py     # 原生：快照持久化 ccache + 克隆
└── 04_ccache_rollback.py     # 原生：回滚到热缓存快照
```

## 8. 相关链接

- [使用自定义镜像（envd）](../../docs/guide/tutorials/bring-your-own-image.md)
- [快照 · 回滚 · 克隆](../snapshot-rollback-clone) —— 原生 SDK 快照 API
- [代码沙箱快速入门](../code-sandbox-quickstart) —— 基础 E2B 流程
