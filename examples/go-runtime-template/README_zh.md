# Go 运行时模板

[English](README.md)

这是一个基于 `ghcr.io/tencentcloud/cubesandbox-base:2026.16` 的 Cube Go
运行时镜像，适合通用 Go 开发场景：编译小型服务、运行单元测试、执行 CLI
工具，以及在 Cube Sandbox 中保留有状态工作区。

它不是 Code Interpreter 或 Jupyter 模板，只暴露 `envd` 的 `49983` 端口，
可用于 `Sandbox.commands.run()` 和文件 API。

大多数 CubeSandbox 示例展示的是如何使用已有模板；本示例展示的是如何制作
可复用模板，所以 `Dockerfile` 本身就是示例的一部分：它是
`cubemastercli tpl create-from-image` 会转换成 Cube 模板的模板定义。通用
镜像契约可参考[自带镜像接入 (envd)](../../docs/zh/guide/tutorials/bring-your-own-image.md)。

## 包含内容

- 安装在 `/usr/local/go` 的 Go 工具链
- 位于 `/workspace/hello-go` 的示例工作区
- 从 `cubesandbox-base` 继承的 `envd`，监听 `:49983`
- 可选 SDK smoke 脚本 `smoke.py`，会在沙箱内执行 `go version`、
  `go test ./...` 和 `go run .`

## 构建镜像

```bash
docker build --platform linux/amd64 \
  -t cubesandbox-go-runtime:local \
  examples/go-runtime-template
```

构建参数：

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `CUBE_BASE_IMAGE` | `ghcr.io/tencentcloud/cubesandbox-base:2026.16` | 带有 `envd` 与 Cube 入口脚本的基础镜像 |
| `CUBE_PLATFORM` | `linux/amd64` | 拉取 `cubesandbox-base` 时使用的平台；当前基础镜像发布为 amd64 |
| `GO_VERSION` | `1.25.4` | 从 `go.dev/dl` 下载的 Go 工具链版本 |

覆盖示例：

```bash
docker build \
  --platform linux/amd64 \
  --build-arg GO_VERSION=1.25.4 \
  -t cubesandbox-go-runtime:local \
  examples/go-runtime-template
```

## 本地验证

```bash
docker run --rm -d \
  --platform linux/amd64 \
  -p 49983:49983 \
  --name cube-go-runtime \
  cubesandbox-go-runtime:local

curl -s -o /dev/null -w "envd /health => %{http_code}\n" \
  http://127.0.0.1:49983/health

docker exec cube-go-runtime sh -lc \
  'go version && cd /workspace/hello-go && go test ./... && go run .'

docker rm -f cube-go-runtime
```

期望输出包含：

```text
envd /health => 204
ok  	example.com/cubesandbox/hello-go
hello cube from Go inside CubeSandbox
```

## 注册为 Cube 模板

先将镜像推送到 Cube 集群可访问的镜像仓库，然后创建模板：

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/cubesandbox-go-runtime:latest \
  --writable-layer-size 2G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

等待模板任务进入 `template_status: READY`：

```bash
cubemastercli tpl watch --job-id <job-id>
```

## 可选 SDK Smoke

```bash
pip install -r requirements.txt

cp env.example .env
# 编辑 .env，填写 E2B_API_URL 和 CUBE_TEMPLATE_ID

python smoke.py
```

脚本会用 `CUBE_TEMPLATE_ID` 创建沙箱并执行：

```bash
go version
cd /workspace/hello-go && go test ./...
cd /workspace/hello-go && go run .
mkdir -p /workspace/runtime-smoke && printf '%s\n' go-runtime-ok > /workspace/runtime-smoke/marker.txt
cat /workspace/runtime-smoke/marker.txt
```

## 资源建议

- Smoke 测试：1 vCPU、1 GiB 内存、2 GiB 可写层
- 真实模块构建：建议 2+ vCPU、2-4 GiB 内存，并为模块缓存和构建产物预留更大的可写层
- 长时开发会话建议结合 quickstart 示例中的快照、暂停和恢复流程，避免每轮重复构建依赖

## 网络与安全说明

- 内置 sample 不依赖外部 Go 包，因此运行时 smoke 不需要出网下载依赖。
- 镜像构建阶段需要访问 `ghcr.io`、`archive.ubuntu.com`、
  `security.ubuntu.com` 等 Ubuntu APT 源，以及 `go.dev`。
- 真实项目如果需要 `go get` 或下载模块，请在 CubeEgress 中放行对应域名，
  常见包括 `proxy.golang.org`、`sum.golang.org` 和相关源码托管域名。
- 不要把 API key 或源码凭证写入镜像。请在创建沙箱时传入，或使用平台侧密钥机制。
- 该模板不需要特权容器、Docker socket 挂载或宿主机目录挂载。

## 已知限制

- 默认不暴露 Web 应用端口；如果 Go 程序需要监听 HTTP，请自行增加并暴露服务端口。
- 不包含 Jupyter 或 `e2b-code-interpreter` 运行时。
- 默认镜像定位是可复用起点，不是极致精简的生产 Go 容器镜像。

## 清理

```bash
docker rm -f cube-go-runtime 2>/dev/null || true
docker rmi cubesandbox-go-runtime:local

# 如果注册过模板：
cubemastercli tpl delete --template-id <template-id>
```
