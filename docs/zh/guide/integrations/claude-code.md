---
title: Claude Code 集成指南
author: shsaihdsaiudh
date: 2026-07-06
tags:
  - integration
  - claude-code
  - coding-agent
lang: zh-CN
---

# Claude Code

[Claude Code](https://docs.anthropic.com/en/docs/claude-code) 是 Anthropic 出品的、基于终端的 AI 编码 agent,它在终端里执行命令、编辑文件、运行代码。

本指南介绍如何让 Claude Code 继续跑在**你的宿主机**上,同时用一个 `PreToolUse` hook 把它执行的**每一条 Bash 命令**透明转发进隔离的 CubeSandbox MicroVM。模型完全感知不到沙箱层,也无需改动 prompt 或使用方式。

## 为什么用 hook

基于 MCP 或 SDK 的沙箱依赖 agent *主动选择*沙箱工具;一条普通的 `Bash` 调用仍会落到宿主机上。`PreToolUse` hook 拦截工具调用本身,因此对 Bash 而言隔离是**透明且完整**的 —— 没有命令能绕过它。

## 架构

```
Claude Code(宿主机)
    ├── Read / Write / Edit ─────────────► 宿主机项目文件
    │
    └── Bash ──► PreToolUse hook ──► cubesandbox_exec ──► CubeAPI ──► MicroVM
                 (cubesandbox_rewrite.py)                 (:3000)     └─ 按会话复用
```

只有 `Bash` 工具被转发。`Read`、`Write`、`Edit` 仍在宿主机上操作文件,所以 Claude Code 在本地编辑你的项目,而它的 shell 命令跑在独立的内核 / 文件系统 / 网络里。

## 集成对象与版本

| 组件 | 版本 |
|---|---|
| Claude Code | 任意支持 `PreToolUse` hook 的版本 |
| cubesandbox Python SDK | 通过 `requirements.txt` 安装 |
| Python | 运行 Claude Code 的宿主机上需 3.9+ |

## 前置条件

- 运行中的 [CubeSandbox 部署](/zh/guide/quickstart),CubeAPI 可达(如 `http://127.0.0.1:3000`)
- 运行 Claude Code 的宿主机上有 Python 3.9+
- 一个用于创建沙箱的 CubeSandbox 模板(见下文[模板](#模板))

## 模板

hook 在沙箱里执行任意 Bash 命令,因此模板只需是一个通用的代码沙箱 —— **沙箱内不需要安装 Claude Code 本身**。用现成的 `sandbox-code` 镜像即可:

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 2G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
```

把生成的模板 ID(`cubemastercli tpl list` 中 `STATUS: READY`)填入下面的 `CUBE_TEMPLATE_ID`。

## 安装 hook

在示例目录下:

```bash
python3 -m pip install -r requirements.txt
cp .env.example .env
# 在 .env 里设置 CUBE_API_URL 和 CUBE_TEMPLATE_ID

cd hooks
./install.sh
```

安装后重启 Claude Code。安装脚本会把 `Bash` matcher 合并进 `~/.claude/settings.json`,**不**覆盖你其它设置,并且只把白名单内的 `CUBE_*` 值写入 hook 配置 —— 绝不复制 LLM provider API key。

之后照常使用 Claude Code:它发出的每条 Bash 命令都会在 MicroVM 内执行。

## 工作原理

1. **`cubesandbox_rewrite.py`**(hook)接收 `PreToolUse` 载荷。对 `Bash` 调用,它把 `tool_input.command` 改写为对执行器的调用,原命令作为单个 `shlex` 引用参数传入,并通过 `updatedInput` 返回。原命令里的任何内容都无法在宿主机执行。非 `Bash` 工具原样放行。hook 无条件改写**每一条** Bash 命令;若把已包裹的执行器调用再次喂回 hook,嵌套调用只会在沙箱内失败(沙箱内不存在宿主 hook 路径),绝不会落到宿主机。

2. **`cubesandbox_exec.py`**(执行器)按 Claude Code `session_id` 复用一个沙箱(映射存于 `~/.cache/cubesandbox-hook/`,由每会话文件锁保护),重放持久化的工作目录与导出的环境变量,在 MicroVM 内运行命令,并在命令结束后返回 stdout/stderr 和退出码(缓冲返回,非流式)。

## 宿主项目挂载

会话首次调用时,hook 可请求把 Claude Code 的项目目录按相同路径**只读**挂载进沙箱。把项目根路径加入 CubeMaster 的挂载白名单:

```yaml
extra_conf:
  allowed_host_mount_prefixes:
    - "/data/shared/"
    - "/home/you/projects/"
```

`hostPath` 在被调度的 Cubelet 节点上解析,而非运行 Claude Code 的机器。只有当 Claude Code 与该 Cubelet 同机、或项目已以相同绝对路径存在于每个可调度 Cubelet 上时,这种共享视图才成立。hook 不会把本地项目上传或同步到远端部署;不要把一个仅客户端存在、在 Cubelet 上可能指向无关数据的路径加入白名单。

该 hook 只覆盖 `Bash` 工具。`Read`、`Write`、`Edit` 仍访问宿主机,且挂载是只读的,沙箱命令不能写项目文件或构建产物。若 CubeMaster 拒绝挂载,执行会回退到无挂载沙箱:Bash 仍隔离,但与宿主侧文件工具失去文件一致性。

## 安全特性

- **fail-closed** —— 无法安全改写时以非零退出**阻断**命令,而不是放它到宿主机执行。
- **防注入** —— 原命令作为单个 `shlex` 引用参数传入,shell 元字符和换行无法越出到宿主机。
- **无条件改写** —— 每条 Bash 调用都会被改写;已包裹的执行器调用若再次经过 hook,只会在沙箱内失败(沙箱内不存在宿主 hook 路径),绝不会落到宿主机。
- **自动批准** —— hook 对改写后的 Bash 调用返回 `permissionDecision: "allow"`,Claude Code 的逐命令确认提示被抑制;请相应使用 `--permission-mode` / hooks 策略。
- **凭据不外泄** —— 安装脚本只把白名单 `CUBE_*` 值写入 hook 配置。

## 重置与卸载

```bash
# 丢弃某会话绑定的沙箱(下次调用新建)
python3 ~/.claude/hooks/cubesandbox_exec.py --reset --session <session-id>

# 从 ~/.claude/settings.json 移除 hook
cd hooks
./install.sh --uninstall
```

## 关键代码片段

### `~/.claude/settings.json` 中的 hook matcher

安装脚本会把类似下面的 `Bash` matcher 组合并进你的 settings(command 为你的绝对 home 路径):

```json
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "/home/you/.claude/hooks/cubesandbox_rewrite.py || exit 2"
          }
        ]
      }
    ]
  }
}
```

### 手动测试 hook

```bash
echo '{"tool_name":"Bash","cwd":"/tmp","session_id":"t","tool_input":{"command":"whoami"}}' \
  | python3 ~/.claude/hooks/cubesandbox_rewrite.py
```

## 注意事项

- **Read/Write/Edit 仍在宿主机。** 只有 `Bash` 工具调用被转发;Claude Code 的文件编辑仍落在宿主项目文件上。
- **同会话 Bash 串行。** 同一会话内并发的 Bash 调用会经每会话锁串行执行 —— 一次只跑一条,不并行。
- **输出为缓冲返回。** stdout/stderr 在命令结束后才返回;长时间运行的命令没有增量输出。
- **自动批准。** hook 对改写后的 Bash 调用返回 `permissionDecision: "allow"`,Claude Code 的逐命令确认提示被抑制 —— 请相应设置 `--permission-mode` / hooks 策略。
- **持久环境变量会被清理。** 导出的环境变量在命令间保留,但 `BASH_ENV`、`ENV`、`LD_PRELOAD`、`PROMPT_COMMAND` 会从持久化环境中被清除。

## 排错

### Bash 命令仍在宿主机执行

hook 未注册,或 Claude Code 未重启。确认 `~/.claude/settings.json` 有指向 `~/.claude/hooks/cubesandbox_rewrite.py` 的 `PreToolUse` `Bash` matcher,然后重启 Claude Code。可直接测试 hook:

```bash
echo '{"tool_name":"Bash","cwd":"/tmp","session_id":"t","tool_input":{"command":"whoami"}}' \
  | python3 ~/.claude/hooks/cubesandbox_rewrite.py
```

### `the cubesandbox SDK is required`

把依赖装进 Claude Code 使用的 Python 环境:`pip install -r requirements.txt`。

### `CUBE_TEMPLATE_ID is not set` / `Template not found`

在 `.env` 里把 `CUBE_TEMPLATE_ID` 设为一个 `READY` 模板(`cubemastercli tpl list`),然后重新运行 `hooks/install.sh`。

### 挂载被拒

路径不在 `allowed_host_mount_prefixes` 内,或在被调度的 Cubelet 上不存在。把前缀加入 CubeMaster `extra_conf`,确保同机,或接受无挂载回退。

## 示例仓库

完整可运行示例见 [`examples/claude-code-integration/`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/claude-code-integration),包含:

- `hooks/cubesandbox_rewrite.py` —— 改写 Bash 调用的 `PreToolUse` hook
- `hooks/cubesandbox_exec.py` —— 执行器:按会话复用 MicroVM、shell 状态持久化
- `hooks/install.sh` —— 幂等安装 / 卸载
- `tests/` —— hook 改写、执行器、安装生命周期的测试

## 参考

- Claude Code hooks 文档:<https://docs.anthropic.com/en/docs/claude-code/hooks>
- 可运行示例:[`examples/claude-code-integration/`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/claude-code-integration)
- 项目快速开始:[快速开始](/zh/guide/quickstart)
