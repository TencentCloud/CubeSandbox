# 踩坑记录 — Claude Code + CubeSandbox hook 集成

本文档记录在开发和使用 **PreToolUse hook**(把 Claude Code 的 Bash 命令透明转发进
CubeSandbox MicroVM)过程中的常见问题与解决方案。

---

## hook 与安装

### 1. Bash 命令仍在宿主机执行

**现象**:装了 hook,但命令看起来还是跑在宿主机上。

**原因**:hook 未注册成功,或 Claude Code 未重启加载。

**排查**:
- 确认 `~/.claude/settings.json` 的 `hooks.PreToolUse` 里有 `Bash` matcher 指向
  `~/.claude/hooks/cubesandbox_rewrite.py`
- 重启 Claude Code
- 手动验证 hook:
  ```bash
  echo '{"tool_name":"Bash","cwd":"/tmp","session_id":"t","tool_input":{"command":"whoami"}}' \
    | python3 ~/.claude/hooks/cubesandbox_rewrite.py
  ```
  应输出把 `command` 改写为 `cubesandbox_exec.py ... -- whoami` 的 JSON。

---

### 2. `the cubesandbox SDK is required` / `python-dotenv is required`

**原因**:hook 依赖未安装到运行 Claude Code 的那个 Python 环境。

**解决**:`pip install -r requirements.txt`(需要 `cubesandbox` 和 `python-dotenv`)。

---

### 3. `CUBE_TEMPLATE_ID is not set`

**原因**:`.env` 未设置模板 ID,或安装时未把它复制进 hook 配置。

**解决**:在 `.env` 里设置 `CUBE_TEMPLATE_ID`(见 `cubemastercli tpl list`),重新运行
`hooks/install.sh`。安装脚本只复制白名单内的 `CUBE_*` 值。

---

## 沙箱与模板

### 4. 模板快照 CPU 特性不兼容(CpuidCheckCompatibility)

**现象**:
```
CubeMaster returned error code -1: failed to run container:
Error checking cpu feature compatibility: CpuidCheckCompatibility
```

**原因**:服务器重启后 CPU 微码或内核参数变化,导致之前创建的快照无法在新环境恢复。

**解决**:删除旧模板并重建。

```bash
sudo cubemastercli tpl delete --template-id <旧模板ID>
sudo cubemastercli tpl create-from-image \
  --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 2G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
```

**教训**:模板快照与 CPU 特性绑定,系统升级/重启后可能需要重建。

---

### 5. `Template not found`

**原因**:`CUBE_TEMPLATE_ID` 写错,或模板已被删除。

**解决**:用 `cubemastercli tpl list` 核对 ID。

---

## 宿主项目挂载

### 6. 挂载被拒,命令回退到无挂载沙箱

**现象**:stderr 出现
`warning: read-only host mount ... was rejected; creating a sandbox without the mount`。

**原因**:项目路径不在 CubeMaster 的 `allowed_host_mount_prefixes` 内,或 `hostPath`
在被调度的 Cubelet 上不存在。

**解决**:
- 在 CubeMaster `extra_conf.allowed_host_mount_prefixes` 加入项目路径前缀;
- 确认 Claude Code 与 Cubelet 同机,或项目已在每个可调度 Cubelet 上以相同绝对路径存在;
- 若不需要共享文件视图,接受无挂载回退即可(Bash 仍隔离)。

**注意**:挂载是**只读**的。需要沙箱写回宿主的场景,请用本示例之外的显式同步方案。

---

## 会话与状态

### 7. 某会话的沙箱状态想重来

**解决**:重置该会话绑定的沙箱,下次调用会新建。

```bash
python3 ~/.claude/hooks/cubesandbox_exec.py --reset --session <session-id>
```

会话到沙箱的映射存于 `~/.cache/cubesandbox-hook/`(可用 `CUBE_HOOK_STATE_DIR` 覆盖),
并由每会话文件锁保护并发调用。

---

### 8. npm/pip 等安装类命令超时

**现象**:沙箱内 `npm install` / `pip install` 报 `context deadline exceeded`。

**原因**:单条命令的默认执行超时不足(跨国网络尤甚)。

**解决**:调大 `CUBE_EXEC_TIMEOUT`(单条命令超时,秒),必要时也调大
`CUBE_SANDBOX_TIMEOUT`(沙箱存活 TTL)。

---

## 安全设计要点

- **fail-closed**:hook 无法安全改写时以非零退出**阻断**命令,绝不放行到宿主机。
- **防注入**:原命令作为单个 `shlex` 引用参数传入执行器,shell 元字符和换行无法越出。
- **无条件改写**:每条 Bash 调用都会被改写;已包裹的执行器调用若再次经过 hook,只会在沙箱内失败(沙箱内不存在宿主 hook 路径),绝不会落到宿主机。
- **自动批准**:hook 对改写后的 Bash 调用返回 `permissionDecision: "allow"`,Claude Code 的逐命令确认提示被抑制;请相应使用 `--permission-mode` / hooks 策略。
- **凭据不外泄**:安装脚本只复制白名单 `CUBE_*` 值,不会把 provider API key 写进 hook 配置。
