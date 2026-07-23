# CodeBuddy CI + CubeSandbox 示例

[English](README.md)

本示例在一次性 CubeSandbox MicroVM 中运行无交互的 [CodeBuddy Code CLI](https://www.npmjs.com/package/@tencent-ai/codebuddy-code)。CI Runner 上传源码归档，Agent 在隔离环境中检查或测试代码，再由驱动输出报告。MicroVM 不会获得 GitHub、镜像仓库或 Runner 凭据。

## 目录结构

```text
codebuddy-ci-integration/
├── Dockerfile                 # Node.js + 固定版本 CodeBuddy CLI 模板
├── build-template.sh          # 构建并推送模板镜像
├── run_codebuddy_ci.py        # 创建 VM、上传 .tar、执行一次 CI 任务
├── resume_codebuddy_ci.py     # 恢复已暂停的任务及 CLI 会话
├── config.py                  # 环境校验与无交互命令构造
├── github-actions.yml         # 复制到使用方仓库
└── test_config.py             # 离线单元测试
```

## 构建并注册模板

```bash
cd examples/codebuddy-ci-integration
./build-template.sh <你的镜像仓库>/codebuddy-ci-cube:2.125.5

cubemastercli tpl create-from-image \
  --image <你的镜像仓库>/codebuddy-ci-cube:2.125.5 \
  --writable-layer-size 4G \
  --expose-port 49983 --probe 49983 --probe-path /health
cubemastercli tpl watch --job-id <job-id>
```

将就绪后的模板 ID 填入 `CUBE_TEMPLATE_ID`。镜像固定 `@tencent-ai/codebuddy-code` 为 `2.125.5`；升级前请重新验证 CLI 接口。

## 配置凭据与网络出口

```bash
cp .env.example .env
# 填入 E2B_API_URL、E2B_API_KEY、CUBE_TEMPLATE_ID、CODEBUDDY_AUTH_TOKEN
python -m pip install -r requirements.txt
```

共享集群应为该模板配置 **CubeEgress 默认拒绝**，仅放行租户实际使用的 CodeBuddy API 主机。Runner 可以访问 CubeAPI，但 MicroVM 不应接收 GitHub、镜像仓库或 CI 平台凭据。`--permission-mode bypassPermissions` 仅限一次性、出口受限的 MicroVM；不要与宽松外网或生产机密并用。

## 本地运行

通过归档上传源码，不给 MicroVM 提供宿主机挂载：

```bash
tar --exclude=.git -cf /tmp/project.tar .
cd examples/codebuddy-ci-integration
python run_codebuddy_ci.py --source-tar /tmp/project.tar
```

默认提示词会执行最小相关测试并写入 `/workspace/codebuddy-ci-report.md`。生产流水线应使用更小、更便于审计的 `--prompt`，并保留“不 commit、不 push”的约束。

## 长任务暂停与恢复

```bash
python run_codebuddy_ci.py --source-tar /tmp/project.tar --pause
# 复制输出的 resume handle，然后：
python resume_codebuddy_ci.py <sandbox-id>
```

`--pause` 会快照 `/workspace` 与 `/root/.codebuddy`。恢复驱动连接同一 Sandbox，以相同 `--session-id` 并加上 `--resume` 继续 CodeBuddy 会话。快照可能包含源码、报告和 Agent 状态，应按敏感数据处理，并在完成后销毁。

## GitHub Actions

将 [`github-actions.yml`](github-actions.yml) 复制到使用方仓库的 `.github/workflows/codebuddy-cubesandbox.yml`，然后将 `CUBE_API_URL`、`CUBE_API_KEY`、`CODEBUDDY_AUTH_TOKEN` 配置为 Secret，并将 `CUBE_TEMPLATE_ID` 配置为 Variable。示例没有使用 `pull_request_target`，因此不受信任 PR 不会获得仓库写权限。

## 常见问题

| 现象 | 原因与处理 |
| --- | --- |
| `codebuddy: command not found` | 重建并重新注册镜像，再确认 `codebuddy --version`。 |
| 鉴权失败 | 检查 CI Secret；不要把 Token 放进镜像或源码归档。 |
| 出口 403 或模型超时 | 仅将需要的 CodeBuddy API 主机加入 CubeEgress allowlist。 |
| 恢复后找不到会话 | 保持 `CODEBUDDY_SESSION_ID` 不变，且不要提前 kill 已暂停的 Sandbox。 |
| 归档被拒绝 | 使用不超过 100 MiB 的普通 `.tar`，排除 `.git` 和机密文件。 |

## 无集群验证

```bash
python -m py_compile config.py run_codebuddy_ci.py resume_codebuddy_ci.py
python -m unittest -v test_config.py
bash -n build-template.sh
docker build --check .
```

单元测试覆盖参数校验、无交互 JSON 输出、恢复参数，以及只把 `CODEBUDDY_AUTH_TOKEN` 转发给 Sandbox 命令这一安全边界。
