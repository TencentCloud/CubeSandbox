# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Node-local cubecli log helpers for single-node envd init-log checks.

``cubecli logs`` reads ``/data/cubelet/log`` on the host, then the legacy
bundle path inside the Cubelet mount namespace. That only works when the
pytest process is on the same host as Cubelet, so these helpers refuse to
run on a multi-node cluster (skip) and skip when cubecli or the Cubelet
socket is missing.

Sandbox checks trigger envd through the SDK process API
(``commands.run`` → ``/process.Process/Start``) and then look for that RPC
in the captured envd access log. Template-build checks read envd's own
startup line via ``cubecli logs --tpl``.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
from pathlib import Path
from typing import Any

import pytest

DEFAULT_CUBECLI = "/usr/local/services/cubetoolbox/Cubelet/bin/cubecli"
DEFAULT_CUBELET_ADDRESS = "/data/cubelet/cubelet.sock"
HOST_LOG_ROOT = "/data/cubelet/log"
TEMPLATE_LOG_ROOT = "/data/log/template"

CLUSTER_SKIP_REASON = (
    "cubecli init-log checks are single-node only; skipping on a multi-node cluster"
)
UNKNOWN_TOPOLOGY_SKIP_REASON = (
    "could not determine the Cubelet node count; "
    "refusing to guess this is a single-node deployment"
)
CUBECLI_SKIP_REASON = (
    "cubecli is not available on this host; set CUBECLI to the cubecli binary"
)
SOCKET_SKIP_REASON = (
    "Cubelet address is not present on this host; "
    "set CUBELET_ADDRESS when cubecli lives next to Cubelet"
)

# envd / code images print this on the template-build console.
TEMPLATE_ENVD_MARKER = "envd started"

# SDK commands.run hits envd's Connect RPC; some envd builds echo that path,
# others only log HTTP probes such as GET /health on the same access log.
ENVD_PROCESS_RPC = "process.Process/Start"
ENVD_ACCESS_MARKERS = (ENVD_PROCESS_RPC, "/health")


def resolve_cubecli(env: dict[str, str] | None = None) -> str | None:
    values = os.environ if env is None else env
    explicit = (values.get("CUBECLI") or "").strip()
    if explicit:
        return explicit if _is_executable(explicit) else None
    if _is_executable(DEFAULT_CUBECLI):
        return DEFAULT_CUBECLI
    return shutil.which("cubecli")


def resolve_cubelet_address(env: dict[str, str] | None = None) -> str:
    values = os.environ if env is None else env
    return (values.get("CUBELET_ADDRESS") or DEFAULT_CUBELET_ADDRESS).strip()


def parse_ops_node_count(payload: object) -> int | None:
    if isinstance(payload, list):
        return len(payload)
    if isinstance(payload, dict):
        for key in ("data", "nodes", "items"):
            value = payload.get(key)
            if isinstance(value, list):
                return len(value)
    return None


def parse_master_nodes_scanned(text: str) -> int | None:
    for line in text.splitlines():
        parts = line.split()
        if len(parts) >= 2 and parts[0] == "NODES_SCANNED" and "/" in parts[1]:
            total = parts[1].split("/", 1)[1]
            if total.isdigit():
                return int(total)
    return None


def discover_node_count(
    *,
    env: dict[str, str] | None = None,
    runner: Any = subprocess.run,
) -> int | None:
    """Return the registered Cubelet count, or None when it cannot be proven."""
    values = os.environ if env is None else env
    ops = (values.get("CUBEOPSCLI") or "").strip() or shutil.which("cubeopscli")
    if ops:
        try:
            proc = runner(
                [ops, "node", "list", "--json"],
                capture_output=True,
                text=True,
                timeout=15,
                check=False,
            )
        except (OSError, subprocess.TimeoutExpired):
            proc = None
        if proc is not None and proc.returncode == 0 and proc.stdout.strip():
            try:
                count = parse_ops_node_count(json.loads(proc.stdout))
            except json.JSONDecodeError:
                count = None
            if count is not None:
                return count

    master = (values.get("CUBEMASTERCLI") or "").strip() or shutil.which(
        "cubemastercli"
    )
    if not master:
        master = shutil.which("cubemaster")
    if not master:
        return None
    try:
        proc = runner(
            [master, "list", "--all", "--size", "1"],
            capture_output=True,
            text=True,
            timeout=15,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired):
        return None
    if proc.returncode != 0:
        return None
    return parse_master_nodes_scanned(proc.stdout)


def cubecli_logs_argv(
    cubecli: str,
    target_id: str,
    *,
    address: str | None = None,
    template: bool = False,
    stream: str = "stdout",
    all_lines: bool = True,
) -> list[str]:
    cmd = [cubecli]
    if address and not template:
        cmd.extend(["--address", address])
    cmd.append("logs")
    if template:
        cmd.append("--tpl")
    if stream == "stderr":
        cmd.append("--stderr")
    if all_lines:
        cmd.append("--all")
    cmd.append(target_id)
    return cmd


def skip_unless_single_node_cubecli(
    *,
    env: dict[str, str] | None = None,
    runner: Any = subprocess.run,
) -> tuple[str, str]:
    """Return ``(cubecli, address)`` or skip this test.

    Cluster (node count > 1) and unknown topology skip rather than fail, so
    the shared sdk_compat suite stays green on multi-node runners.
    """
    count = discover_node_count(env=env, runner=runner)
    if count is None:
        pytest.skip(UNKNOWN_TOPOLOGY_SKIP_REASON)
    if count > 1:
        pytest.skip(CLUSTER_SKIP_REASON)

    cubecli = resolve_cubecli(env)
    if cubecli is None:
        pytest.skip(CUBECLI_SKIP_REASON)

    address = resolve_cubelet_address(env)
    if not os.path.exists(address):
        pytest.skip(SOCKET_SKIP_REASON)
    return cubecli, address


def read_cubecli_logs(
    cubecli: str,
    target_id: str,
    *,
    address: str | None = None,
    template: bool = False,
    stream: str = "stdout",
    timeout: float = 30,
) -> str:
    argv = cubecli_logs_argv(
        cubecli,
        target_id,
        address=address,
        template=template,
        stream=stream,
        all_lines=True,
    )
    proc = subprocess.run(
        argv,
        capture_output=True,
        text=True,
        timeout=timeout,
        check=False,
    )
    if proc.returncode != 0:
        raise AssertionError(
            f"cubecli logs failed rc={proc.returncode} argv={argv!r} "
            f"stdout={proc.stdout!r} stderr={proc.stderr!r}"
        )
    return proc.stdout


def trigger_envd(adapter, *, timeout: int) -> None:
    """Hit envd so it prints an access log for ``process.Process/Start``."""
    from framework.assertions import assert_command_ok

    result = adapter.run_command("true", timeout=timeout)
    assert_command_ok(result)


def read_cubecli_logs_both(
    cubecli: str,
    target_id: str,
    *,
    address: str | None = None,
    template: bool = False,
    timeout: float = 30,
) -> str:
    stdout = read_cubecli_logs(
        cubecli,
        target_id,
        address=address,
        template=template,
        stream="stdout",
        timeout=timeout,
    )
    stderr = read_cubecli_logs(
        cubecli,
        target_id,
        address=address,
        template=template,
        stream="stderr",
        timeout=timeout,
    )
    return stdout + "\n" + stderr


def count_envd_rpc(logs: str, needle: str = ENVD_PROCESS_RPC) -> int:
    return logs.count(needle)


def contains_envd_access_log(logs: str) -> bool:
    return any(marker in logs for marker in ENVD_ACCESS_MARKERS)


def wait_for_envd_rpc(
    cubecli: str,
    sandbox_id: str,
    *,
    address: str,
    timeout: float,
    min_count: int = 1,
    interval: float = 0.5,
) -> str:
    from framework.lifecycle import wait_until

    last = ""

    def _seen() -> bool:
        nonlocal last
        last = read_cubecli_logs_both(cubecli, sandbox_id, address=address)
        if min_count <= 1:
            return contains_envd_access_log(last)
        return count_envd_rpc(last) >= min_count

    try:
        wait_until(
            _seen,
            timeout=timeout,
            interval=interval,
            description=(
                "cubecli logs to contain envd access output "
                f"({', '.join(ENVD_ACCESS_MARKERS)})"
            ),
        )
    except AssertionError as exc:
        raise AssertionError(f"{exc}; last cubecli logs={last!r}") from exc
    return last


def host_log_dir(sandbox_id: str) -> Path:
    return Path(HOST_LOG_ROOT) / sandbox_id


def host_log_paths(sandbox_id: str) -> tuple[Path, Path]:
    directory = host_log_dir(sandbox_id)
    return directory / "stdout", directory / "stderr"


def assert_host_logs_present(sandbox_id: str) -> None:
    stdout, stderr = host_log_paths(sandbox_id)
    assert stdout.is_file(), f"missing host stdout log {stdout}"
    assert stderr.is_file(), f"missing host stderr log {stderr}"


def wait_host_logs_absent(sandbox_id: str, *, timeout: float = 30) -> None:
    from framework.lifecycle import wait_until

    directory = host_log_dir(sandbox_id)
    wait_until(
        lambda: not directory.exists(),
        timeout=timeout,
        interval=0.5,
        description=f"host log dir {directory} to be removed",
    )


def template_log_dir(template_id: str) -> Path:
    return Path(TEMPLATE_LOG_ROOT) / f"{template_id}_0"


def _is_executable(path: str) -> bool:
    return os.path.isfile(path) and os.access(path, os.X_OK)
