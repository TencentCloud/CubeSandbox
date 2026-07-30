# Moved into code-sandbox-quickstart

This directory is **not** a standalone example anymore.

Per review on [#1062](https://github.com/TencentCloud/CubeSandbox/pull/1062):
host-side argv tool allowlisting belongs in
[`../code-sandbox-quickstart/`](../code-sandbox-quickstart/),
next to `cmd.py` / `network_no_internet.py`.

| File | Role |
|------|------|
| [`tool_allowlist.py`](../code-sandbox-quickstart/tool_allowlist.py) | Host argv gate |
| [`tool_allowlist_limits.py`](../code-sandbox-quickstart/tool_allowlist_limits.py) | Threat model / non-goals |
| [`test_tool_allowlist.py`](../code-sandbox-quickstart/test_tool_allowlist.py) | Host-only unittest |
| [`tool_allowlist_deny.py`](../code-sandbox-quickstart/tool_allowlist_deny.py) | Deny before `Sandbox.create` |
| [`tool_allowlist_allow.py`](../code-sandbox-quickstart/tool_allowlist_allow.py) | Allow + airgap |
| [`tool_agent_loop.py`](../code-sandbox-quickstart/tool_agent_loop.py) | Toy agent propose → gate → MicroVM |

[中文说明](README_zh.md)
