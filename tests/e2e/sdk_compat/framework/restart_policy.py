# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Helpers for node-local restart-policy e2e cases.

Status is read from CubeAPI ``restartStatus``. A guest crash is triggered
from the data plane with sysrq so the suite does not need host access.
"""

from __future__ import annotations

import shlex
import time
from collections.abc import Callable
from typing import Any

from framework.lifecycle import wait_until

MARKER_PATH = "/root/cube-e2e-restart-marker"
# Background the signal so the command can return before the guest dies.
CRASH_COMMAND = "sh -c 'sleep 1; echo c > /proc/sysrq-trigger' >/dev/null 2>&1 &"
# Container init is uvicorn. It handles SIGTERM and exits 0. The kernel drops
# SIGKILL sent to a pid-namespace init, so kill -9 1 does not end this guest.
EXIT_ZERO_COMMAND = "sh -c 'sleep 1; kill -TERM 1' >/dev/null 2>&1 &"
# uvicorn only handles SIGINT and SIGTERM, and both exit 0. Injecting
# exit_group(1) into the main process gives the restart manager a real non-zero
# exit on both supported guest architectures.
EXIT_ERROR_PATH = "/tmp/cube-e2e-exit1.py"
EXIT_ERROR_LAUNCH = f"python3 {EXIT_ERROR_PATH}"
EXIT_ERROR_SCRIPT = """\
import ctypes
import ctypes.util
import os
import platform

PTRACE_PEEKTEXT = 1
PTRACE_POKETEXT = 4
PTRACE_CONT = 7
PTRACE_GETREGS = 12
PTRACE_SETREGS = 13
PTRACE_ATTACH = 16
PTRACE_GETREGSET = 0x4204
PTRACE_SETREGSET = 0x4205
NT_PRSTATUS = 1

libc = ctypes.CDLL(ctypes.util.find_library("c"), use_errno=True)
libc.ptrace.restype = ctypes.c_long


def ptrace(request, pid, addr=0, data=0):
    ctypes.set_errno(0)
    result = libc.ptrace(
        request,
        pid,
        ctypes.c_void_p(addr),
        ctypes.c_void_p(data) if isinstance(data, int) else ctypes.cast(data, ctypes.c_void_p),
    )
    if result == -1 and ctypes.get_errno() != 0:
        errno = ctypes.get_errno()
        raise OSError(errno, os.strerror(errno))
    return result


class X86Regs(ctypes.Structure):
    _fields_ = [(name, ctypes.c_ulonglong) for name in (
        "r15", "r14", "r13", "r12", "rbp", "rbx", "r11", "r10",
        "r9", "r8", "rax", "rcx", "rdx", "rsi", "rdi", "orig_rax",
        "rip", "cs", "eflags", "rsp", "ss", "fs_base", "gs_base",
        "ds", "es", "fs", "gs")]


class Arm64Regs(ctypes.Structure):
    _fields_ = [
        ("regs", ctypes.c_ulonglong * 31),
        ("sp", ctypes.c_ulonglong),
        ("pc", ctypes.c_ulonglong),
        ("pstate", ctypes.c_ulonglong),
    ]


class Iovec(ctypes.Structure):
    _fields_ = [("iov_base", ctypes.c_void_p), ("iov_len", ctypes.c_size_t)]


def inject_x86_64(pid):
    regs = X86Regs()
    ptrace(PTRACE_GETREGS, pid, data=ctypes.byref(regs))
    addr = regs.rip & ~7
    ptrace(PTRACE_POKETEXT, pid, addr, 0x909090909090050F)
    regs.rip = addr
    regs.rax = 231
    regs.rdi = 1
    ptrace(PTRACE_SETREGS, pid, data=ctypes.byref(regs))


def inject_aarch64(pid):
    regs = Arm64Regs()
    iov = Iovec(ctypes.addressof(regs), ctypes.sizeof(regs))
    ptrace(PTRACE_GETREGSET, pid, NT_PRSTATUS, ctypes.byref(iov))
    addr = regs.pc & ~7
    original = ptrace(PTRACE_PEEKTEXT, pid, addr)
    word = bytearray(int(original & 0xFFFFFFFFFFFFFFFF).to_bytes(8, "little"))
    offset = regs.pc - addr
    word[offset:offset + 4] = (0xD4000001).to_bytes(4, "little")  # svc #0
    ptrace(PTRACE_POKETEXT, pid, addr, int.from_bytes(word, "little"))
    regs.regs[0] = 1
    regs.regs[8] = 94
    iov = Iovec(ctypes.addressof(regs), ctypes.sizeof(regs))
    ptrace(PTRACE_SETREGSET, pid, NT_PRSTATUS, ctypes.byref(iov))


pid = 1
machine = platform.machine().lower()
if machine in ("x86_64", "amd64"):
    inject = inject_x86_64
elif machine in ("aarch64", "arm64"):
    inject = inject_aarch64
else:
    raise RuntimeError(f"unsupported guest architecture: {machine}")

attached = False
try:
    ptrace(PTRACE_ATTACH, pid)
    attached = True
    os.waitpid(pid, 0)
    inject(pid)
    ptrace(PTRACE_CONT, pid)
    attached = False
finally:
    if attached:
        try:
            ptrace(PTRACE_CONT, pid)
        except OSError:
            pass
"""
# Stop envd without killing the container init, so the default liveness probe
# fails while the main process is still the one that was started.
STOP_PROBE_COMMAND = (
    "sh -c 'for f in /proc/[0-9]*/cmdline; do "
    "cmd=$(tr \"\\0\" \" \" < \"$f\" 2>/dev/null || true); "
    "case \"$cmd\" in *envd*) kill \"$(basename \"$(dirname \"$f\")\")\";; esac; "
    "done'"
)

_STATUS_ALIASES = {
    "restartPolicy": ("restartPolicy", "restart_policy"),
    "restartState": ("restartState", "restart_state"),
    "restartCount": ("restartCount", "restart_count"),
    "lastExitReason": ("lastExitReason", "last_exit_reason"),
    "lastExitCode": ("lastExitCode", "last_exit_code"),
    "nextRestartAt": ("nextRestartAt", "next_restart_at"),
    # restartCount is the consumed retry budget, not the number of successful
    # rebuilds (lifecycle.md: a failed attempt still spends maxRestarts). These
    # timestamps are what tell the two apart in a failure message.
    "lastRestartAt": ("lastRestartAt", "last_restart_at"),
    "lastSuccessfulRestartAt": ("lastSuccessfulRestartAt", "last_successful_restart_at"),
    "lastFailedRestartAt": ("lastFailedRestartAt", "last_failed_restart_at"),
}


def restart_status(raw: Any) -> dict[str, Any] | None:
    """Return a normalized restartStatus dict, or None when absent."""
    if not isinstance(raw, dict):
        return None
    status = raw.get("restartStatus")
    if status is None:
        status = raw.get("restart_status")
    if not isinstance(status, dict) or not status:
        return None
    normalized: dict[str, Any] = {}
    for canonical, aliases in _STATUS_ALIASES.items():
        for key in aliases:
            if key in status:
                normalized[canonical] = status[key]
                break
    return normalized


def wait_for_restart_status(
    adapter: Any,
    predicate: Callable[[dict[str, Any] | None], bool],
    *,
    timeout: float,
    interval: float = 2,
    hold: float = 0,
) -> dict[str, Any] | None:
    """Poll info() until predicate(status) is true. info() errors keep waiting.

    With ``hold`` > 0 the predicate must also stay true for that many seconds
    without a false sample, which restarts the window. A restart can queue the
    next attempt the moment a rebuild succeeds, so a status that is only
    momentarily true does not prove the sandbox settled.
    """
    last: dict[str, Any] | None = None
    stable_since: float | None = None

    def _matches() -> bool:
        nonlocal last, stable_since
        try:
            last = restart_status(adapter.info().raw)
        except Exception:
            last = None
            stable_since = None
            return False
        if not predicate(last):
            stable_since = None
            return False
        if hold <= 0:
            return True
        now = time.monotonic()
        if stable_since is None:
            stable_since = now
            return False
        return now - stable_since >= hold

    try:
        wait_until(
            _matches,
            timeout=timeout,
            interval=interval,
            description="restart status",
        )
    except AssertionError as exc:
        raise AssertionError(f"{exc}; last={last!r}") from exc
    return last


def signal_guest(
    adapter: Any,
    command: str,
    *,
    timeout: int,
    allow_signaled: bool = False,
) -> None:
    """Run a guest signal command. A dropped channel means the signal landed."""
    try:
        result = adapter.run_command(command, timeout=timeout)
    except Exception:
        return
    exit_code = getattr(result, "exit_code", 0)
    signal_landed = exit_code in (-1, 0, None) or (
        allow_signaled and isinstance(exit_code, int) and exit_code >= 128
    )
    if not signal_landed:
        raise AssertionError(
            "guest signal command failed before the signal: "
            f"exit={exit_code} stdout={getattr(result, 'stdout', '')!r} "
            f"stderr={getattr(result, 'stderr', '')!r}"
        )


def crash_guest(adapter: Any, *, timeout: int) -> None:
    """Ask the guest to panic."""
    signal_guest(adapter, CRASH_COMMAND, timeout=timeout)


def exit_main_zero(adapter: Any, *, timeout: int) -> None:
    """Ask uvicorn, the container init, to exit 0."""
    signal_guest(adapter, EXIT_ZERO_COMMAND, timeout=timeout)


def exit_main_error(adapter: Any, *, timeout: int) -> None:
    """Make the container init call exit_group(1)."""
    adapter.write_file(EXIT_ERROR_PATH, EXIT_ERROR_SCRIPT)
    signal_guest(adapter, EXIT_ERROR_LAUNCH, timeout=timeout, allow_signaled=True)


def stop_liveness_endpoint(adapter: Any, *, timeout: int) -> None:
    """Stop envd so the default probe fails. The container init keeps running."""
    try:
        result = adapter.run_command(STOP_PROBE_COMMAND, timeout=timeout)
    except Exception:
        return
    exit_code = getattr(result, "exit_code", 0)
    # Killing envd closes the command channel, which the SDK reports as -1.
    if exit_code not in (0, -1, None):
        raise AssertionError(
            "stopping the liveness endpoint failed: "
            f"exit={exit_code} stdout={getattr(result, 'stdout', '')!r} "
            f"stderr={getattr(result, 'stderr', '')!r}"
        )


def guest_ipv4(adapter: Any, *, timeout: int) -> str:
    """Return the guest address. The template has no iproute2, so read fib_trie."""
    command = (
        "python3 -c '"
        "import pathlib\n"
        "lines=pathlib.Path(\"/proc/net/fib_trie\").read_text().splitlines()\n"
        "prev=\"\"\n"
        "found=[]\n"
        "for line in lines:\n"
        "    if \"32 host LOCAL\" in line and prev and not prev.startswith(\"127.\"):\n"
        "        found.append(prev)\n"
        "    parts=line.split()\n"
        "    if parts and parts[-1].count(\".\")==3:\n"
        "        prev=parts[-1]\n"
        "picked=[a for a in found if not a.startswith(\"169.254.\")] or found\n"
        "print(picked[0] if picked else \"\")\n"
        "'"
    )
    result = adapter.run_command(command, timeout=timeout)
    exit_code = getattr(result, "exit_code", 0)
    stdout = str(getattr(result, "stdout", "")).strip()
    if exit_code not in (0, None) or not stdout:
        raise AssertionError(
            f"reading guest address failed: exit={exit_code} stdout={stdout!r} "
            f"stderr={getattr(result, 'stderr', '')!r}"
        )
    return stdout.splitlines()[-1].strip()


def write_durable_marker(adapter: Any, value: str, *, timeout: int, path: str = MARKER_PATH) -> None:
    """Write a file on the overlay upper and flush it."""
    quoted = shlex.quote(value)
    result = adapter.run_command(
        f"printf %s {quoted} > {path} && sync && cat {path}",
        timeout=timeout,
    )
    exit_code = getattr(result, "exit_code", 0)
    stdout = getattr(result, "stdout", "")
    if exit_code not in (0, None) or stdout != value:
        raise AssertionError(
            f"durable marker was not written to {path}: "
            f"exit={exit_code} stdout={stdout!r} stderr={getattr(result, 'stderr', '')!r}"
        )


def read_marker(adapter: Any, *, timeout: int, path: str = MARKER_PATH) -> str:
    result = adapter.run_command(f"cat {path}", timeout=timeout)
    exit_code = getattr(result, "exit_code", 0)
    if exit_code not in (0, None):
        raise AssertionError(
            f"reading {path} failed: exit={exit_code} "
            f"stdout={getattr(result, 'stdout', '')!r} "
            f"stderr={getattr(result, 'stderr', '')!r}"
        )
    return str(getattr(result, "stdout", ""))
