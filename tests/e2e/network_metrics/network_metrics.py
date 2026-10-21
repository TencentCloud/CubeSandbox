#!/usr/bin/env python3
"""CubeVS network metrics E2E for a deployed CubeSandbox node."""

from __future__ import annotations

import argparse
import json
import os
import re
import shlex
import sys
import time
from collections.abc import Callable
from pathlib import Path

import requests


def add_repo_sdk_to_path() -> Path:
    configured = os.environ.get("CUBE_REPO_ROOT")
    if configured:
        root = Path(configured).expanduser().resolve()
        sys.path.insert(0, str(root / "sdk" / "python"))
        return root
    for parent in Path(__file__).resolve().parents:
        if (parent / "sdk" / "python" / "cubesandbox").is_dir():
            sys.path.insert(0, str(parent / "sdk" / "python"))
            return parent
    raise RuntimeError("cannot locate repository sdk/python")


add_repo_sdk_to_path()

from cubesandbox import ApiError, Config, Sandbox  # noqa: E402

EGRESS_PACKETS = "cube_cubebox_network_sandbox_egress_packets_total"
EGRESS_BYTES = "cube_cubebox_network_sandbox_egress_bytes_total"
INGRESS_PACKETS = "cube_cubebox_network_sandbox_ingress_packets_total"
INGRESS_BYTES = "cube_cubebox_network_sandbox_ingress_bytes_total"
SANDBOX_METRICS = (
    INGRESS_PACKETS,
    INGRESS_BYTES,
    "cube_cubebox_network_sandbox_ingress_drop_packets_total",
    "cube_cubebox_network_sandbox_ingress_drop_bytes_total",
    EGRESS_PACKETS,
    EGRESS_BYTES,
    "cube_cubebox_network_sandbox_egress_drop_packets_total",
    "cube_cubebox_network_sandbox_egress_drop_bytes_total",
    "cube_cubebox_network_sandbox_snat_alloc_failures_total",
)
SNAT_SESSIONS = "cube_cubebox_network_snat_sessions_in_use"
SNAT_CAPACITY = "cube_cubebox_network_snat_ports_capacity"
SNAT_UNUSED = "cube_cubebox_network_snat_ports_unused"
SNAT_METRICS = (SNAT_SESSIONS, SNAT_CAPACITY, SNAT_UNUSED)
TRAFFIC_STATUS = "/tmp/network-metrics-e2e-traffic.status"
METRIC_RE = re.compile(
    r'^(cube_cubebox_network_[^{]+)\{sandbox_ip="([^"]+)"\}\s+(\S+)$'
)
SNAT_METRIC_RE = re.compile(
    r'^(cube_cubebox_network_[^{]+)\{snat_ip="([^"]+)"\}\s+(\S+)$'
)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--template", default=os.environ.get("CUBE_TEMPLATE_ID"))
    parser.add_argument("--node", default=os.environ.get("CUBE_E2E_NODE"))
    parser.add_argument(
        "--metrics-url",
        default=os.environ.get(
            "CUBE_NETWORK_METRICS_URL",
            "http://127.0.0.1:9998/v1/metrics/network",
        ),
    )
    parser.add_argument(
        "--target-url",
        default=os.environ.get("CUBE_NETWORK_METRICS_TARGET", "https://example.com/"),
    )
    parser.add_argument("--requests", type=int, default=4)
    parser.add_argument("--traffic-delay", type=float, default=5.0)
    parser.add_argument("--poll-timeout", type=float, default=60.0)
    parser.add_argument("--delete-timeout", type=float, default=30.0)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--keep-resources", action="store_true")
    args = parser.parse_args()
    if not args.template:
        parser.error("set --template or CUBE_TEMPLATE_ID")
    if not args.node:
        parser.error(
            "set --node or CUBE_E2E_NODE to pin the sandbox to the scraped Cubelet"
        )
    if args.requests < 1:
        parser.error("--requests must be positive")
    if args.traffic_delay <= 0:
        parser.error("--traffic-delay must be positive")
    return args


def log(message: str) -> None:
    print(f"[network-metrics-e2e] {message}", file=sys.stderr, flush=True)


def wait_for(
    description: str,
    timeout: float,
    probe: Callable[[], object | None],
    interval: float = 1.0,
) -> object:
    deadline = time.monotonic() + timeout
    last_error: Exception | None = None
    while time.monotonic() < deadline:
        try:
            value = probe()
            if value is not None:
                return value
        except Exception as exc:  # noqa: BLE001 - report the last transient error
            last_error = exc
        time.sleep(interval)
    suffix = f": {last_error}" if last_error else ""
    raise TimeoutError(f"timed out waiting for {description}{suffix}")


def scrape(metrics_url: str) -> tuple[str, dict[str, dict[str, float]]]:
    response = requests.get(metrics_url, timeout=10)
    response.raise_for_status()
    metrics: dict[str, dict[str, float]] = {}
    for line in response.text.splitlines():
        match = METRIC_RE.match(line)
        if not match:
            continue
        name, sandbox_ip, raw_value = match.groups()
        metrics.setdefault(sandbox_ip, {})[name] = float(raw_value)
    return response.text, metrics


def parse_snat_metrics(body: str) -> dict[str, dict[str, float]]:
    metrics: dict[str, dict[str, float]] = {}
    for line in body.splitlines():
        match = SNAT_METRIC_RE.match(line)
        if not match:
            continue
        name, snat_ip, raw_value = match.groups()
        metrics.setdefault(snat_ip, {})[name] = float(raw_value)
    return metrics


def source_healthy(body: str, source: str) -> bool:
    return f'cube_cubebox_network_exporter_up{{source="{source}"}} 1' in body


def snat_metrics_valid(metrics: dict[str, dict[str, float]]) -> bool:
    if not metrics:
        return False
    for sample in metrics.values():
        if not all(name in sample for name in SNAT_METRICS):
            return False
        if sample[SNAT_SESSIONS] < 0 or sample[SNAT_CAPACITY] <= 0:
            return False
        if not 0 <= sample[SNAT_UNUSED] <= sample[SNAT_CAPACITY]:
            return False
    return True


def start_delayed_traffic(
    sandbox: Sandbox,
    target_url: str,
    requests_count: int,
    delay: float,
) -> None:
    code = (
        "import pathlib,time,urllib.request\n"
        f"status=pathlib.Path({TRAFFIC_STATUS!r})\n"
        "try:\n"
        f" time.sleep({delay!r})\n"
        f" url={target_url!r}\n"
        f" count={requests_count}\n"
        " total=0\n"
        " for _ in range(count):\n"
        "  with urllib.request.urlopen(url, timeout=15) as response:\n"
        "   total += len(response.read(65536))\n"
        " status.write_text(f'ok {total}\\n')\n"
        "except BaseException as exc:\n"
        " status.write_text(f'error {exc!r}\\n')\n"
        " raise\n"
    )
    command = (
        f"rm -f {shlex.quote(TRAFFIC_STATUS)}; "
        f"nohup python3 -c {shlex.quote(code)} "
        ">/tmp/network-metrics-e2e-traffic.log 2>&1 </dev/null &"
    )
    result = sandbox.commands.run(
        command,
        timeout=30,
    )
    if result.exit_code != 0:
        raise RuntimeError(
            f"traffic launch failed with exit code {result.exit_code}: {result.stderr}"
        )


def wait_for_data_plane(sandbox_id: str, config: Config, timeout: float) -> Sandbox:
    def ready() -> Sandbox | None:
        candidate = Sandbox.connect(sandbox_id, config=config)
        try:
            result = candidate.commands.run("true", timeout=15)
        except ApiError as exc:
            if exc.status_code in {502, 503, 504}:
                return None
            raise
        return candidate if result.exit_code == 0 else None

    value = wait_for("CubeProxy/envd readiness", timeout, ready, interval=2.0)
    assert isinstance(value, Sandbox)
    return value


def counters_increased(
    before: dict[str, float],
    after: dict[str, float],
) -> bool:
    names = (EGRESS_PACKETS, EGRESS_BYTES, INGRESS_PACKETS, INGRESS_BYTES)
    return all(name in after and after[name] > before.get(name, 0) for name in names)


def main() -> int:
    args = parse_args()
    config = Config()
    sandbox: Sandbox | None = None
    result: dict[str, object] = {
        "metrics_url": args.metrics_url,
        "target_url": args.target_url,
        "samples": {},
    }

    try:

        def healthy_snapshot() -> (
            tuple[
                dict[str, dict[str, float]],
                dict[str, dict[str, float]],
            ]
            | None
        ):
            body, sandbox_metrics = scrape(args.metrics_url)
            snat_metrics = parse_snat_metrics(body)
            if not source_healthy(body, "sandbox") or not source_healthy(body, "snat"):
                return None
            if not snat_metrics_valid(snat_metrics):
                return None
            return sandbox_metrics, snat_metrics

        healthy = wait_for(
            "healthy sandbox and SNAT metric sources",
            args.poll_timeout,
            healthy_snapshot,
        )
        assert isinstance(healthy, tuple)
        baseline, snat_metrics = healthy
        result["samples"]["snat"] = snat_metrics
        baseline_ips = set(baseline)

        log(f"creating sandbox from template {args.template}")
        sandbox = Sandbox.create(
            template=args.template,
            timeout=900,
            allow_internet_access=True,
            distribution_scope=[args.node],
            config=config,
        )
        result["sandbox_id"] = sandbox.sandbox_id
        sandbox = wait_for_data_plane(sandbox.sandbox_id, config, args.poll_timeout)

        def discover() -> tuple[str, dict[str, float]] | None:
            _, current = scrape(args.metrics_url)
            candidates = sorted(set(current) - baseline_ips)
            if len(candidates) != 1:
                return None
            sandbox_ip = candidates[0]
            sample = current[sandbox_ip]
            return (
                (sandbox_ip, sample)
                if all(name in sample for name in SANDBOX_METRICS)
                else None
            )

        discovered = wait_for(
            "new sandbox network metric series",
            args.poll_timeout,
            discover,
        )
        assert isinstance(discovered, tuple)
        sandbox_ip, initial = discovered
        result["sandbox_ip"] = sandbox_ip
        result["samples"]["initial"] = initial

        log(f"scheduling delayed target traffic for sandbox IP {sandbox_ip}")
        start_delayed_traffic(
            sandbox,
            args.target_url,
            args.requests,
            args.traffic_delay,
        )
        _, current = scrape(args.metrics_url)
        traffic_baseline = current.get(sandbox_ip)
        if traffic_baseline is None:
            raise AssertionError("sandbox metric series disappeared before traffic")
        result["samples"]["before_target_traffic"] = traffic_baseline

        def increased() -> dict[str, float] | None:
            _, current = scrape(args.metrics_url)
            sample = current.get(sandbox_ip)
            return (
                sample
                if sample and counters_increased(traffic_baseline, sample)
                else None
            )

        after = wait_for("network counters to increase", args.poll_timeout, increased)
        assert isinstance(after, dict)
        result["samples"]["after_traffic"] = after

        def traffic_completed() -> str | None:
            status = sandbox.commands.run(
                f"cat {shlex.quote(TRAFFIC_STATUS)}",
                timeout=15,
            )
            if status.exit_code != 0:
                return None
            value = status.stdout.strip()
            if value.startswith("error "):
                raise RuntimeError(f"target traffic failed: {value}")
            return value if value.startswith("ok ") else None

        result["target_traffic"] = wait_for(
            "target traffic completion",
            args.poll_timeout,
            traffic_completed,
        )

        log("destroying sandbox and waiting for metric series cleanup")
        sandbox.kill()
        sandbox = None

        def removed() -> bool | None:
            body, current = scrape(args.metrics_url)
            if not source_healthy(body, "sandbox"):
                return None
            return True if sandbox_ip not in current else None

        wait_for("network metric series cleanup", args.delete_timeout, removed)
        result["cleanup"] = "complete"
        return 0
    finally:
        if sandbox is not None and not args.keep_resources:
            try:
                sandbox.kill()
            except Exception as exc:  # noqa: BLE001 - best-effort cleanup
                result.setdefault("cleanup_errors", []).append(str(exc))
        encoded = json.dumps(result, indent=2, sort_keys=True)
        print(encoded)
        if args.output:
            args.output.parent.mkdir(parents=True, exist_ok=True)
            args.output.write_text(encoded + "\n", encoding="utf-8")


if __name__ == "__main__":
    raise SystemExit(main())
