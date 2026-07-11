# CubeSandbox Hook 方案设计记录

> 状态：已实现，见同目录下 `cubesandbox_exec.py` / `cubesandbox_rewrite.py` / `install.sh` / `README.md`
> 日期：2026-07-05（设计）/ 2026-07-06（实现）
> 分支：`feat/sandbox-backend`

## 相对本文档的实现变更

实现时对"待解决问题"一节做了如下处理，供后续review参考：

1. **`cd` 不再特殊放行**：不判断命令是否是 `cd`，而是让 `cd`/`export` 都统一走
   `cubesandbox-exec`。执行时在沙箱内用每会话随机、仅所有者可读写的状态目录
   （`/tmp/.cubesandbox-state-<random>`）持久化 cwd 和已导出的
   环境变量：每次执行前 `source`/`cd` 恢复，执行后写回。这样多次独立的
   `commands.run()` 调用之间也能保持连续的 shell 状态，不需要在 hook 里猜测哪些
   命令是"纯 shell builtin"。
2. **文件系统一致性**：创建沙箱时，用 `host-mount`（见 `../host-mount/`）把 hook
   输入里的 `cwd`（Claude Code 项目目录）以只读方式挂载到沙箱内的同一路径，
   使 Bash 能读取与 Read/Write/Edit 在 host 上相同的项目文件，同时不能通过挂载修改 host。如果该路径
   不在 CubeMaster 的 `allowed_host_mount_prefixes` 白名单内，会捕获 `ApiError` 并
   降级为不挂载的普通沙箱（打印警告，不中断执行）。
3. **沙箱生命周期**：一个 Claude Code session（hook 输入里的 `session_id`）对应
   一个沙箱，用本地状态文件（`~/.cache/cubesandbox-hook/<session_id>.json`）缓存
   `sandbox_id`，后续调用 `Sandbox.connect()` 复用；提供 `--reset` 手动销毁。
4. **性能**：未做量化测试；每次调用仍是一次新的 Python 进程 + 一次
   `commands.run()` HTTP 往返，冷启动成本已通过会话级复用消除，但比原生本地 Bash
   仍有网络往返开销，待实测补充数据。

## 背景

目前 `examples/sandboxed-claude/` 已实现了两种 Claude Code 集成方式：

1. **Headless 执行**：在沙箱内部运行 Claude Code 本身
2. **MCP Server**：Claude Code 作为 MCP client，通过 `sandbox_run_code` / `sandbox_run_command` 等工具显式调用沙箱

MCP 方案的问题是：Claude Code **必须主动选择**沙箱工具，如果 AI 选择直接用 Bash 工具就能绕过沙箱。

## 灵感来源：RTK (Rust Token Killer)

RTK 是一个用 Rust 写的 CLI 代理，通过 Claude Code 的 **PreToolUse Hook** 透明拦截所有 Bash 命令，改写后再交还 Claude Code 执行。AI 完全不知道命令被改写过。

### RTK 的三件套

```
~/.claude/
├── settings.json              ← 注册 PreToolUse hook
├── hooks/
│   └── rtk-rewrite.sh         ← 拦截脚本
├── RTK.md                     ← 元命令说明
└── CLAUDE.md                  ← 追加 @RTK.md 引用
```

### RTK 的数据流

```
Claude Code 要执行 "git status"
    │
    ▼
PreToolUse hook 触发 → rtk-rewrite.sh
    │
    ▼
rtk rewrite "git status" → 返回 "rtk git status"
    │
    ▼
返回 JSON: {"updatedInput": {"command": "rtk git status"}}
    │
    ▼
Claude Code 执行改写后的命令（输出被 rtk 压缩 ~80%）
```

### rtk-rewrite.sh 核心逻辑

```bash
INPUT=$(cat)  # 从 stdin 读 PreToolUse JSON
COMMAND=$(提取 tool_input.command)

# 已有 rtk 前缀的命令直接放行
[[ "$COMMAND" == rtk\ * ]] && exit 0

# 调用 rtk Rust 二进制做改写
REWRITTEN=$(rtk rewrite "$COMMAND")

# 返回 updatedInput 给 Claude Code
cat <<ENDJSON
{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","updatedInput":{"command":"$REWRITTEN"}}}
ENDJSON
```

## CubeSandbox Hook 方案设计

### 核心思路

把 RTK 的 "命令改写" 模式从 **token 压缩** 变成 **沙箱隔离**：

```
Claude Code 要执行 "python3 -c '...'"
    │
    ▼
PreToolUse hook 触发 → cubesandbox_rewrite.py
    │
    ▼
判断命令是否需要沙箱化
    │
    ▼
改写: "python3 -c '...'" → "cubesandbox-exec python3 -c '...'"
    │
    ▼
updatedInput 交回 Claude Code
    │
    ▼
cubesandbox-exec 在 CubeSandbox MicroVM 中执行，返回结果
```

### 组件

#### 1. `cubesandbox_exec.py` — 沙箱执行 CLI

- 接收任意 shell 命令作为参数
- 管理沙箱生命周期（懒创建、缓存复用、超时清理）
- 使用原生 `cubesandbox` SDK（非 e2b_code_interpreter）
- 行为类似 `ssh` 或 `docker exec`：把命令发送到远端，stdout/stderr 原样返回，exit code 透传

```
用法:
  cubesandbox-exec "python3 -c 'print(1+1)'"
  cubesandbox-exec "npm test"
  cubesandbox-exec --reset  # 销毁当前沙箱，下次命令走新沙箱
```

关键设计点：
- 沙箱缓存（`_sandbox` 全局变量），避免每次命令都冷启动
- 环境变量驱动配置（`CUBE_API_URL`、`CUBE_TEMPLATE_ID`）
- 真正的 `sys.exit(result.exit_code)` 透传退出码

#### 2. `cubesandbox_rewrite.py` — PreToolUse Hook 脚本

- 从 stdin 读取 Claude Code 的 PreToolUse JSON
- 提取 `tool_input.command`
- 判断是否需要沙箱化（跳过 `cubesandbox-exec` 自身、`cd` 等纯 shell built-in）
- 改写命令，返回 `updatedInput` JSON

```python
# 伪代码
input = json.loads(sys.stdin.read())
command = input["tool_input"]["command"]

# 不需要沙箱化的命令
if command.startswith("cubesandbox-exec") or command.startswith("cd "):
    sys.exit(0)  # exit 0 = 原样执行

# 改写
new_command = f"cubesandbox-exec {shlex.quote(command)}"
output = {
    "hookSpecificOutput": {
        "hookEventName": "PreToolUse",
        "permissionDecision": "allow",
        "updatedInput": {"command": new_command}
    }
}
print(json.dumps(output))
sys.exit(0)
```

#### 3. `install.sh` — 安装脚本

- 在 `~/.claude/settings.json` 中注册 PreToolUse hook（幂等）
- 创建 `~/.claude/hooks/cubesandbox_rewrite.py`
- 可选：在 CLAUDE.md 中添加 sandbox 相关说明

### 文件布局

```
examples/sandbox-backend/
├── DESIGN.md                  ← 本文档
├── cubesandbox_exec.py        ← 沙箱执行 CLI
├── cubesandbox_rewrite.py     ← PreToolUse hook 脚本
├── sandbox_exec.py            ← 独立 CLI（e2b SDK，opt-in）
├── mcp_server.py              ← MCP Server（opt-in 沙箱工具）
├── install.sh                 ← 安装脚本
└── README.md                  ← 使用说明
```

### 与 MCP 方案的对比

| | MCP 方案 (已实现) | Hook 方案 (已实现) |
|---|---|---|
| 拦截方式 | AI 显式调用 `sandbox_run_code` 工具 | PreToolUse hook 自动拦截 Bash |
| 透明度 | AI 知道自己在用沙箱 | AI 完全无感 |
| 可靠性 | 依赖 AI 配合，可能被绕过 | 覆盖 Claude Code 的 Bash 工具调用 |
| 粒度控制 | 精细，可选择性沙箱化 | 粗粒度，默认全部拦截 |
| 额外功能 | 可自定义工具（sandbox_reset 等） | 纯执行代理 |
| 实现复杂度 | 中（MCP 协议、Content-Length 帧解析） | 中（hook 脚本 + 会话化 CLI） |

### 待解决问题

1. **哪些命令不该沙箱化？** — `cd`、`export`、`cubesandbox-exec` 自身需要在 host 执行
2. **文件操作？** — Claude Code 的 Read/Write/Edit 工具操作的是 host 文件系统，但 Bash 命令在沙箱中——两者看到的文件不一致
3. **沙箱生命周期？** — 一个会话用一个沙箱还是一系列命令各用各的？
4. **性能？** — Python 进程启动 + 沙箱通信延迟 vs 直接执行，差异多大？

### 参考资料

- [RTK GitHub](https://github.com/rtk-ai/rtk)
- [Claude Code Hooks 文档](https://code.claude.com/docs/en/hooks)
- [CubeSandbox SDK Python API](/home/dev/CubeSandbox/sdk/python/cubesandbox/)
- [已有的 sandbox_exec.py](/home/dev/CubeSandbox/examples/sandbox-backend/sandbox_exec.py)
