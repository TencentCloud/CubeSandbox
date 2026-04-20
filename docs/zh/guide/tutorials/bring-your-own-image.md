# 自带镜像接入 (envd)

本教程介绍如何用**最少的改动**把你自己的应用或容器镜像接入 Cube-Sandbox
模板体系。

想全面了解 OCI 镜像如何变成模板，请继续阅读
[从 OCI 镜像制作模板](./template-from-image.md)。本教程是那篇文档的
**前置步骤**：保证你的镜像满足它所要求的探活 (readiness probe) 条件。

---

## 1. 为什么我的镜像需要 `envd`？

Cube-Sandbox 与沙箱容器之间的所有通信都是通过容器内的 `envd` 守护进程
完成的。它是 Cube Master、Cube SDK 以及 `cubemastercli` 唯一识别的协议
端点：

| Cube 能力                         | 沙箱内端点                    | 没有 `envd` 会怎样             |
| --------------------------------- | ----------------------------- | ------------------------------ |
| 模板探活                          | `GET :49983/health` → 204     | 模板创建因探活失败而 FAILED    |
| `Sandbox.commands.run()`          | `POST :49983/process`         | SDK 命令调用全部 404           |
| `Sandbox.files.read/write()`      | `POST :49983/files`           | 文件操作不可用                 |
| Sandbox 初始化（env、时间同步等） | `POST :49983/init`            | Sandbox 永远无法 Ready         |

换句话说：**任何要用作 Cube 模板的镜像，启动时都必须有 `envd` 在
`:49983` 上监听**。最简单的方式就是直接基于官方 `cubesandbox-base`
构建——下一节就是完整流程。

---

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

拿到 `template_id` 就可以用 Cube SDK / `cubemastercli` 去创建沙箱了，
完整 SDK 用法见
[从 OCI 镜像制作模板](./template-from-image.md)。

### 可用的基础镜像 tag

| Tag                       | 基础系统       | envd 版本    |
| ------------------------- | ------------- | ------------ |
| `2026.16` / `latest`      | `ubuntu:22.04` | `2026.16`    |
| `2026.16-ubuntu22.04`     | `ubuntu:22.04` | `2026.16`    |

为了保证可复现构建，**推荐 pin 到精确的 envd 版本 (`2026.16`)**。

---

## 3. 备选：往现有镜像里注入 `envd`

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

---

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
| `ENVD_EXTRA_ARGS`  | *(空)*              | 追加到 `-port` 之后的额外参数                          |
| `ENVD_LOG_FILE`    | `/var/log/envd.log` | envd stdout/stderr 落盘位置；设为 `-` 则继承容器 stdio |
| `ENVD_BIN`         | `/usr/bin/envd`     | 当 envd 安装在别处时覆盖                               |

### 自己手动拉起 envd

如果你已经有一个复杂的 entrypoint 不方便交给 `cube-entrypoint.sh`，
只需要在交出控制权前加一行：

```bash
#!/bin/bash
# your-entrypoint.sh

# 后台启动 envd
/usr/bin/envd -port 49983 >/var/log/envd.log 2>&1 &

# ... 你原本的启动流程 ...
exec "$@"
```

---

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

---

## 6. 排错速查

| 现象                                   | 可能原因                                                   | 解决                                                                                    |
| -------------------------------------- | ---------------------------------------------------------- | --------------------------------------------------------------------------------------- |
| 模板创建探活失败                       | envd 未启动 / 起在错误端口                                  | 确认 `ENTRYPOINT` 为 `cube-entrypoint.sh`，或你自己的脚本里有 `envd -port 49983 &`      |
| `curl :49983/health` 返回 `000`        | 端口无人监听；入口被用户 CMD 整个替换                      | 检查 <code v-pre>docker inspect --format '{{json .Config.Entrypoint}}'</code>，保留 `cube-entrypoint.sh` |
| envd 立刻退出                          | 二进制版本与容器预期不匹配                                 | `docker exec ... /usr/bin/envd -version` 确认版本；从 pin 的 base tag 重新拷贝          |
| 49983 端口冲突                         | 你自己的应用也在监听 49983                                 | 把自家应用迁到别的端口，并一起 `--expose-port` 暴露                                     |
| `sudo: command not found`              | 基于 `-slim` / `-alpine` 这种无 sudo 的镜像构建            | `apt-get install -y sudo`，或直接把 `sudo` 从 CMD 里去掉——`cube-entrypoint.sh` 不依赖它 |
| 模板创建长时间卡在 `PULLING`           | registry 从 Cube 节点不可达                                | 推送到集群可访问的 registry，或用 `--registry-username` / `--registry-password`         |

---

## 7. 进阶 —— 自己重建基础镜像

基础镜像由仓库内单个 GitHub Actions workflow 自动构建：
[`.github/workflows/build-envd-base-image.yml`](https://github.com/TencentCloud/CubeSandbox/blob/master/.github/workflows/build-envd-base-image.yml)。
它会 checkout `e2b-dev/infra` 的指定 tag（默认 `2026.16`），用 Go 1.25.4
在同一个 job 里直接把 envd 编译进来，构建 `docker/Dockerfile.cube-base`，
对 `:49983/health` 做 smoke test，然后推送到
`ghcr.io/tencentcloud/cubesandbox-base`。
