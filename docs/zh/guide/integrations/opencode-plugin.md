---
title: OpenCode 集成指南（插件钩子方案）
author: Tantanovo
date: 2026-07-31
tags:
  - integration
  - opencode
  - coding-agent
  - plugin
lang: zh-CN
---

# OpenCode 集成指南（插件钩子方案）

让 OpenCode 继续跑在宿主机上，但它下发的每一条 `bash` 命令都在隔离的
CubeSandbox MicroVM 中执行。重定向发生在插件钩子里，模型侧不需要改动
提示词、工具或工作流。

## 集成对象与版本

| 项目 | 已测试版本 |
|---|---|
| OpenCode | 支持 `tool.execute.before` 的插件 API（见 [OpenCode 插件文档](https://opencode.ai/docs/plugins/)） |
| CubeSandbox | v0.6.0（`8721dd15`，构建于 2026-07-24） |
| Python SDK | `e2b-code-interpreter` |
| 宿主环境 | WSL2 上的 Ubuntu 24.04.4 LTS，内核 6.18.33.2，glibc 2.39 |

可运行示例：[`examples/opencode-plugin-sandbox`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/opencode-plugin-sandbox)

## 为什么用插件钩子

OpenCode 运行在开发者宿主机上，因此它下发的每条 `bash` 命令都在那里执行，
使用的是开发者本人的权限。

基于 MCP 或 SDK 的沙箱集成，只有在模型*主动选择*调用沙箱工具时才起作用。
普通的 `bash` 调用依然落在宿主上，而且失败是静默的 —— 你往往事后才知道。

OpenCode 的 `tool.execute.before` 钩子可以在工具执行前改写其参数，
这就提供了无条件拦截 `bash` 的位置：

```
OpenCode（宿主）
  ├── read / write / edit ──────────────────► 宿主项目文件
  │
  └── bash ──► tool.execute.before ──► exec_backend.py ──► CubeAPI ──► MicroVM
               (cubesandbox-bash.js)                       (:3000)
```

只有 `bash` 被重定向。`read`、`write`、`edit` 仍然作用于宿主文件，
所以 Agent 依旧在编辑真实项目，而它的 shell 命令跑在一次性的
内核、文件系统与网络命名空间里。

另一种设计 —— 把 OpenCode 整个放进沙箱 —— 会让 Agent 失去对工作区、
git 凭据和编辑器状态的访问。只重定向 `bash`，能让 Agent 留在代码所在的位置，
只把危险的那部分挪走。

## 前置条件

- Cube Sandbox 部署：单机一键安装即可（[快速开始](./../quickstart.md)）
- SDK / CLI 依赖：`pip install e2b-code-interpreter`；已安装 OpenCode
- 必需环境变量：`CUBE_TEMPLATE_ID`、`E2B_API_URL`、`E2B_API_KEY`、`SSL_CERT_FILE`

## 集成步骤

### 1. 构建模板

平台在建模板期间会通过 HTTP 探针判断就绪，因此 `--expose-port` 与 `--probe`
是必填的（见[从 OCI 镜像创建模板](./../tutorials/template-from-image.md)）。

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
```

等状态变为 `READY`，然后记下 `template_id`：

```bash
cubemastercli tpl list
```

中国大陆以外请使用 `cube-sandbox-int.tencentcloudcr.com`。

### 2. 安装 SDK

```bash
pip install e2b-code-interpreter
```

Ubuntu 24.04 因 PEP 668 会阻止直接装进系统解释器。请用虚拟环境并告知插件：

```bash
python3 -m venv ~/.venvs/cube
~/.venvs/cube/bin/pip install e2b-code-interpreter
export CUBE_OPENCODE_PYTHON=~/.venvs/cube/bin/python
```

### 3. 导出配置

```bash
export CUBE_TEMPLATE_ID=tpl-xxxxxxxx
export E2B_API_URL=http://127.0.0.1:3000
export E2B_API_KEY=e2b_000000
export SSL_CERT_FILE=/root/.local/share/mkcert/rootCA.pem
```

请在启动 OpenCode **之前**导出；之后再导出的变量，编辑器的子进程看不到。

### 4. 安装插件

OpenCode 在启动时会加载插件目录下所有 `.js` / `.ts` 文件，
所以安装就是放一个文件：

```bash
cd examples/opencode-plugin-sandbox
./plugin/install.sh            # 项目级：./.opencode/plugin/
./plugin/install.sh --global   # 全局：~/.config/opencode/plugin/
./plugin/install.sh --status   # 查看安装位置
./plugin/install.sh --uninstall
```

之后重启 OpenCode —— 插件只在启动时加载。

### 5. 验证隔离

向 OpenCode 提问：

> 执行 `uname -r` 并告诉我内核版本。

再在你自己的终端跑一次 `uname -r` 对比。**两个值必须不同。**
这个差异就是证据：如果是共享你内核的容器，报告的版本会完全一样。

在上表环境中实测：

```
沙箱内 : 6.6.1199-0009-03_2.0.1
宿主机 : 6.18.33.2-microsoft-standard-WSL2
```

另外两个检查：

> 执行 `ls /` 并描述你看到了什么。

列出的是沙箱的 rootfs。

> 创建 `/tmp/only-in-sandbox` 并确认它存在。

对模型来说文件存在；在宿主上 `ls /tmp/only-in-sandbox` 会报
「No such file or directory」。

## 关键代码

### 钩子实现

整个重定向就是一个钩子。`output.args.command` 是可变的，
赋值即改变实际执行的内容：

```js
export const CubeSandboxBashPlugin = async ({ client }) => ({
  "tool.execute.before": async (input, output) => {
    if (input.tool !== "bash") return;

    // 失败即阻断：后端缺失时阻断命令，而不是回落到宿主执行。
    if (!fs.existsSync(BACKEND)) {
      throw new Error("[cubesandbox-bash] backend not found; refusing to run on host");
    }

    // 原始命令成为单个 argv 元素，其中的 shell 元字符
    // 永远不会被宿主 shell 解释。
    output.args.command = [
      shellQuote(pythonInterpreter()),
      shellQuote(BACKEND),
      "--session", shellQuote(resolveSessionId(input)),
      "--command", shellQuote(output.args.command),
    ].join(" ");
  },
});
```

三个性质值得单独说明：

**失败即阻断。** 若命令无法被安全重定向，钩子抛出异常，OpenCode 随即阻断该调用。
一个会静默回落到宿主的沙箱集成，比没有集成更糟 —— 因为失败是不可见的。

**防注入。** `shellQuote` 用单引号包裹命令，并按 POSIX 惯用法转义内部引号。
以 `echo A'; touch /tmp/pwned; echo '` 为例，宿主 shell 会把改写后的字符串
解析成恰好六个 argv 元素，最后一个就是原文本身，`touch` 不会被执行。

**幂等。** 多个插件可能观察同一次调用，因此当命令已引用后端时钩子直接返回。

### 保留 session 状态

只有连续命令能共享状态，shell 才好用。后端会包装每条命令，
让 guest 回报最终的 cwd 与环境变量，宿主按 session 存下来，下次调用前恢复：

```python
lines = [
    "set +e",
    f"cd {shlex.quote(cwd)} 2>/dev/null || cd {shlex.quote(DEFAULT_WORKDIR)}",
    *(f"export {k}={shlex.quote(str(v))}" for k, v in env.items()),
    f"bash -c {shlex.quote(command)}",
    "__cube_rc=$?",
    f'echo "{_STATE_BEGIN}"',
    "python3 -c 'import json,os;print(json.dumps({\"cwd\": os.getcwd(), ...}))'",
    f'echo "{_STATE_END}"',
    'echo "__CUBE_RC__=$__cube_rc"',
]
```

效果：

```
> cd /workspace/demo && pwd     →  /workspace/demo
> pwd                           →  /workspace/demo   （已保留）
> export TOKEN=abc123
> echo $TOKEN                   →  abc123            （已保留）
```

状态存放于 `~/.cache/cubesandbox-opencode/<session>.json`，权限 `0600`
（因为可能保存命令导出的敏感值）。session id 在用于文件名前会被净化，
因此无法逃出状态目录。

### 并发处理

OpenCode 可能同时下发多条 `bash` 调用。同一 session 内的调用通过
`O_CREAT | O_EXCL` 锁文件串行化，并带过期锁回收，
避免崩溃的调用把整个 session 卡死。不同 session 之间并行执行。

## 注意事项

**每次调用一个 MicroVM，而非每 session 一个。** session *状态*会保留，
但 VM 每次都重建（参考环境下约 1 秒）。让它在调用间保持存活需要常驻辅助进程
或 CubeSandbox 的 pause/resume。

**`read` / `write` / `edit` 未被沙箱化。** 这是有意为之 —— Agent 必须能编辑项目 ——
但恶意的 `write` 不在本集成的防护范围内。

**`git` 默认留在宿主。** 沙箱有自己的文件系统，在里面执行 `git commit`
操作的是另一个仓库，而不是 OpenCode 正在编辑的那个。
可通过 `CUBE_OPENCODE_PASSTHROUGH` 配置，默认值为 `git,gh,opencode`。
匹配只看首个 token，所以 `git status` 会放行，而 `foo && git push` 不会。

**钩子入参结构不是稳定契约。** session id 的键名在不同 OpenCode 版本间
拼写有差异，因此代码会探测多种变体并提供兜底。猜错只会损失沙箱复用的粒度，
绝不会削弱隔离性。

**交互式命令不可用。** 输出是被捕获的，任何需要 TTY 的程序
（`vim`、`top`）都不会正常工作。

**需要通配 DNS。** SDK 通过 `<端口>-<sandboxId>.cube.app` 访问沙箱。
一键安装自带 CoreDNS 处理这件事，但某些环境会覆盖 `/etc/resolv.conf`。
WSL 每次启动都会重写，除非 `/etc/wsl.conf` 中包含：

```ini
[network]
generateResolvConf = false
```

详见 [HTTPS 与域名解析](./../https-and-domain.md)。

**这里没有配置网络策略。** 如果不希望 Agent 访问任意主机，
请用[网络策略](./../network-policy.md)限制沙箱出网。

## 测试

```bash
cd examples/opencode-plugin-sandbox
node tests/test_plugin.mjs
```

21 项断言，覆盖命令改写、放行名单、幂等性、session id 处理与引号注入抵抗。
**仅依赖 Node 标准库** —— 不需要 npm install、不需要联网、不需要 CubeSandbox 部署。

注入相关断言会按 POSIX shell 的语义解析改写后的命令，
并检查原始文本是否完整落在唯一一个 argv 元素中。
这比子串匹配是更强的性质：引号处理有误时，payload 会被拆到多个元素从而断言失败。

## 参考

- 相关文档：[快速开始](./../quickstart.md)、
  [从 OCI 镜像创建模板](./../tutorials/template-from-image.md)、
  [HTTPS 与域名解析](./../https-and-domain.md)、
  [网络策略](./../network-policy.md)
- 示例仓库：[`examples/opencode-plugin-sandbox`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/opencode-plugin-sandbox)
- 上游项目：[OpenCode](https://opencode.ai/) ·
  [插件文档](https://opencode.ai/docs/plugins/)
