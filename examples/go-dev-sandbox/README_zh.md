# Go 开发沙箱

[English](README.md)

一个开箱即用的 **Go 工具链模板**，外加两个可运行示例：基础的 `构建 → 运行 → 测试`
流程，以及一个长时任务 —— **用快照打断点，崩溃后回滚续跑**。

## 适用场景

| 场景 | 这个模板解决什么 |
|---|---|
| Agent 写 Go 代码并编译 | `go build` / `go test` / `go run` 通过 SDK 的命令 API 直接可用 |
| 运行 LLM 生成的不可信 Go 代码 | 每个沙箱是独立内核的 KVM MicroVM，失控的 `go run` 碰不到宿主机 |
| 反复编译-测试的长循环 | 模块缓存预热后打一次快照，后续沙箱直接从快照启动 |
| 需要容错的多步骤任务 | `create_snapshot()` 打断点，`rollback()` 丢弃坏掉的一步并从断点续跑 |
| 一次为多个平台出包 | 每个 `GOOS/GOARCH` 目标一个沙箱，源码经只读 host mount 共享、产物写入共享的读写 `dist/` |

两个示例跑的都是**纯标准库**代码，所以即使沙箱配了严格的出口网络策略、连不上
`proxy.golang.org`，也照样能跑通。

## 目录内容

| 文件 | 说明 |
|---|---|
| [`Dockerfile`](./Dockerfile) | 模板镜像：官方 Go 叠在 `cubesandbox-base` 之上 |
| [`demo.py`](./demo.py) | 上传一个小模块，`go build`、运行、`go test` |
| [`snapshot_resume.py`](./snapshot_resume.py) | 第 5 步打断点，第 8 步崩溃，回滚后跑完 |
| [`fanout_build.py`](./fanout_build.py) | 多个 `GOOS/GOARCH` 目标在并行沙箱中交叉编译，经共享 host mount 收集产物 |
| [`env.py`](./env.py) | 共用的环境变量加载与命令失败检查 |

## 前置条件

- 一套已部署的 Cube Sandbox（[快速开始](../../docs/zh/guide/quickstart.md)）
- `cubemastercli` 在 `$PATH` 中，且已设置 `CUBEMASTER_ADDR`
- 带 **buildx** 组件的 Docker（某些发行版只装了 CLI 而没带插件，需先
  `apt-get install docker-buildx`），以及一个 Cube 集群能拉取的镜像仓库
- Python 3.8+

## 1. 构建并推送镜像

```bash
docker build -t <your-registry>/go-dev-sandbox:latest examples/go-dev-sandbox
docker push <your-registry>/go-dev-sandbox:latest
```

要固定到其他 Go 版本，传 `--build-arg GO_VERSION=1.23.6`。

推送前可以先本地自检 —— envd 必须返回 `204`：

```bash
docker run --rm -d -p 49983:49983 --name go-dev <your-registry>/go-dev-sandbox:latest
curl -s -o /dev/null -w "envd /health => %{http_code}\n" http://127.0.0.1:49983/health
docker exec go-dev go version
docker rm -f go-dev
```

## 2. 注册模板

```bash
cubemastercli tpl create-from-image \
  --image       <your-registry>/go-dev-sandbox:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health
```

镜像本身不跑任何应用，所以就绪探测直接用 envd 的 `:49983/health`。等它变成
`READY`：

```bash
cubemastercli tpl watch --job-id <job_id>
```

记下 `template_id`。完整流程见
[从 OCI 镜像创建模板](../../docs/zh/guide/tutorials/template-from-image.md)。

慢主机上 `--writable-layer-size` 保持 1 GB —— 见[排障](#排障)。

## 3. 配置环境变量

```bash
pip install -r requirements.txt

cp .env.example .env
# 编辑 .env，填入 CUBE_API_URL 和 CUBE_TEMPLATE_ID
```

如果部署环境没有为 `*.cube.app` 配泛解析 DNS，还需要设 `CUBE_PROXY_NODE_IP`
指向 CubeProxy 所在节点（单机部署填 `127.0.0.1`）。不设的话 SDK 会走系统
DNS 去解析 `<port>-<sandbox_id>.cube.app`，连不上沙箱。

## 4. 运行示例

```bash
python demo.py
```

预期输出（`GOARCH` 跟随构建模板所用主机的架构）：

```
sandbox: ab13e8af9d2f48bf8bcb8de2dee1d67a
go version go1.24.9 linux/arm64
build ok
go1.24.9 on linux/arm64
fib(30) = 832040
ok  	cubedemo	12.504s
OK
```

测试耗时主要花在编译上而不是 `Fib` 本身 —— 上面这次是在嵌套虚拟化主机上跑的，
属于偏慢的情况。

```bash
python snapshot_resume.py
```

预期输出：

```
sandbox: <sandbox-id>
build ok
resuming from step 0
step 1 done
...
step 5 done
job finished at step 5
checkpoint at step 5: snap-xxxxxxxx
crashed as designed (exit_code=1): fatal: step 8 crashed
dirty progress: 7
rolled back — progress: 5
resuming from step 5
step 6 done
...
step 10 done
job finished at step 10
final log:
step 1 ok
...
step 10 ok
snapshot deleted: snap-xxxxxxxx
OK
```

第二个示例的重点：崩溃之后沙箱里 `progress=7`、日志里留了一条 `CORRUPTED`
记录。一次 `rollback()` 就把**内存和文件系统**一起恢复到第 5 步的断点 ——
沙箱 ID 不变、不用重启、不用重新编译 —— 然后从那里继续跑完。

第三个示例把交叉编译矩阵扇出到并行沙箱 —— 每个 `GOOS/GOARCH` 目标一个沙箱，
源码树经 [host mount](../../docs/zh/guide/persistent-storage.md) 只读共享，
公共 `dist/` 读写共享。它必须**在沙箱宿主节点上**运行（host mount 映射的是
节点本地路径），且 `hostPath` 必须落在白名单前缀之下 —— 默认为
`/data/shared/`。一次性准备：

```bash
sudo install -d -o "$(id -u)" -g "$(id -g)" /data/shared/go-fanout
python fanout_build.py
```

预期输出（顺序会变 —— 构建是真并发的）：

```
workspace: /data/shared/go-fanout/b63b15ae
targets:   linux/amd64, linux/arm64
[linux/amd64] sandbox: 5fe18673bb5d4145af7552cfb8d3f23a
[linux/arm64] sandbox: 851d5ca4dc6e4423a85df1af93c04ede
[linux/amd64] touch: cannot touch '/mnt/src/should-fail': Read-only file system
read-only enforced
[linux/arm64] build ok
[linux/arm64] runs natively: built for linux/arm64 by go1.24.9
[linux/amd64] build ok

dist/ on the host:
  hello-linux-amd64          2.2 MB
  hello-linux-arm64          2.3 MB

per-target: 642s, 610s  wall total: 642s
artifacts left in /data/shared/go-fanout/b63b15ae/dist — clean up old runs when done
OK
```

每个目标的首次构建都要在冷 `GOCACHE` 下现编标准库，所以上面的耗时（嵌套虚拟化
慢主机）是最坏情况 —— 但注意总墙钟时间仍等于最慢的单个目标，而不是各目标之和。
按[资源建议](#资源建议)所述先预热缓存再打快照，fan-out 就降到秒级。

通过 `FANOUT_TARGETS="linux/amd64,darwin/arm64,windows/amd64"` 可以换目标 ——
每个条目起一个沙箱，按节点能同时启动的数量伸缩列表。全程没有上传下载：
跑完后各平台二进制已经躺在宿主机的 `dist/` 里。沙箱内命令以 root 运行，
所以写回宿主的文件属主是 root。

## 资源建议

| 负载 | 建议规格 | 可写层 |
|---|---|---|
| 跑这两个示例 | 1 vCPU / 1 GB | 1 GB |
| 编译一个小服务 | 2 vCPU / 2 GB | 4 GB |
| 大型模块树上 `go build` | 4 vCPU / 4 GB | 8 GB+ |

镜像约 600 MB（`linux/arm64` 实测 594 MB，其中大部分是 Go 发行包）。可写层还要
装得下构建缓存 —— 反复构建时 `GOCACHE` 涨得很快。建议在模块缓存预热后给沙箱打
一次快照，后续沙箱直接从该快照创建，省掉整个下载过程。可写层超过 1 GB 在裸金属
上没问题，但在慢主机上可能撞上 shim 固定的 10 秒启动预算。

## 已知限制

- **这个模板用不了 `sandbox.run_code()`。** 该 API 需要 Jupyter 内核，只有
  `sandbox-code` 镜像带。请改用 `sandbox.commands.run()` 和
  `sandbox.files.write()` —— 两个示例都是这么做的。
- **`cgo` 已关闭**（`CGO_ENABLED=0`），镜像里没有 C 工具链。需要 cgo 的话在
  Dockerfile 里加上 `gcc` / `libc6-dev`。
- **官方发布的基础镜像只有 `linux/amd64`。** 这是发布口径的问题，不是可移植性
  问题：CI 构建 `cubesandbox-base` 时写死了 `platforms: linux/amd64`
  （`.github/workflows/build-envd-base-image.yml`），而
  `docker/Dockerfile.cube-base` 本身是架构无关的。在 `aarch64` 主机上自行构建
  基础镜像再指过来即可，**不需要改动仓库任何文件**：

  ```bash
  docker build -f docker/Dockerfile.cube-base -t cubesandbox-base:local docker/
  docker build --build-arg CUBE_BASE_IMAGE=cubesandbox-base:local \
    -t <your-registry>/go-dev-sandbox:latest examples/go-dev-sandbox
  ```

  这条路径已在 `linux/arm64` 上端到端验证过。注意 `Dockerfile.cube-base` 用了
  `RUN --mount=type=cache`，这一步同样需要 buildx。
- **拉取模块需要出网。** `go get` 以及任何非标准库依赖都需要能访问
  `proxy.golang.org`（或你自己的 `GOPROXY`）—— 请在沙箱网络策略里放行，或者
  在构建镜像时就把依赖 vendor 进去。

## 排障

| 症状 | 原因 | 处理 |
|---|---|---|
| 模板任务在 `CREATING_TEMPLATE` 阶段报 `Receive event timeout after 10000ms` | shim 等新启动的 MicroVM 发出 `VsockServerReady` 的预算固定为 10 秒且不可配置。可写层越大，慢主机上启动越容易超出这个预算。 | 把 `--writable-layer-size` 降到 1 GB 后重试。嵌套虚拟化主机实测：1 GB 约 9.6 秒达成信号，2 GB 超时。裸金属余量充足。 |
| 构建下载了错误架构的 Go 发行包 | 没有 buildx 时，旧版构建器不会注入 `TARGETARCH`。Dockerfile 会回退到 `dpkg --print-architecture`，但只有基于 BuildKit 的构建才认 `--platform`。 | `apt-get install docker-buildx`。镜像构建末尾的 `go version` 会直接失败，不会产出坏镜像。 |
| `Sandbox.create()` 成功但执行命令超时 | SDK 通过 `<port>-<sandbox_id>.cube.app` 访问沙箱，而你的解析器没有对应的泛解析记录。 | 把 `CUBE_PROXY_NODE_IP` 设为 CubeProxy 所在节点（单机部署填 `127.0.0.1`）。 |
| 首次 `go build` 要跑好几分钟 | Go 1.20 起标准库不再预编译分发，首次构建需要现编译用到的包。 | 属正常现象。等 `GOCACHE` 预热后给沙箱打快照，后续沙箱从该快照创建。 |
| `fanout_build.py` 报 `hostPath ... is not within an allowed mount prefix` | CubeMaster 对 `hostPath` 有目录前缀白名单，默认只放行 `/data/shared/`。 | 把 `FANOUT_WORK_DIR` 保持在 `/data/shared/` 下，或在 CubeMaster 配置里扩展 `allowed_host_mount_prefixes`（见[持久化存储](../../docs/zh/guide/persistent-storage.md)）。 |
