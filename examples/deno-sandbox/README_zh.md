# Deno 2 沙箱模板

[English](README.md)

这是一个可复现的 CubeSandbox Deno 2 + TypeScript 运行时模板。示例会构建
`amd64` 镜像，通过 Python SDK 启动带文件持久化的 HTTP 服务，并验证应用状态和
Deno 依赖缓存都能跨越沙箱 pause/resume 周期保持不变。两个 SDK 脚本默认禁止
沙箱访问公网，并要求通过 CubeProxy 的每沙箱访问令牌访问服务。

## 包含内容

- 固定使用 Deno `2.8.1`，从官方 Release 资源下载
- 构建 `linux/amd64` 镜像，并在安装 Deno 前校验官方 SHA-256
- JSR 依赖精确锁定版本，并提交 `deno.lock`
- 构建镜像时预热依赖缓存，运行阶段无需临时下载包
- 以非 root 的 `user` 运行，只授予 Deno 必要的文件和网络权限
- 端口 `8000` 上提供 `/health` 和文件持久化的 `/counter` 接口
- 覆盖格式、lint、类型、持久化、并发写入和 HTTP 行为的测试
- 提供普通运行和 pause/resume 恢复验证两套 Python SDK 脚本

Deno 服务由 SDK 示例按需启动。镜像继承的 `envd` 仍在 `49983` 端口承担 Cube
模板就绪探针，因此模板启动不依赖演示应用预先运行。

## 适用场景

- 为 TypeScript/JavaScript 代码提供隔离、固定版本的 Deno 2 运行环境
- 依赖在镜像构建阶段预热、运行时不允许访问公网的代码执行任务
- 需要在长时间空闲时暂停，并在恢复后继续使用内存和文件状态的 Web 任务
- 需要一个可由 Agent 或工作流系统复用、但不绑定特定 Agent 框架的运行底座

## 架构

```text
Python 示例
  |-- CubeSandbox SDK --> Cube API --> Deno MicroVM
  |                                    |-- envd :49983
  |                                    |-- Deno 服务 :8000
  |                                    |     |-- GET  /health
  |                                    |     |-- GET  /counter
  |                                    |     `-- POST /counter
  `-- 通过 CubeProxy 访问 HTTPS ------------------^

/workspace/deno-app/data/counter.json   应用状态
/home/user/.cache/deno                  冻结的依赖缓存
```

## 前置条件

- 已正常运行的 CubeSandbox 部署和 `cubemastercli`
- Docker BuildKit（或兼容 OCI 的镜像构建器）
- CubeSandbox 节点能够拉取的镜像仓库
- 宿主机安装 Python 3.10 或更高版本

如果控制面和 KVM/PVM 节点尚未部署，请先完成
[CubeSandbox 快速开始](../../docs/zh/guide/quickstart.md)。

## 1. 构建并验证镜像

在仓库根目录运行：

```bash
docker build \
  --tag deno-sandbox:2.8.1 \
  examples/deno-sandbox

docker run --rm \
  --user 1000:1000 \
  --entrypoint deno \
  deno-sandbox:2.8.1 \
  task verify
```

`deno task verify` 会依次执行格式检查、lint、类型检查和 7 个专项测试。
Dockerfile 在构建时也会运行相同命令，任一检查失败都会终止构建。

可在本机启动服务进行验证：

```bash
docker run --rm --detach \
  --name cube-deno-local \
  --user 1000:1000 \
  --publish 8000:8000 \
  --entrypoint deno \
  deno-sandbox:2.8.1 \
  task start

curl --fail http://127.0.0.1:8000/health
curl --fail --request POST http://127.0.0.1:8000/counter
curl --fail http://127.0.0.1:8000/counter

docker stop cube-deno-local
```

按 Cube 官方基础镜像当前发布的平台构建并推送镜像：

```bash
docker buildx build \
  --platform linux/amd64 \
  --tag <your-registry>/deno-sandbox:2.8.1 \
  --push \
  examples/deno-sandbox
```

当前固定的 `cubesandbox-base:2026.16` 仅发布 `linux/amd64`。当
`CUBE_BASE_IMAGE` 被覆盖为兼容 arm64 的 Cube 基础镜像时，Dockerfile 也会选择
Deno 官方 `aarch64` 资源；但在默认基础镜像提供 arm64 manifest 之前，不能用默认
标签构建 arm64 镜像。

## 2. 注册 Cube 模板

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/deno-sandbox:2.8.1 \
  --alias deno-2-sandbox \
  --writable-layer-size 1G \
  --cpu 2000 \
  --memory 2000 \
  --expose-port 8000 \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

请保存命令返回的模板 ID。`49983` 是镜像继承的 `envd` 就绪端点，`8000`
是由 CubeProxy 转发的 Deno 应用端口。

### 资源建议

下列配置是本示例验证时使用的基准值：

| 资源 | 建议值 | 说明 |
|---|---:|---|
| CPU | `2000` 毫核（2 核） | 足够运行验证任务及轻量 HTTP 服务；并发编译或执行时应提高 |
| 内存 | `2000` MB | 覆盖 Deno、envd 和 MicroVM 基础开销；大型依赖图应提高 |
| 可写层 | `1G` | 适合示例代码、依赖缓存和少量状态；长期或大量文件任务应扩大 |
| 暴露端口 | `8000`、`49983` | 分别用于演示应用和平台就绪探针 |

`--alias deno-2-sandbox` 提供稳定、符合 kebab-case 约定的模板名称；脚本仍推荐
使用命令返回的模板 ID，避免不同环境中的别名冲突。

## 3. 配置 Python 示例

```bash
cd examples/deno-sandbox
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
cp .env.example .env
```

编辑 `.env`：

```dotenv
E2B_API_URL=http://<cube-api-host>:3000
E2B_API_KEY=e2b_000000
CUBE_TEMPLATE_ID=<template-id>
```

Cube 本地默认 key 只需非空；生产部署请使用该环境要求的凭据和 TLS 配置。如果
CubeProxy 使用私有 CA，请设置 `REQUESTS_CA_BUNDLE` 或 `SSL_CERT_FILE`。示例
不会通过关闭证书校验来绕过 TLS 错误。

## 4. 运行冒烟测试

```bash
python run_example.py
```

脚本会创建沙箱、检查固定版本的 Deno、在 MicroVM 内执行完整验证任务、启动
服务、证明公网 TCP 连接被阻断、证明无令牌请求被 CubeProxy 拒绝、连续写入两次
计数器，并证明后续读取拿到持久化后的值。公共辅助函数会为正常请求自动附加令牌。
成功运行的结尾类似：

```text
Default-deny egress PASS: public egress blocked
Service ready (pid=...): {'status': 'ok', 'runtime': 'deno', ...}
Restricted public access PASS: HTTP 403 without token
Counter persistence PASS: {'counter': 2}
Restricted counter URL (traffic token required): https://<cubeproxy-host>/counter
```

可用 `--template` 覆盖 `CUBE_TEMPLATE_ID`，远程环境启动较慢时可调整
`--poll-timeout`。

## 5. 验证 pause/resume 恢复

```bash
python resume_example.py
```

第二个脚本会：

1. 创建沙箱并写入一次计数器；
2. 确认 `/home/user/.cache/deno` 至少包含一个缓存文件，并计算整体哈希；
3. 暂停沙箱，再通过 sandbox ID 重新连接；
4. 保留创建时返回的访问令牌，并用原始句柄访问恢复后的数据面；
5. 对比恢复前后的计数器与依赖缓存哈希；
6. 恢复后再执行一次写入，并在 `finally` 中确保销毁沙箱。

预期结尾：

```text
State restore PASS: {'counter': 1}
Dependency cache restore PASS: <sha256>
Post-resume write PASS: {'counter': 2}
Sandbox <id> killed.
```

pause/resume 会保留 MicroVM 文件系统和进程状态；kill 是终止操作，不属于持久化
方案。

## 安全性与可复现性

- Deno 版本、JSR 包版本和传递依赖哈希均被锁定。升级时应同时更新
  `deno.json`、`deno.lock`，然后重建镜像。
- Dockerfile 使用官方配套 `.sha256sum` 校验 Deno Release 文件后才安装。
- 服务以 UID `1000` 运行，只能监听 `0.0.0.0:8000`，只可读写自己的数据目录。
- `test` 任务在当前工作区内创建临时状态，并将读写权限限定为 `.`；生产用 `start`
  任务仍只允许访问 `/workspace/deno-app/data`。
- 两个 SDK 脚本均设置 `allow_internet_access=False`，Deno 运行时默认无法访问公网。
- 两个 SDK 脚本均设置 `network={"allow_public_traffic": False}`；CubeProxy 要求每个
  请求携带临时的 `e2b-traffic-access-token`，辅助函数不会记录该令牌。
- API 密钥不会写入镜像；Git 和 Docker 构建上下文都会排除 `.env` 与本地虚拟环境。
- 演示 API 刻意不包含应用层用户认证。平台访问令牌只保护进入沙箱的流量；用于
  生产前仍需增加符合业务身份模型的认证和授权。

## 已知限制

- 官方 `cubesandbox-base:2026.16` 当前只有 `linux/amd64` manifest；默认构建不能
  生成 arm64 镜像。
- `/counter` 使用单进程文件存储，只在当前 Deno 进程内串行化写入，并以原子替换
  更新状态文件。无效或被外部破坏的状态会以失败结束并要求人工修复，绝不会静默
  重置；它不是数据库，不适合多进程或高吞吐生产负载。
- 应用服务由 SDK 脚本按需启动，模板就绪只代表 `envd` 可用，不代表端口 `8000`
  已开始监听。
- pause/resume 依赖部署版本和节点后端支持；与外部对端建立的 TCP 连接不会保证跨
  暂停保持，应用需要自行重连。
- 平台访问令牌不替代应用自己的用户鉴权；示例端点只用于验证模板行为。
- `Sandbox.connect()` 的响应不会再次返回创建时的访问令牌；恢复受限沙箱时，调用方
  必须安全保留原始令牌。示例通过保留创建时的沙箱句柄来避免访问令牌落盘。

## 常见问题

| 现象 | 排查方法 |
|---|---|
| 模板就绪探针超时 | 探测 `49983/health`，不要探测按需启动的 Deno 端口；确认镜像继承 `cubesandbox-base`。 |
| `Template not found` | 运行 `cubemastercli tpl list`，更新 `CUBE_TEMPLATE_ID`。 |
| HTTPS 证书校验失败 | 将 `REQUESTS_CA_BUNDLE` 或 `SSL_CERT_FILE` 指向 CubeProxy 私有 CA，不要设置 `verify=False`。 |
| Deno 服务未就绪 | 查看 `/tmp/cube-deno-app.log`；Python 辅助函数超时时会附带最后 80 行。 |
| 提示没有 `traffic_access_token` | 确认 CubeMaster/CubeProxy 支持受限公网访问，并保留脚本中的 `allow_public_traffic=False`。 |
| 运行时仍在下载依赖 | 确认已提交 `deno.lock`、重新构建镜像，并保留任务中的 `--frozen`。 |
| 无法 pause/resume | 确认部署的 CubeSandbox 版本及节点后端支持该生命周期能力。 |

## 文件结构

```text
deno-sandbox/
|-- Dockerfile             固定版本并校验校验和的运行时镜像
|-- deno.json              任务、权限和精确 JSR 依赖
|-- deno.lock              冻结的传递依赖图
|-- main.ts                HTTP 服务与文件持久化存储
|-- main_test.ts           Deno 行为及并发测试
|-- common.py              Cube SDK 和 HTTP 公共辅助函数
|-- run_example.py         创建、验证、启动服务的冒烟测试
|-- resume_example.py      pause/resume 状态及缓存验证
|-- tests/test_common.py   宿主机辅助函数单元测试
|-- requirements.txt       Python 依赖
`-- .env.example           本地配置模板
```
