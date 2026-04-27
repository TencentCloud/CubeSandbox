"""
cube_e2b_code_interpreter
~~~~~~~~~~~~~~~~~~~~~~~~~
Drop-in replacement for e2b_code_interpreter that talks to CubeSandbox.

原版用法（e2b cloud）：
    from e2b_code_interpreter import Sandbox
    with Sandbox.create() as sb:
        execution = sb.run_code("x = 1+1; x")
        print(execution.text)  # "2"

现在用法（CubeSandbox 本地部署）：
    from cube_e2b_code_interpreter import Sandbox
    with Sandbox.create() as sb:
        execution = sb.run_code("x = 1+1; x")
        print(execution.text)  # "2"

DNS 说明：
    原版 e2b：
        get_host(49999) → "49999-<id>.e2b.app"
        DNS 由 e2b cloud 维护，*.e2b.app 泛解析到对应节点

    CubeSandbox（本地部署）：
        get_host(49999) → "49999-<id>.cube.app"
        CoreDNS 在 127.0.0.54:53 处理 *.cube.app → 9.135.79.34
        但其他机器没有这个 DNS，所以需要 CUBE_PROXY_NODE_IP 绕过

    当 CUBE_PROXY_NODE_IP=9.135.79.34 时：
        - TCP 直连 9.135.79.34:80
        - 请求头 Host: 49999-<id>.cube.app
        - CubeProxy 按 Host 路由到对应 sandbox
        - 不需要 DNS 解析 *.cube.app
"""
from .sandbox import Sandbox
from .models import Execution, Result, Logs, ExecutionError, OutputMessage, Context
from .config import SandboxConfig
from .exceptions import CubeCodeInterpreterError

__all__ = [
    "Sandbox",
    "Execution",
    "Result",
    "Logs",
    "ExecutionError",
    "OutputMessage",
    "Context",
    "SandboxConfig",
    "CubeCodeInterpreterError",
]

__version__ = "0.1.0"
