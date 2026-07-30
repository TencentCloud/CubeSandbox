# 已并入 code-sandbox-quickstart

本目录**不再**作为独立示例。

根据 [#1062](https://github.com/TencentCloud/CubeSandbox/pull/1062) 审稿意见，
宿主机 argv 工具白名单应放在
[`../code-sandbox-quickstart/`](../code-sandbox-quickstart/)，
与 `cmd.py` / `network_no_internet.py` 同级。

| 文件 | 作用 |
|------|------|
| [`tool_allowlist.py`](../code-sandbox-quickstart/tool_allowlist.py) | 宿主机 argv 门控 |
| [`tool_allowlist_limits.py`](../code-sandbox-quickstart/tool_allowlist_limits.py) | 威胁模型 / 非目标 |
| [`test_tool_allowlist.py`](../code-sandbox-quickstart/test_tool_allowlist.py) | 纯宿主机单测 |
| [`tool_allowlist_deny.py`](../code-sandbox-quickstart/tool_allowlist_deny.py) | 拒绝（不创建沙箱） |
| [`tool_allowlist_allow.py`](../code-sandbox-quickstart/tool_allowlist_allow.py) | 放行 + 断网 |
| [`tool_agent_loop.py`](../code-sandbox-quickstart/tool_agent_loop.py) | 迷你 Agent 调度环 |

[English](README.md)
