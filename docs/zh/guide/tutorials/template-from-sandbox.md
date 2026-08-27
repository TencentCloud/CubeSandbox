# 将运行中的沙箱提交为模板

本文介绍如何将运行中沙箱的当前文件系统和内存状态制作成一个新模板。对于一些不适合写 Dockerfile 以及已经有一个符合运行环境的沙箱的情况，可以直接将其制作为模板以供后续使用。

建议在开始前先阅读[模板概览](../templates.md)，了解 OCI 镜像、模板快照、端口、探针和 `envd` 等相关概念。

## 操作沙箱

创建沙箱后可以使用 cubecli 进入该沙箱（和 docker 命令类似），后续可以在沙箱内部安装依赖，执行命令等等

```bash
cubecli exec -it <sandbox-id> bash
```

## 提交沙箱

```bash
cubemastercli tpl commit --sandbox-id <sandbox-id>
```

提交成功后，CubeMaster 会生成一个新的 `tpl-...` 模板 ID，并在源沙箱所在的节点上创建初始副本。

## 指定模板后续创建沙箱时使用的配置（高级）

执行 `tpl commit` 时，CubeMaster 不仅会保存沙箱当前的文件系统和内存状态，还会为模板保存一份 `CreateCubeSandboxReq`。以后使用这个模板创建沙箱时，这份请求负责恢复容器、启动命令、端口和网络等配置。

默认情况下，不需要提供额外文件。CubeMaster 会自动恢复源沙箱原来的创建请求：

```bash
cubemastercli tpl commit --sandbox-id <sandbox-id>
```

只有在希望新模板使用一套不同的创建配置时，才需要通过 `--file <path>` 提供 JSON 格式的完整 `CreateCubeSandboxReq`：

```bash
cubemastercli tpl commit \
  --sandbox-id <sandbox-id> \
  --file template-request.json
```

这里的 `--file` **不是**把某个文件复制进模板，也不是只修改 JSON 中出现的几个字段。它会用文件中的请求**完整替换** CubeMaster 自动恢复的原创建请求，两者不会合并。例如，文件中没有包含原请求的容器、启动命令、环境变量或暴露端口时，这些配置不会被自动保留，生成的模板可能无法按预期启动。

一个仅用于说明字段结构的 `template-request.json` 示例：

```json
{
  "instance_type": "cubebox",
  "network_type": "tap",
  "cube_network_config": {
    "allowInternetAccess": true,
    "allowOut": ["10.0.0.0/8"],
    "denyOut": []
  }
}
```

> 上述 JSON 只是简化示例，不一定足以替代你的源沙箱创建请求。使用 `--file` 前，应确认文件包含新模板运行所需的全部配置。如果只是希望把当前沙箱中的文件和运行状态保存为模板，请不要传入 `--file`。

使用 `--file` 时，还可以通过以下命令行参数修改文件请求中的 `cube_network_config`：

| 参数 | 作用 |
| --- | --- |
| `--allow-internet-access=false` | 显式设置请求中的联网开关。 |
| `--allow-out-cidr <cidr>` | 追加允许访问的出站 CIDR；该参数可重复传入。 |
| `--deny-out-cidr <cidr>` | 追加禁止访问的出站 CIDR；该参数可重复传入。 |

这些网络参数只修改 `--file` 所提供的请求，因此必须与 `--file` 一同使用。例如：

```bash
cubemastercli tpl commit \
  --sandbox-id <sandbox-id> \
  --file template-request.json \
  --allow-internet-access=false
```
