# Agent 工具白名单沙箱

[English](README.md)

宿主机 argv 工具白名单 + **带真实 guest 能力**的 BYOI 镜像（`cube-tool`），对应
[#645](https://github.com/TencentCloud/CubeSandbox/issues/645)。

**你会得到**
- 宿主机在 `Sandbox.create` / `commands.run` 前拒绝非法工具
- 镜像安装 `/usr/local/bin/cube-tool`、`tool-profile.txt`、`/workspace`——guest
  会再校验工具名（不是只放一个文本文件）
- 可叠加断网、CIDR `allow_out`、pause/resume、多沙箱扇出

**这不是：** 内核级 jail、无 bash 基础镜像，或 LLM Agent。

## 快速开始

```bash
pip install -r requirements.txt
cp .env.example .env

docker build -t agent-tool-allowlist-sandbox:latest .
docker run --rm agent-tool-allowlist-sandbox:latest cube-tool echo build-ok

cubemastercli tpl create-from-image \
  --image <仓库或本地>/agent-tool-allowlist-sandbox:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

`--probe 49983 --probe-path /health` 指向基础镜像自带的 **envd**（继承
entrypoint）；本 Dockerfile 只 `EXPOSE` 该端口，不另起健康检查服务。

把 READY 模板 id 写入 `.env` 的 `CUBE_TEMPLATE_ID`。

| 步骤 | 命令 | 期望 |
|------|------|------|
| 边界/单测/拒绝 | `tool_allowlist_limits.py` / unittest / `deny` | OK |
| 模板冒烟 | `verify_template.py` | `TEMPLATE_VERIFY_OK` |
| Guest runner | `tool_allowlist_guest_runner.py` | `GUEST_RUNNER_OK` |
| allow / loop / checkpoint / egress / fanout | 见英文 README 表 | 对应 `*_OK` |

## 原理

推荐路径：宿主白名单放行 `cube-tool` → 镜像内 `cube-tool` 对照
`/etc/cube-sandbox/tool-profile.txt` 再 `exec`。裸 `echo`/`cat` 仍可过宿主门控
（演示用）；生产更应只放行 `cube-tool`。

## 限制

- 基础镜像仍有 shell；绕过 `cube-tool` 直接调 bash/路径二进制，不在 guest wrapper 范围内。
- 宿主白名单含裸 `cat` 时，`cat /etc/passwd` 仍过宿主门控。
- 文档化残差（本门控不是 shell）：`echo … > file`（guest 写）、`cat < /etc/passwd`
  （输入重定向）、以及 `*` / `?` 通配（可能由 guest shell 先展开）——此处不按串联元字符拦截。
- 扩白名单需 `extra_binaries` + `allow_unsafe_allowlist_extension=True`。
- Fan-out 会创建真实 VM，共享集群请保持小 `N`。
