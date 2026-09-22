# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import pytest

from framework.assertions import assert_code_ok, assert_command_ok
from framework.capabilities import PAUSE_RESUME, RUN_CODE
from framework.lifecycle import (
    wait_until,
    wait_until_data_plane_ready,
    wait_until_paused,
    wait_until_running,
)

_BLOCK_IO_DIR = "/root/sdk-compat-pause-block-io"
_BLOCK_IO_SCRIPT = f"{_BLOCK_IO_DIR}/workload.py"
_BLOCK_IO_READY = f"{_BLOCK_IO_DIR}/ready"
_BLOCK_IO_ACKNOWLEDGED = f"{_BLOCK_IO_DIR}/acknowledged"
_BLOCK_IO_STOP = f"{_BLOCK_IO_DIR}/stop"
_BLOCK_IO_DONE = f"{_BLOCK_IO_DIR}/done"
_BLOCK_IO_PID = f"{_BLOCK_IO_DIR}/workload.pid"
_BLOCK_IO_LOG = f"{_BLOCK_IO_DIR}/workload.log"
_BLOCK_IO_RECORD_SIZE = 4096
_BLOCK_IO_WORKLOAD = f'''\
import os
import sys
import time
from pathlib import Path

DIRECTORY = Path("{_BLOCK_IO_DIR}")
DATA = DIRECTORY / "durable-records.bin"
READY = DIRECTORY / "ready"
ACKNOWLEDGED = DIRECTORY / "acknowledged"
ACKNOWLEDGED_TMP = DIRECTORY / "acknowledged.tmp"
STOP = DIRECTORY / "stop"
DONE = DIRECTORY / "done"
RECORD_SIZE = {_BLOCK_IO_RECORD_SIZE}
MAX_RECORDS = 4096
WRITE_INTERVAL = 0.01


def record(sequence):
    return sequence.to_bytes(8, "big") + bytes([sequence % 251]) * (RECORD_SIZE - 8)


def verify(minimum_records):
    with DATA.open("rb", buffering=0) as source:
        os.posix_fadvise(source.fileno(), 0, 0, os.POSIX_FADV_DONTNEED)
        size = os.fstat(source.fileno()).st_size
        if size == 0 or size % RECORD_SIZE:
            raise SystemExit(f"invalid durable file size: {{size}}")
        records = size // RECORD_SIZE
        if records < minimum_records:
            raise SystemExit(
                f"durable file contains {{records}} records, expected at least {{minimum_records}}"
            )
        for sequence in range(records):
            actual = source.read(RECORD_SIZE)
            expected = record(sequence)
            if actual != expected:
                raise SystemExit(f"durable record {{sequence}} is corrupt")
    print(records)


def write_forever():
    DIRECTORY.mkdir(parents=True, exist_ok=True)
    directory_fd = os.open(DIRECTORY, os.O_RDONLY | os.O_DIRECTORY)
    try:
        with DATA.open("wb", buffering=0) as output:
            sequence = 0
            while not STOP.exists():
                if sequence >= MAX_RECORDS:
                    break
                expected = record(sequence)
                written = 0
                while written < len(expected):
                    written += output.write(expected[written:])
                os.fdatasync(output.fileno())
                ACKNOWLEDGED_TMP.write_text(f"{{sequence + 1}}\\n", encoding="ascii")
                with ACKNOWLEDGED_TMP.open("rb", buffering=0) as marker:
                    os.fsync(marker.fileno())
                ACKNOWLEDGED_TMP.replace(ACKNOWLEDGED)
                if sequence == 0:
                    READY.write_text("ready\\n", encoding="ascii")
                    with READY.open("rb", buffering=0) as marker:
                        os.fsync(marker.fileno())
                os.fsync(directory_fd)
                sequence += 1
                time.sleep(WRITE_INTERVAL)
        DONE.write_text("done\\n", encoding="ascii")
        with DONE.open("rb", buffering=0) as marker:
            os.fsync(marker.fileno())
        os.fsync(directory_fd)
    finally:
        os.close(directory_fd)


if sys.argv[1:2] == ["--verify"]:
    verify(int(sys.argv[2]))
elif sys.argv[1:]:
    raise SystemExit("usage: workload.py [--verify MINIMUM_RECORDS]")
else:
    write_forever()
'''

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.lifecycle,
    pytest.mark.p1,
    pytest.mark.requires_capability(PAUSE_RESUME),
]


def _pause_and_resume(sdk_sandbox, sdk_e2e_config):
    sdk_sandbox.pause(timeout=sdk_e2e_config.default_timeout)
    wait_until_paused(sdk_sandbox, timeout=sdk_e2e_config.default_timeout)
    resumed = sdk_sandbox.resume_or_connect(timeout=sdk_e2e_config.default_timeout)
    wait_until_running(resumed, timeout=sdk_e2e_config.default_timeout)
    wait_until_data_plane_ready(
        resumed,
        timeout=sdk_e2e_config.default_timeout,
        command_timeout=sdk_e2e_config.command_timeout,
    )
    return resumed


def test_pause_sets_state_paused(sdk_sandbox, sdk_e2e_config):
    sdk_sandbox.pause(timeout=sdk_e2e_config.default_timeout)
    state = wait_until_paused(sdk_sandbox, timeout=sdk_e2e_config.default_timeout)
    assert state == "paused"


def test_pause_and_connect_resume_preserves_files(sdk_sandbox, sdk_e2e_config):
    sdk_sandbox.write_file("/tmp/sdk-compat-pause.txt", "before-pause")

    resumed = _pause_and_resume(sdk_sandbox, sdk_e2e_config)
    try:
        assert resumed.sandbox_id == sdk_sandbox.sandbox_id
        assert resumed.read_file("/tmp/sdk-compat-pause.txt") == "before-pause"
    finally:
        resumed.close()


def test_pause_resume_flushes_active_block_io(sdk_sandbox, sdk_e2e_config):
    """Pause with writes in flight and verify every acknowledged record after resume."""
    setup = sdk_sandbox.run_command(
        f"mkdir -p {_BLOCK_IO_DIR}",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(setup)
    sdk_sandbox.write_file(_BLOCK_IO_SCRIPT, _BLOCK_IO_WORKLOAD)
    start = sdk_sandbox.run_command(
        f"rm -f {_BLOCK_IO_READY} {_BLOCK_IO_ACKNOWLEDGED} "
        f"{_BLOCK_IO_ACKNOWLEDGED}.tmp {_BLOCK_IO_STOP} {_BLOCK_IO_DONE}; "
        f"nohup python3 {_BLOCK_IO_SCRIPT} >{_BLOCK_IO_LOG} 2>&1 </dev/null & "
        f"printf '%s\\n' $! >{_BLOCK_IO_PID}",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(start)

    def _workload_ready() -> bool:
        result = sdk_sandbox.run_command(
            f"test -s {_BLOCK_IO_READY} && kill -0 $(cat {_BLOCK_IO_PID})",
            timeout=sdk_e2e_config.command_timeout,
        )
        return result.exit_code == 0

    resumed = None
    try:
        wait_until(
            _workload_ready,
            timeout=sdk_e2e_config.default_timeout,
            description="active block-I/O workload to durably commit its first record",
        )

        high_water = sdk_sandbox.run_command(
            f"cat {_BLOCK_IO_ACKNOWLEDGED}",
            timeout=sdk_e2e_config.command_timeout,
        )
        assert_command_ok(high_water)
        acknowledged_before_pause = int(high_water.stdout.strip())
        assert acknowledged_before_pause >= 1

        # Do not quiesce the guest workload: pause must drain and flush virtio-blk
        # while the process is actively appending individually durable records.
        resumed = _pause_and_resume(sdk_sandbox, sdk_e2e_config)
        mark_stop = resumed.run_command(
            f"touch {_BLOCK_IO_STOP}",
            timeout=sdk_e2e_config.command_timeout,
        )
        assert_command_ok(mark_stop)

        def _workload_stopped_cleanly() -> bool:
            result = resumed.run_command(
                f"test -s {_BLOCK_IO_DONE}",
                timeout=sdk_e2e_config.command_timeout,
            )
            return result.exit_code == 0

        wait_until(
            _workload_stopped_cleanly,
            timeout=sdk_e2e_config.default_timeout,
            description="block-I/O workload to stop cleanly",
        )

        restored_high_water = resumed.run_command(
            f"cat {_BLOCK_IO_ACKNOWLEDGED}",
            timeout=sdk_e2e_config.command_timeout,
        )
        assert_command_ok(restored_high_water)
        acknowledged_after_resume = int(restored_high_water.stdout.strip())
        assert acknowledged_after_resume >= acknowledged_before_pause

        verify = resumed.run_command(
            f"python3 {_BLOCK_IO_SCRIPT} --verify {acknowledged_after_resume}",
            timeout=sdk_e2e_config.command_timeout,
        )
        assert_command_ok(verify)
        assert int(verify.stdout.strip()) >= acknowledged_after_resume
    finally:
        cleanup_adapter = resumed or sdk_sandbox
        try:
            cleanup_adapter.run_command(
                f"touch {_BLOCK_IO_STOP}; "
                f"if test ! -s {_BLOCK_IO_DONE} && test -s {_BLOCK_IO_PID}; then "
                f"kill $(cat {_BLOCK_IO_PID}) 2>/dev/null || true; fi",
                timeout=sdk_e2e_config.command_timeout,
            )
        except Exception:  # noqa: BLE001 - fixture teardown still kills the sandbox
            pass
        finally:
            if resumed is not None:
                resumed.close()


def test_pause_and_connect_resume_allows_commands(sdk_sandbox, sdk_e2e_config):
    resumed = _pause_and_resume(sdk_sandbox, sdk_e2e_config)
    try:
        result = resumed.run_command(
            "printf resumed",
            timeout=sdk_e2e_config.command_timeout,
        )
        assert_command_ok(result)
        assert result.stdout == "resumed"
    finally:
        resumed.close()


@pytest.mark.sandbox_create_options(env_vars={"SDK_COMPAT_PAUSE_ENV": "pause-env"})
def test_pause_and_connect_resume_preserves_env_vars(sdk_sandbox, sdk_e2e_config):
    resumed = _pause_and_resume(sdk_sandbox, sdk_e2e_config)
    try:
        result = resumed.run_command(
            'printf "%s" "$SDK_COMPAT_PAUSE_ENV"',
            timeout=sdk_e2e_config.command_timeout,
        )
        assert_command_ok(result)
        assert result.stdout == "pause-env"
    finally:
        resumed.close()


@pytest.mark.requires_capability(RUN_CODE)
@pytest.mark.requires_code_interpreter
def test_pause_and_connect_resume_preserves_run_code_state(sdk_sandbox, sdk_e2e_config):
    first = sdk_sandbox.run_code(
        "sdk_compat_pause_value = 84",
        timeout=sdk_e2e_config.run_code_timeout,
    )
    assert_code_ok(first)

    resumed = _pause_and_resume(sdk_sandbox, sdk_e2e_config)
    try:
        second = resumed.run_code(
            "sdk_compat_pause_value + 1",
            timeout=sdk_e2e_config.run_code_timeout,
        )
        assert_code_ok(second)
        assert second.text == "85"
    finally:
        resumed.close()
