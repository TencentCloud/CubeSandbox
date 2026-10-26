# 模板检查与请求预览

当你手里只有一个 `template_id`，通常最关心的是这几件事：

- 这个模板现在是不是可用状态，哪些节点上已经有副本？
- 模板本身保存的请求体到底是什么？
- 如果我现在拿它创建沙箱，最终会生效什么请求？

这页文档对应的就是 `cubemastercli tpl info` 和 `cubemastercli tpl render` 两条命令。

## 先选对命令

| 你想确认什么 | 用哪条命令 |
|--------------|------------|
| 模板状态、副本、版本、错误信息 | `tpl info` |
| 模板里保存的请求体 | `tpl info --include-request` |
| 模板合并后最终会生效的创建请求 | `tpl render` |
| CubeMaster 最终转换给 Cubelet 的请求结构 | `tpl render` |

可以这样记：

- 想先看模板本身，用 `tpl info`
- 想看“现在创建会传什么”，用 `tpl render`

## 先看模板本身

查看基础信息：

```bash
cubemastercli tpl info <template-id>
```

模板 ID 既可以用位置参数传入（与 docker/kubectl 风格一致），也可以继续使用 `--template-id`，两种写法等价。

查看原始 JSON：

```bash
cubemastercli tpl info <template-id> --json
```

把模板里保存的请求体一并带出来：

```bash
cubemastercli tpl info <template-id> --json --include-request
```

典型返回结构如下：

```json
{
  "template_id": "tpl-xxx",
  "instance_type": "cubebox",
  "status": "READY",
  "replicas": [
    {
      "node_id": "node-a",
      "status": "READY"
    }
  ],
  "create_request": {
    "containers": [],
    "volumes": [],
    "annotations": {
      "cube.master.appsnapshot.template.id": "tpl-xxx"
    }
  }
}
```

其中 `create_request` 可以这样理解：

- 它是模板定义里保存下来的请求体。
- 它适合回答“这个模板最初是基于什么请求构建或提交出来的”。
- 它不是未来每次创建沙箱都会直接使用的最终请求，因为后续用户输入还可能继续叠加合并。

## 再看最终会生效的请求

只用模板做预览：

```bash
cubemastercli tpl render --template-id <template-id> --json
```

如果你已经有自己的请求覆盖项，可以带上请求文件一起预览：

```bash
cubemastercli tpl render -f req.json --template-id <template-id> --json
```

`tpl render` 支持从请求文件读取输入；如果你同时传了 `--template-id`，命令行参数会覆盖文件里的模板注解。

典型返回结构如下：

```json
{
  "api_request": {
    "annotations": {
      "cube.master.appsnapshot.template.id": "tpl-xxx",
      "cube.master.appsnapshot.template.version": "v2"
    }
  },
  "merged_request": {
    "containers": [
      {
        "name": "main"
      }
    ],
    "volumes": [
      {
        "name": "workspace"
      }
    ],
    "network_type": "tap"
  },
  "cubelet_request": {
    "containers": [
      {
        "name": "main"
      }
    ],
    "volumes": [
      {
        "name": "workspace"
      }
    ]
  }
}
```

## 这三层分别看什么

`tpl render` 会返回三层视图：

- `api_request`：CubeMaster 收到并解析后的请求，还没做模板解析和合并。
- `merged_request`：模板解析和请求合并完成后的结果。对大多数用户来说，这一层最值得看。
- `cubelet_request`：CubeMaster 基于合并结果生成的、最终发给 Cubelet 的请求结构。

如果你时间有限，优先看 `merged_request`。

## 一个最实用的排查顺序

如果你想弄清楚“拿这个模板创建沙箱时到底会传什么”，建议按下面顺序看：

1. 先看模板状态和副本。

```bash
cubemastercli tpl info <template-id>
```

2. 如果你想确认模板本身存了什么，再看 `create_request`。

```bash
cubemastercli tpl info <template-id> --json --include-request
```

3. 再看当前真正会生效的请求预览。

```bash
cubemastercli tpl render --template-id <template-id> --json
```

4. 如果你的业务还会追加 labels、annotations、网络配置或其他参数，把这些内容写进 `req.json`，再跑一次预览。

```bash
cubemastercli tpl render -f req.json --template-id <template-id> --json
```

## 这页不能替你确认什么

预览很有用，但它也有明确边界：

- 它不能告诉你最终会调度到哪台节点，调度仍然是运行时决策。
- 它不能保证某个节点在真实运行时一定会接受这个请求。
- 它展示的是请求内容，不等于沙箱实际启动后的最终运行结果。
