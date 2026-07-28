# Claude Code + CubeSandbox —— 透明隔离 Bash

[English](./README.md)

让 [Claude Code](https://docs.anthropic.com/en/docs/claude-code) 继续跑在你的宿主机上,
但把它执行的**每一条 Bash 命令**透明地转发进隔离的 **CubeSandbox** MicroVM 里执行。
一个 `PreToolUse` hook 会在每次 `Bash` 工具调用执行前将其改写 —— 模型完全感知不到沙箱层,
也不需要改变任何使用方式。

```
Claude Code(宿主机)
    ├── Read / Write / Edit ─────────────► 宿主机项目文件
    │
    └── Bash ──► PreToolUse hook ──► cubesandbox_exec ──► CubeAPI ──► MicroVM
                 (cubesandbox_rewrite.py)                 (:3000)     └─ 按会话复用
```

只有 `Bash` 工具被转发。`Read`、`Write`、`Edit` 仍在宿主机上操作文件,所以 Claude Code
在本地编辑你的项目,而它的 shell 命令跑在独立的内核 / 文件系统 / 网络里。

## 为什么用 hook

基于 MCP 或 SDK 的沙箱依赖 agent *主动选择*沙箱工具;一条普通的 `Bash` 调用仍会落到宿主机上。
`PreToolUse` hook 堵住了这个缺口:它拦截工具调用本身,因此对 Bash 而言隔离是**透明且完整**的
—— 没有任何命令能绕过它。

| 特性 | 行为 |
|------|------|
| **透明** | 模型发起普通的 `Bash` 调用,由 hook 改写。无需改 prompt 或工具。 |
| **按会话复用沙箱** | 每个 Claude Code `session_id` 复用一个 MicroVM,同一会话的命令共享状态。 |
| **shell 状态延续** | 同一会话内,`cd` 和导出的环境变量在多次 Bash 调用间保留。 |
| **只读挂载宿主项目** | 会话首次调用可把项目按相同路径只读挂载,沙箱命令可读取(不可修改)宿主文件。 |
| **fail-closed** | 若无法安全改写,hook 以非零退出**阻断**命令,而不是放它到宿主机执行。 |
| **防注入** | 原命令作为单个 `shlex` 引用参数传入,shell 元字符和换行都无法越出到宿主机。 |

## 前置条件

- 运行中的 CubeSandbox 部署(CubeAPI 可达,如 `http://127.0.0.1:3000`)
- 运行 Claude Code 的宿主机上有 Python 3.9+
- 一个用于创建沙箱的 CubeSandbox 模板(`cubemastercli tpl list`)

## 快速开始

### 1 —— 安装依赖并配置

```bash
python3 -m pip install -r requirements.txt
cp .env.example .env
# 编辑 .env:设置 CUBE_API_URL / E2B_API_URL 和 CUBE_TEMPLATE_ID
```

### 2 —— 安装 hook

```bash
cd hooks
./install.sh
```

安装脚本会把 hook 注册进 `~/.claude/settings.json`,**不**覆盖你其它设置,并且只把 `../.env`
里白名单内的 `CUBE_*` 值复制进 hook 配置。重启 Claude Code 以加载 hook。

### 3 —— 照常使用 Claude Code

直接用 Claude Code。它发出的每条 Bash 命令现在都在 MicroVM 内执行:

```
> 执行 `uname -a && whoami`,告诉我它在哪运行
```

命令在沙箱里运行(不同内核、沙箱用户),而 Claude Code 的文件编辑仍在你的宿主机上。

## 工作原理

1. **`cubesandbox_rewrite.py`**(hook)接收 `PreToolUse` JSON。对 `Bash` 调用,它把
   `tool_input.command` 改写为:

   ```
   <python> <hooks>/cubesandbox_exec.py --session=<id> --mount=<cwd> --timeout=<秒> -- <原命令>
   ```

   并通过 `updatedInput` 返回。原命令是单个引用参数,里面的内容无法在宿主机执行。非 `Bash`
   工具原样放行;已经被包裹过的命令不再重复包裹。

2. **`cubesandbox_exec.py`**(执行器)按 `session_id` 复用一个沙箱(映射存于
   `~/.cache/cubesandbox-hook/`,由每会话文件锁保护),重放持久化的工作目录与环境变量,
   在 MicroVM 内运行命令,并回传 stdout/stderr 和退出码。

## 宿主项目挂载

会话的首次 Bash 调用可把 Claude Code 的项目目录按相同绝对路径**只读**挂载进沙箱 ——
沙箱命令可读取宿主项目文件,但不能修改或往里写构建产物。

`hostPath` 在被调度到的 Cubelet 节点上解析,而非运行 Claude Code 的机器。因此只有当
Claude Code 与该 Cubelet 同机、或项目已经以相同绝对路径存在于每个可调度 Cubelet 上时,
这种共享视图才成立。hook 绝不会把本地项目上传或同步到远端部署 —— 不要把一个仅客户端存在、
在 Cubelet 上可能指向无关数据的路径加入白名单。

项目路径必须被 CubeMaster 允许:

```yaml
extra_conf:
  allowed_host_mount_prefixes:
    - "/data/shared/"
    - "/home/you/projects/"
```

若挂载被拒,执行会回退到无挂载的隔离沙箱:Bash 仍然隔离,但不再与 Claude Code 宿主侧文件
工具共享文件视图。

## 重置与卸载

```bash
# 丢弃某会话绑定的沙箱(下次调用重新创建)
python3 ~/.claude/hooks/cubesandbox_exec.py --reset --session <session-id>

# 从 ~/.claude/settings.json 移除 hook
cd hooks
./install.sh --uninstall
```

## 目录结构

```
claude-code-integration/
├── hooks/
│   ├── cubesandbox_rewrite.py   # PreToolUse hook:把 Bash 改写为沙箱执行
│   ├── cubesandbox_exec.py      # 执行器:按会话复用 MicroVM + 状态持久化
│   └── install.sh               # 幂等安装 / 卸载
├── tests/
│   ├── conftest.py
│   ├── test_cubesandbox_rewrite.py
│   ├── test_cubesandbox_exec.py
│   └── test_hook_install.py
├── requirements.txt
├── .env.example
├── TROUBLESHOOTING.md
├── README.md
└── README_zh.md                 # 本文件
```

## 测试

```bash
python3 -m pip install -r requirements.txt pytest
pytest tests
```

## 排错

| 现象 | 可能原因 | 处理 |
|------|---------|------|
| Bash 命令仍在宿主机执行 | hook 未注册 / Claude Code 未重启 | 重新执行 `hooks/install.sh` 并重启 Claude Code |
| `CUBE_TEMPLATE_ID is not set` | `.env` 缺模板 | 设置 `CUBE_TEMPLATE_ID`(见 `cubemastercli tpl list`) |
| `the cubesandbox SDK is required` | 未装依赖 | `pip install -r requirements.txt` |
| `Template not found` | 模板 ID 错误 | 检查 `cubemastercli tpl list` |
| 挂载被拒(警告) | 路径不在 `allowed_host_mount_prefixes` | 在 CubeMaster `extra_conf` 加前缀,或接受无挂载回退 |

更多见 [TROUBLESHOOTING.md](./TROUBLESHOOTING.md)。
