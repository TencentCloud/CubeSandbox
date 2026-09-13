# cubesandbox-base-nginx 示例

[English](README.md)

这是一个基于 [`cubesandbox-base`](../../docker/Dockerfile.cube-base) 安装 nginx 的最小镜像示例。无需准备实际应用，即可端到端验证“自定义模板镜像”流程。

- `envd` 继承自基础镜像，监听 `:49983`，用作 Cube 就绪探针。
- nginx 监听 `:80`，并提供一个简单的静态页面。

完整说明请参阅[自定义模板镜像](../../docs/zh/guide/tutorials/bring-your-own-image.md)。

## 构建镜像

```bash
docker build -t cubesandbox-demo-nginx:latest .
```

## 本地运行与验证

```bash
docker run --rm -d \
    -p 8080:80 \
    -p 49983:49983 \
    --name cube-demo-nginx \
    cubesandbox-demo-nginx:latest

# nginx：应输出示例首页的 HTML
curl -s http://127.0.0.1:8080/

# envd 就绪探针：应返回 204
curl -s -o /dev/null -w "envd /health => %{http_code}\n" \
    http://127.0.0.1:49983/health

docker rm -f cube-demo-nginx
```

## 注册为 Cube 模板

将镜像推送到 Cube 集群可以访问的镜像仓库，然后执行：

```bash
cubemastercli tpl create-from-image \
    --image       <your-registry>/cubesandbox-demo-nginx:latest \
    --writable-layer-size 1G \
    --expose-port 49983 \
    --expose-port 80 \
    --probe       49983 \
    --probe-path  /health
```

`--probe 49983 --probe-path /health` 将 Cube 的模板探针指向 `envd`。`envd` 就绪时返回 `204`；nginx 的 `:80` 端口保持暴露，用于接收实际业务流量。

## 使用 E2B SDK 验证

注册模板后，[`test_files.py`](./test_files.py) 会基于该模板启动一个沙箱，并完成两项检查：

1. 通过 `sandbox.files.read(...)` 读取 `/etc/nginx/nginx.conf`。
2. 向沙箱的 `80` 端口发送 HTTPS 请求并输出 nginx 响应。

```bash
pip install -r requirements.txt

cp env.example .env
# 填写 E2B_API_URL 和 CUBE_TEMPLATE_ID

python3 test_files.py
```

## 显式选择 Rust

Go 仍为默认。从仓库根目录构建可选 Rust base，并复用示例参数：

```bash
make cube-base-rust CUBE_BASE_RUST_IMAGE=cubesandbox-base:rust-local
docker build --build-arg CUBE_BASE_IMAGE=cubesandbox-base:rust-local \
  -t cubesandbox-demo-nginx:rust-local examples/cubesandbox-base-nginx
```

Rust 使用可写的 cgroup v2 设置额外的进程资源分组。初始化失败时（包括普通 Docker
中的只读 cgroup），会记录原因并继续运行，不应用这些额外分组，与 Go 一致。
已有 VM/平台限制是独立的一层。使用 [docker/README.md](../../docker/README.md) 中的
隔离镜像测试覆盖两条路径，不要开放宿主 cgroup 或时钟权限。入口在 tini 下监督 envd 和
nginx，nginx 并非 PID 1。模板探针应为 **49983/health**，断言 HTTP 204；80 端口只能
证明 nginx 可用。启动耗时取决于平台和镜像。

新建模板、daemon 身份、CubeSandbox SDK 命令/文件断言及可选 Go/Rust 性能对比，见
[现有 SDK E2E 入口](../../tests/e2e/sdk_compat/README_zh.md#选定-envd-验收)。本地标签只有
在模板构建器能访问同一 Docker 镜像存储时才可使用，并不会自动出现在远端节点。
