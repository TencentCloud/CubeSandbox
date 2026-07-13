# 踩坑记录 — Claude Code + CubeSandbox 集成

本文档记录在开发和测试 Claude Code 集成 CubeSandbox 过程中遇到的所有问题及解决方案。

---

## 环境与基础设施

### 1. 腾讯云 CVM 无法访问 GitHub

**现象**：`curl github.com`、`git clone https://github.com/...` 全部超时

**原因**：腾讯云 CVM 对 GitHub 的出站连接被限制

**解决**：使用 cnb.cool 镜像站 clone CubeSandbox 仓库

```bash
git clone --depth 1 https://cnb.cool/CubeSandbox/CubeSandbox.git
```

---

### 2. Docker Hub 不可达

**现象**：`docker pull`、`docker push` 到 Docker Hub 全部超时。`cubemastercli tpl create-from-image` 会把本地镜像名解析为 `docker.io/library/...` 然后尝试拉取

**影响**：无法通过"构建自定义 Docker 镜像 → push → 创建模板"的标准流程

**解决**：
- 开发/演示阶段直接用 sandbox-code 基础模板，在沙箱启动时在线安装 Claude Code
- 生产环境需要将镜像推到可访问的 registry（如 cube-sandbox-cn.tencentcloudcr.com）
- 备用方案：用 `sandbox.pause()` + `cubemastercli tpl commit` 将已配置的沙箱提交为模板（需要提供 create_sandbox request JSON 文件）

---

### 3. Docker 无法 pull registry 镜像

**现象**：想启动本地 registry 推送镜像，但 `docker pull registry:2` 超时（Docker Hub 不可达）

**影响**：也无法使用本地 registry 方案

**解决**：接受演示模式限制，在线安装依赖

---

## 沙箱与模板

### 4. 模板快照 CPU 特性不兼容（CpuidCheckCompatibility）

**现象**：
```
CubeMaster returned error code -1: failed to run container: 
Error checking cpu feature compatibility: CpuidCheckCompatibility
```

**原因**：服务器重启后 CPU 微码或内核参数变化，导致之前创建的快照无法在新环境中恢复

**解决**：删除旧模板，重新创建

```bash
sudo cubemastercli tpl delete --template-id <旧模板ID>
sudo cubemastercli tpl create-from-image \
  --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 2G --expose-port 49999 --probe 49999
```

**教训**：模板快照与 CPU 特性绑定，系统升级/重启后可能需要重建模板

---

### 5. npm install 超时（120 秒不足）

**现象**：`npm install -g @anthropic-ai/claude-code` 在沙箱内执行时报 `context deadline exceeded`

**原因**：npm 下载和安装 Claude Code（含依赖）耗时可能超过 120 秒

**解决**：将 npm install 的 timeout 参数增加到 300 秒

**教训**：沙箱内的网络安装操作需要足够的超时余量，尤其在跨国网络环境下

---

## Claude Code 运行

### 6. `which claude` 未找到时抛异常而非返回非零退出码

**现象**：检查 Claude Code 是否已安装时 `which claude` 抛出 `CommandExitException`，导致脚本崩溃

**原因**：E2B SDK 的 `sandbox.commands.run()` 对非零退出码抛 `CommandExitException` 而非返回结果对象

**解决**：用 try/except 包裹 `sandbox.commands.run()` 调用

```python
def run_command(sandbox, cmd, user="root", timeout=300):
    try:
        return sandbox.commands.run(cmd, user=user, timeout=timeout)
    except CommandExitException as e:
        return e  # 返回异常对象，调用者检查 .exit_code
```

**注意**：`cubesandbox` SDK（用于 network_policy.py）的行为不同——它的 `commands.run()` 直接返回 `CommandResult`，不抛异常。不要在两个 SDK 间照搬这个 try/except 模式。

---

### 7. `--dangerously-skip-permissions` 不能用于 root 用户

**现象**：以 root 身份运行 Claude Code 时输出：
```
--dangerously-skip-permissions cannot be used with root/sudo privileges 
for security reasons
```

**原因**：Claude Code 的安全策略禁止 root 用户跳过权限检查

**解决**：在沙箱内创建非 root 用户，以该用户身份运行 Claude Code

```python
run_command(sandbox, "useradd -m -s /bin/bash dev")
# 然后以 user="dev" 执行 claude 命令
```

**额外发现**：root 用户只能用 `--permission-mode acceptEdits`（只允许编辑文件），不能执行命令。要完整功能就必须用非 root 用户。

---

### 8. 环境变量不传递到非 root 用户

**现象**：通过 root shell 设置的 `export` 环境变量，在 `user="dev"` 执行命令时不可见

**原因**：E2B SDK 的 `commands.run(user="dev")` 为每次调用创建独立 shell 会话，不继承 root 用户的环境变量

**解决**：将环境变量直接拼接到 claude 命令前面

```python
# 错误：env vars 只对 root shell 有效
run_command(sandbox, env_export_string(claude_env), user="root")
run_command(sandbox, "claude --print '...'", user="dev")  # 看不到 env vars

# 正确：env vars 内联在命令中
cmd = "export KEY=val && export KEY2=val2 && claude --print '...'"
run_command(sandbox, cmd, user="dev")
```

---

### 9. 命令拼接 bug：`&&` 插在 `claude` 和选项之间

**现象**：生成的命令是 `claude && --dangerously-skip-permissions && --print ...`，导致 Claude Code 收到空参数报错 "Input must be provided through stdin or as a prompt argument"

**原因**：`" && ".join()` 把 claude 和它的 CLI 选项也当成了独立命令

```python
# 错误
parts = ["cd /workspace", "claude", "--print", "'prompt'"]
cmd = " && ".join(parts)  # cd /workspace && claude && --print && 'prompt'

# 正确
prefix = ["cd /workspace"]
claude_parts = ["claude", "--print", "'prompt'"]
claude_cmd = " ".join(claude_parts)
cmd = " && ".join(prefix + [claude_cmd])  # cd /workspace && claude --print 'prompt'
```

---

### 10. `--print` 模式下 prompt 被 shell 误解

**现象**：prompt 中包含特殊字符（如单引号 `'`、`$`）时，shell 解析错误

**解决**：使用 `shlex.quote(prompt)` 对 prompt 进行 shell 安全转义

```python
import shlex
claude_args.extend(["--print", "--output-format", "text", shlex.quote(prompt)])
```

---

## 网络与安全

### 11. E2B SDK 与 cubesandbox SDK 的 network.rules 格式冲突

**现象**：使用 `e2b_code_interpreter.Sandbox.create(network={"rules": [...]})` 时报错：
```
AttributeError: 'list' object has no attribute 'items'
```

**原因**：E2B SDK 内部将 `network.rules` 按字典（`{host: rules}`）处理，而 CubeAPI 期望的是列表格式 `[{...}]`

**解决**：`network_policy.py` 改用 cubesandbox 原生 SDK

```python
# 错误
from e2b_code_interpreter import Sandbox  # E2B SDK 将 rules 转为 dict

# 正确
from cubesandbox import Sandbox  # cubesandbox SDK 原样传递 rules 列表
```

---

### 12. allow_internet_access 默认值与安全模式

**说明**：`network_policy.py` 默认 `allow_internet_access=False`（安全模式），沙箱无法直接访问外网，所有流量必须经过 CubeEgress。

**首次安装**：`allow_internet_access=False` 时沙箱完全无法访问外网，无法在线安装 Claude Code。此时有两种方式：
- 加 `--allow-internet` 标志临时开启互联网进行首次安装
- 使用 Dockerfile 构建预装镜像（推荐，生产环境首选）

**生产环境**：使用 Dockerfile 构建预装镜像 + 默认的 `allow_internet_access=False`，实现真正的默认拒止。

---

### 13. 凭据注入依赖 CubeEgress 拦截 TLS

**说明**：`network_policy.py` 的核心安全机制——沙箱内放占位 API Key `sk-placeholder`，CubeEgress 在 TLS 层替换为真实 Key——依赖 CubeEgress 正确拦截匹配规则的流量。

- 当 `allow_internet_access=True` 时，不匹配的流量**绕过** CubeEgress 直接出站
- 当 `allow_internet_access=False` 时，所有流量**必须**经过 CubeEgress，不匹配的直接丢弃
- 生产环境必须使用 `allow_internet_access=False` 才能实现真正的默认拒止

---

## SDK 差异

### 14. cubesandbox.Sandbox vs e2b_code_interpreter.Sandbox 行为差异

| 行为 | e2b_code_interpreter | cubesandbox |
|------|---------------------|-------------|
| 非零退出码 | 抛 `CommandExitException` | 返回 `CommandResult`（正常） |
| network.rules 格式 | 内部转为 dict（不兼容） | 原样传递 list（兼容） |
| 适用场景 | run_claude_code.py, resume_claude_code.py | network_policy.py |

**教训**：不要在这两个 SDK 之间照搬 `try/except CommandExitException` 模式

---

## 代码审查发现

### 15. Shell 注入漏洞

**位置**：`sandbox_exec.py`、`mcp_server.py`

**问题**：`python3 -c '{code}'` 中 code 未转义，含单引号时命令执行错误甚至可注入

**修复**：使用 `shlex.quote(code)` 转义

### 16. `files.read()` 返回 bytes 导致 JSON 序列化失败

**位置**：`mcp_server.py`

**问题**：`sandbox.files.read()` 返回 bytes，`json.dumps()` 无法序列化

**修复**：检测类型，bytes 时 decode 为 str

### 17. MCP stdio 使用了错误的 Content-Length 帧

**位置**：`mcp_server.py`

**问题**：原实现沿用了 LSP 的 `Content-Length` 帧，但 MCP stdio 要求每条 JSON-RPC 消息独占一行

**修复**：按换行读取和写入 JSON-RPC 消息，并在 stdin EOF 时退出

### 18. workdir 未转义

**位置**：`env_utils.py`

**问题**：`cd {workdir}` 中 workdir 含空格等特殊字符时命令执行错误

**修复**：使用 `shlex.quote(workdir)` 转义

### 19. Sandbox.create 失败时 finally 块 NameError

**位置**：`run_claude_code.py`、`network_policy.py`

**问题**：`Sandbox.create()` 抛异常时 `sandbox` 变量未赋值，finally 块访问时崩溃

**修复**：创建前初始化为 None，finally 块检查非空再清理

---

## 总结

| 类型 | 数量 |
|------|------|
| 环境/网络 | 3 |
| 沙箱/模板 | 2 |
| Claude Code 运行 | 5 |
| 网络/安全 | 3 |
| SDK 差异 | 1 |
| 代码 bug | 5 |

最关键的教训：
1. **测试环境网络受限时代码设计要考虑离线方案**（预装镜像 vs 在线安装）
2. **E2B SDK 和 cubesandbox SDK 有微妙差异**（异常处理、参数格式），不要假设一致
3. **Shell 命令拼接必须用 shlex.quote 转义**，否则有安全漏洞
4. **Claude Code 的 root 限制**必须在架构设计阶段就考虑到
