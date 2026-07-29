# OpenCode + CubeSandbox 示例

[English](README.md)

本示例把稳定版 [OpenCode](https://opencode.ai/) 终端编码 Agent 运行在
CubeSandbox MicroVM 中，并以腾讯云 TokenHub Hy3 作为可复现的
OpenAI-compatible 模型端点。Agent 的文件编辑、命令执行和测试都发生在隔离虚拟机内。

## 目录

```text
opencode-integration/
├── Dockerfile               # 固定 Cube 基础镜像与 OpenCode 二进制
├── build-template.sh        # amd64 镜像构建/推送
├── opencode.json            # Hy3 provider 与权限边界
├── .env.example             # 仅主机使用的配置模板
├── env_utils.py             # URL/密钥校验与命令构造
├── _opencode_common.py      # E2B 命令与 JSONL 解析
├── run_opencode.py          # 失败测试 -> Hy3 修复 -> 本地验收
├── resume_opencode.py       # OpenCode 会话与 VM 暂停恢复
├── network_policy.py        # 默认拒绝出口 + CubeEgress 注入
├── tests/                   # 离线单元测试
├── README.md
└── README_zh.md
```

## 兼容版本

| 组件 | 版本 |
|---|---|
| OpenCode | `1.18.9`，稳定版 V1 配置 |
| Cube 基础镜像 | `ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| CubeSandbox | 暂停恢复 `>= 0.3.0`；CubeEgress 保险库 `>= 0.4.0` |
| 主机 Python | 3.10+ |
| 模型 API | TokenHub Hy3，OpenAI-compatible Chat Completions |

OpenCode 2 当前仍是 beta，配置改为复数 `providers`。本示例固定稳定版，并使用单数
`provider`，两套格式不可混用。

## 前置条件

- 已运行的 CubeSandbox，且主机可访问 CubeAPI；
- 已连接集群的 `cubemastercli`；
- Docker 和 Cube 节点可拉取的镜像仓库；
- 可调用模型 `hy3` 的 TokenHub API Key；
- 主机 Python 3.10+。

## 1. 构建并注册模板

```bash
IMAGE=<your-registry>/opencode-cube:1.18.9 PUSH=1 \
  ./examples/opencode-integration/build-template.sh
```

Dockerfile 同时固定 OpenCode 版本和 SHA256，并关闭更新、会话分享、外部插件、LSP 下载与
models.dev 拉取，使运行时只需访问模型端点。

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/opencode-cube:1.18.9 \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health

cubemastercli tpl watch --job-id <job_id>
```

任务到达 `READY` 后记录 `template_id`。

## 2. 配置主机驱动

```bash
cd examples/opencode-integration
cp .env.example .env
# 填写 E2B_API_URL、E2B_API_KEY、CUBE_TEMPLATE_ID 与 HY3_API_KEY。
python -m venv .venv
.venv/bin/pip install -r requirements.txt
```

真实 Key 只保存在被忽略的主机 `.env` 中；镜像配置只有
`{env:HY3_API_KEY}`。

## 3. 运行修复闭环

```bash
.venv/bin/python run_opencode.py
```

流程为：

```text
写入一个确定性失败测试
-> 启动 MicroVM
-> Key 只注入本次 OpenCode 进程
-> Hy3 驱动 OpenCode 读取/测试/编辑
-> 主机断言测试文件未被改动
-> 主机重新执行测试并检查 diff
-> 销毁 MicroVM
```

缺陷是均值函数误用整除。是否完成由测试与 Git diff 判断，不以模型自述为准。

> 直接注入模式保持开放出口，受攻击的 Agent 仍可能外传进程内 Key。共享集群应使用第 5 节
> 的保险库模式。

## 4. 暂停与恢复

```bash
.venv/bin/python resume_opencode.py
```

第一轮创建 `plan.md` 并从真实 JSONL 中取得 `sessionID`；驱动暂停 MicroVM、重新连接，
验证 `/workspace` 和 `/root/.local/share/opencode` 均保留，再用同一个会话 ID 执行
第二轮。每轮单独注入 Key，镜像内不保存凭据。

## 5. 默认拒绝出口与凭据保险库

```bash
.venv/bin/python network_policy.py
```

推荐模式：

- `allow_internet_access=False`，默认拒绝所有出口；
- 只允许从 `HY3_BASE_URL` 解析出的主机；
- CubeEgress 在线路上注入 `Authorization: Bearer <secret>`；
- VM 内只能看到无权限的占位 Key；
- 记录 TokenHub 请求元数据；
- 主动验证无关站点被阻断。

脚本同时设置 `SSL_CERT_FILE` 与 `NODE_EXTRA_CA_CERTS`，使 OpenCode 信任
CubeEgress 拦截 CA。路径不同可通过 `OPENCODE_CA_BUNDLE` 覆盖。

## 无集群验证

```bash
python -m unittest discover -s tests -v
python -m compileall -q .
docker build --check .
```

2026-07-29 已用固定的 OpenCode `1.18.9` 和真实 Hy3 完成配置验证，并跑通一次原生
`read` 工具调用。暂停恢复和 CubeEgress 全链路仍需在实际 CubeSandbox 集群验收。

## 常见问题

| 现象 | 原因 | 处理 |
|---|---|---|
| `opencode: command not found` | 模板过旧 | 重新构建并注册镜像 |
| 找不到 `tokenhub/hy3` | 混用了 V1/V2 配置 | 固定 `1.18.9` 与单数 `provider` |
| `401` | 直接模式未传 Key 或保险库未注入 | 检查 `.env` 与 Authorization 规则 |
| `404` | Base URL 路径错误 | 末尾只保留一个 `/v1` |
| `403 Forbidden - CubeEgress` | 端点主机未匹配规则 | 从实际 `HY3_BASE_URL` 解析主机 |
| 保险库模式 TLS 错误 | 未信任 CubeEgress CA | 设置 `OPENCODE_CA_BUNDLE` |
| OpenCode 尝试访问其他站点 | 未禁用更新/模型表/插件 | 保留镜像环境变量与 `--pure` |
| 缺少 `sessionID` | 未使用 JSON 或第一轮中断 | 保持 `--format json` 并等待完成 |
| 模板长期 `PULLING` | 节点无法访问镜像仓库 | 使用可达仓库并配置鉴权 |

## 参考

- [集成指南](../../docs/zh/guide/integrations/opencode.md)
- [快照、克隆与回滚](../../docs/zh/guide/snapshot-rollback-clone.md)
- [安全代理](../../docs/zh/guide/security-proxy.md)
- [OpenCode Providers](https://opencode.ai/docs/providers/)
