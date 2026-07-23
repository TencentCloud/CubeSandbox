# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""OpenClaw (QClaw) 示例脚本的共享沙箱命令工具函数。

OpenClaw 以网关守护进程方式运行（端口 18789）。这些工具函数管理
网关生命周期：启动、就绪检查、HTTP 交互和清理。
"""

from __future__ import annotations

import json
import sys
import time
from collections.abc import Callable
from typing import Any

GATEWAY_PORT = 18789
GATEWAY_READY_SCRIPT = """python3 - <<'PY'
import json, os, socket, sys
try:
    token = json.load(open("/root/.openclaw/openclaw.json")).get("gateway", {}).get("auth", {}).get("token", "")
    port = int(os.environ.get("OPENCLAW_PORT", "18789"))
    if not token:
        sys.exit(1)
    s = socket.create_connection(("127.0.0.1", port), timeout=0.5)
    s.close()
except Exception:
    sys.exit(1)
PY"""


def stream_writer(stream) -> Callable[[object], None]:
    def write(chunk: object) -> None:
        text = getattr(chunk, "line", chunk)
        stream.write(str(text))
        stream.flush()

    return write


def run_command(
    sandbox: Any,
    command: str,
    *,
    cwd: str | None = None,
    envs: dict[str, str] | None = None,
    timeout: int | float | None = None,
    stream: bool = False,
    user: str = "root",
):
    kwargs = {"cwd": cwd, "timeout": timeout, "user": user}
    kwargs = {key: value for key, value in kwargs.items() if value is not None}
    if envs:
        kwargs["envs"] = envs
    if stream:
        kwargs["on_stdout"] = stream_writer(sys.stdout)
        kwargs["on_stderr"] = stream_writer(sys.stderr)

    try:
        return sandbox.commands.run(command, **kwargs)
    except TypeError as exc:
        if "envs" not in kwargs or "envs" not in str(exc):
            raise
        kwargs["env"] = kwargs.pop("envs")
        return sandbox.commands.run(command, **kwargs)


def ensure_success(result, action: str) -> None:
    exit_code = getattr(result, "exit_code", None)
    if exit_code not in (None, 0):
        stdout = getattr(result, "stdout", "")
        stderr = getattr(result, "stderr", "")
        raise SystemExit(
            f"Failed to {action} (exit {exit_code}).\nSTDOUT:\n{stdout}\nSTDERR:\n{stderr}"
        )


def sandbox_identifier(sandbox: Any) -> str:
    return getattr(sandbox, "sandbox_id", getattr(sandbox, "id", "unknown"))


def start_gateway(sandbox: Any, timeout: int = 60) -> None:
    """在沙箱内启动 OpenClaw 网关。"""
    # 优先使用 supervisorctl，失败则直接 nohup 启动。
    start_cmd = (
        "if command -v supervisorctl >/dev/null 2>&1; then "
        "  supervisorctl start openclaw || supervisorctl restart openclaw; "
        "else "
        "  pkill -f 'openclaw gateway' 2>/dev/null || true; "
        "  mkdir -p /var/log; "
        "  nohup openclaw gateway run >/var/log/openclaw.log 2>&1 & "
        "fi"
    )
    result = run_command(sandbox, start_cmd, timeout=timeout)
    ensure_success(result, "start OpenClaw gateway")


def wait_gateway_ready(sandbox: Any, max_wait: int = 30) -> bool:
    """轮询等待 OpenClaw 网关在 18789 端口就绪。"""
    for _ in range(max_wait):
        result = run_command(
            sandbox,
            f"OPENCLAW_PORT={GATEWAY_PORT} {GATEWAY_READY_SCRIPT}",
            timeout=10,
        )
        if getattr(result, "exit_code", 1) == 0:
            return True
        time.sleep(0.5)
    return False


def get_gateway_token(sandbox: Any) -> str:
    """从沙箱内的配置文件读取网关认证令牌。"""
    read_cmd = (
        "python3 -c \"import json; "
        "print(json.load(open('/root/.openclaw/openclaw.json'))"
        ".get('gateway',{}).get('auth',{}).get('token',''))\""
    )
    result = run_command(sandbox, read_cmd, timeout=10)
    ensure_success(result, "read gateway token")
    return getattr(result, "stdout", "").strip()


def send_prompt_via_gateway(
    sandbox: Any,
    prompt: str,
    token: str,
    timeout: int = 900,
) -> Any:
    """通过 REST API 向 OpenClaw 网关发送提示词并收集响应。

    将 prompt 序列化为 JSON 后通过 base64 写入临时文件，
    彻底避免用户输入中的特殊字符被当作 shell 命令执行。
    """
    import base64

    payload = json.dumps({"prompt": prompt, "stream": False}, ensure_ascii=False)
    payload_b64 = base64.b64encode(payload.encode()).decode()
    header = f"Authorization: Bearer {token}"
    header_b64 = base64.b64encode(header.encode()).decode()
    write_files_cmd = (
        f"echo {payload_b64} | base64 -d > /tmp/qclaw_payload.json && "
        f"echo {header_b64} | base64 -d > /tmp/qclaw_header.txt"
    )
    run_command(sandbox, write_files_cmd, timeout=10)
    send_cmd = (
        f"curl -s -S -m {timeout} "
        f"-H 'Content-Type: application/json' "
        f"-H @/tmp/qclaw_header.txt "
        f"http://127.0.0.1:{GATEWAY_PORT}/v1/chat "
        f"-d @/tmp/qclaw_payload.json"
    )
    return run_command(sandbox, send_cmd, timeout=timeout + 30, stream=True)


def gateway_status(sandbox: Any) -> str:
    """获取 OpenClaw 网关进程状态。"""
    cmd = (
        "if command -v supervisorctl >/dev/null 2>&1; then "
        "  supervisorctl status openclaw || true; "
        "else "
        "  ps -ef | grep -E '[o]penclaw|node .*openclaw' || true; "
        "fi"
    )
    result = run_command(sandbox, cmd, timeout=10)
    return getattr(result, "stdout", "")
