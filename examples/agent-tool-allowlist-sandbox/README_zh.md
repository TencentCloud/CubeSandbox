# Agent 工具白名单沙箱

[English](README.md)

宿主机 argv 工具白名单 + 本目录 BYOI 工具集镜像，对应
[#645](https://github.com/TencentCloud/CubeSandbox/issues/645)。

**你会得到：** 在 `Sandbox.create` / `commands.run` **之前**拒绝非法工具；
放行的命令在本 Dockerfile 构建的 MicroVM 里执行，并叠加
`allow_internet_access=False`。

**这不是：** guest 内 confinement、无 bash 镜像，或 LLM Agent。
`tool_agent_loop.py` 使用固定提案列表。

## 适用场景

- Agent 宿主不应为 `bash` / `curl` 探测去创建沙箱
- 演示「策略在宿主机、负载在沙箱」的常见分工
- 最小 BYOI：镜像内 `tool-profile.txt` 与默认白名单对齐

## 前置条件

- 可用的 Cube Sandbox 集群 + `cubemastercli`
- Docker（构建镜像）
- Python 3.10+

```bash
pip install -r requirements.txt
cp .env.example .env
```

## 快速开始

### 1 — 构建镜像

```bash
docker build -t agent-tool-allowlist-sandbox:latest .

# 可选：tool_agent_loop 断网轮需要 guest curl 时
# docker build --build-arg INSTALL_CURL=1 -t agent-tool-allowlist-sandbox:latest .
```

本地 envd 探活：

```bash
docker run --rm -d --name agent-tool-box \
  -p 49983:49983 agent-tool-allowlist-sandbox:latest
curl -s -o /dev/null -w "envd /health => %{http_code}\n" http://127.0.0.1:49983/health
docker rm -f agent-tool-box
```

### 2 — 注册模板

推送到节点可拉取的仓库后：

```bash
cubemastercli tpl create-from-image \
  --image <仓库或本地>/agent-tool-allowlist-sandbox:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

READY 后把 template id 写入 `.env` 的 `CUBE_TEMPLATE_ID`。资源：可写层 1G 足够。

### 3 — 纯宿主机检查（不连集群）

```bash
python tool_allowlist_limits.py
python -m unittest test_tool_allowlist.py -v
python tool_allowlist_deny.py
```

### 4 — 集群演示

```bash
python verify_template.py          # TEMPLATE_VERIFY_OK
python tool_allowlist_allow.py     # 白名单 echo + 断网产物
python tool_agent_loop.py          # AGENT_LOOP_OK（固定提案）
```

## 原理

```
propose command
    │
    ▼
tool_allowlist.assert_allowlisted   ← 仅宿主机
    │ deny → 不调用 Sandbox.create / commands.run
    ▼ allow
Sandbox.create(本 BYOI, allow_internet_access=False)
    │
    ▼
sandbox.commands.run(command)
```

镜像内 `/etc/cube-sandbox/tool-profile.txt` 列出默认工具名；真正门控仍在宿主机，
文件表示意图，不是 guest 内强制执行。

## 目录

```
├── Dockerfile                 # cubesandbox-base + tool-profile
├── tool_allowlist.py          # 宿主机 argv 门控
├── tool_allowlist_limits.py   # 威胁模型（纯宿主机）
├── tool_allowlist_deny.py     # 拒绝路径
├── tool_allowlist_allow.py    # 放行 + 断网
├── tool_agent_loop.py         # 参考环（非 LLM）
├── test_tool_allowlist.py     # 单测
├── verify_template.py         # 本镜像集群冒烟
├── env_utils.py
├── requirements.txt
└── .env.example
```

## 限制

- 基础镜像仍有 shell；profile ≠ confinement。
- 白名单含 `cat` 时，`cat /etc/passwd` 仍过本门控——依赖 MicroVM 与最小权限。
- `echo … > file` 仍可写 guest（文档化残差）。
- 扩白名单需同时传 `extra_binaries` 与 `allow_unsafe_allowlist_extension=True`。
- `enable_code_execution=True` 是显式解释器提权。
- 默认不 apt 装 curl；基础镜像可能仍带 curl。
- `create-from-image` 需要节点能拉取的镜像引用。
