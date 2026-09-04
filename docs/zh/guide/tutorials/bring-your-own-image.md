# 自定义模板镜像

本教程介绍如何为**你自己的应用或容器镜像**加入 `envd`，以便通过 CubeSandbox SDK 和 E2B SDK 操作沙箱。

从 OCI 镜像创建模板以及配置应用端口和 readiness probe 的通用流程，请参阅[从 OCI 镜像制作模板](./template-from-image.md)。

---

## 1. 我的镜像什么时候需要 `envd`？

`envd` 是 CubeSandbox SDK 和 E2B SDK 执行命令、读写文件和建立 PTY 等沙箱操作所使用的数据面服务：

| 能力 | 沙箱内的 `envd` 接口 | 没有 `envd` 会怎样 |
| --- | --- | --- |
| `envd` 健康检查（可作为模板 probe） | `GET :49983/health` → 204 | 该探活端点不可用 |
| `Sandbox.commands.run()` | `:49983` 上的 Process API | 命令 API 不可用 |
| `Sandbox.files.read/write()` | `:49983` 上的 Files API | 文件 API 不可用 |
| 创建时环境变量初始化 | `POST :49983/init` | 传入创建时环境变量时，沙箱创建失败 |

对于交互式开发或代码执行沙箱，建议保留 `envd`，便于通过 SDK 执行命令、读写文件和排障。仅提供自有业务服务且不使用上述能力的镜像可以不包含 `envd`，此时应将模板 probe 配置为应用自己的 HTTP 健康检查端点。

## 2. 快速开始：基于 `cubesandbox-base`

`cubesandbox-base` 是一个普通的 `ubuntu:22.04`，在 `/usr/bin/envd` 预装
了 `envd`，并附带一个通用入口脚本——后台拉起 `envd`、前台 `exec` 你
提供的 `CMD`。你只需要三步：**写 Dockerfile → 构建推送 → 创建模板**。

> 想看一个能直接跑通的完整示例？可以参考仓库里的
> [`examples/cubesandbox-base-nginx`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/cubesandbox-base-nginx)，
> 里面是把 nginx 叠在 `cubesandbox-base` 上的最小 demo。

### 2.1 写 Dockerfile

```dockerfile
FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16

# 安装你自己需要的工具链
RUN apt-get update \
    && apt-get install -y --no-install-recommends python3 python3-pip \
    && rm -rf /var/lib/apt/lists/*

RUN pip install --no-cache-dir pandas matplotlib numpy

# 如果你的应用需要作为前台进程运行，在这里设置 CMD 即可。
# envd 仍会作为后台进程持续运行。
# CMD ["python3", "/srv/app.py"]
```

### 2.2 构建并推送

```bash
docker build -t my-registry.example.com/my-team/my-sandbox:v1 .
docker push   my-registry.example.com/my-team/my-sandbox:v1
```

镜像仓库需要能被 Cube 集群拉到。

::: tip 明文 HTTP 仓库
镜像引用须加 `http://` 前缀，例如 `http://my-registry.example.com/my-team/my-sandbox:v1`。
:::

### 2.3 创建 Cube 模板

暴露 `49983`（envd），外加你自己应用监听的端口：

```bash
cubemastercli tpl create-from-image \
  --image       my-registry.example.com/my-team/my-sandbox:v1 \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --expose-port <your-custom-port> \
  --probe       49983 \
  --probe-path  /health
```

拿到 `template_id` 后，可以通过 CubeSandbox SDK 或 E2B SDK 创建沙箱，示例见[从 OCI 镜像制作模板](./template-from-image.md)。

相关实战内容见[本地与远程镜像实战](./template-build-practice.md)。

## 3. 往现有镜像里注入 `envd`

如果现有镜像不包含 `envd`，可以在构建自定义镜像时从 `cubesandbox-base` 复制，也可以在执行 `create-from-image` 时由 `cubemastercli` 注入。

### 在 Dockerfile 中复制

如果你想使用你自定义的镜像，可以用 `COPY --from=` 从 `cubesandbox-base`
镜像中**拷贝** `envd` 和入口脚本：

```dockerfile
FROM e2bdev/code-interpreter:latest

USER root

# 从 cubesandbox-base 拉取 envd 与通用入口脚本
COPY --from=ghcr.io/tencentcloud/cubesandbox-base:2026.16 \
     /usr/bin/envd /usr/bin/envd
COPY --from=ghcr.io/tencentcloud/cubesandbox-base:2026.16 \
     /usr/local/bin/cube-entrypoint.sh /usr/local/bin/cube-entrypoint.sh

# 上游镜像通常已有自己的 entrypoint/CMD。推荐用 cube-entrypoint.sh 包裹它；
# 或者自己写 entrypoint 并手动拉起 envd —— 见第 4 节。
ENTRYPOINT ["/usr/local/bin/cube-entrypoint.sh"]
CMD ["/bin/sh", "-c", "sudo --preserve-env=E2B_LOCAL /root/.jupyter/start-up.sh"]
```

另一个例子，从轻量的 Python 镜像出发：

```dockerfile
FROM python:3.11-slim

COPY --from=ghcr.io/tencentcloud/cubesandbox-base:2026.16 \
     /usr/bin/envd /usr/bin/envd
COPY --from=ghcr.io/tencentcloud/cubesandbox-base:2026.16 \
     /usr/local/bin/cube-entrypoint.sh /usr/local/bin/cube-entrypoint.sh

RUN pip install --no-cache-dir fastapi uvicorn

COPY app.py /srv/app.py

EXPOSE 49983 8000
ENTRYPOINT ["/usr/local/bin/cube-entrypoint.sh"]
CMD ["uvicorn", "app:app", "--app-dir", "/srv", "--host", "0.0.0.0", "--port", "8000"]
```

构建、推送、创建模板的流程和第 2.2 / 2.3 节一致。

### 在模板构建阶段注入

如果不希望修改 Dockerfile，可以在创建模板时通过 `--enable-inject-envd` 上传并注入 `envd`：

```bash
cubemastercli tpl create-from-image \
  --image <your-image> \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health \
  --enable-inject-envd
```

| 参数 | 说明 |
| --- | --- |
| `--enable-inject-envd` | 从 `cubemastercli` 上传一个 `envd` 二进制并写入模板 rootfs。 |
| `--envd-path` | 运行 `cubemastercli` 的机器上的本地路径；仅在设置 `--enable-inject-envd` 时生效。若省略，CLI 会在可用时使用构建期内嵌的默认 `envd`。 |

`--envd-path` 是运行 CLI 的机器上的路径，不是 CubeMaster 宿主机路径。CLI 会通过 `create-from-image` 的 multipart 请求上传二进制；CubeMaster 校验上传内容后，将其写入模板 rootfs 的 `/usr/local/bin/envd`，并把二进制的 SHA-256 纳入 rootfs artifact 指纹，避免复用由不同 `envd` 构建的 artifact。

上传的文件必须是非空 ELF 二进制，大小不能超过 16 MiB，并与目标 rootfs 的操作系统和 CPU 架构兼容。例如，Linux x86_64 镜像需要 Linux x86_64 版本的 `envd`。

如果 `cubemastercli` 构建时没有内嵌默认 `envd`，则必须同时指定 `--envd-path`。如需构建带默认 `envd` 的 CLI，请先准备二进制并执行：

```bash
make cubemastercli ENVD_LOCAL_PATH=/path/to/envd
```

对于 `cubebox` 类型，CubeMaster 还会保留注入标记，在创建沙箱时自动包装主容器的启动命令：先在后台运行 `/usr/local/bin/envd`，再执行镜像原有命令，并补充暴露 `49983` 端口。因此这种方式无需修改原镜像的入口程序。非 `cubebox` 类型不会应用该启动包装。

## 4. 入口脚本契约

`cube-entrypoint.sh` 实现了一个非常简单的 "envd 后台 + 用户应用前台" 的
组合模式：

1. 启动时一律后台拉起 `envd -port "${ENVD_PORT:-49983}"`，使 `/health`
   在容器启动约 1 秒内就能响应。
2. 如果启动时**带了** `CMD`，脚本会 `exec` 执行它：`envd` 在后台伴跑，
   用户进程占用 `stdout`/`stderr`，并接收 `SIGTERM`。
3. 如果**没有** `CMD`，脚本会 `wait` 住 `envd`，让它成为前台主进程。

可用的环境变量：

| 变量               | 默认值              | 说明                                                   |
| ------------------ | ------------------- | ------------------------------------------------------ |
| `ENVD_PORT`        | `49983`             | envd 监听的端口                                        |
| `ENVD_EXTRA_ARGS`  | *(空)*              | 追加到 `-port` 之后的额外参数。若未包含 `-isnotfc`，脚本会自动追加以跳过 Firecracker MMDS 查询。 |
| `ENVD_LOG_FILE`    | `/var/log/envd.log` | envd stdout/stderr 落盘位置；设为 `-` 则继承容器 stdio |
| `ENVD_BIN`         | `/usr/bin/envd`     | 当 envd 安装在别处时覆盖                               |

### 自己手动拉起 envd

如果你已经有一个复杂的 entrypoint 不方便交给 `cube-entrypoint.sh`，
只需要在交出控制权前加一行：

```bash
#!/bin/bash
# your-entrypoint.sh

# 后台启动 envd
# -isnotfc 是必须的：它让 envd 跳过对 169.254.169.254 的 Firecracker MMDS
# 查询。CubeSandbox 不使用 Firecracker，MMDS 服务不存在。缺少此参数时
# envd 会尝试访问不存在的 MMDS，可能引发网络超时、/init 延迟、
# env_vars 注入失败等各种问题。
/usr/bin/envd -port 49983 -isnotfc >/var/log/envd.log 2>&1 &

# ... 你原本的启动流程 ...
exec "$@"
```

## 5. 本地验证镜像（可选）

创建模板前，可以跑一遍 CI 用于验证 base 镜像的同款 smoke test：

```bash
IMG=my-registry.example.com/my-team/my-sandbox:v1
cid=$(docker run -d --rm "$IMG")

docker exec "$cid" curl -s -o /dev/null -w "envd /health => %{http_code}\n" \
    http://127.0.0.1:49983/health
# => envd /health => 204

docker exec "$cid" /usr/bin/envd -version
# => 2026.16

docker rm -f "$cid"
```

如果几秒之内 `/health` 没有返回 `204`，检查容器里 envd 的日志：

```bash
docker exec "$cid" cat /var/log/envd.log
```

## 6. 排错速查

| 现象                                   | 可能原因                                                   | 解决                                                                                    |
| -------------------------------------- | ---------------------------------------------------------- | --------------------------------------------------------------------------------------- |
| 模板创建探活失败                       | envd 未启动 / 起在错误端口                                  | 确认 `ENTRYPOINT` 为 `cube-entrypoint.sh`，或你自己的脚本里有 `envd -port 49983 &`      |
| `curl :49983/health` 返回 `000`        | 端口无人监听；入口被用户 CMD 整个替换                      | 检查 <code v-pre>docker inspect --format '{{json .Config.Entrypoint}}'</code>，保留 `cube-entrypoint.sh` |
| envd 立刻退出                          | 二进制版本与容器预期不匹配                                 | `docker exec ... /usr/bin/envd -version` 确认版本；从 pin 的 base tag 重新拷贝          |
| envd `/init` 异常缓慢 / `create_time env_vars` 失败 | 缺少 `-isnotfc` 参数；envd 尝试访问不存在的 MMDS (`169.254.169.254`) | 使用 `cube-entrypoint.sh`（会自动追加 `-isnotfc`），或在手动拉起 envd 时自行加上 `-isnotfc` |
| 49983 端口冲突                         | 你自己的应用也在监听 49983                                 | 把自家应用迁到别的端口，并一起 `--expose-port` 暴露                                     |
| `sudo: command not found`              | 基于 `-slim` / `-alpine` 这种无 sudo 的镜像构建            | `apt-get install -y sudo`，或直接把 `sudo` 从 CMD 里去掉——`cube-entrypoint.sh` 不依赖它 |
| 模板创建长时间卡在 `PULLING`           | registry 从 Cube 节点不可达                                | 推送到集群可访问的 registry，或用 `--registry-username` / `--registry-password`         |

## 7. 进阶 —— 自己重建基础镜像

基础镜像由仓库内单个 GitHub Actions workflow 自动构建：
[`.github/workflows/build-envd-base-image.yml`](https://github.com/TencentCloud/CubeSandbox/blob/master/.github/workflows/build-envd-base-image.yml)。
它会 checkout `e2b-dev/infra` 的指定 tag（默认 `2026.16`），在原生
`linux/amd64` 与 `linux/arm64` runner 上用 Go 1.25.4 编译 envd，构建
`docker/Dockerfile.cube-base`，分别对 `:49983/health` 做 smoke test，再
合成 multi-arch manifest list 推送到
`ghcr.io/tencentcloud/cubesandbox-base`。
