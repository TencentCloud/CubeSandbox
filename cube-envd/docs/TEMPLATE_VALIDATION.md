# 模板验证

本文验证从 `cube-envd` 基础镜像到「模板 READY → 创建沙箱 → 执行命令 → 文件读写」的完整链路。

本机已具备在 WSL2 中运行**单机 all-in-one CubeSandbox** 的能力（`/dev/kvm` + systemd + Docker），
并已完成一次完整实测：用 Rust `cube-envd` 基础镜像构建模板、创建沙箱，并在沙箱内执行命令与读写文件全部通过，
详见 [单机 WSL2 端到端复现（验收 3）](#单机-wsl2-端到端复现验收-3)。
同时仓库仍保留**无集群的本地兜底验证**，便于在没有部署 CubeSandbox 时快速自测。

## 1. 构建并发布基础镜像

在仓库根目录、WSL2 中执行。默认实现为 Rust `cube-envd`，镜像内 envd 监听 `49983`。

```bash
make build-cube-base-image CUBE_BASE_PLATFORM=linux/amd64
docker tag cubesandbox-base:local ghcr.io/<org>/cubesandbox-base:<tag>
docker login ghcr.io
docker push ghcr.io/<org>/cubesandbox-base:<tag>
```

若检出的 Makefile 覆盖了镜像名，可用 `make -n build-cube-base-image` 查看实际名称，并在
`docker tag` 中沿用；该镜像必须能被执行 `BuildTemplate` 的节点访问到。

发布前建议先跑本地兜底冒烟：

```bash
make smoke-cube-base-image CUBE_BASE_PLATFORM=linux/amd64
```

冒烟需输出 `health=204` 以及非空的 `cube-envd` 版本/commit。除非有意回滚到上游实现，否则不要使用
`Dockerfile.cube-base-upstream`：

```bash
make smoke-cube-base-image CUBE_BASE_PLATFORM=linux/amd64 ENVD_IMPL=upstream-e2b
```

## 2. BuildTemplate

仓库内的完整 Go 验证器位于 `sdk/go/examples/template-validation/main.go`，
它通过 SDK 的 `BuildTemplate` 向 `POST /templates` 发送如下请求：

```json
{
  "image": "ghcr.io/<org>/cubesandbox-base:<tag>",
  "name": "cube-envd-verify",
  "instanceType": "cubebox",
  "writableLayerSize": "1G",
  "exposedPorts": [49983],
  "probePort": 49983,
  "probePath": "/health",
  "cpu": 1000,
  "memory": 1024
}
```

探测参数是刻意选择的：`ProbePort=49983` 与 `ProbePath=/health` 与 envd 监听端口及
CubeSandbox 模板就绪语义一致。

预期 `202 Accepted` 响应：

```json
{
  "jobID": "job-01J...",
  "templateID": "tpl-01J...",
  "status": "building",
  "phase": "pull",
  "progress": 0,
  "errorMessage": ""
}
```

等价的 curl 请求：

```bash
export CUBE_API_URL=http://<cube-api>:3000
export CUBE_API_KEY=<api-key>
export CUBE_ENVD_BASE_IMAGE=ghcr.io/<org>/cubesandbox-base:<tag>

curl -sS -X POST "$CUBE_API_URL/templates" \
  -H "Authorization: Bearer $CUBE_API_KEY" \
  -H 'Content-Type: application/json' \
  -d "{\"image\":\"$CUBE_ENVD_BASE_IMAGE\",\"name\":\"cube-envd-verify\",\"instanceType\":\"cubebox\",\"writableLayerSize\":\"1G\",\"exposedPorts\":[49983],\"probePort\":49983,\"probePath\":\"/health\",\"cpu\":1000,\"memory\":1024}"
```

保存响应中的 `jobID` 与 `templateID`。不同安装的认证方式可能不同，
请使用本地 CubeAPI 实际接受的请求头。

> 说明：部分部署要求显式提供 `writableLayerSize`（缺失会返回 HTTP 400 `writable_layer_size is required`）。
> 若部署未启用 CubeEgress，需将 `with_cube_ca` 置为 `false`（CLI 用 `--with-cube-ca=false`，
> SDK 可通过 `Extra: {"with_cube_ca": false}` 注入），否则会因缺少
> `/etc/cube/ca/cube-root-ca.crt` 而构建失败。

## 3. 轮询构建就绪

验证器每 5 秒轮询一次
`GET /templates/<templateID>/builds/<jobID>/status`，直到状态为
`success`、`ready`、`succeeded` 或 `completed`。成功响应示例：

```json
{
  "buildID": "job-01J...",
  "templateID": "tpl-01J...",
  "status": "success",
  "progress": 100,
  "message": "template is ready"
}
```

等价的 curl 轮询命令：

```bash
curl -sS "$CUBE_API_URL/templates/$TEMPLATE_ID/builds/$JOB_ID/status" \
  -H "Authorization: Bearer $CUBE_API_KEY"
```

当 `message` 或 `errorMessage` 非空时，停止并检查 CubeMaster/Cubelet 以及镜像拉取日志。
在状态成功之前，不要创建沙箱。

## 4. 创建沙箱

构建成功后，调用 `POST /sandboxes`（建议三分钟超时）：

```json
{
  "templateID": "tpl-01J...",
  "timeout": 180
}
```

预期 `201 Created`（或 `200 OK`）响应：

```json
{
  "templateID": "tpl-01J...",
  "sandboxID": "sb-01J...",
  "clientID": "client-01J...",
  "envdVersion": "cube-envd 0.1.0 ...",
  "envdAccessToken": "<token>",
  "trafficAccessToken": "<token>",
  "domain": "cube.app"
}
```

验证器要求 `sandboxID` 与 `envdVersion` 非空。请妥善保管返回的 token，
并在数据面请求中使用返回的 sandbox ID。

## 5. 命令与文件

验证器针对 `49983` 上的 envd 数据面执行以下 SDK 调用：

```go
command, _ := sandbox.Commands().Run(ctx,
    "echo -n cube-envd-template; whoami", cubesandbox.CommandOptions{})
_ = sandbox.Files().Write(ctx, "/tmp/cube-envd.txt",
    []byte("hello from cube-envd template"))
content, _ := sandbox.Files().Read(ctx, "/tmp/cube-envd.txt")
```

SDK 会将它们映射到虚拟主机 `49983-<sandboxID>.<domain>` 上的
`POST /process.Process/Start`、`POST /files`、`GET /files`。
命令走 Connect 流式协议，SDK 归一化后的结果：

```json
{
  "stdout": "cube-envd-templateroot\n",
  "stderr": "",
  "exitCode": 0
}
```

文件写入返回任意 2xx 即视为成功（通常无 body），读取返回原始文件字节：

```text
hello from cube-envd template
```

验证器会检查命令前缀、非空的 `whoami` 结果、退出码为零、文件内容完全一致，
最后调用 `DELETE /sandboxes/<sandboxID>` 完成清理。

在设置好控制面与数据面变量后，从 SDK 模块目录运行完整验证器：

```bash
cd sdk/go
export CUBE_API_URL=http://<cube-api>:3000
export CUBE_API_KEY=<api-key>
export CUBE_ENVD_BASE_IMAGE=ghcr.io/<org>/cubesandbox-base:<tag>
export CUBE_PROXY_NODE_IP=<cube-proxy-node>
export CUBE_PROXY_PORT_HTTP=80
export CUBE_PROXY_SCHEME=http
export CUBE_SANDBOX_DOMAIN=cube.app
go run ./examples/template-validation
```

预期最后一行：

```text
PASS: template ready, sandbox create, commands, and files
```

## 单机 WSL2 端到端复现（验收 3）

本节记录在本机 WSL2 中用**单机 all-in-one CubeSandbox**（无需集群）跑通上述完整链路的实际步骤与输出。

### 环境

- WSL2 Ubuntu 22.04，内核 `5.15.146.1-microsoft-standard-WSL2`
- `/dev/kvm` 可用（嵌套虚拟化），CPU `vmx`，16 vCPU，约 15 GiB 内存
- systemd 运行中，Docker 29.1.3
- CubeSandbox 版本：`v0.7.1`（CN 镜像版 one-click 包）

### WSL2 特有适配

安装器对宿主机有硬性要求，WSL2 需要补齐：

1. **XFS 前置**：`/data/cubelet` 必须位于 XFS。用 80G 稀疏镜像 + loop 挂载，
   并写入 `/etc/fstab` 持久化：

   ```bash
   truncate -s 80G /opt/cube-data.img
   mkfs.xfs -f /opt/cube-data.img
   mkdir -p /data && mount -o loop /opt/cube-data.img /data
   # /etc/fstab: /opt/cube-data.img /data xfs loop,defaults,nofail 0 0
   ```

2. **bpffs**：`/sys/fs/bpf` 需为 bpf 文件系统（可临时挂载，或放入 cubelet 的私有命名空间）。

3. **NUMA sysfs 缺失**：WSL2 内核没有 `/sys/devices/system/node`，会导致 cubelet 启动即 panic。
   由于 Cubelet 的 `initNumaInfo` 只读取 `/sys/devices/system/node/nodeN/cpulist`
   且要求节点数等于最大节点号 +1，可用 systemd drop-in 在其私有挂载命名空间内伪造：

   ```ini
   # /etc/systemd/system/cube-sandbox-cubelet.service.d/ns.conf
   [Service]
   ExecStart=
   ExecStart=/usr/bin/bash /usr/local/services/cubetoolbox/scripts/systemd/cubelet-ns-wrapper.sh
   ```

   其中 wrapper 执行 `unshare -m`，在其中挂载 `tmpfs` 到 `/sys/devices/system`、
   伪造 `node0/cpulist=0-15` 并回绑真实子目录，同时挂载 bpffs。

4. **镜像源**：腾讯云 CR 的 blob 存储（COS）在部分网络不可达，可
   - 标准镜像（mysql/redis/minio/coredns/openresty）改从 `dockerproxy.net` / `docker.m.daocloud.io` 拉取后按腾讯名 retag；
   - 自定义镜像（`cube-proxy`、`cube-lifecycle-manager`）用仓库内 Dockerfile 本地自建；
   - 可选服务（`webui`、`cube-egress`）无镜像时可 mask。

5. **节点健康**：Cubelet 通过 `POST /internal/v1/node-agent/nodes/register` 与
   `POST /internal/v1/node-agent/nodes/<id>/status` 向 CubeOps(3010) 心跳；
   CubeOps 将指标写入 Redis `cube:v1:master:node:metric:<nodeIP>`，
   CubeMaster 据此判定节点健康。WSL2 偶发重启后需重新拉起控制面并等待心跳：

   ```bash
   sudo systemctl start cube-sandbox-control.target
   ```

### 复现命令

```bash
# 1) 安装（CN 镜像源，全程 root）
curl -sL https://cnb.cool/CubeSandbox/CubeSandbox/-/git/raw/master/deploy/one-click/online-install.sh \
  | MIRROR=cn bash -s -- -y

# 2) 准备本地 registry 并推送我们的 cube-envd 基础镜像
docker run -d --name cube-dev-registry -p 127.0.0.1:5000:5000 registry:2
docker tag cubesandbox-base:acc-amd64 127.0.0.1:5000/cubesandbox-base:acc-amd64
docker push 127.0.0.1:5000/cubesandbox-base:acc-amd64

# 3) 确认节点健康（NODES_SCANNED 应为 1/1）
cubemastercli cubebox list

# 4) CLI 方式创建模板（可选，用于快速确认 READY）
cubemastercli tpl create-from-image \
  --image http://127.0.0.1:5000/cubesandbox-base:acc-amd64 \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --probe 49983 --probe-path /health \
  --with-cube-ca=false
cubemastercli tpl watch --job-id <job_id>

# 5) SDK 全流程验证（在 golang 容器中，使用 host 网络）
docker run --rm --network host -v <sdk-go-src>:/app -w /app \
  -e CUBE_API_URL=http://127.0.0.1:3000 \
  -e CUBE_API_KEY=e2b_000000 \
  -e CUBE_ENVD_BASE_IMAGE=http://127.0.0.1:5000/cubesandbox-base:acc-amd64 \
  -e CUBE_PROXY_NODE_IP=<node-ip> \
  -e CUBE_PROXY_PORT_HTTP=80 -e CUBE_PROXY_SCHEME=http \
  -e CUBE_SANDBOX_DOMAIN=cube.app \
  golang:1.25-bookworm bash -lc 'go run ./examples/template-validation'
```

> SDK 侧 `BuildTemplate` 需带 `writableLayerSize`，并通过 `Extra` 传 `{"with_cube_ca": false}`。

### 实测输出

CLI 创建模板（`cubemastercli tpl watch`）：

```text
job_id:                   <job_id>
template_id:              tpl-96a21c443be44a988f69620c
attempt_no:               1
artifact_id:              rfs-2174d0f93be17f7112e48ad5-b0b16bb6
status:                   READY
phase:                    READY
progress:                 100%
pull:                     33.7MiB/33.7MiB
pull_layers:              6/6
distribution:             1/1 ready, 0 failed
artifact_status:          READY
artifact_sha256:          e1244d28793f4d6c91d1b369683c279944e727b9d3121cedcaf86d565c0cf4cd
template_status:          READY
```

SDK 全流程验证器 `go run ./examples/template-validation`：

```text
BuildTemplate: templateID=tpl-f4cae920aad44accbcf83b11 jobID=092c3a30-8f1a-48e3-942b-5ab4e2357c91
build status=building progress=0 msg=PULLING
build status=building progress=85 msg=CREATING_TEMPLATE
build status=ready progress=100 msg=READY
Create: sandboxID=8b519dd188f340c5b0cbd95ebfbd0708 envdVersion=0.1.0
Commands: {"Stdout":"cube-envd-templateroot\n","Stderr":"","ExitCode":0,"Termination":{"reason":"exited"}}
Files: "hello from cube-envd template"
PASS: template ready, sandbox create, commands, and files
```

补充证据：

- 模板列表（`cubemastercli tpl list`）中 `cube-envd-verify` 状态为 `READY`，
  `IMAGE_INFO` 指向 `http://127.0.0.1:5000/cubesandbox-base:acc-amd64@sha256:f65acc5b...`；
- 沙箱内 `envdVersion=0.1.0`，即本仓库的 Rust `cube-envd`。
  （注：此后 `-version` 已改为上报所模拟的上游世代 `0.5.13`；上面保留的是改动前的实测原始输出。）
- 验证结束后沙箱被清理，`SANDBOX_COUNT 0`，Api `/health=200`，`NODES_SCANNED 1/1`。

### 注意事项

- 该环境依赖上述 WSL2 适配，属于「可复现」而非「开箱即用」；真实交付仍建议具备 KVM 的 Linux 主机。
- WSL2 可能自行重启，重启后 `/data`（fstab）会自动挂载，但需重新
  `systemctl start cube-sandbox-control.target` 并等待节点心跳恢复（约 1～2 分钟）。
- 本轮实测未启用 CubeEgress，故模板构建需 `with_cube_ca=false`。

## 无集群本地兜底验证

在没有 CubeSandbox 集群时，可用仓库内置的构建与冒烟命令本地验证：

- `make smoke-cube-base-image CUBE_BASE_PLATFORM=linux/amd64`：构建并运行镜像，
  envd `/health` 返回 `204`，版本/commit 非空。
- `cargo test --release`：覆盖 Connect 分帧、进程、PTY、文件系统、WatchDir、
  请求 ID 与状态行为。
- `sdk/go` 的 `envd_local_test.go`（通过 `CUBE_ENVD_LOCAL_ADDR` 开关）：
  用真实 Go SDK 直连本地 cube-envd 容器，验证命令 stdout/stderr/exitCode 与文件读写。

生成的日志与基准报告不纳入提交。

## 排障

### 探测返回 404 或超时

- 确认模板使用 `probePort: 49983` 与 `probePath: "/health"`。
- 运行 `docker run --rm -p 49983:49983 <image>` 并检查
  `curl -i http://127.0.0.1:49983/health`，预期状态为 `204`。
- 确认 `/usr/bin/envd` 正在监听，且容器入口保持 PID 1。入口必须在任何用户命令之前启动 envd。
- 确认集群能把探测流量路由到沙箱的 `49983` 端口。

### 入口未启动 envd

- 检查容器日志，并确认 `docker/cube-entrypoint.sh` 为 Unix LF 行尾且具备可执行权限。
- 检查 `ENVD_PORT`（默认 `49983`），并在镜像内运行 `/usr/bin/envd -version`。
- 避免用绕过 `/usr/bin/tini` 与 `cube-entrypoint.sh` 的应用命令替换入口。

### 镜像拉取失败

- 确认镜像 tag 存在且集群节点可访问该仓库。
- 私有仓库需配置模板的 registry 凭据。
- 检查镜像架构与节点匹配（本地 WSL2 验证为 `linux/amd64`），并查看 Cubelet 的镜像拉取事件。

### 命令或文件返回 404

- 使用端口 `49983` 的数据面虚拟主机，而不是 Jupyter 端口 `49999`，也不是控制面 API 主机。
- 保留 `Create` 返回的 `envdAccessToken` 与 `trafficAccessToken`；
  Go 验证器已通过 SDK 完成配置。
