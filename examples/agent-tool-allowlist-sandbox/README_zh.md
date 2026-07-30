# Agent 工具白名单 — BYOI 模板 + 宿主机门控

本目录提供可构建的 Cube **模板**（Dockerfile），并与
[`../code-sandbox-quickstart/`](../code-sandbox-quickstart/)
中的宿主机 argv **工具白名单**演示配合使用。

| 层级 | 位置 | 作用 |
|------|------|------|
| Guest 意图 | `Dockerfile` → `/etc/cube-sandbox/tool-profile.txt` | 声明预期工具集 |
| 宿主机策略 | `../code-sandbox-quickstart/tool_allowlist.py` | `Sandbox.create` 前的权威 argv 门控 |
| 平台能力 | `allow_internet_access=False` | 出口与 argv 白名单正交 |

这不是完整 Agent 框架，也不是 LLM 循环；quickstart 的
`tool_agent_loop.py` 仅为写死 propose 的参考环。

## 适用场景

- Agent 宿主在创建 MicroVM 前拒绝非法工具
- 分层演示：宿主机门控 + guest 工具清单 + 断网
- 讲清 argv 策略 ≠ guest confinement

## 资源建议

- 可写层 **1G** 足够
- 演示用短 `timeout`（60s）
- 无需 GPU / 大型依赖

## 构建

```bash
docker build -t agent-tool-allowlist-sandbox:latest .
# push 到 Cubelet 可拉取的仓库
```

## 注册模板

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/agent-tool-allowlist-sandbox:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

模板 **READY** 后填写 `CUBE_TEMPLATE_ID`。

## 配置与运行

```bash
pip install -r requirements.txt
cp .env.example .env   # 填写 E2B_API_URL / E2B_API_KEY / CUBE_TEMPLATE_ID

python verify_template.py
# → host_deny bash，再读 tool-profile + echo，TEMPLATE_VERIFY_OK
```

纯宿主机门控 / 威胁模型 / 单测（不依赖本镜像）：

```bash
cd ../code-sandbox-quickstart
python tool_allowlist_limits.py
python -m unittest test_tool_allowlist.py -v
python tool_allowlist_deny.py
```

对着已注册模板（同一套环境变量）：

```bash
cd ../code-sandbox-quickstart
python tool_allowlist_allow.py
python tool_agent_loop.py   # 参考环，非 LLM Agent
```

## 已知限制

- 基础镜像仍含 shell；本 Dockerfile **不能**证明无 bash 的 confinement，宿主机白名单仍必需。
- 镜像内安装了 `curl`，便于 quickstart 中临时放行后的断网探测；默认宿主机门控仍拒绝 `curl`。
- 白名单含 `echo` 时，`echo … > file` 可写 guest 任意路径——超出 argv 门控范围（见 quickstart 威胁模型）。
- 不能替代 `sandbox-code` 类数据科学 / 解释器负载。

[English](README.md)
