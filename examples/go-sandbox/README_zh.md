# Go 运行时沙箱

一个开箱即用的 **Go 运行时模板**，配合通过 E2B 兼容 Python SDK 驱动的最小示例。
可在隔离的 KVM MicroVM 中编译并单元测试 Go 程序。

[English](README.md)

## 1. 目录内容

| 文件 | 说明 |
|------|------|
| `Dockerfile` | 基于 `cubesandbox-base` 构建 Go 运行时镜像（预装 `go` 工具链，envd 保留在 `:49983`）。 |
| `env_utils.py` | 共享的 `.env` 加载器——与其它示例使用的 `env_utils` 辅助模块一致。 |
| `go_test.py` | 写入 Go 模块与单元测试，运行 `go test -v` 并断言通过。 |
| `go_egress_restricted.py` | 在受限出口策略下运行 Go：离线零依赖构建/测试、验证出口确被管控、依赖下载的白名单方案。 |

## 2. 前置条件

- 已部署并运行中的 Cube Sandbox（`cubemastercli` 在 `$PATH`，已设置 `CUBEMASTER_ADDR`）。
- Docker（用于构建/推送模板镜像）。
- Python 3.8+。

```bash
pip install -r requirements.txt
```

脚本通过 `python-dotenv` 尽力加载当前目录或工作目录下的 `.env`
（不覆盖已设置的环境变量）。复制模板并填写即可：

```bash
cp .env.example .env
# 编辑 .env：填入 E2B_API_URL 与 CUBE_TEMPLATE_ID
```


## 3. 构建、推送并注册模板

### 第一步 —— 构建镜像

```bash
docker build -t cubesandbox-go:latest examples/go-sandbox
```

> **跨架构说明：** 若你的 CubeMaster 节点为 ARM64，请在上面的 `docker build`
> 命令加上 `--platform linux/arm64`（Go 工具链默认从 `golang:1.23.4-bookworm`
> 复制，其为 `amd64`）。

### 第二步 —— 推送到可达的镜像仓库

CubeMaster 会从**集群节点可访问**的镜像仓库拉取镜像。请使用你自己的腾讯云
镜像仓库（TCR）命名空间（或任何节点可拉取的仓库）：

```bash
docker tag  cubesandbox-go:latest <your-registry>/cubesandbox-go:latest
docker push <your-registry>/cubesandbox-go:latest
```

### 第三步 —— 注册为 Cube 模板

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/cubesandbox-go:latest \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health
```

- `49983` 是 envd 的就绪/探针端口——模板达到 `READY` **必须**暴露它；探针
  `GET /health` 返回 204 即视为就绪。
- `4G` 可写层：Go 模块与构建缓存体积较大，请按你的依赖规模调整。

跟踪构建进度：

```bash
cubemastercli tpl watch --job-id <job_id>   # READY 或 FAILED 时退出
```

记下输出中的 `template_id`。

### 第四步 —— 配置环境变量

```bash
export E2B_API_KEY=e2b_000000
export E2B_API_URL=http://<your-node-ip>:3000
export CUBE_TEMPLATE_ID=<template-id>
```

## 4. 运行示例

```bash
python go_test.py            # 编译 + 单元测试
```

脚本从环境变量（或 `.env`）读取 `CUBE_TEMPLATE_ID`，通过 `E2B_API_URL` /
`E2B_API_KEY` 连接，并基于 Go 模板拉起沙箱。脚本成功时打印 `✓`，失败时返回非零退出码。


## 5. 资源建议

| 配置项 | 建议 | 原因 |
|--------|------|------|
| `writable-layer-size` | `4G`（依赖多时更大） | 模块缓存（`GOPATH=/workspace/go`）与构建缓存（`GOCACHE=/workspace/.cache/go-build`）均在可写层中。 |
| 实例 CPU/内存 | 至少 1 vCPU / 1–2 GB | `go build` 偏 CPU；测试按 `GOMAXPROCS` 并行。 |

## 6. 已知限制

- **无官方预构建 Go 镜像。** 与 `sandbox-code` 不同，Go 镜像由本仓库
  `Dockerfile`（基于 `cubesandbox-base`）构建。请固定 `GOLANG_VERSION` 与
  base 标签以保证可复现。
- **架构。** 工具链从 `golang:<ver>-bookworm` 按构建机架构复制。请用
  `--platform` 与节点架构对齐，否则模板启动可能失败。
- **`go mod` 需要出口。** 无依赖的纯 `go test`/`go build` 可完全离线运行；
  但拉取模块需要公网或出口白名单。
- **未预装 `git`。** 为使镜像构建不依赖 Ubuntu 软件源，`Dockerfile` 未安装
  `git`；示例将 `GOPROXY` 固定为 `proxy.golang.org`，模块以 zip 从代理下载，
  无需 VCS 访问。如需 `GOPROXY=direct` 或私有仓库的 VCS 拉取，请在派生镜像中
  自行 `apt-get install git`。
- **`GOPROXY` 默认值。** 模板保留上游默认（`proxy.golang.org,direct`）。在受
  监管环境请将 `GOPROXY` 指向私有代理并相应收紧出口。

## 7. 安全对齐

- 模板继承 `cubesandbox-base` 安全基线（envd 鉴权、隔离内核/网络），启动时
  不开放额外端口或服务。
- 镜像不内置任何密钥；运行期配置通过 `Sandbox.create` 的 `env_vars`/`network`
  传入。
- 镜像固定 `GOTOOLCHAIN=local`，工具链绝不自动从网络下载——既是供应链安全
  防护，也保证离线构建可复现。

## 8. 出口受限场景（差异化能力）

Go 在受监管环境中最大的优势是：**没有任何外部依赖的模块可以完全离线编译并
单元测试**。Cube Sandbox 在 Cubelet 的 tap 网络层（内核级）执行出口策略，
因此沙箱内部无法绕过。`go_egress_restricted.py` 演示这一能力：

```bash
python go_egress_restricted.py
```

它依次执行三项检查：

1. **离线零依赖构建/测试** —— 以 `allow_internet_access=False` 创建沙箱，写入
   仅依赖标准库的模块并运行 `go test -v`。因 `GOTOOLCHAIN=local` + `-mod=readonly`
   杜绝一切网络访问，完全离线通过。
2. **出口确被管控（非摆设）** —— 同一离线沙箱无法 `go mod download` 外部模块
   （`rsc.io/quote`），命令失败，证明策略真实生效。
3. **依赖白名单方案（可选）** —— 设置 `CUBE_GOPROXY_CIDRS`，仅放行你的私有模块
   代理（+ DNS）CIDR，其余一律阻断：

   ```bash
   export CUBE_GOPROXY_CIDRS="10.0.1.5/32,10.0.0.53/32"   # 代理 + DNS
   export CUBE_GOPROXY_URL="http://10.0.1.5:8080"          # 可达的代理
   python go_egress_restricted.py
   ```

   白名单模式下 `go mod download` 仅能到达该代理，公网仍然不可达。这正是受监管
   环境的推荐做法：通过内部代理让 `go mod` 可用，而不是放开通用出口。

| 模式 | `Sandbox.create` 参数 | 效果 |
|------|----------------------|------|
| 离线 | `allow_internet_access=False` | 零依赖 Go 构建/测试可跑；全部出口被阻断 |
| 白名单 | `allow_internet_access=False`，`network={"allow_out":[...]}` | 仅列出的 CIDR（如代理+DNS）可达 |

## 9. 目录结构

```
go-sandbox/
├── Dockerfile                 # Go 运行时镜像（cubesandbox-base + go 工具链）
├── README.md                  # 英文文档
├── README_zh.md               # 本文件
├── requirements.txt           # Python 依赖
├── env_utils.py               # 共享 .env 加载器（与其它示例一致）
├── .env.example               # 环境变量模板
├── go_test.py                 # go test 示例
└── go_egress_restricted.py    # 离线 / 白名单出口示例
```


