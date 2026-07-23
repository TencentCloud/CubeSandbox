#!/usr/bin/env python3
"""Reusable CubeAPI/CubeProxy resource-metrics lifecycle E2E.

The script uses the repository Python SDK for sandbox lifecycle and CubeProxy
data-plane commands. It never creates a sandbox through Cubelet directly. It
validates the cache-only Prometheus endpoint for one sandbox and emits a JSON
result suitable for CI logs or review evidence.
"""

from __future__ import annotations

import argparse
import json
import os
import shlex
import sys
import time
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from typing import Callable

import requests


def add_repo_sdk_to_path() -> Path:
    configured = os.environ.get("CUBE_REPO_ROOT")
    if configured:
        root = Path(configured).expanduser().resolve()
        sdk = root / "sdk" / "python"
        if not (sdk / "cubesandbox").is_dir():
            raise RuntimeError(f"CUBE_REPO_ROOT has no sdk/python/cubesandbox: {root}")
        sys.path.insert(0, str(sdk))
        return root

    current = Path(__file__).resolve()
    for parent in current.parents:
        sdk = parent / "sdk" / "python"
        if (sdk / "cubesandbox").is_dir():
            sys.path.insert(0, str(sdk))
            return parent
    raise RuntimeError("cannot locate repository sdk/python from script path")


REPO_ROOT = add_repo_sdk_to_path()

from cubesandbox import ApiError, Config, Sandbox, Template  # noqa: E402


GUEST_CPU = "cubesandbox_guest_workload_cpu_usage_seconds_total"
GUEST_MEMORY = "cubesandbox_guest_workload_memory_current_bytes"
GUEST_EPOCH = "cubesandbox_guest_workload_metrics_epoch"
HOST_CPU = "cubesandbox_host_sandbox_cpu_usage_seconds_total"


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--template", default=os.environ.get("CUBE_TEMPLATE_ID"))
    parser.add_argument("--image", default=os.environ.get("CUBE_E2E_IMAGE"))
    parser.add_argument("--node", default=os.environ.get("CUBE_E2E_NODE"))
    parser.add_argument("--template-cpu-millicores", type=int, default=500)
    parser.add_argument("--template-memory-mib", type=int, default=512)
    parser.add_argument("--template-build-timeout", type=float, default=900.0)
    parser.add_argument(
        "--metrics-url",
        default=os.environ.get(
            "CUBE_RESOURCE_METRICS_URL",
            "http://127.0.0.1:9998/v1/metrics/resource",
        ),
    )
    parser.add_argument(
        "--mode",
        choices=("full", "capability-unavailable"),
        default="full",
        help="full lifecycle validation or old/incompatible guest fail-closed validation",
    )
    parser.add_argument("--cpu-seconds", type=float, default=2.0)
    parser.add_argument("--load-memory-mib", type=int, default=64)
    parser.add_argument("--memory-hold-seconds", type=float, default=8.0)
    parser.add_argument("--poll-timeout", type=float, default=60.0)
    parser.add_argument("--delete-timeout", type=float, default=30.0)
    parser.add_argument("--output", type=Path)
    parser.add_argument(
        "--keep-resources",
        action="store_true",
        help="leave the sandbox and snapshot for debugging instead of cleanup",
    )
    args = parser.parse_args()
    if bool(args.template) == bool(args.image):
        parser.error("set exactly one of --template/CUBE_TEMPLATE_ID or --image/CUBE_E2E_IMAGE")
    if args.load_memory_mib <= 0 or args.cpu_seconds <= 0:
        parser.error("CPU duration and memory size must be positive")
    return args


def log(message: str) -> None:
    print(f"[resource-metrics-e2e] {message}", file=sys.stderr, flush=True)


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
        except Exception as exc:  # noqa: BLE001 - preserve the last probe error
            last_error = exc
        time.sleep(interval)
    suffix = f": {last_error}" if last_error else ""
    raise TimeoutError(f"timed out waiting for {description}{suffix}")


def scrape(metrics_url: str, sandbox_id: str) -> dict[str, float]:
    response = requests.get(metrics_url, timeout=10)
    response.raise_for_status()
    values: dict[str, float] = {}
    label = f'sandbox_id="{sandbox_id}"'
    for line in response.text.splitlines():
        if not line or line.startswith("#") or label not in line:
            continue
        metric, raw_value = line.rsplit(None, 1)
        name = metric.split("{", 1)[0]
        values[name] = float(raw_value)
    return values


def snapshot(metrics_url: str, sandbox_id: str, *, require_guest: bool) -> dict[str, float] | None:
    values = scrape(metrics_url, sandbox_id)
    if HOST_CPU not in values:
        return None
    guest_present = GUEST_CPU in values and GUEST_MEMORY in values and GUEST_EPOCH in values
    if require_guest and not guest_present:
        return None
    if not require_guest and guest_present:
        return None
    return values


def no_series(metrics_url: str, sandbox_id: str) -> bool | None:
    return True if not scrape(metrics_url, sandbox_id) else None


def run_workload(sandbox: Sandbox, code: str, *, timeout: float = 60.0) -> None:
    result = sandbox.commands.run(
        f"python3 -c {shlex.quote(code)}",
        timeout=timeout,
    )
    if result.exit_code != 0:
        raise RuntimeError(
            f"guest execution failed with exit code {result.exit_code}: {result.stderr}"
        )


def wait_for_data_plane(
    sandbox_id: str,
    config: Config,
    timeout: float,
) -> Sandbox:
    retryable_statuses = {502, 503, 504}

    def ready() -> Sandbox | None:
        candidate = Sandbox.connect(sandbox_id, config=config)
        try:
            run_workload(candidate, "print('resource-metrics-e2e-ready')", timeout=15)
        except ApiError as exc:
            if exc.status_code in retryable_statuses:
                return None
            raise
        return candidate

    connected = wait_for(
        "CubeProxy/envd data-plane readiness",
        timeout,
        ready,
        interval=2.0,
    )
    assert isinstance(connected, Sandbox)
    return connected


def build_template(args: argparse.Namespace, config: Config) -> str:
    name = f"resource-metrics-e2e-{int(time.time())}"
    log(f"building template {name} from {args.image}")
    build = Template.build(
        name=name,
        image=args.image,
        cpu_count=args.template_cpu_millicores,
        memory_mb=args.template_memory_mib,
        writable_layer_size="1G",
        network_type="tap",
        nodes=[args.node] if args.node else None,
        config=config,
    )
    template_id = build.template_id
    if not template_id or not build.build_id:
        raise RuntimeError(f"template build response is incomplete: {build}")

    terminal_failure = {"ERROR", "FAILED", "CANCELED", "CANCELLED"}

    def ready() -> str | None:
        current = Template.get_build_status(
            template_id,
            build.build_id,
            config=config,
        )
        status = current.status.upper()
        if status in terminal_failure:
            raise RuntimeError(
                f"template build failed: status={current.status} "
                f"phase={current.phase} error={current.error_message or current.message}"
            )
        if status in {"READY", "SUCCESS", "SUCCEEDED"}:
            return template_id
        return None

    return str(
        wait_for(
            "template build",
            args.template_build_timeout,
            ready,
            interval=3.0,
        )
    )


def main() -> int:
    args = parse_args()
    config = Config()
    sandbox: Sandbox | None = None
    snapshot_id: str | None = None
    built_template_id: str | None = None
    cleaned = False
    result: dict[str, object] = {
        "source_commit": os.environ.get("CUBE_COMMIT", ""),
        "mode": args.mode,
        "metrics_url": args.metrics_url,
        "samples": {},
    }

    try:
        template_id = args.template
        if args.image:
            built_template_id = build_template(args, config)
            template_id = built_template_id
        result["template_id"] = template_id

        log(f"creating sandbox from template {template_id}")
        sandbox = Sandbox.create(template=template_id, timeout=900, config=config)
        sandbox_id = sandbox.sandbox_id
        result["sandbox_id"] = sandbox_id

        require_guest = args.mode == "full"
        initial = wait_for(
            "initial resource sample",
            args.poll_timeout,
            lambda: snapshot(args.metrics_url, sandbox_id, require_guest=require_guest),
        )
        assert isinstance(initial, dict)
        result["samples"]["initial"] = initial

        if args.mode == "capability-unavailable":
            result["capability_fail_closed"] = {
                "guest_series_present": False,
                "host_series_present": True,
            }
            return 0

        sandbox = wait_for_data_plane(sandbox_id, config, args.poll_timeout)

        initial_epoch = initial[GUEST_EPOCH]
        initial_guest_cpu = initial[GUEST_CPU]
        initial_host_cpu = initial[HOST_CPU]
        initial_memory = initial[GUEST_MEMORY]

        log("running controlled CPU load through the SDK data plane")
        run_workload(
            sandbox,
            "import time\nend=time.perf_counter()+%r\nx=0\nwhile time.perf_counter()<end:\n x=(x+1)%%1000003\nprint(x)"
            % args.cpu_seconds,
            timeout=args.cpu_seconds + 30,
        )
        after_cpu = wait_for(
            "guest CPU counter increase",
            args.poll_timeout,
            lambda: (
                values
                if (values := snapshot(args.metrics_url, sandbox_id, require_guest=True))
                and values[GUEST_CPU] > initial_guest_cpu + 0.05
                else None
            ),
        )
        assert isinstance(after_cpu, dict)
        result["samples"]["after_cpu"] = after_cpu

        log("holding guest memory while polling the cached endpoint")
        memory_code = (
            "import time\n"
            f"buf=bytearray({args.load_memory_mib}*1024*1024)\n"
            "for i in range(0,len(buf),4096): buf[i]=1\n"
            f"time.sleep({args.memory_hold_seconds!r})\n"
            "print(len(buf))"
        )
        with ThreadPoolExecutor(max_workers=1) as pool:
            future = pool.submit(
                run_workload,
                sandbox,
                memory_code,
                timeout=args.memory_hold_seconds + 30,
            )
            during_memory = wait_for(
                "guest memory increase",
                min(args.poll_timeout, args.memory_hold_seconds + 5),
                lambda: (
                    values
                    if (values := snapshot(args.metrics_url, sandbox_id, require_guest=True))
                    and values[GUEST_MEMORY] > initial_memory + args.load_memory_mib * 1024 * 1024 / 2
                    else None
                ),
                interval=0.5,
            )
            future.result()
        assert isinstance(during_memory, dict)
        result["samples"]["during_memory"] = during_memory

        log("creating snapshot, advancing CPU, and rolling back")
        checkpoint = sandbox.create_snapshot()
        snapshot_id = checkpoint.snapshot_id
        result["snapshot_id"] = snapshot_id
        before_rollback = wait_for(
            "post-snapshot sample",
            args.poll_timeout,
            lambda: snapshot(args.metrics_url, sandbox_id, require_guest=True),
        )
        assert isinstance(before_rollback, dict)
        run_workload(
            sandbox,
            "import time\nend=time.perf_counter()+1.5\nwhile time.perf_counter()<end: pass",
            timeout=30,
        )
        sandbox.rollback(snapshot_id)
        after_rollback = wait_for(
            "new rollback metrics epoch",
            args.poll_timeout,
            lambda: (
                values
                if (values := snapshot(args.metrics_url, sandbox_id, require_guest=True))
                and values[GUEST_EPOCH] > initial_epoch
                else None
            ),
        )
        assert isinstance(after_rollback, dict)
        sandbox = wait_for_data_plane(sandbox_id, config, args.poll_timeout)
        if after_rollback[HOST_CPU] < initial_host_cpu:
            raise AssertionError("host_sandbox CPU counter regressed across rollback")
        result["samples"]["before_rollback"] = before_rollback
        result["samples"]["after_rollback"] = after_rollback

        log("validating pause removes series and resume keeps the rollback epoch")
        sandbox.pause(timeout=args.poll_timeout)
        wait_for(
            "resource series removal while paused",
            args.poll_timeout,
            lambda: no_series(args.metrics_url, sandbox_id),
        )
        sandbox = Sandbox.connect(sandbox_id, config=config)
        sandbox = wait_for_data_plane(sandbox_id, config, args.poll_timeout)
        after_resume = wait_for(
            "resource series after resume",
            args.poll_timeout,
            lambda: snapshot(args.metrics_url, sandbox_id, require_guest=True),
        )
        assert isinstance(after_resume, dict)
        if after_resume[GUEST_EPOCH] != after_rollback[GUEST_EPOCH]:
            raise AssertionError("pause/resume changed the guest metrics epoch")
        result["samples"]["after_resume"] = after_resume

        log("destroying sandbox and waiting for cached series cleanup")
        sandbox.kill()
        sandbox = None
        wait_for(
            "resource series removal after delete",
            args.delete_timeout,
            lambda: no_series(args.metrics_url, sandbox_id),
        )
        cleaned = True
        result["cleanup"] = "complete"
        return 0
    finally:
        if not args.keep_resources:
            if sandbox is not None:
                try:
                    sandbox.kill()
                except Exception as exc:  # noqa: BLE001 - best-effort cleanup
                    result.setdefault("cleanup_errors", []).append(f"sandbox: {exc}")
            if snapshot_id:
                try:
                    Sandbox.delete_snapshot(snapshot_id, config=config)
                except Exception as exc:  # noqa: BLE001 - best-effort cleanup
                    result.setdefault("cleanup_errors", []).append(f"snapshot: {exc}")
            if built_template_id:
                try:
                    Template.delete(built_template_id, config=config)
                except Exception as exc:  # noqa: BLE001 - best-effort cleanup
                    result.setdefault("cleanup_errors", []).append(f"template: {exc}")
        elif not cleaned:
            result["cleanup"] = "kept for debugging"

        encoded = json.dumps(result, indent=2, sort_keys=True)
        print(encoded)
        if args.output:
            args.output.parent.mkdir(parents=True, exist_ok=True)
            args.output.write_text(encoded + "\n", encoding="utf-8")


if __name__ == "__main__":
    raise SystemExit(main())
