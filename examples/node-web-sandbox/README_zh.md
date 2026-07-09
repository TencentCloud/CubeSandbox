# Node.js Web 沙箱

[English README](README.md)

这是一个最小化的 CubeSandbox Node.js 20 Web 开发沙箱示例。它展示如何从
OCI 镜像构建 Cube 模板、通过兼容 E2B 的 SDK 拉起沙箱、经由 envd 执行命令，
并访问 MicroVM 内暴露出来的 Web 服务。

当你需要一个小型 Node.js Web 运行时起点，而不是 Python 代码解释器、浏览器自动化、
压测或快照示例时，可以从本示例开始。

## 示例展示内容

- 基于 `cubesandbox-base`，继承 `49983` 端口上的 envd；entrypoint 会显式
  启动 envd，因此在提供 envd 但没有 Docker entrypoint 的 CubeSandbox 运行时镜像上也可验证。
- 在 `3000` 端口暴露 Node.js 20 HTTP 服务。
- 本地验证脚本会创建沙箱、在沙箱内运行 smoke check、访问公开服务 URL，
  并在退出时清理沙箱。
- 展示一个可复用模板/示例贡献应包含的最小文档、资源、安全和审查信息。

## 场景元数据

| 字段 | 值 |
|------|----|
| Slug | `node-web-sandbox` |
| 分类 | Web 开发 / 语言运行时 |
| 目标用户 | 希望在 CubeSandbox 中使用小型 Node.js Web 服务模板的开发者 |
| 模板来源 | `examples/node-web-sandbox/Dockerfile` |
| 必需端口 | `49983` 用于 envd，`3000` 用于 Node.js 服务 |
| 最小可运行流程 | 构建/推送镜像，创建模板，设置 `CUBE_TEMPLATE_ID`，运行 `python validate.py` |
| 状态 | 有可用 CubeSandbox 部署后可进行本地审查 |

## 前置条件

- 已部署并可从本机访问的 CubeSandbox。
- 已安装并配置好的 `cubemastercli`。
- Docker 或其他 OCI 镜像构建工具。
- 如果不在集群节点本地构建，需有 CubeMaster 节点可拉取的镜像仓库。
- 本机 Python 3.8+。

## 创建模板

构建镜像：

```bash
docker build --platform linux/amd64 \
  -t <your-registry>/cubesandbox-node-web:latest \
  examples/node-web-sandbox
docker push <your-registry>/cubesandbox-node-web:latest
```

`cubesandbox-base:2026.16` 当前发布的是 `linux/amd64` 镜像；如果在 ARM
开发机上构建，请保留 `--platform linux/amd64`。

创建 CubeSandbox 模板：

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/cubesandbox-node-web:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --expose-port 3000 \
  --probe 3000 \
  --probe-path /health
```

等待模板就绪：

```bash
cubemastercli tpl watch --job-id <job-id>
```

记录返回的 `template_id`，验证脚本会从 `CUBE_TEMPLATE_ID` 读取它。

## 环境变量

```bash
cp .env.example .env
```

填写以下值：

| 变量 | 必填 | 说明 |
|------|------|------|
| `E2B_API_URL` | 是 | CubeAPI 地址，例如 `http://<cube-host>:3000` |
| `E2B_API_KEY` | 是 | CubeAPI 鉴权 key；仅在开发部署接受占位 key 时使用示例值 |
| `CUBE_TEMPLATE_ID` | 是 | `cubemastercli tpl create-from-image` 返回的模板 ID |
| `CUBE_SSL_CERT_FILE` | 否 | 使用本地 mkcert 证书时的 CA bundle 路径 |
| `NODE_WEB_PORT` | 否 | Web 服务公开端口，默认 `3000` |

不要提交 `.env`；`.env.example` 只能放占位值。

## 安装依赖

```bash
pip install -r requirements.txt
```

## 运行最小示例

```bash
python validate.py
```

预期输出：

```text
Template: tpl-xxxxxxxxxxxxxxxxxxxxxxxx
CubeAPI:  http://<cube-host>:3000
Port:     3000
Sandbox:  <sandbox-id>
localhost smoke ok: hello from CubeSandbox Node.js
runtime: v20.x.x
public HTTP ok: hello from CubeSandbox Node.js
node-web-sandbox validation ok
```

脚本会基于 `CUBE_TEMPLATE_ID` 创建沙箱，通过 envd 在沙箱内运行
`smoke_test.py`，访问 `https://<sandbox-public-host>/api/hello`，并在
`with` 代码块退出时由 SDK 清理沙箱。

## 本地容器 Smoke Check

注册 CubeSandbox 模板前，可以先验证镜像：

```bash
docker run --rm -d \
  --platform linux/amd64 \
  -p 3000:3000 \
  -p 49983:49983 \
  --name cube-node-web \
  <your-registry>/cubesandbox-node-web:latest

curl -s http://127.0.0.1:3000/api/hello
curl -s -o /dev/null -w "envd /health => %{http_code}\n" \
  http://127.0.0.1:49983/health

docker rm -f cube-node-web
```

预期结果：

- `/api/hello` 返回包含 `ok: true` 和
  `message: "hello from CubeSandbox Node.js"` 的 JSON。
- envd `/health` 返回 HTTP `204`。

## 资源建议

- Writable layer：`1G` 足够运行本最小服务和 smoke test。
- CPU 和内存：默认基线资源即可完成默认流程。
- Timeout：模板就绪后，`120` 秒足够完成验证。
- 如果要安装更多 npm 包、运行带热更新的开发服务器或执行构建工具，可能需要更大的
  writable layer 和更长 timeout。

## 安全与网络说明

- 模板本身不需要应用凭证。
- `E2B_API_KEY` 是控制面凭证，不能提交到仓库。
- Node.js 服务通过 CubeSandbox public URL 路由暴露在 `3000` 端口；除非你的部署限制了公网访问，否则应视为外部可访问。
- 本示例不设置自定义出站 egress 规则，使用部署默认策略。
- Docker 镜像保留 `49983` 上的 envd，因此 SDK 命令执行和文件访问仍可用。
- 不要把 secret 写进镜像；需要运行时配置时，通过沙箱环境变量传入。

## CubeSandbox 能力说明

- **Snapshot / resume**：本示例不演示 pause、resume 或 snapshot restore。相关流程请看快照和生命周期示例。
- **有状态工作区**：服务本身是无状态的。沙箱会话内创建的文件默认是临时状态，除非结合 CubeSandbox 快照或存储能力。
- **多服务协作**：模板运行 envd 和一个 Node.js 服务，不演示多个用户服务或 sidecar。
- **公开访问**：`3000` 端口有意暴露给 Web 流量。私有服务应结合 restricted public access。
- **受限网络**：默认验证流程不设置 allowlist 或 denylist。以出站管控为主的场景请参考 network-policy 或 route-aware egress 示例。

相关指南：

- [从 OCI 镜像制作模板](../../docs/zh/guide/tutorials/template-from-image.md)
- [模板检查与请求预览](../../docs/zh/guide/template-inspection-and-preview.md)
- [网络策略](../../docs/zh/guide/network-policy.md)
- [限制公开访问](../../docs/zh/guide/restrict-public-access.md)
- [快照、回滚与克隆](../../docs/zh/guide/snapshot-rollback-clone.md)

## 已知限制

- 示例假设 CubeMaster 节点可以拉取构建出的镜像。
- 验证脚本默认需要可以通过 public URL 访问 `3000` 端口，除非你的部署提供等价路由。
- 镜像在构建阶段从 NodeSource 安装 Node.js 20。如果构建环境无法访问
  NodeSource，请镜像该软件源，或基于已预置 Node.js 的内部基础镜像构建。
- 本地 Docker 验证只能证明服务和 envd 可启动；完整验收需要一个 ready 状态的 CubeSandbox 模板。

## 贡献者交付清单

后续生态示例可参考本示例的最小模式：

- [x] `examples/<slug>/` 下有一个自包含目录。
- [x] 通过 Dockerfile 或已文档化镜像提供模板来源。
- [x] 有模板构建或获取路径。
- [x] 有最小可运行示例流程。
- [x] `README.md` 包含用途、前置条件、命令、预期输出、资源建议、安全说明、已知限制和清理方式。
- [x] 有本地 runner 所需依赖声明。
- [x] 环境变量示例只包含占位值。
- [x] 在示例索引中登记。
- [x] 有可供 reviewer 复现的验证证据。

## Maintainer 审查清单

结合 `specs/001-sandbox-template-ecosystem/contracts/` 下的规划契约使用：

- [ ] 模板来源可复现，并明确所有必需端口。
- [ ] README 不依赖隐藏设置即可跑通。
- [ ] 预期输出足够具体，可与真实运行结果对比。
- [ ] 资源建议覆盖最小可运行示例。
- [ ] 安全说明覆盖凭证、公开访问、外部访问和 secret 处理。
- [ ] 示例索引条目区别于已有示例，并指向正确路径。
- [ ] 验证证据已脱敏且可复现。
- [ ] 示例没有削弱 CubeSandbox 的鉴权、egress、资源限制或隔离机制。

## 重复场景判断

本条目和以下已有示例不同：

- `code-sandbox-quickstart`：Python 代码和 shell 执行基础能力。
- `browser-sandbox`：Chromium 与 Playwright/CDP 自动化。
- `openai-agents-code-interpreter`：数据分析 Agent 与 Jupyter 风格代码执行。
- `cubesandbox-base-nginx`：静态 nginx 服务 smoke test。

如果后续贡献也是 Node.js Web 示例，应提供清晰差异点，例如框架、有状态工作区、受限网络模式或多服务编排；否则应修改本示例，而不是新增重复条目。

## 验证证据

Reviewer 证据模板：

```text
validated_on: 2026-07-09
validated_by: <contributor-or-maintainer>
deployment: <single-node|multi-node|other>, <host-arch>
template_build: cubemastercli tpl create-from-image ... -> template_status READY
template_id: tpl-<redacted>
environment: E2B_API_URL=<redacted>, E2B_API_KEY=<redacted>, CUBE_TEMPLATE_ID=tpl-<redacted>
install: pip install -r requirements.txt
smoke_test: python validate.py
observed_output: node-web-sandbox validation ok
cleanup: sandbox deleted by SDK context manager
limitations: <anything discovered during validation>
```

当前仓库验证：

```text
validated_on: 2026-07-09
validated_by: Codex
scope: repository 静态检查、本地 Docker 镜像 smoke 验证以及 CubeSandbox 模板验证
syntax_checks: node --check server.js; python3 -m py_compile smoke_test.py validate.py
image_build: docker build --build-arg CUBE_BASE_IMAGE=cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest -t cubesandbox-node-web:validation . -> success
image_digest: sha256:1016a5a28e20cbe9c56aa15d52a239bedf124ac48183c6c4c925c69abd3a2c57
container_smoke: python3 smoke_test.py -> localhost smoke ok: hello from CubeSandbox Node.js
envd_health: GET http://127.0.0.1:49983/health -> 204
registry_image: 127.0.0.1:5000/cubesandbox-node-web:validation
deployment: single-node one-click CubeSandbox 测试部署，Linux x86_64
template_build: cubemastercli -a 127.0.0.1 -p 8089 --timeout 60s tpl create-from-image --image 127.0.0.1:5000/cubesandbox-node-web:validation --writable-layer-size 1G --expose-port 49983 --expose-port 3000 --probe 3000 --probe-path /health -> READY
job_id: a5708319-9944-4c18-a9da-335a0d6b415c
template_id: tpl-32bee19794f94962a686937a
artifact_id: rfs-dc5359ab6d74b5ec26cfaa0a
install: python3 -m pip install --user -r requirements.txt -> 离线 wheel bootstrap 后再次执行文档命令，requirements already satisfied
smoke_test: E2B_API_URL=http://127.0.0.1:3000 E2B_API_KEY=<redacted> CUBE_TEMPLATE_ID=tpl-32bee19794f94962a686937a python3 validate.py
observed_output: Sandbox deed131c0f5c42228bb7abb396522eab; runtime v20.20.2; public HTTP ok; node-web-sandbox validation ok
cleanup: sandbox deleted by SDK context manager
limitations: 测试机首次下载依赖时 DNS 临时不可用，因此先离线 bootstrap e2b wheels，再重新执行文档中的安装命令
```

## 清理

`validate.py` 退出 SDK context manager 时会自动清理沙箱。若使用本地容器做预检，可执行：

```bash
docker rm -f cube-node-web
```

确认测试模板不再需要后再删除：

```bash
cubemastercli tpl delete --template-id <template-id>
```
