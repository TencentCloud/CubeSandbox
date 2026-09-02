# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Node-local cubecli log helpers for single-node envd init-log checks.

``cubecli logs`` reads ``/data/cubelet/log`` on the host, then the legacy
bundle path inside the Cubelet mount namespace. That only works when the
pytest process is on the same host as Cubelet, so these helpers refuse to
run on a multi-node cluster (skip) and skip when cubecli or the Cubelet
socket is missing.

Sandbox checks GET envd ``/health`` from inside the guest so the access
log writes a new line to init stdout. Template-build checks read envd's
own startup line via ``cubecli logs --tpl``.
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

# envd access-log needles. Some images log GET /health or Process/Start;
# the current code image prints uvicorn's bind line. trigger_envd also
# writes cube-e2e-log-<hex> to init stdout so cubecli can see a unique line.
ENVD_PROCESS_RPC = "process.Process/Start"
ENVD_E2E_MARKER_PREFIX = "cube-e2e-log-"
ENVD_ACCESS_MARKERS = (
    ENVD_PROCESS_RPC,
    "/health",
    "Uvicorn running",
    ENVD_E2E_MARKER_PREFIX,
)
ENVD_HEALTH_PORT = 49983
ENVD_HEALTH_PATH = "/health"
GUEST_ENVD_HEALTH_CMD = (
    "python3 -c "
    "\"import urllib.request; "
    f"urllib.request.urlopen('http://127.0.0.1:{ENVD_HEALTH_PORT}{ENVD_HEALTH_PATH}')\""
)


def guest_envd_probe_cmd(path: str) -> str:
    if not path.startswith("/"):
        path = f"/{path}"
    return (
        "python3 -c "
        "\"import urllib.request,urllib.error\n"
        f"req=urllib.request.Request('http://127.0.0.1:{ENVD_HEALTH_PORT}{path}')\n"
        "try:\n"
        "    urllib.request.urlopen(req)\n"
        "except urllib.error.HTTPError:\n"
        "    pass\""
    )


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


def trigger_envd(adapter, *, timeout: int) -> str:
    """GET envd /health and write a unique marker to init stdout.

    Resume recreates host log files; /health alone may not flush a new
    access-log line, so the marker is also written to ``/proc/1/fd/1``.
    """
    from framework.assertions import assert_command_ok

    marker = f"cube-e2e-log-{os.urandom(6).hex()}"
    assert_command_ok(adapter.run_command(GUEST_ENVD_HEALTH_CMD, timeout=timeout))
    assert_command_ok(adapter.run_command(guest_envd_probe_cmd(f"/{marker}"), timeout=timeout))
    assert_command_ok(
        adapter.run_command(f"printf '%s\\n' '{marker}' > /proc/1/fd/1", timeout=timeout)
    )
    return marker


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


def count_envd_access(logs: str) -> int:
    return sum(logs.count(marker) for marker in ENVD_ACCESS_MARKERS)


def contains_envd_access_log(logs: str) -> bool:
    return any(marker in logs for marker in ENVD_ACCESS_MARKERS)


def host_stdout_size(sandbox_id: str) -> int:
    path = host_log_paths(sandbox_id)[0]
    try:
        return path.stat().st_size
    except FileNotFoundError:
        return 0


def wait_for_host_log_growth(
    sandbox_id: str,
    *,
    before_size: int,
    timeout: float,
    interval: float = 0.5,
) -> int:
    from framework.lifecycle import wait_until

    last = before_size

    def _grew() -> bool:
        nonlocal last
        last = host_stdout_size(sandbox_id)
        return last > before_size

    try:
        wait_until(
            _grew,
            timeout=timeout,
            interval=interval,
            description=(
                f"host stdout log to grow past {before_size} bytes "
                f"for {sandbox_id}"
            ),
        )
    except AssertionError as exc:
        raise AssertionError(
            f"{exc}; stdout_size={last} before={before_size}"
        ) from exc
    return last


def wait_for_host_log_contains(
    sandbox_id: str,
    needle: str,
    *,
    timeout: float,
    interval: float = 0.5,
) -> str:
    from framework.lifecycle import wait_until

    last = ""
    path = host_log_paths(sandbox_id)[0]

    def _has() -> bool:
        nonlocal last
        try:
            last = path.read_text(errors="replace")
        except FileNotFoundError:
            last = ""
        return needle in last

    try:
        wait_until(
            _has,
            timeout=timeout,
            interval=interval,
            description=f"host stdout to contain {needle!r} for {sandbox_id}",
        )
    except AssertionError as exc:
        raise AssertionError(f"{exc}; stdout={last!r}") from exc
    return last


def wait_for_envd_rpc(
    cubecli: str,
    sandbox_id: str,
    *,
    address: str,
    timeout: float,
    min_count: int = 1,
    interval: float = 0.5,
    needles: tuple[str, ...] | None = None,
) -> str:
    from framework.lifecycle import wait_until

    last = ""
    required = needles or ENVD_ACCESS_MARKERS

    def _seen() -> bool:
        nonlocal last
        last = read_cubecli_logs_both(cubecli, sandbox_id, address=address)
        if needles:
            return all(needle in last for needle in needles)
        return count_envd_access(last) >= min_count

    try:
        wait_until(
            _seen,
            timeout=timeout,
            interval=interval,
            description=(
                "cubecli logs to contain envd access output "
                f"({', '.join(required)})"
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
