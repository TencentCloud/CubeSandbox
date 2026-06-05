# 数字助手

数字助手（AgentHub）基于 Cube Sandbox 创建和管理 OpenClaw 助手，支持助手实例、存档、回档、创建分身、发布助手模板和操作流水。

::: warning 预览版
当前数字助手能力是 Preview 版本，主要用于演示和早期试用。API、数据库 schema、部署参数和交互细节仍可能在后续版本中调整，生产环境使用前建议先在测试环境验证。
:::

## 数字助手模板

AgentHub 会基于 CubeSandbox 模板创建数字助手。部署前需要先制作数字助手模板（见下方命令），然后将自动生成的 `tpl-` 前缀模板 ID 写入 `.env`：

```env
AGENTHUB_DS_OPENCLAW_TEMPLATE=<your-digital-assistant-template-id>
```

自定义模板必须使用和 `wecom-ds-openclaw` **相同的数字助手 / OpenClaw 镜像**构建。该镜像需要包含 OpenClaw 运行时、`supervisorctl` 服务配置，以及 AgentHub 使用的端口：

- OpenClaw Gateway UI：`18789`
- 助手环境 UI：`8080`

默认模板基于 all-in-one OpenClaw 镜像制作，镜像体积较大。首次制作或重建模板时，需要预留充足的下载、解压、快照和分发空间；在常见演示环境中，模板制作时间约为 15 分钟，具体耗时取决于镜像缓存、磁盘性能和节点数量。部署前建议确认宿主机和 Cubelet 数据盘有足够可用空间，避免模板构建中途因为磁盘不足失败。

磁盘空间可以按以下方式粗略估算：

- 单模板约 `3 GB`（rootfs `1G` + memory `2G`）。
- 单 snapshot 约 `2~3 GB`（memory 必定 `2G` + rootfs 增量）。
- 单运行实例主要是 reflink 增量，通常约几十 MB。
- Docker 基础设施约 `3.2 GB`，属于固定开销。

因此，如果只保留 `1` 个模板、`2` 个 snapshot 和几个运行实例，建议至少预留约 `12~15 GB` 可用磁盘空间。

可以使用 `cubemastercli tpl create-from-image` 基于相同镜像构建或重建模板：

```bash
OPENCLAW_IMAGE=cube-sandbox-image.tencentcloudcr.com/demo/aio-sandbox-envd-openclaw:latest

cubemastercli tpl create-from-image \
  --image "${OPENCLAW_IMAGE}" \
  --writable-layer-size 20Gi \
  --expose-port 18789 \
  --expose-port 8080 \
  --probe 18789 \
  --probe-path /
```

如果明确需要 DeepSeek 预置版本，可以在确认 tag digest 符合预期后使用 `cube-sandbox-image.tencentcloudcr.com/demo/aio-sandbox-envd-openclaw-deepseek:latest`。

该命令会输出构建任务和 `template_id`；需要等待模板构建完成后再使用 AgentHub。如果集群需要分发到指定节点，可以重复传入 `--node <node-id-or-ip>`，或在初始构建后执行已有的模板 redo 流程。

从模板创建 sandbox 后，可以在 sandbox 内验证镜像布局：

```bash
supervisorctl status openclaw
curl -fsS http://127.0.0.1:18789/ >/dev/null
```

如果模板不存在、使用了不同镜像，或没有包含 OpenClaw 服务布局，创建数字助手时可能在 setup、restart、读取 token 或生成 gateway URL 阶段失败。

## 环境变量

### AgentHub 数据库

CubeAPI 使用 MySQL 保存数字助手的元数据，包括助手实例、存档、模板和操作流水。配置方式如下：

```bash
DATABASE_URL=mysql://cube:cube_pass@127.0.0.1:3306/cube_mvp
```

如果 `DATABASE_URL` 未设置，CubeAPI 也会读取：

```bash
CUBE_API_DATABASE_URL=mysql://cube:cube_pass@127.0.0.1:3306/cube_mvp
```

在 one-click 部署中，如果没有显式设置 `DATABASE_URL`，启动脚本会根据 `CUBE_SANDBOX_MYSQL_HOST`、`CUBE_SANDBOX_MYSQL_PORT`、`CUBE_SANDBOX_MYSQL_USER`、`CUBE_SANDBOX_MYSQL_PASSWORD`、`CUBE_SANDBOX_MYSQL_DB` 自动拼接。

### DeepSeek API Key

创建或重新配置 OpenClaw 数字助手时，CubeAPI 需要 DeepSeek API Key。读取优先级为：

```bash
AGENTHUB_DEEPSEEK_API_KEY=sk-...
# fallback:
OPENCLAW_DEEPSEEK_API_KEY=sk-...
```

CubeAPI 会把读取到的 key 通过 envd 命令注入 sandbox，环境变量名为：

```bash
OPENCLAW_DEEPSEEK_API_KEY
```

sandbox 内的 OpenClaw 装配脚本会把该 key 写入：

```text
/root/.openclaw/agents/main/agent/auth-profiles.json
```

同时会更新：

```text
/root/.openclaw/openclaw.json
/root/.openclaw/agents/main/agent/models.json
```

用于配置 DeepSeek provider 和默认模型。

## 模板快路径

如果从已发布的助手模板创建新助手，并且不需要重新绑定企业微信，CubeAPI 会使用模板快路径：新 sandbox 直接沿用模板快照里已有的 OpenClaw 配置，不会重新注入 DeepSeek API Key。

## 安全建议

- 不要把真实 API Key 提交到 Git 仓库。
- 在 one-click 部署中，将 key 写入目标机的 `.env`。
- 在其他部署系统中，建议通过 Secret 或受控环境变量注入。
