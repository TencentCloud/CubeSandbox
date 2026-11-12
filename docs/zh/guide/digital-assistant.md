# 数字助手

数字助手（AgentHub）基于 Cube Sandbox 创建和管理 OpenClaw 助手，支持助手实例、存档、回档、创建分身、发布助手模板和操作流水。

::: warning 预览版
当前数字助手能力是 Preview 版本，主要用于演示和早期试用。API、数据库 schema、部署参数和交互细节仍可能在后续版本中调整，生产环境使用前建议先在测试环境验证。
:::

## 数字助手模板

AgentHub 会基于 CubeSandbox 模板创建数字助手。部署前需要先制作数字助手模板（见下方命令），然后把它注册到 AgentHub。只制作模板是不够的：AgentHub 只会从已注册的模板里挑选。详见[未指定模板时如何选择](#未指定模板时如何选择)。

::: tip
本文早期版本要求把模板 ID 以 `AGENTHUB_DS_OPENCLAW_TEMPLATE` 写入 `.env`。代码中没有任何地方读取这个变量，设置它不会生效，请改为注册模板。
:::

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

## 未指定模板时如何选择

创建请求既没有 `templateId` 也没有快照时，AgentHub 按以下顺序选择模板：

1. 在 AgentHub 页面被标记为**推荐**的模板（模板上的**标记推荐**操作）。有多个时取其中最近注册的一个。
2. 否则取最近注册的模板。
3. 否则使用内置标识 `wecom-ds-openclaw`，由 CubeMaster 按模板别名解析。只有在某个模板带有该别名的环境里才能解析成功，标准安装不会创建这个别名。

因此注册模板会改变默认选择：只要注册了任何模板，就会优先使用它，而不再使用 `wecom-ds-openclaw` 别名。

### "no agent template is registered"

如果没有注册任何模板，内置标识也无法解析，创建会返回 HTTP `400`：

```text
no agent template is registered: register one from the template market (POST /api/v1/agenthub/templates/market) or publish one from a running agent (POST /api/v1/agenthub/instances/{agentID}/publish-template), or pass templateId explicitly
```

任选以下一种方式即可解决：

- **模板市场（WebUI）。** 在已安装的 OpenClaw 模板上点击**启用到数字助手**，或用**安装并启用到数字助手**一步完成制作与注册。安装弹窗要等模板制作完成后才会注册：如果在镜像仍在下载时关闭弹窗，模板会在 CubeSandbox 中就绪，但没有注册到 AgentHub。此时重新打开模板市场，在该模板上点击**启用到数字助手**即可。
- **API。** 用 `POST /api/v1/agenthub/templates/market` 注册已有模板，只有 `templateId` 是必填，例如 `{"templateId": "<tpl-id>", "name": "OpenClaw"}`。该接口不会检查模板是否真的存在于 CubeSandbox，填错的 ID 也会被接受，并且作为最新注册的模板成为默认模板。

  注册被删除过的模板 ID 目前无法再次注册，无论通过该接口还是**启用到数字助手**：被删除的注册仍占用该 ID，请求会返回 HTTP `500`，报唯一键冲突。请改为注册其他模板，或从助手发布一个。
- **从助手发布。** 在已有助手上使用**发布助手模板**，把它的某个存档发布为模板（`POST /api/v1/agenthub/instances/{agentID}/publish-template`）。
- **按请求指定。** 显式传入 `templateId`，此时不会查询已注册模板。

从模板市场注册不会把模板标记为**推荐**。如果希望某个模板在之后注册了更新的模板时仍保持为默认，请对它使用**标记推荐**。

### "the agent template selected by default could not be resolved"

如果模板已注册，但 CubeMaster 已无法解析它——模板已从 CubeSandbox 删除、注册时填写的模板 ID 并不存在，或已发布模板背后的存档已不存在——创建会返回 HTTP `409`。重试无效：在已注册模板发生变化之前，每次都会选中同一个模板。出问题的就是上文所说的默认模板——被标记为推荐的那个；没有推荐时是最近注册的那个——在 AgentHub 页面或 `GET /api/v1/agenthub/templates` 中都能看到是哪一个。请把一个可用的模板标记为推荐，或显式传入 `templateId`。也可以删除这条失效的注册（`DELETE /api/v1/agenthub/templates/{templateID}`），但删除后该模板 ID 就无法再次注册（见上文）。CubeOps 会在日志中记录它发出的标识以及 CubeMaster 的原始错误（后者不一定包含该标识）。

只有"找不到"这一类失败会被这样改写。选中的模板无法使用的其他情况——例如模板存在，但在健康节点上没有就绪副本——会以 HTTP `502` 返回 CubeMaster 的原始信息，其中带有 AgentHub 选中的模板 ID。请在 CubeSandbox 中检查该模板，或显式传入 `templateId`。

## 环境变量

### AgentHub 数据库

CubeOps 使用 MySQL 保存数字助手的元数据，包括助手实例、存档、模板和操作流水。配置方式如下：

```bash
DATABASE_URL=mysql://cube:cube_pass@127.0.0.1:3306/cube_mvp
```

在 one-click 部署中，如果没有显式设置 `DATABASE_URL`，启动脚本会直接导出 `CUBE_SANDBOX_MYSQL_*` 字段，由 CubeOps 直接映射为数据库配置。

### LLM API Key

创建数字助手前，请在 WebUI 的 **AgentHub 设置** 中填写 LLM API Key（以及 provider、Base URL、模型）。

未配置时无法创建或重新配置助手，页面会提示先完成设置。

配置完成后，CubeAPI 会自动把 key 注入 sandbox 内的 OpenClaw，并写入相关配置文件（如 `auth-profiles.json`），用于连接 LLM 服务。

### 凭证交付方式与模型命名空间

凭证交付支持两种方式：

- **凭证托管（推荐）**：托管的只是 **API Key**。出站请求由 CubeEgress 仅对配置的 LLM Base URL 注入 `Authorization` 头，真实 Key 不进入沙箱，OpenClaw 配置里只保存一个占位 Key。
- **环境变量注入（兼容旧版）**：把真实 API Key 直接写入 OpenClaw 环境与配置，仅建议在未启用 CubeEgress 的环境使用。

无论哪种方式，模型 ID 注入 OpenClaw 时都会归一化为 `{Provider}/{模型ID}` 的命名空间：`{Provider}` 取自 AgentHub 设置中的 Provider，斜杠后的部分作为真实模型名发往上游。使用**自定义上游**时，请确保 Provider 与模型 ID 与上游一致，否则 OpenClaw 可能报 `Unknown model`。例如 Provider 为 `openai-compatible`、模型为 `deepseek-v4-flash` 时，OpenClaw 内部解析为 `openai-compatible/deepseek-v4-flash`，上游收到的模型名为 `deepseek-v4-flash`。

## 模板快路径

如果从已发布的助手模板创建新助手，并且不需要重新绑定企业微信，CubeAPI 会使用模板快路径：新 sandbox 直接沿用模板快照里已有的 OpenClaw 配置，不会重新注入 LLM API Key。

## 安全建议

- 请妥善保管 LLM API Key，不要提交到 Git 仓库。
- 请妥善保护数据库备份与访问权限（Key 保存在数据库中）。
