# 模板别名

模板别名是 CubeSandbox 模板稳定且易读的名称。应用配置无需保存 `tpl-01abc...` 这样的生成 ID，可以改用 `python-runtime`：

```python
from cubesandbox import Sandbox

with Sandbox.create(template="python-runtime") as sandbox:
    print(sandbox.commands.run("python3 --version").stdout)
```

模板重新构建或替换后，应用仍可使用固定别名，由运维人员将别名切换到新模板。

## 命名规则与行为

模板别名必须满足以下规则：

- 只能包含小写字母、数字和连字符；
- 必须以字母或数字开头；
- 长度为 1 到 64 个字符；
- 不能使用保留的 `tpl-` 或 `snap-` 前缀；
- 同一时间只能属于一个 READY 模板；
- 不能分配给快照。

`python`、`python-3-12` 和 `app-v2` 都是有效别名；`MyApp`、`my_app`、`-my-app` 和 `tpl-custom` 无效。

每个模板最多拥有一个别名。如果把已有别名分配给另一个模板，别名会转移到目标模板，并从原模板清除。应将其视为一次部署变更：已经运行的沙箱不受影响，后续通过该别名执行的操作会解析到新模板。

## 创建模板时设置别名

从 OCI 镜像创建模板时传入 `--alias`：

```bash
cubemastercli tpl create-from-image \
  --image ghcr.io/example/python-runtime:3.12 \
  --alias python-runtime \
  --writable-layer-size 2G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

模板 ID 仍由服务端生成。模板达到 `READY` 并成功取得别名后，该别名才可使用。

通过 CubeAPI 兼容的 SDK 构建模板时，E2B 风格的 `name` 字段会作为稳定别名：

```python
from cubesandbox import Template

job = Template.build(
    image="ghcr.io/example/python-runtime:3.12",
    name="python-runtime",
    writable_layer_size="2G",
    exposed_ports=[49983],
    probe_port=49983,
    probe_path="/health",
)
print(job.template_id)
```

## 设置、修改或清除别名

为已有的 READY 模板设置或修改别名：

```bash
cubemastercli tpl set-alias tpl-01abc --alias python-runtime
```

第一个参数既可以是生成的模板 ID，也可以是模板当前的别名：

```bash
cubemastercli tpl set-alias python-runtime --alias python-runtime-v2
```

清除别名：

```bash
cubemastercli tpl set-alias tpl-01abc --clear
```

各语言 SDK 提供相同操作：

::: code-group

```python [Python]
from cubesandbox import Template

Template.set_alias("tpl-01abc", "python-runtime")
Template.set_alias("tpl-01abc", None)  # 清除
```

```go [Go]
info, err := client.SetTemplateAlias(ctx, "tpl-01abc", "python-runtime")
if err != nil {
    panic(err)
}
_, err = client.SetTemplateAlias(ctx, info.TemplateID, "") // 清除
```

```ts [Node.js]
import { Template } from "@cubesandbox/sdk";

await Template.setAlias("tpl-01abc", "python-runtime");
await Template.setAlias("tpl-01abc", null); // 清除
```

:::

## 使用和查询别名

任何接受模板标识符的位置都可以传入别名，包括创建沙箱：

::: code-group

```python [Python]
sandbox = Sandbox.create(template="python-runtime")
```

```go [Go]
sandbox, err := client.Create(ctx, cubesandbox.CreateOptions{
    TemplateID: "python-runtime",
})
```

```ts [Node.js]
const sandbox = await Sandbox.create({ template: "python-runtime" });
```

:::

模板列表和详情响应会在 `aliases` 数组中返回已配置的别名。也可以直接通过 CubeAPI 解析别名：

```bash
curl http://<cubeapi-host>:3000/templates/aliases/python-runtime
```

响应示例：

```json
{
  "templateID": "tpl-01abc",
  "public": false
}
```

## 安全发布流程

建议按以下步骤更新生产模板：

1. 构建新模板，暂时不修改生产环境使用的别名。
2. 等待新模板达到 `READY`，并通过生成的模板 ID 完成验证。
3. 将生产别名分配给新模板。
4. 使用别名创建测试沙箱，确认别名解析符合预期。
5. 根据回滚策略保留或删除旧模板。

修改别名不会改变或重启已经存在的沙箱。
