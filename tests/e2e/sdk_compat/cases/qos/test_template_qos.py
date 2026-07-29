"""End-to-end tests for template QoS configuration and enforcement.

The control/runtime case runs with the normal SDK compatibility ``--run-e2e`` opt-in. Dataplane cases additionally require ``CUBE_QOS_DATAPLANE_E2E=1`` because they use infrastructure outside CubeAPI:

* bandwidth tests require an iperf3 server reachable from the sandbox; start one with ``iperf3 -s -p 5201`` and set ``CUBE_QOS_IPERF_SERVER``;
* block I/O tests require a writable virtio-blk-backed filesystem;
* PPS tests additionally require execution on the compute node so the test can read the sandbox TAP counters.

Set ``CUBE_QOS_E2E_TEMPLATE_ID`` to reuse an existing template. Otherwise the suite creates and cleans up a temporary template using ``CUBE_TEMPLATE_E2E_IMAGE``. For a temporary template, an IP-literal iperf server is automatically included in ``allow_out``. A reused template must already allow the target.

The pytest runner does not need iperf3 or fio. The commands are required inside the sandbox; an affected case is skipped when its command is absent. Set ``CUBE_QOS_E2E_INSTALL_TOOLS=1`` to install missing commands with the image's configured apt or dnf source, or provide ``CUBE_QOS_E2E_INSTALL_COMMAND``.
"""

from __future__ import annotations

import ipaddress
import json
import os
import re
import shlex
import shutil
import socket
import subprocess
import sys
import threading
import time
import uuid
from collections.abc import Iterator
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager
from dataclasses import dataclass
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[5] / "sdk" / "python"))

import pytest
import requests

from cubesandbox import Config, Sandbox, Template
from cubesandbox._config import _auth_headers
from cubesandbox._exceptions import ApiError, SandboxNotFoundError, TemplateNotFoundError

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.qos,
    pytest.mark.p2,
    pytest.mark.slow,
    pytest.mark.self_managed_template,
]


@dataclass
class QosEnvironment:
    sandbox: Sandbox
    qos: dict


def _positive_int(name: str, default: int) -> int:
    raw = os.environ.get(name)
    value = int(raw) if raw is not None else default
    if value <= 0:
        pytest.fail(f"{name} must be a positive integer")
    return value


def _positive_float(name: str, default: float) -> float:
    raw = os.environ.get(name)
    value = float(raw) if raw is not None else default
    if value <= 0:
        pytest.fail(f"{name} must be positive")
    return value


def _require_qos_dataplane() -> None:
    if os.environ.get("CUBE_QOS_DATAPLANE_E2E") != "1":
        pytest.skip("set CUBE_QOS_DATAPLANE_E2E=1 to measure live QoS enforcement")


def _qos() -> dict:
    return {
        "network": {
            "bandwidthMbps": _positive_int("CUBE_QOS_BANDWIDTH_MBPS", 100),
        },
        "blockIo": {
            "throughputMiBps": _positive_int("CUBE_QOS_BLOCK_THROUGHPUT_MIBPS", 32),
            "iops": _positive_int("CUBE_QOS_BLOCK_IOPS", 1000),
        },
    }


def _template_raw(config: Config, template_id: str) -> dict:
    response = requests.get(
        f"{config.api_url}/templates/{template_id}",
        headers=_auth_headers(config),
        timeout=30,
    )
    response.raise_for_status()
    return response.json()


def _server_allow_out(server: str | None) -> list[str] | None:
    if server is None:
        return None
    try:
        address = ipaddress.ip_address(server)
    except ValueError:
        return None
    prefix = 32 if address.version == 4 else 128
    return [f"{address}/{prefix}"]


def _wait_template_ready(config: Config, template_id: str, timeout: float = 600) -> None:
    deadline = time.monotonic() + timeout
    last_status = "not found"
    while time.monotonic() < deadline:
        try:
            detail = Template.get(template_id, config=config)
            last_status = detail.status
            if detail.status == "READY":
                return
            if detail.status == "FAILED":
                pytest.fail(f"QoS template {template_id} build failed: {detail.last_error}")
        except TemplateNotFoundError:
            pass
        time.sleep(2)
    pytest.fail(f"QoS template {template_id} did not become READY; last status: {last_status}")


def _run(sandbox: Sandbox, command: str, timeout: float) -> str:
    result = sandbox.commands.run(command, timeout=timeout)
    assert result.exit_code == 0, (
        f"sandbox command failed with exit code {result.exit_code}: {command}\n"
        f"stdout:\n{result.stdout}\nstderr:\n{result.stderr}"
    )
    return result.stdout


def _require_command(sandbox: Sandbox, command: str) -> None:
    probe = sandbox.commands.run(f"command -v {shlex.quote(command)}", timeout=30)
    if probe.exit_code == 0:
        return
    if os.environ.get("CUBE_QOS_E2E_INSTALL_TOOLS") != "1":
        pytest.skip(
            f"{command} is not installed in the sandbox image; preinstall it or set "
            "CUBE_QOS_E2E_INSTALL_TOOLS=1 to use the image's configured package source"
        )

    install = os.environ.get("CUBE_QOS_E2E_INSTALL_COMMAND")
    if install is None:
        install = (
            f"if command -v apt-get >/dev/null; then "
            f"DEBIAN_FRONTEND=noninteractive apt-get update && "
            f"DEBIAN_FRONTEND=noninteractive apt-get install -y {shlex.quote(command)}; "
            f"elif command -v dnf >/dev/null; then dnf -y install {shlex.quote(command)}; "
            f"else exit 127; fi"
        )
    _run(sandbox, install, timeout=300)
    _run(sandbox, f"command -v {shlex.quote(command)}", timeout=30)


@contextmanager
def _provision_qos_environment(
    config: Config,
    expected_qos: dict,
    *,
    template_id: str | None,
    allow_out: list[str] | None,
    name_prefix: str,
) -> Iterator[QosEnvironment]:
    created_template = False
    sandbox: Sandbox | None = None

    if template_id is None:
        image = os.environ.get("CUBE_TEMPLATE_E2E_IMAGE")
        if image is None:
            pytest.skip(
                "set CUBE_QOS_E2E_TEMPLATE_ID to reuse a QoS template, or set "
                "CUBE_TEMPLATE_E2E_IMAGE so the test can create one"
            )
        job = Template.build(
            name=f"{name_prefix}-{uuid.uuid4().hex[:8]}",
            image=image,
            writable_layer_size=os.environ.get("CUBE_TEMPLATE_E2E_WRITABLE_LAYER_SIZE", "1G"),
            exposed_ports=[49983, 49999],
            probe_port=49999,
            probe_path="/health",
            nodes=[node] if (node := os.environ.get("CUBE_TEMPLATE_E2E_NODE")) else None,
            allow_internet_access=True,
            allow_out=allow_out,
            qos=expected_qos,
            config=config,
        )
        template_id = job.template_id
        created_template = True

    try:
        _wait_template_ready(config, template_id)
        actual_qos = _template_raw(config, template_id).get("configuredQos")
        if actual_qos != expected_qos:
            pytest.fail(
                f"template {template_id} configuredQos is {actual_qos!r}; "
                f"expected {expected_qos!r}. Set the CUBE_QOS_* limits to match "
                "a reused template."
            )

        sandbox = Sandbox.create(template=template_id, timeout=600, config=config)
        detail = sandbox.get_info()
        assert detail.get("configuredQos") == expected_qos
        assert detail.get("qosApplied") is True
        _run(sandbox, "printf qos-dataplane-ready", timeout=30)
        yield QosEnvironment(sandbox=sandbox, qos=expected_qos)
    finally:
        if sandbox is not None:
            try:
                sandbox.kill()
            except (ApiError, SandboxNotFoundError, OSError):
                pass
        if created_template and template_id is not None:
            try:
                Template.delete(template_id, config=config)
            except (ApiError, TemplateNotFoundError):
                pass


def test_template_qos_control_and_runtime_state(sdk_e2e_config) -> None:
    expected_qos = {
        "network": {"bandwidthMbps": 100, "packetsPerSecond": 1000},
        "blockIo": {"throughputMiBps": 32, "iops": 1000},
    }
    config = Config(api_url=sdk_e2e_config.cube_api_url)
    with _provision_qos_environment(
        config,
        expected_qos,
        template_id=os.environ.get("CUBE_QOS_CONTROL_TEMPLATE_ID"),
        allow_out=None,
        name_prefix="e2e-qos-control",
    ):
        pass


@pytest.fixture(scope="module")
def qos_environment(sdk_e2e_config) -> Iterator[QosEnvironment]:
    _require_qos_dataplane()
    server = os.environ.get("CUBE_QOS_IPERF_SERVER")
    template_id = os.environ.get("CUBE_QOS_E2E_TEMPLATE_ID")
    config = Config(api_url=sdk_e2e_config.cube_api_url)
    with _provision_qos_environment(
        config,
        _qos(),
        template_id=template_id,
        allow_out=_server_allow_out(server),
        name_prefix="e2e-qos-dataplane",
    ) as environment:
        yield environment


@pytest.fixture(scope="module")
def iperf_server() -> tuple[str, int]:
    server = os.environ.get("CUBE_QOS_IPERF_SERVER")
    if server is None:
        pytest.skip("set CUBE_QOS_IPERF_SERVER for bandwidth tests")
    return server, _positive_int("CUBE_QOS_IPERF_PORT", 5201)


def _iperf_result(
    sandbox: Sandbox,
    server: str,
    port: int,
    *,
    reverse: bool,
) -> tuple[float, float]:
    duration = _positive_int("CUBE_QOS_IPERF_DURATION", 15)
    omit = _positive_int("CUBE_QOS_IPERF_OMIT", 3)
    streams = _positive_int("CUBE_QOS_IPERF_STREAMS", 4)
    parts = [
        "iperf3",
        "-c",
        server,
        "-p",
        str(port),
        "-t",
        str(duration),
        "-O",
        str(omit),
        "-J",
    ]
    if reverse:
        parts.append("-R")
    parts.extend(["-P", str(streams)])

    command = " ".join(shlex.quote(part) for part in parts)
    output = _run(sandbox, command, timeout=duration + omit + 30)
    payload = json.loads(output)
    if error := payload.get("error"):
        pytest.fail(f"iperf3 failed: {error}")
    # Measure receiver goodput in both directions. For uploads, sum_sent can
    # include bytes still queued in the guest TCP stack ahead of the TAP
    # limiter when the test ends.
    summary_key = "sum_received"
    summary = payload.get("end", {}).get(summary_key)
    assert summary is not None, (
        f"iperf3 JSON does not contain end.{summary_key}: {payload.get('end')!r}"
    )
    return float(summary["bits_per_second"]), float(summary["seconds"])


def _assert_near_ceiling(actual: float, ceiling: float, *, unit: str) -> None:
    lower_ratio = _positive_float("CUBE_QOS_E2E_MIN_RATIO", 0.50)
    upper_ratio = _positive_float("CUBE_QOS_E2E_MAX_RATIO", 1.30)
    assert actual >= ceiling * lower_ratio, (
        f"measured {actual:.2f} {unit}, below {lower_ratio:.2f}x the "
        f"configured ceiling {ceiling:.2f} {unit}; the test path may be bottlenecked elsewhere"
    )
    assert actual <= ceiling * upper_ratio, (
        f"measured {actual:.2f} {unit}, above {upper_ratio:.2f}x the "
        f"configured ceiling {ceiling:.2f} {unit}"
    )


@pytest.mark.parametrize("reverse", [False, True], ids=["upload", "download"])
def test_network_bandwidth_ceiling(
    qos_environment: QosEnvironment,
    iperf_server: tuple[str, int],
    reverse: bool,
) -> None:
    server, port = iperf_server
    _require_command(qos_environment.sandbox, "iperf3")
    bits_per_second, _ = _iperf_result(
        qos_environment.sandbox,
        server,
        port,
        reverse=reverse,
    )
    actual_mbps = bits_per_second / 1_000_000
    ceiling_mbps = qos_environment.qos["network"]["bandwidthMbps"]
    _assert_near_ceiling(actual_mbps, ceiling_mbps, unit="Mbit/s")


def _require_virtio_blk_path(sandbox: Sandbox, test_path: str) -> None:
    directory = str(Path(test_path).parent)
    quoted_directory = shlex.quote(directory)
    command = (
        f"set -- $(findmnt -n -o SOURCE,FSTYPE,OPTIONS -T {quoted_directory}) || exit 20; "
        'source=$1; fstype=$2; options=$3; device=""; '
        'if [ "$fstype" = overlay ]; then '
        'upperdir=$(printf "%s\\n" "$options" | tr , "\\n" | sed -n s/^upperdir=//p); '
        'case "$upperdir" in /run/blk-cube/*/*) '
        'device=${upperdir#/run/blk-cube/}; device=${device%%/*} ;; esac; '
        'elif [ "${source#/dev/}" != "$source" ]; then '
        'device=$(lsblk -s -n -o KNAME "$source" | tail -n 1); fi; '
        'test -n "$device" || exit 21; '
        'driver=$(readlink -f "/sys/class/block/$device/device/driver" || true); '
        'printf "%s\\n%s\\n%s\\n%s\\n" "$source" "$fstype" "$device" "$driver"'
    )
    result = sandbox.commands.run(command, timeout=30)
    if result.exit_code != 0 or "virtio_blk" not in result.stdout:
        pytest.skip(
            f"{directory} is not confirmed as a virtio-blk-backed filesystem: "
            f"exit={result.exit_code}, output={result.stdout.strip()!r}"
        )


def _fio_result(
    sandbox: Sandbox,
    test_path: str,
    *,
    rw: str,
    block_size: str,
    duration: int,
) -> dict:
    size = os.environ.get("CUBE_QOS_FIO_SIZE", "512M")
    command = " ".join(
        shlex.quote(part)
        for part in [
            "fio",
            "--output-format=json",
            f"--name=qos-{rw}",
            f"--filename={test_path}",
            f"--size={size}",
            f"--rw={rw}",
            f"--bs={block_size}",
            "--ioengine=libaio",
            "--direct=1",
            "--iodepth=32",
            f"--runtime={duration}",
            "--time_based",
            "--group_reporting",
        ]
    )
    output = _run(sandbox, command, timeout=duration + 60)
    json_start = output.find("{")
    assert json_start >= 0, f"fio did not produce JSON output: {output!r}"
    payload = json.loads(output[json_start:])
    jobs = payload.get("jobs") or []
    assert len(jobs) == 1, f"unexpected fio jobs payload: {jobs!r}"
    return jobs[0]


def test_block_io_throughput_and_iops(qos_environment: QosEnvironment) -> None:
    sandbox = qos_environment.sandbox
    _require_command(sandbox, "fio")
    test_path = os.environ.get("CUBE_QOS_FIO_PATH", "/tmp/cube-qos-e2e.dat")
    _require_virtio_blk_path(sandbox, test_path)
    duration = _positive_int("CUBE_QOS_FIO_DURATION", 45)

    try:
        for rw, direction in (("write", "write"), ("read", "read")):
            result = _fio_result(
                sandbox,
                test_path,
                rw=rw,
                block_size="1M",
                duration=duration,
            )
            actual_mibps = float(result[direction]["bw_bytes"]) / (1024 * 1024)
            ceiling_mibps = qos_environment.qos["blockIo"]["throughputMiBps"]
            _assert_near_ceiling(actual_mibps, ceiling_mibps, unit=f"MiB/s {direction}")

        for rw, direction in (("randwrite", "write"), ("randread", "read")):
            result = _fio_result(
                sandbox,
                test_path,
                rw=rw,
                block_size="4k",
                duration=duration,
            )
            actual_iops = float(result[direction]["iops"])
            ceiling_iops = qos_environment.qos["blockIo"]["iops"]
            _assert_near_ceiling(actual_iops, ceiling_iops, unit=f"IOPS {direction}")
    finally:
        sandbox.commands.run(f"rm -f -- {shlex.quote(test_path)}", timeout=30)


@pytest.fixture(scope="module")
def node_pps_enabled() -> None:
    if os.environ.get("CUBE_QOS_NODE_PPS_E2E") != "1":
        pytest.skip("set CUBE_QOS_NODE_PPS_E2E=1 when pytest runs on the sandbox compute node")


@pytest.fixture(scope="module")
def node_pps_environment(
    sdk_e2e_config,
    node_pps_enabled: None,
) -> Iterator[tuple[QosEnvironment, str]]:
    _require_qos_dataplane()
    server = os.environ.get("CUBE_QOS_IPERF_SERVER")
    if server is None:
        pytest.skip("set CUBE_QOS_IPERF_SERVER to the compute-node address reachable from the sandbox")
    expected_qos = {
        "network": {
            "packetsPerSecond": _positive_int("CUBE_QOS_PACKETS_PER_SECOND", 100),
        }
    }
    config = Config(api_url=sdk_e2e_config.cube_api_url)
    with _provision_qos_environment(
        config,
        expected_qos,
        template_id=os.environ.get("CUBE_QOS_PPS_TEMPLATE_ID"),
        allow_out=_server_allow_out(server),
        name_prefix="e2e-qos-pps",
    ) as environment:
        yield environment, server


@pytest.fixture(scope="module")
def node_udp_sink() -> Iterator[int]:
    port = _positive_int("CUBE_QOS_PPS_UDP_PORT", 5202)
    udp_socket = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    udp_socket.settimeout(0.2)
    try:
        udp_socket.bind(("0.0.0.0", port))
    except OSError as error:
        udp_socket.close()
        pytest.fail(f"cannot bind node UDP sink on port {port}: {error}")

    stopped = threading.Event()

    def receive() -> None:
        while not stopped.is_set():
            try:
                udp_socket.recvfrom(65535)
            except TimeoutError:
                pass
            except OSError:
                return

    thread = threading.Thread(target=receive, name="qos-pps-udp-sink", daemon=True)
    thread.start()
    try:
        yield port
    finally:
        stopped.set()
        udp_socket.close()
        thread.join(timeout=1)


def _tap_interface(sandbox_id: str) -> str:
    curl = shutil.which("curl")
    if curl is None:
        pytest.skip("node PPS e2e requires curl on the compute node")
    cubelet_url = os.environ.get("CUBE_QOS_CUBELET_HTTP_URL", "http://127.0.0.1:9998")
    if not cubelet_url.startswith(("http://", "https://")):
        pytest.fail("CUBE_QOS_CUBELET_HTTP_URL must start with http:// or https://")
    request = [
        curl,
        "-fsS",
        f"{cubelet_url.rstrip('/')}/v1/network/taps",
    ]
    deadline = time.monotonic() + _positive_float("CUBE_QOS_TAP_LOOKUP_TIMEOUT", 30.0)
    last_error = ""
    while time.monotonic() < deadline:
        try:
            response = subprocess.run(
                request,
                check=True,
                capture_output=True,
                text=True,
                timeout=5,
            )
            taps = json.loads(response.stdout).get("taps") or []
            matches = [tap for tap in taps if tap.get("sandboxID") == sandbox_id]
            if len(matches) == 1:
                interface = matches[0].get("tapName", "")
                if re.fullmatch(r"[A-Za-z0-9_.-]+", interface):
                    return interface
                last_error = f"unsafe TAP name: {interface!r}"
            else:
                last_error = f"expected one TAP for sandbox {sandbox_id}, got {matches!r}"
        except subprocess.CalledProcessError as error:
            last_error = error.stderr.strip() or f"curl exited with {error.returncode}"
        except (json.JSONDecodeError, subprocess.TimeoutExpired) as error:
            last_error = str(error)
        time.sleep(0.5)
    pytest.fail(f"Cubelet TAP lookup did not become ready: {last_error}")


def _interface_counter(interface: str, counter: str) -> int:
    path = Path("/sys/class/net") / interface / "statistics" / counter
    try:
        return int(path.read_text().strip())
    except (OSError, ValueError) as error:
        pytest.skip(f"cannot read node TAP counter {path}: {error}")


@pytest.mark.qos_node
def test_network_upload_pps_ceiling_from_tap_counter(
    node_pps_environment: tuple[QosEnvironment, str],
    node_udp_sink: int,
) -> None:
    environment, server = node_pps_environment
    duration = _positive_int("CUBE_QOS_PPS_DURATION", 15)
    warmup_seconds = _positive_float("CUBE_QOS_PPS_WARMUP_SECONDS", 2.0)
    sample_seconds = _positive_float("CUBE_QOS_PPS_SAMPLE_SECONDS", 5.0)
    activity_timeout = 5.0
    if duration <= activity_timeout + warmup_seconds + sample_seconds + 1:
        pytest.fail(
            "CUBE_QOS_PPS_DURATION must exceed activity wait, warmup, and sample time "
            "by at least one second"
        )
    payload_size = _positive_int("CUBE_QOS_PPS_PAYLOAD_BYTES", 64)
    port = node_udp_sink
    source = (
        "import json, socket, time\n"
        f"target = ({server!r}, {port})\n"
        f"payload = b'x' * {payload_size}\n"
        "sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)\n"
        "sock.setblocking(False)\n"
        "started = time.monotonic()\n"
        f"deadline = started + {duration}\n"
        "attempted = 0\n"
        "sent = 0\n"
        "while time.monotonic() < deadline:\n"
        "    attempted += 1\n"
        "    try:\n"
        "        sock.sendto(payload, target)\n"
        "        sent += 1\n"
        "    except BlockingIOError:\n"
        "        pass\n"
        "print(json.dumps({\n"
        "    'attempted': attempted,\n"
        "    'sent': sent,\n"
        "    'seconds': time.monotonic() - started,\n"
        "}))\n"
    )
    command = f"python3 -c {shlex.quote(source)}"
    _require_command(environment.sandbox, "python3")
    interface = _tap_interface(environment.sandbox.sandbox_id)
    initial = _interface_counter(interface, "rx_packets")
    with ThreadPoolExecutor(max_workers=1) as executor:
        sender_future = executor.submit(
            _run,
            environment.sandbox,
            command,
            duration + 30,
        )
        activity_deadline = time.monotonic() + activity_timeout
        while time.monotonic() < activity_deadline:
            if _interface_counter(interface, "rx_packets") > initial:
                break
            if sender_future.done():
                pytest.fail(f"PPS sender exited before producing TAP traffic: {sender_future.result()}")
            time.sleep(0.05)
        else:
            pytest.fail("PPS sender produced no TAP traffic within five seconds")

        time.sleep(warmup_seconds)
        before = _interface_counter(interface, "rx_packets")
        sample_started = time.monotonic()
        time.sleep(sample_seconds)
        after = _interface_counter(interface, "rx_packets")
        measured_seconds = time.monotonic() - sample_started
        output = sender_future.result(timeout=duration + 10)

    sender = json.loads(output)
    assert sender["sent"] > 0
    offered_pps = float(sender["attempted"]) / float(sender["seconds"])
    actual_pps = (after - before) / measured_seconds
    ceiling_pps = environment.qos["network"]["packetsPerSecond"]
    assert offered_pps >= ceiling_pps * 2, (
        f"sender offered only {offered_pps:.2f} packets/s, below 2x the "
        f"configured ceiling {ceiling_pps}"
    )
    assert actual_pps >= ceiling_pps * 0.50, (
        f"measured {actual_pps:.2f} packet operations/s, below 0.50x the "
        f"configured ceiling {ceiling_pps}; the test path may be bottlenecked elsewhere"
    )
    assert actual_pps <= ceiling_pps * 1.50, (
        f"measured {actual_pps:.2f} packet operations/s, above 1.50x the "
        f"configured ceiling {ceiling_pps}"
    )
