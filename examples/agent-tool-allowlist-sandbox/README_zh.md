# Agent 工具白名单沙箱

[English](README.md)

宿主机 argv 工具白名单 + 本目录 BYOI 工具集镜像，对应
[#645](https://github.com/TencentCloud/CubeSandbox/issues/645)。

**你会得到：** 在 `Sandbox.create` / `commands.run` **之前**拒绝非法工具；
放行命令在本 Dockerfile 构建的 MicroVM 中执行，并可叠加断网、CIDR
`allow_out`、pause/resume、并行多沙箱。

**这不是：** guest 内 confinement、无 bash 镜像，或 LLM Agent。
`tool_agent_loop.py` 使用固定提案。

## 适用场景

- Agent 宿主不应为 `bash` / `curl` 探测创建沙箱
- 演示「策略在宿主机、负载在沙箱」
- 最小 BYOI：`tool-profile.txt` 与默认白名单对齐
- 差异化叠层：checkpoint、受限出口、多沙箱扇出

## 前置条件

- Cube Sandbox 集群 + `cubemastercli`
- Docker
- Python 3.10+

```bash
pip install -r requirements.txt
cp .env.example .env
```

## 快速开始

### 1 — 构建并注册

```bash
docker build -t agent-tool-allowlist-sandbox:latest .

cubemastercli tpl create-from-image \
  --image <仓库或本地>/agent-tool-allowlist-sandbox:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

READY 后写入 `.env` 的 `CUBE_TEMPLATE_ID`。资源：可写层 1G。

### 2 — 运行演示

| 步骤 | 命令 | 期望 | 需集群 |
|------|------|------|--------|
| 边界 | `python tool_allowlist_limits.py` | `LIMITS_DEMO_OK` | 否 |
| 单测 | `python -m unittest test_tool_allowlist.py -v` | OK | 否 |
| 拒绝 | `python tool_allowlist_deny.py` | `denied_as_expected` | 否 |
| 模板冒烟 | `python verify_template.py` | `TEMPLATE_VERIFY_OK` | 是 |
| 放行+断网 | `python tool_allowlist_allow.py` | echo + artifact | 是 |
| 参考环 | `python tool_agent_loop.py` | `AGENT_LOOP_OK` | 是 |
| Checkpoint | `python tool_allowlist_checkpoint.py` | `CHECKPOINT_OK` | 是 |
| 出口叠层 | `python tool_allowlist_egress.py` | `EGRESS_STACK_OK` | 是 |
| 扇出 | `python tool_allowlist_fanout.py` | `FANOUT_OK` | 是 |

`ALLOWLIST_FANOUT_N` 默认 `2`（最大 `4`）。

## 原理

```
propose command
    │
    ▼
tool_allowlist.assert_allowlisted   ← 仅宿主机
    │ deny → 不调用 Sandbox.create / commands.run
    ▼ allow
Sandbox.create(本 BYOI, 网络/生命周期选项…)
    │
    ▼
sandbox.commands.run(command)
```

镜像内 `tool-profile.txt` 表示意图；强制执行仍在宿主机。

## 目录

```
├── Dockerfile
├── tool_allowlist.py
├── tool_allowlist_limits.py / deny.py / allow.py
├── tool_allowlist_checkpoint.py
├── tool_allowlist_egress.py
├── tool_allowlist_fanout.py
├── tool_agent_loop.py
├── test_tool_allowlist.py
├── verify_template.py
├── env_utils.py
├── requirements.txt
└── .env.example
```

## 限制

- 基础镜像仍有 shell；profile ≠ confinement。
- 白名单含 `cat` 时，`cat /etc/passwd` 仍过本门控。
- `echo … > file` 仍可写 guest（文档化残差）。
- 扩白名单需 `extra_binaries` + `allow_unsafe_allowlist_extension=True`。
- `enable_code_execution=True` 是显式解释器提权。
- 默认不 apt 装 curl。
- Fan-out 会创建真实 VM，共享集群请保持小 `N`。
