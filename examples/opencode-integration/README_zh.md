# OpenCode + CubeSandbox 示例

[English](README.md)

本示例演示如何在 CubeSandbox MicroVM 中运行
[OpenCode](https://opencode.ai/)。宿主端脚本创建隔离沙箱，让 OpenCode 修改一个
很小的 Python 项目，并验证生成产物；同时覆盖 pause/resume 会话保持和受限 LLM
出网。

## 包含内容

```text
opencode-integration/
|-- Dockerfile              # Node.js + OpenCode 的 CubeSandbox 模板镜像
|-- .env.example            # 复制为 .env 后填写本地配置
|-- build-template.sh       # 打印 docker/cubemastercli 命令
|-- env_utils.py            # provider、model、env、命令构造 helper
|-- _opencode_common.py     # 沙箱命令 helper
|-- egress_policy.py        # 共享的默认拒绝出网与 CubeEgress helper
|-- run_opencode.py         # 一次性编码任务 demo
|-- resume_opencode.py      # pause/resume 会话保持 demo
|-- checkpoint_fork_opencode.py # checkpoint、fork、继续任务 demo
|-- network_policy.py       # 默认拒绝出网 + CubeEgress 注入 demo
|-- test_env_utils.py       # 配置处理单测
|-- test_commands.py        # 命令构造单测
|-- test_network_policy.py  # 安全策略断言测试
|-- test_opencode_common.py # 沙箱 helper 测试
|-- test_workflows.py       # 完整编排流程 mock 测试
`-- requirements.txt        # 宿主端 Python 依赖
```

## 前置条件

- 已部署 CubeSandbox，CubeAPI 可通过 `http://<node>:3000` 访问。
- `cubemastercli` 已连接集群。
- 构建机有 Docker，并能推送到 Cube 节点可拉取的镜像仓库。
- 宿主端 Python 3.10+。
- 一个 OpenCode 支持的 LLM provider API key。

## 1. 构建镜像

```bash
cd examples/opencode-integration
docker build --platform linux/amd64 -t <registry>/opencode-cube:latest .
docker push <registry>/opencode-cube:latest
```

镜像基于 digest 固定的 `ghcr.io/tencentcloud/cubesandbox-base:2026.16`，安装经过 SHA-256
校验的 Node.js 22.23.1 和 OpenCode 1.17.13。

## 2. 注册 CubeSandbox 模板

```bash
cubemastercli tpl create-from-image \
  --image <registry>/opencode-cube:latest \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

等待模板任务进入 `READY` 后，把返回的 `template_id` 写入 `.env`。

## 3. 配置宿主端脚本

```bash
cp .env.example .env
pip install -r requirements.txt
```

必填变量：

| 变量 | 说明 |
|---|---|
| `E2B_API_URL` | CubeAPI 地址，例如 `http://<node>:3000` |
| `E2B_API_KEY` | 本地开发可填任意非空值；启用鉴权时填写真实 key |
| `CUBE_TEMPLATE_ID` | 第 2 步创建的模板 ID |
| `OPENCODE_MODEL` | `provider/model` 形式的 OpenCode 模型 |
| `<PROVIDER>_API_KEY` | 与 provider 前缀匹配的 API key |
| `CUBE_API_URL` | 受限出网和 checkpoint/fork 流程必填；原生 CubeSandbox SDK 的 CubeAPI 地址 |
| `CUBE_PROXY_NODE_IP` | 受限出网和 checkpoint/fork 流程必填；命令流使用的 CubeProxy IP |

如需使用 OpenAI-compatible 自定义端点，设置 `OPENCODE_BASE_URL`。如果未设置，
脚本也接受 provider-specific 变量，例如 `OPENAI_BASE_URL`。脚本会在沙箱
工作目录写入 `opencode.json`，配置 `provider.<provider_prefix>.options.baseURL`。

如果连接的是本仓库的 `dev-env/` VM，还需要设置：

```bash
CUBE_DEV_SIDECAR=1
```

sidecar 会 patch E2B SDK，让 sandbox 流量走 dev-env 的 CubeProxy 端口转发。

`network_policy.py` 和 `checkpoint_fork_opencode.py` 使用原生 `cubesandbox`
SDK，而不是 E2B SDK，所以会读取 `CUBE_API_URL` 和 `CUBE_PROXY_NODE_IP`。
用 `dev-env/` 跑任一脚本时，使用下面的原生 SDK API 和代理端口转发变量：

```bash
CUBE_API_URL=http://127.0.0.1:13000
CUBE_PROXY_NODE_IP=127.0.0.1
CUBE_PROXY_PORT_HTTP=11080
```

## 4. 运行一次性编码任务

```bash
python3 run_opencode.py --dry-run
python3 run_opencode.py
```

脚本会在 `/workspace` 放入一个小型 Python 项目，让 OpenCode 实现
`calculator.add`，运行 `python3 -m unittest discover -v`，并验证 `result.md`
包含 `OPENCODE_CUBE_OK`。

## 5. 验证 pause/resume 会话保持

```bash
python3 resume_opencode.py
```

第一轮让 OpenCode 写入 `plan.md`；`sandbox.pause()` 会保存 VM 和根文件系统快照。
脚本再连接同一个 sandbox ID，验证 `/workspace` 和 OpenCode 状态目录仍存在，
然后继续任务并检查 `OPENCODE_RESUME_OK`。

## 6. 创建可复用 checkpoint 并 fork 任务

```bash
python3 checkpoint_fork_opencode.py
```

该流程在 source sandbox 中运行 OpenCode，创建显式 checkpoint，再从该 checkpoint
启动一个新 sandbox，并在 fork 中继续同一个 OpenCode 会话。脚本验证 workspace 和
OpenCode 状态已继承、sandbox ID 相互独立、测试通过且结果包含 `OPENCODE_FORK_OK`，
最后清理两个 sandbox 和临时 checkpoint。source 与 fork 都使用默认拒绝出网和同一条
LLM host 放行规则；真实 provider key 由 CubeEgress 在链路上注入，OpenCode 命令环境
中只有占位值。脚本会在两个 sandbox 中分别验证 secret 与网络策略不变量。

## 7. 受限出网模式

```bash
python3 network_policy.py
```

该路径使用原生 `cubesandbox` SDK 创建沙箱：默认拒绝出网，只允许配置的 LLM host。
真实 provider key 由 CubeEgress 在链路上注入 HTTP header；VM 全局环境中必须没有
该 key，OpenCode 命令环境中只能有占位值。如果任一条件不满足，或
`example.com` 可以访问，脚本都会失败。

自定义端点请设置：

```bash
OPENCODE_LLM_HOST=api.openai.com python3 network_policy.py
```

只检查策略、不实际调用 LLM：

```bash
python3 network_policy.py --skip-agent
```

## 验证

```bash
python3 -m py_compile env_utils.py _opencode_common.py egress_policy.py run_opencode.py resume_opencode.py checkpoint_fork_opencode.py network_policy.py
python3 -m unittest discover . -p 'test_*.py'
bash -n build-template.sh
```

可选镜像检查：

```bash
docker build --platform linux/amd64 -t opencode-cube:verify .
docker run --rm opencode-cube:verify opencode --version
docker run --rm opencode-cube:verify node --version
docker run --rm opencode-cube:verify python3 --version
```

## 排错

| 现象 | 可能原因 | 处理方式 |
|---|---|---|
| `Missing required environment variable` | `.env` 未填完整 | 补齐 `.env` 必填项 |
| `OPENCODE_MODEL must be in provider/model form` | 模型缺少 provider 前缀 | 使用 `openai/gpt-4.1-mini` 这类格式 |
| OpenCode 鉴权失败 | provider key 名称不匹配 | `openai/*` 对应 `OPENAI_API_KEY` |
| 模板 probe 失败 | envd 端口不可达 | 使用 CubeSandbox base entrypoint 时配置 `--probe 49983 --probe-path /health` |
| `403 Forbidden - CubeEgress` | 严格出网模式阻断了目标 host | 设置真实的 `OPENCODE_LLM_HOST` |
| Node.js 下载校验失败 | 上游归档发生变化或下载内容损坏 | 更新 `NODE_SHA256` 前先对照 Node.js `SHASUMS256.txt` 验证 `NODE_VERSION` |

## 安全说明

- Docker 镜像不包含任何 provider secret。
- `run_opencode.py` 和 `resume_opencode.py` 只在单次命令环境中注入当前 provider 的 key，并会在失败输出中脱敏已知 secret，适合本地验证。
- `network_policy.py` 和 `checkpoint_fork_opencode.py` 是共享集群推荐路径：默认拒绝出网 + CubeEgress header 注入。
- 不要提交 `.env`。
